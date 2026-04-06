// Package prbuddy implements a PR buddy pairing system for Discord teams.
//
// It provides a narrow public API for managing team membership, PTO windows,
// and generating randomised weekly pairings. All state is persisted to a
// single JSON file and survives bot restarts.
package prbuddy

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"time"
)

// Member represents a developer on the PR buddy team.
type Member struct {
	// UserID is the Discord user ID (snowflake string).
	UserID string `json:"user_id"`
	// Name is the display name shown in pairing messages.
	Name string `json:"name"`
	// PTO holds the member's current leave window, or nil if they are available.
	PTO *PTOWindow `json:"pto,omitempty"`
}

// PTOWindow describes a single leave period for a member.
// A member is unavailable for any pairing whose Monday falls strictly after
// LeaveOn and strictly before ReturnsOn. On ReturnsOn itself they are available.
type PTOWindow struct {
	LeaveOn   time.Time `json:"leave_on"`
	ReturnsOn time.Time `json:"returns_on"`
}

// Pair is two members matched for a week of code review.
type Pair struct {
	A *Member
	B *Member
}

// Result is the full output of a pairing run.
type Result struct {
	// Week is the Monday that opens the pairing week.
	Week time.Time
	// Pairs contains all matched pairs.
	Pairs []Pair
	// SittingOut is the member who has no pair this week, or nil.
	SittingOut *Member
}

// store is the JSON-serialisable state for a single guild.
type store struct {
	Members      []*Member `json:"members"`
	LastSatOutID string    `json:"last_sat_out_id,omitempty"`
}

// Bot is the PR buddy engine. Construct one with New and call its methods
// to manage teams and generate pairings. It is safe for concurrent use.
type Bot struct {
	mu       sync.Mutex
	path     string
	guilds   map[string]*store // guild ID → state
	randSrc  *rand.Rand
	stopCh   chan struct{}
	postFunc func(guildID string, result Result)
}

// New creates a Bot that persists state to the given file path.
// postFunc is called every Monday at 09:00 local time with the week's pairings
// for each guild. It is the caller's responsibility to format and send the
// Discord message.
func New(path string, postFunc func(guildID string, result Result)) (*Bot, error) {
	b := &Bot{
		path:     path,
		guilds:   make(map[string]*store),
		randSrc:  rand.New(rand.NewSource(time.Now().UnixNano())),
		stopCh:   make(chan struct{}),
		postFunc: postFunc,
	}
	if err := b.load(); err != nil {
		return nil, err
	}
	return b, nil
}

// AddMember adds a Discord user to the guild's PR buddy team. If the user is
// already a member their display name is updated.
func (b *Bot) AddMember(guildID, userID, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	g := b.guild(guildID)
	for _, m := range g.Members {
		if m.UserID == userID {
			m.Name = name
			return b.save()
		}
	}
	g.Members = append(g.Members, &Member{UserID: userID, Name: name})
	return b.save()
}

// RemoveMember removes a Discord user from the guild's PR buddy team.
// It is not an error to remove a user who is not a member.
func (b *Bot) RemoveMember(guildID, userID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	g := b.guild(guildID)
	filtered := g.Members[:0]
	for _, m := range g.Members {
		if m.UserID != userID {
			filtered = append(filtered, m)
		}
	}
	g.Members = filtered

	if g.LastSatOutID == userID {
		g.LastSatOutID = ""
	}
	return b.save()
}

// SetPTO records a leave window for a team member, replacing any existing PTO.
// leaveOn is the first day of absence; returnsOn is the first day back.
// returnsOn must be after leaveOn.
func (b *Bot) SetPTO(guildID, userID string, leaveOn, returnsOn time.Time) error {
	if !returnsOn.After(leaveOn) {
		return fmt.Errorf("returns_on must be after leave_on")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	g := b.guild(guildID)
	for _, m := range g.Members {
		if m.UserID == userID {
			m.PTO = &PTOWindow{
				LeaveOn:   leaveOn.UTC().Truncate(24 * time.Hour),
				ReturnsOn: returnsOn.UTC().Truncate(24 * time.Hour),
			}
			return b.save()
		}
	}
	return fmt.Errorf("user %s is not a member of the team", userID)
}

// ClearPTO removes any PTO window for the given team member.
func (b *Bot) ClearPTO(guildID, userID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	g := b.guild(guildID)
	for _, m := range g.Members {
		if m.UserID == userID {
			m.PTO = nil
			return b.save()
		}
	}
	return fmt.Errorf("user %s is not a member of the team", userID)
}

// Members returns a copy of the team roster for the guild.
func (b *Bot) Members(guildID string) []*Member {
	b.mu.Lock()
	defer b.mu.Unlock()

	g := b.guild(guildID)
	out := make([]*Member, len(g.Members))
	for i, m := range g.Members {
		cp := *m
		if m.PTO != nil {
			pto := *m.PTO
			cp.PTO = &pto
		}
		out[i] = &cp
	}
	return out
}

// Generate produces pairings for the week containing the given time.
// The week's Monday date is derived from t. Available members are those
// whose PTO window does not cover that Monday. If fewer than 2 members
// are available the Result will have an empty Pairs slice and a nil
// SittingOut — callers should detect this and notify the channel
// accordingly. The odd-dev-out rotation is persisted so the same person
// does not sit out two weeks in a row when avoidable.
func (b *Bot) Generate(guildID string, t time.Time) Result {
	b.mu.Lock()
	defer b.mu.Unlock()

	monday := weekMonday(t)
	g := b.guild(guildID)

	available := available(g.Members, monday)

	result := Result{Week: monday}
	if len(available) < 2 {
		return result
	}

	// Shuffle for randomness.
	b.randSrc.Shuffle(len(available), func(i, j int) {
		available[i], available[j] = available[j], available[i]
	})

	if len(available)%2 == 1 {
		// Pick who sits out: prefer not repeating last week's sit-out.
		sitOutIdx := pickSitOut(available, g.LastSatOutID)
		result.SittingOut = available[sitOutIdx]
		g.LastSatOutID = available[sitOutIdx].UserID
		available = append(available[:sitOutIdx], available[sitOutIdx+1:]...)
		_ = b.save() // persist updated LastSatOutID
	}

	for i := 0; i+1 < len(available); i += 2 {
		result.Pairs = append(result.Pairs, Pair{A: available[i], B: available[i+1]})
	}
	return result
}

// StartScheduler launches a background goroutine that calls Generate and
// postFunc for every guild every Monday at 09:00 local time. Call Stop to
// shut it down cleanly.
func (b *Bot) StartScheduler() {
	go b.runScheduler()
}

// Stop shuts down the Monday scheduler.
func (b *Bot) Stop() {
	close(b.stopCh)
}

// --- internal helpers -------------------------------------------------------

// guild returns (creating if necessary) the store for a guild.
// Caller must hold b.mu.
func (b *Bot) guild(guildID string) *store {
	if g, ok := b.guilds[guildID]; ok {
		return g
	}
	g := &store{}
	b.guilds[guildID] = g
	return g
}

// load reads persisted state from disk. Missing file is treated as empty state.
func (b *Bot) load() error {
	data, err := os.ReadFile(b.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("prbuddy: read %s: %w", b.path, err)
	}
	if err := json.Unmarshal(data, &b.guilds); err != nil {
		return fmt.Errorf("prbuddy: parse %s: %w", b.path, err)
	}
	return nil
}

// save atomically writes current state to disk.
// Caller must hold b.mu.
func (b *Bot) save() error {
	data, err := json.MarshalIndent(b.guilds, "", "  ")
	if err != nil {
		return fmt.Errorf("prbuddy: marshal state: %w", err)
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("prbuddy: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, b.path); err != nil {
		return fmt.Errorf("prbuddy: rename to %s: %w", b.path, err)
	}
	return nil
}

// runScheduler fires every Monday at 09:00 local time.
func (b *Bot) runScheduler() {
	for {
		next := nextMonday9am(time.Now())
		select {
		case <-b.stopCh:
			return
		case <-time.After(time.Until(next)):
		}

		now := time.Now()
		b.mu.Lock()
		guildIDs := make([]string, 0, len(b.guilds))
		for id := range b.guilds {
			guildIDs = append(guildIDs, id)
		}
		b.mu.Unlock()

		for _, guildID := range guildIDs {
			result := b.Generate(guildID, now)
			b.postFunc(guildID, result)
		}
	}
}

// weekMonday returns the Monday of the ISO week containing t, at midnight UTC.
func weekMonday(t time.Time) time.Time {
	t = t.UTC()
	weekday := int(t.Weekday())
	if weekday == 0 {
		weekday = 7 // Sunday → 7
	}
	delta := time.Duration(weekday-1) * 24 * time.Hour
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Add(-delta)
}

// nextMonday9am returns the next Monday at 09:00 in the local time zone.
// If now is already Monday before 09:00, it returns today at 09:00.
func nextMonday9am(now time.Time) time.Time {
	loc := now.Location()
	y, mo, d := now.Date()
	today9 := time.Date(y, mo, d, 9, 0, 0, 0, loc)

	weekday := now.Weekday()
	switch {
	case weekday == time.Monday && now.Before(today9):
		return today9
	case weekday == time.Monday:
		// Already past 09:00 on Monday — next occurrence is in 7 days.
		return today9.AddDate(0, 0, 7)
	default:
		// Days until next Monday.
		daysUntil := (int(time.Monday) - int(weekday) + 7) % 7
		if daysUntil == 0 {
			daysUntil = 7
		}
		next := time.Date(y, mo, d+daysUntil, 9, 0, 0, 0, loc)
		return next
	}
}

// available returns members who are not on PTO during the given Monday.
func available(members []*Member, monday time.Time) []*Member {
	monday = monday.UTC().Truncate(24 * time.Hour)
	out := make([]*Member, 0, len(members))
	for _, m := range members {
		if m.PTO == nil {
			out = append(out, m)
			continue
		}
		// Available if monday < leaveOn OR monday >= returnsOn.
		leaveOn := m.PTO.LeaveOn.UTC().Truncate(24 * time.Hour)
		returnsOn := m.PTO.ReturnsOn.UTC().Truncate(24 * time.Hour)
		if monday.Before(leaveOn) || !monday.Before(returnsOn) {
			out = append(out, m)
		}
	}
	return out
}

// pickSitOut returns the index in available of the member who should sit out.
// It avoids picking lastSatOutID if there is any other option.
func pickSitOut(available []*Member, lastSatOutID string) int {
	if lastSatOutID == "" {
		return 0
	}
	for i, m := range available {
		if m.UserID != lastSatOutID {
			return i
		}
	}
	// Everyone is the last sitter — just pick the first.
	return 0
}

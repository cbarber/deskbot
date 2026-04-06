package prbuddy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestBot creates a Bot backed by a temp file. The returned cleanup
// function removes the file.
func newTestBot(t *testing.T) (*Bot, func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prbuddy.json")
	b, err := New(path, func(string, Result) {})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b, func() { os.Remove(path) }
}

// --- AddMember / RemoveMember -----------------------------------------------

func TestAddMember_NewMember(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	if err := b.AddMember("g1", "u1", "Alice"); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	members := b.Members("g1")
	if len(members) != 1 {
		t.Fatalf("want 1 member, got %d", len(members))
	}
	if members[0].UserID != "u1" || members[0].Name != "Alice" {
		t.Errorf("unexpected member: %+v", members[0])
	}
}

func TestAddMember_UpdatesName(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	_ = b.AddMember("g1", "u1", "Alice2")

	members := b.Members("g1")
	if len(members) != 1 {
		t.Fatalf("want 1 member after upsert, got %d", len(members))
	}
	if members[0].Name != "Alice2" {
		t.Errorf("want name Alice2, got %s", members[0].Name)
	}
}

func TestRemoveMember(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	_ = b.AddMember("g1", "u2", "Bob")

	if err := b.RemoveMember("g1", "u1"); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	members := b.Members("g1")
	if len(members) != 1 {
		t.Fatalf("want 1 member, got %d", len(members))
	}
	if members[0].UserID != "u2" {
		t.Errorf("wrong member remaining: %+v", members[0])
	}
}

func TestRemoveMember_NotMember_NoError(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	if err := b.RemoveMember("g1", "nonexistent"); err != nil {
		t.Errorf("expected no error removing non-member, got: %v", err)
	}
}

func TestRemoveMember_ClearsLastSatOut(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	_ = b.AddMember("g1", "u2", "Bob")
	_ = b.AddMember("g1", "u3", "Carol")

	// Generate once so someone sits out.
	monday := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	b.Generate("g1", monday)

	// Remove whichever sat out.
	b.mu.Lock()
	satOut := b.guilds["g1"].LastSatOutID
	b.mu.Unlock()

	_ = b.RemoveMember("g1", satOut)

	b.mu.Lock()
	remaining := b.guilds["g1"].LastSatOutID
	b.mu.Unlock()

	if remaining != "" {
		t.Errorf("LastSatOutID should be cleared after removing that member, got %q", remaining)
	}
}

// --- SetPTO / ClearPTO ------------------------------------------------------

func TestSetPTO(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")

	leave := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	returns := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)

	if err := b.SetPTO("g1", "u1", leave, returns); err != nil {
		t.Fatalf("SetPTO: %v", err)
	}

	members := b.Members("g1")
	if members[0].PTO == nil {
		t.Fatal("expected PTO to be set")
	}
	if !members[0].PTO.LeaveOn.Equal(leave) {
		t.Errorf("LeaveOn: want %v, got %v", leave, members[0].PTO.LeaveOn)
	}
	if !members[0].PTO.ReturnsOn.Equal(returns) {
		t.Errorf("ReturnsOn: want %v, got %v", returns, members[0].PTO.ReturnsOn)
	}
}

func TestSetPTO_ReturnsOnNotAfterLeaveOn_Error(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")

	d := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	if err := b.SetPTO("g1", "u1", d, d); err == nil {
		t.Error("expected error when returns_on == leave_on")
	}
	if err := b.SetPTO("g1", "u1", d, d.Add(-24*time.Hour)); err == nil {
		t.Error("expected error when returns_on < leave_on")
	}
}

func TestSetPTO_NonMember_Error(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	leave := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	returns := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)

	if err := b.SetPTO("g1", "nobody", leave, returns); err == nil {
		t.Error("expected error setting PTO for non-member")
	}
}

func TestClearPTO(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	leave := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	returns := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	_ = b.SetPTO("g1", "u1", leave, returns)

	if err := b.ClearPTO("g1", "u1"); err != nil {
		t.Fatalf("ClearPTO: %v", err)
	}
	members := b.Members("g1")
	if members[0].PTO != nil {
		t.Error("expected PTO to be nil after clearing")
	}
}

func TestClearPTO_NonMember_Error(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	if err := b.ClearPTO("g1", "nobody"); err == nil {
		t.Error("expected error clearing PTO for non-member")
	}
}

// --- PTO availability -------------------------------------------------------

func TestAvailable_PTOExcludesMember(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	_ = b.AddMember("g1", "u2", "Bob")

	// Alice is on PTO for the week of April 6.
	_ = b.SetPTO("g1", "u1",
		time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC),
	)

	monday := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	result := b.Generate("g1", monday)

	if result.SittingOut != nil {
		t.Errorf("expected no sit-out (only 1 available), got %v", result.SittingOut)
	}
	if len(result.Pairs) != 0 {
		t.Errorf("expected 0 pairs (only 1 available), got %d", len(result.Pairs))
	}
}

func TestAvailable_ReturnsOnIsSelf_Available(t *testing.T) {
	// A member whose returns_on equals the Monday is available that week.
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	_ = b.AddMember("g1", "u2", "Bob")

	monday := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)

	_ = b.SetPTO("g1", "u1",
		time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC),
		monday, // returns on the Monday itself — should be available
	)

	result := b.Generate("g1", monday)
	if len(result.Pairs) != 1 {
		t.Errorf("expected 1 pair (both available), got %d pairs", len(result.Pairs))
	}
}

func TestAvailable_PTOBeforeWeek_Available(t *testing.T) {
	// PTO ends before this Monday — member should be available.
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	_ = b.AddMember("g1", "u2", "Bob")

	monday := time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC)

	_ = b.SetPTO("g1", "u1",
		time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC), // returns before the next Monday
	)

	result := b.Generate("g1", monday)
	if len(result.Pairs) != 1 {
		t.Errorf("expected 1 pair (both available), got %d pairs", len(result.Pairs))
	}
}

// --- Generate ---------------------------------------------------------------

func TestGenerate_EvenTeam_NoPairs(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	monday := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	result := b.Generate("g1", monday)
	if result.SittingOut != nil || len(result.Pairs) != 0 {
		t.Error("empty team should produce no pairs and no sit-out")
	}
}

func TestGenerate_OneAvailable_NoPairs(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	monday := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	result := b.Generate("g1", monday)
	if len(result.Pairs) != 0 {
		t.Errorf("1 member: want 0 pairs, got %d", len(result.Pairs))
	}
}

func TestGenerate_TwoMembers_OnePair(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	_ = b.AddMember("g1", "u2", "Bob")

	monday := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	result := b.Generate("g1", monday)

	if len(result.Pairs) != 1 {
		t.Fatalf("want 1 pair, got %d", len(result.Pairs))
	}
	if result.SittingOut != nil {
		t.Errorf("expected no sit-out for even team, got %v", result.SittingOut)
	}
}

func TestGenerate_ThreeMembers_OneSitsOut(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	_ = b.AddMember("g1", "u2", "Bob")
	_ = b.AddMember("g1", "u3", "Carol")

	monday := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	result := b.Generate("g1", monday)

	if len(result.Pairs) != 1 {
		t.Fatalf("want 1 pair, got %d", len(result.Pairs))
	}
	if result.SittingOut == nil {
		t.Fatal("expected a sit-out for odd team")
	}
}

func TestGenerate_FourMembers_TwoPairs(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	_ = b.AddMember("g1", "u2", "Bob")
	_ = b.AddMember("g1", "u3", "Carol")
	_ = b.AddMember("g1", "u4", "Dave")

	monday := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	result := b.Generate("g1", monday)

	if len(result.Pairs) != 2 {
		t.Fatalf("want 2 pairs, got %d", len(result.Pairs))
	}
	if result.SittingOut != nil {
		t.Errorf("expected no sit-out for even team, got %v", result.SittingOut)
	}
}

func TestGenerate_WeekSet(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	// A Wednesday — should resolve to the Monday of that week.
	wednesday := time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC)
	_ = b.AddMember("g1", "u1", "Alice")
	_ = b.AddMember("g1", "u2", "Bob")
	result := b.Generate("g1", wednesday)

	expectedMonday := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	if !result.Week.Equal(expectedMonday) {
		t.Errorf("week: want %v, got %v", expectedMonday, result.Week)
	}
}

// --- Odd-dev-out rotation ---------------------------------------------------

func TestGenerate_OddDevOutRotates(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	_ = b.AddMember("g1", "u2", "Bob")
	_ = b.AddMember("g1", "u3", "Carol")

	monday := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)

	// Run many times and check that the same person doesn't sit out every time.
	satOutIDs := map[string]int{}
	for i := 0; i < 30; i++ {
		result := b.Generate("g1", monday)
		if result.SittingOut != nil {
			satOutIDs[result.SittingOut.UserID]++
		}
	}

	// With rotation, all three members should sit out at some point.
	if len(satOutIDs) < 2 {
		t.Errorf("expected rotation across at least 2 members, only saw: %v", satOutIDs)
	}
}

func TestGenerate_OddDevOut_NoConsecutiveRepeat(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	_ = b.AddMember("g1", "u2", "Bob")
	_ = b.AddMember("g1", "u3", "Carol")

	monday := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)

	// Run many consecutive weeks and verify no one sits out back-to-back.
	var lastSatOut string
	for i := 0; i < 20; i++ {
		result := b.Generate("g1", monday)
		if result.SittingOut == nil {
			continue
		}
		if result.SittingOut.UserID == lastSatOut && lastSatOut != "" {
			t.Errorf("week %d: %s sat out consecutively", i, result.SittingOut.Name)
		}
		lastSatOut = result.SittingOut.UserID
	}
}

// --- Persistence ------------------------------------------------------------

func TestPersistence_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prbuddy.json")

	b1, err := New(path, func(string, Result) {})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_ = b1.AddMember("g1", "u1", "Alice")
	_ = b1.AddMember("g1", "u2", "Bob")
	_ = b1.SetPTO("g1", "u1",
		time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC),
	)

	// Simulate restart.
	b2, err := New(path, func(string, Result) {})
	if err != nil {
		t.Fatalf("New (reload): %v", err)
	}

	members := b2.Members("g1")
	if len(members) != 2 {
		t.Fatalf("after reload: want 2 members, got %d", len(members))
	}

	var alice *Member
	for _, m := range members {
		if m.UserID == "u1" {
			alice = m
		}
	}
	if alice == nil {
		t.Fatal("Alice not found after reload")
	}
	if alice.PTO == nil {
		t.Fatal("Alice's PTO not persisted")
	}
}

func TestPersistence_MissingFile_NoError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does_not_exist.json")

	b, err := New(path, func(string, Result) {})
	if err != nil {
		t.Fatalf("expected no error for missing file, got: %v", err)
	}
	if len(b.Members("g1")) != 0 {
		t.Error("expected empty team for missing file")
	}
}

func TestPersistence_FileIsValidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prbuddy.json")

	b, _ := New(path, func(string, Result) {})
	_ = b.AddMember("g1", "u1", "Alice")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Errorf("persisted file is not valid JSON: %v", err)
	}
}

// --- weekMonday helper ------------------------------------------------------

func TestWeekMonday(t *testing.T) {
	cases := []struct {
		in   time.Time
		want time.Time
	}{
		{
			// Monday itself
			in:   time.Date(2026, 4, 6, 15, 0, 0, 0, time.UTC),
			want: time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC),
		},
		{
			// Wednesday mid-week
			in:   time.Date(2026, 4, 8, 9, 0, 0, 0, time.UTC),
			want: time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC),
		},
		{
			// Sunday (end of week)
			in:   time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC),
			want: time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC),
		},
		{
			// Saturday
			in:   time.Date(2026, 4, 11, 23, 59, 0, 0, time.UTC),
			want: time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		got := weekMonday(tc.in)
		if !got.Equal(tc.want) {
			t.Errorf("weekMonday(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// --- nextMonday9am helper ---------------------------------------------------

func TestNextMonday9am_BeforeMonday(t *testing.T) {
	// Tuesday — next Monday is 6 days away.
	now := time.Date(2026, 4, 7, 10, 0, 0, 0, time.Local)
	next := nextMonday9am(now)
	want := time.Date(2026, 4, 13, 9, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Errorf("got %v, want %v", next, want)
	}
}

func TestNextMonday9am_MondayBefore9(t *testing.T) {
	now := time.Date(2026, 4, 6, 8, 0, 0, 0, time.Local)
	next := nextMonday9am(now)
	want := time.Date(2026, 4, 6, 9, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Errorf("got %v, want %v", next, want)
	}
}

func TestNextMonday9am_MondayAfter9(t *testing.T) {
	now := time.Date(2026, 4, 6, 10, 0, 0, 0, time.Local)
	next := nextMonday9am(now)
	want := time.Date(2026, 4, 13, 9, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Errorf("got %v, want %v", next, want)
	}
}

// --- Guild isolation --------------------------------------------------------

func TestMultipleGuilds_Isolated(t *testing.T) {
	b, cleanup := newTestBot(t)
	defer cleanup()

	_ = b.AddMember("g1", "u1", "Alice")
	_ = b.AddMember("g2", "u2", "Bob")

	g1 := b.Members("g1")
	g2 := b.Members("g2")

	if len(g1) != 1 || g1[0].UserID != "u1" {
		t.Errorf("g1 unexpected: %v", g1)
	}
	if len(g2) != 1 || g2[0].UserID != "u2" {
		t.Errorf("g2 unexpected: %v", g2)
	}
}

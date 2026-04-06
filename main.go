package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/cbarber/deskbot/prbuddy"
)

const (
	USER_DESK_PERMISSIONS int64 = discordgo.PermissionViewChannel | discordgo.PermissionManageChannels
	BOT_DESK_PERMISSIONS  int64 = discordgo.PermissionViewChannel
)

var (
	token                    string
	guildToDeskCategory      *sync.Map
	guildChannelMembersMutex *sync.Mutex
	guildChannelMembers      map[string](map[string]int)

	buddy *prbuddy.Bot
)

func init() {
	flag.StringVar(&token, "t", "", "Bot Token")
	flag.Parse()
}

func main() {
	if token == "" {
		fmt.Println("No token provided. Please run: airhorn -t <bot token>")
		return
	}

	discord, err := discordgo.New("Bot " + token)
	if err != nil {
		fmt.Println("Error creating Discord session: ", err)
		return
	}

	buddy, err = prbuddy.New("./prbuddy.json", func(guildID string, result prbuddy.Result) {
		postPairings(discord, guildID, result)
	})
	if err != nil {
		fmt.Println("Error initialising PR buddy:", err)
		return
	}

	discord.AddHandler(ready)
	discord.AddHandler(guildCreate)
	discord.AddHandler(guildMemberAdd)
	discord.AddHandler(voiceStateUpdate)
	discord.AddHandler(interactionCreate)

	discord.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMembers | discordgo.IntentsGuildVoiceStates

	// Open the websocket and begin listening.
	err = discord.Open()
	if err != nil {
		fmt.Println("Error opening Discord session: ", err)
		return
	}

	if err := registerCommands(discord); err != nil {
		fmt.Println("Error registering slash commands:", err)
		// Non-fatal — bot still works without slash commands.
	}

	buddy.StartScheduler()

	fmt.Println("Deskbot is now running.  Press CTRL-C to exit.")

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	fmt.Println("Closing discord session...")
	buddy.Stop()
	endSession(discord)
	discord.Close()
}

// ---------------------------------------------------------------------------
// Discord event handlers
// ---------------------------------------------------------------------------

func ready(s *discordgo.Session, event *discordgo.Ready) {
	fmt.Println("ready")

	guildToDeskCategory = new(sync.Map)
	guildChannelMembersMutex = new(sync.Mutex)
	guildChannelMembers = make(map[string](map[string]int))
}

func guildCreate(s *discordgo.Session, event *discordgo.GuildCreate) {
	if event.Unavailable {
		return
	}

	fmt.Println("guildCreate", event.Name)

	defaultChannelId := event.SystemChannelID

	if defaultChannelId == "" {
		fmt.Println("Failed to find default channel for guild")
		return
	}

	var deskCategoryId string
	for _, channel := range event.Channels {
		if strings.ToLower(channel.Name) == "desks" && channel.Type == discordgo.ChannelTypeGuildCategory {
			deskCategoryId = channel.ID
			break
		}
	}

	if deskCategoryId != "" {
		fmt.Printf("Deskbot found the DESKS category in %v. Storing (guildId: %s, deskCategoryId: %s)\n", event.Name, event.ID, deskCategoryId)
		guildToDeskCategory.Store(event.ID, deskCategoryId)
	} else {
		fmt.Printf("Deskbot failed to find the DESKS category in %v\n", event.Name)
		return
	}

	guildChannelMembersMutex.Lock()
	guildChannelMembers[event.ID] = make(map[string]int)
	for _, voiceState := range event.VoiceStates {
		guildChannelMembers[event.ID][voiceState.ChannelID] += 1
	}
	guildChannelMembersMutex.Unlock()

	// TODO: paginate when mojo passes 1000 employees
	members, err := s.GuildMembers(event.ID, "", 1000)
	if err != nil {
		fmt.Printf("Deskbot failed to fetch the first 1000 member of %s\n", event.Name)
		return
	}

	for _, member := range members {
		if member.User.Bot || member.User.System {
			continue
		}

		deskChannel := findUserDeskChannel(event.Channels, deskCategoryId, member.User.ID, s.State.User.ID)
		if deskChannel == nil {
			fmt.Printf("Missing desk channel for user %s\n", member.DisplayName())
			err := createDeskChannel(s, event.ID, member.User.ID, member.DisplayName(), deskCategoryId)
			if err != nil {
				fmt.Printf("Failed to create desk channel for user %s: %v\n", member.DisplayName(), err)
				continue
			}
		} else {
			if err := resetDeskPermissions(s, deskChannel, member.User.ID); err != nil {
				fmt.Printf("Failed to reset desk permissions for user %s: %v\n", member.DisplayName(), err)
				continue
			}

			guildChannelMembersMutex.Lock()
			if guildChannelMembers[event.ID][deskChannel.ID] != 0 {
				showDeskChannel(s, event.Guild, deskChannel)
			} else {
				hideDeskChannel(s, event.Guild, deskChannel)
			}
			guildChannelMembersMutex.Unlock()
		}
	}
}

func endSession(s *discordgo.Session) {
	for _, guild := range s.State.Guilds {
		showAllDeskChannels(s, guild)
	}
}

func showAllDeskChannels(s *discordgo.Session, guild *discordgo.Guild) {
	maybeDeskCategoryId, ok := guildToDeskCategory.Load(guild.ID)
	if !ok {
		return
	}
	deskCategoryId := maybeDeskCategoryId.(string)

	for _, channel := range guild.Channels {
		if channel.ParentID == deskCategoryId && getChannelOwner(channel, s.State.User.ID) != "" {
			showDeskChannel(s, guild, channel)
		}
	}
}

func guildMemberAdd(s *discordgo.Session, event *discordgo.GuildMemberAdd) {
	name := event.DisplayName()

	fmt.Println("guildMemberAdd", name)

	maybeDeskCategoryId, ok := guildToDeskCategory.Load(event.GuildID)
	if !ok {
		fmt.Println("Failed to find deskCategory for guildId", event.GuildID)
		return
	}
	deskCategoryId := maybeDeskCategoryId.(string)

	channels, err := s.GuildChannels(event.GuildID)
	if err != nil {
		fmt.Println("Failed to fetch channels", err)
		return
	}

	existingDeskChannel := findUserDeskChannel(channels, deskCategoryId, event.User.ID, s.State.User.ID)

	if existingDeskChannel != nil {
		return
	}

	err = createDeskChannel(s, event.GuildID, event.User.ID, name, deskCategoryId)
	if err != nil {
		fmt.Println("Failed to create channel", err)
		return
	}

	guild, err := s.Guild(event.GuildID)
	if err != nil {
		fmt.Println("Failed to find guild", err)
		return
	}

	_, err = s.ChannelMessageSend(guild.SystemChannelID, fmt.Sprintf("Created a desk for %s", name))
	if err != nil {
		fmt.Println("Failed to send created desk message", err)
	}
}

// Show and hide user desk voice channels when connected to and disconnected from.
func voiceStateUpdate(s *discordgo.Session, event *discordgo.VoiceStateUpdate) {
	fmt.Println("voiceStateUpdate", event.ChannelID)
	guild, err := s.Guild(event.GuildID)
	if err != nil {
		fmt.Println("Failed to find guild", event.GuildID, err)
		return
	}

	maybeDeskCategoryId, ok := guildToDeskCategory.Load(event.GuildID)
	if !ok {
		fmt.Println("Failed to find deskCategory for guildId", event.GuildID)
		return
	}
	deskCategoryId := maybeDeskCategoryId.(string)

	// Check if user connected to a new channel
	if event.ChannelID != "" && (event.BeforeUpdate == nil || event.BeforeUpdate.ChannelID != event.ChannelID) {
		channel, err := s.Channel(event.ChannelID)
		if err != nil {
			fmt.Println("Failed to find channel", err)
			return
		}

		if channel.ParentID != deskCategoryId {
			fmt.Println("Not a desk channel")
			return
		}

		handleDeskConnect(guild.ID, channel)
		showDeskChannel(s, guild, channel)
	}

	// Check if user disconnected from a channel
	if event.BeforeUpdate != nil && event.BeforeUpdate.ChannelID != "" && event.BeforeUpdate.ChannelID != event.ChannelID {
		channel, err := s.Channel(event.BeforeUpdate.ChannelID)
		if err != nil {
			fmt.Println("Failed to find channel", err)
			return
		}

		if channel.ParentID != deskCategoryId {
			fmt.Println("Not a desk channel")
			return
		}

		channelMembers := handleDeskDisconnect(guild.ID, channel)
		if channelMembers == 0 {
			hideDeskChannel(s, guild, channel)
		}
	}
}

// ---------------------------------------------------------------------------
// Slash command registration and dispatch
// ---------------------------------------------------------------------------

// prbuddyCommand is the full /prbuddy command definition registered with Discord.
var prbuddyCommand = &discordgo.ApplicationCommand{
	Name:        "prbuddy",
	Description: "PR buddy pairing system",
	Options: []*discordgo.ApplicationCommandOption{
		{
			Name:        "member",
			Description: "Manage team members",
			Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "add",
					Description: "Add a member to the PR buddy team",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Options: []*discordgo.ApplicationCommandOption{
						{
							Name:        "user",
							Description: "The Discord user to add",
							Type:        discordgo.ApplicationCommandOptionUser,
							Required:    true,
						},
					},
				},
				{
					Name:        "remove",
					Description: "Remove a member from the PR buddy team",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Options: []*discordgo.ApplicationCommandOption{
						{
							Name:        "user",
							Description: "The Discord user to remove",
							Type:        discordgo.ApplicationCommandOptionUser,
							Required:    true,
						},
					},
				},
			},
		},
		{
			Name:        "pto",
			Description: "Manage member PTO",
			Type:        discordgo.ApplicationCommandOptionSubCommandGroup,
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "set",
					Description: "Set a PTO window for a member (dates: YYYY-MM-DD)",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Options: []*discordgo.ApplicationCommandOption{
						{
							Name:        "user",
							Description: "The team member",
							Type:        discordgo.ApplicationCommandOptionUser,
							Required:    true,
						},
						{
							Name:        "leave_on",
							Description: "First day of absence (YYYY-MM-DD)",
							Type:        discordgo.ApplicationCommandOptionString,
							Required:    true,
						},
						{
							Name:        "returns_on",
							Description: "First day back (YYYY-MM-DD)",
							Type:        discordgo.ApplicationCommandOptionString,
							Required:    true,
						},
					},
				},
				{
					Name:        "clear",
					Description: "Clear a member's PTO",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Options: []*discordgo.ApplicationCommandOption{
						{
							Name:        "user",
							Description: "The team member",
							Type:        discordgo.ApplicationCommandOptionUser,
							Required:    true,
						},
					},
				},
			},
		},
		{
			Name:        "generate",
			Description: "Generate this week's PR buddy pairings now",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
		},
	},
}

func registerCommands(s *discordgo.Session) error {
	_, err := s.ApplicationCommandCreate(s.State.User.ID, "", prbuddyCommand)
	return err
}

func interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()
	if data.Name != "prbuddy" {
		return
	}
	handlePRBuddy(s, i)
}

func handlePRBuddy(s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := i.ApplicationCommandData().Options
	if len(opts) == 0 {
		respond(s, i, "Unknown subcommand.")
		return
	}

	switch opts[0].Name {
	case "member":
		handleMember(s, i, opts[0].Options)
	case "pto":
		handlePTO(s, i, opts[0].Options)
	case "generate":
		handleGenerate(s, i)
	default:
		respond(s, i, "Unknown subcommand.")
	}
}

func handleMember(s *discordgo.Session, i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption) {
	if len(opts) == 0 {
		respond(s, i, "Unknown member subcommand.")
		return
	}
	switch opts[0].Name {
	case "add":
		user := opts[0].Options[0].UserValue(s)
		name := user.Username
		if err := buddy.AddMember(i.GuildID, user.ID, name); err != nil {
			respond(s, i, fmt.Sprintf("Failed to add member: %v", err))
			return
		}
		respond(s, i, fmt.Sprintf("Added **%s** to the PR buddy team.", name))

	case "remove":
		user := opts[0].Options[0].UserValue(s)
		if err := buddy.RemoveMember(i.GuildID, user.ID); err != nil {
			respond(s, i, fmt.Sprintf("Failed to remove member: %v", err))
			return
		}
		respond(s, i, fmt.Sprintf("Removed **%s** from the PR buddy team.", user.Username))

	default:
		respond(s, i, "Unknown member subcommand.")
	}
}

func handlePTO(s *discordgo.Session, i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption) {
	if len(opts) == 0 {
		respond(s, i, "Unknown pto subcommand.")
		return
	}
	switch opts[0].Name {
	case "set":
		subOpts := opts[0].Options
		user := subOpts[0].UserValue(s)
		leaveOnStr := subOpts[1].StringValue()
		returnsOnStr := subOpts[2].StringValue()

		leaveOn, err := time.ParseInLocation("2006-01-02", leaveOnStr, time.Local)
		if err != nil {
			respond(s, i, fmt.Sprintf("Invalid leave_on date %q — use YYYY-MM-DD.", leaveOnStr))
			return
		}
		returnsOn, err := time.ParseInLocation("2006-01-02", returnsOnStr, time.Local)
		if err != nil {
			respond(s, i, fmt.Sprintf("Invalid returns_on date %q — use YYYY-MM-DD.", returnsOnStr))
			return
		}

		if err := buddy.SetPTO(i.GuildID, user.ID, leaveOn, returnsOn); err != nil {
			respond(s, i, fmt.Sprintf("Failed to set PTO: %v", err))
			return
		}
		respond(s, i, fmt.Sprintf("PTO set for **%s**: away %s → back %s.", user.Username, leaveOnStr, returnsOnStr))

	case "clear":
		user := opts[0].Options[0].UserValue(s)
		if err := buddy.ClearPTO(i.GuildID, user.ID); err != nil {
			respond(s, i, fmt.Sprintf("Failed to clear PTO: %v", err))
			return
		}
		respond(s, i, fmt.Sprintf("PTO cleared for **%s**.", user.Username))

	default:
		respond(s, i, "Unknown pto subcommand.")
	}
}

func handleGenerate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	result := buddy.Generate(i.GuildID, time.Now())
	msg := formatPairings(result)
	respond(s, i, msg)
	// Also post to #general so the team sees it.
	postPairings(s, i.GuildID, result)
}

// ---------------------------------------------------------------------------
// Pairing output helpers
// ---------------------------------------------------------------------------

// postPairings finds the guild's #general channel and posts the pairing result.
func postPairings(s *discordgo.Session, guildID string, result prbuddy.Result) {
	channels, err := s.GuildChannels(guildID)
	if err != nil {
		fmt.Println("prbuddy: failed to fetch channels:", err)
		return
	}
	var generalID string
	for _, ch := range channels {
		if ch.Type == discordgo.ChannelTypeGuildText && strings.ToLower(ch.Name) == "general" {
			generalID = ch.ID
			break
		}
	}
	if generalID == "" {
		fmt.Println("prbuddy: no #general channel found in guild", guildID)
		return
	}
	msg := formatPairings(result)
	if _, err := s.ChannelMessageSend(generalID, msg); err != nil {
		fmt.Println("prbuddy: failed to post pairings:", err)
	}
}

// formatPairings renders a Result as a human-readable Discord message.
func formatPairings(result prbuddy.Result) string {
	if len(result.Pairs) == 0 {
		return fmt.Sprintf("No PR buddy pairings this week (%s) — not enough available developers.",
			result.Week.Format("Jan 2, 2006"))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**PR Buddy pairings — week of %s**\n", result.Week.Format("Jan 2, 2006")))
	for idx, p := range result.Pairs {
		sb.WriteString(fmt.Sprintf("%d. <@%s> ↔ <@%s>\n", idx+1, p.A.UserID, p.B.UserID))
	}
	if result.SittingOut != nil {
		sb.WriteString(fmt.Sprintf("\n_<@%s> is sitting out this week._", result.SittingOut.UserID))
	}
	return sb.String()
}

// respond sends an ephemeral interaction reply.
func respond(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		fmt.Println("Failed to respond to interaction:", err)
	}
}

// ---------------------------------------------------------------------------
// Desk channel helpers (unchanged)
// ---------------------------------------------------------------------------

// If any user enters, make the desk visible.
func handleDeskConnect(guildID string, channel *discordgo.Channel) int {
	fmt.Println("User connected to desk", channel.ID)

	guildChannelMembersMutex.Lock()
	channelMembers := guildChannelMembers[guildID][channel.ID]
	channelMembers += 1
	guildChannelMembers[guildID][channel.ID] = channelMembers
	guildChannelMembersMutex.Unlock()

	return channelMembers
}

// Make desk visible to @everyone
func showDeskChannel(s *discordgo.Session, guild *discordgo.Guild, channel *discordgo.Channel) {
	for _, permission := range channel.PermissionOverwrites {
		if permission.Type == discordgo.PermissionOverwriteTypeRole && permission.ID == guild.ID {
			if permission.Allow&discordgo.PermissionViewChannel != 0 && permission.Deny&discordgo.PermissionViewChannel == 0 {
				return
			}
			break
		}
	}

	fmt.Println("Enabling desk visibility", channel.ID)
	_, err := s.ChannelEdit(channel.ID, &discordgo.ChannelEdit{
		PermissionOverwrites: append(channel.PermissionOverwrites,
			&discordgo.PermissionOverwrite{
				ID:    guild.ID,
				Type:  discordgo.PermissionOverwriteTypeRole,
				Allow: discordgo.PermissionViewChannel,
			},
		),
	},
	)
	if err != nil {
		fmt.Println("Failed to update channel", err)
	}
}

// If the last user leaves, hide the desk.
func handleDeskDisconnect(guildID string, channel *discordgo.Channel) int {
	fmt.Println("User disconnected from desk", channel.ID)

	guildChannelMembersMutex.Lock()
	channelMembers := guildChannelMembers[guildID][channel.ID]
	channelMembers = max(0, channelMembers-1)
	guildChannelMembers[guildID][channel.ID] = channelMembers
	guildChannelMembersMutex.Unlock()

	return channelMembers
}

// Hide the desk from @everyone except the owner
func hideDeskChannel(s *discordgo.Session, guild *discordgo.Guild, channel *discordgo.Channel) {
	for _, permission := range channel.PermissionOverwrites {
		if permission.Type == discordgo.PermissionOverwriteTypeRole && permission.ID == guild.ID {
			if permission.Allow&discordgo.PermissionViewChannel == 0 && permission.Deny&discordgo.PermissionViewChannel != 0 {
				return
			}
			break
		}
	}

	fmt.Println("Disabling desk visibility", channel.ID)
	_, err := s.ChannelEdit(channel.ID, &discordgo.ChannelEdit{
		PermissionOverwrites: append(channel.PermissionOverwrites,
			&discordgo.PermissionOverwrite{
				ID:   guild.ID,
				Type: discordgo.PermissionOverwriteTypeRole,
				Deny: discordgo.PermissionViewChannel,
			},
		),
	})
	if err != nil {
		fmt.Println("Failed to update channel", err)
	}
}

func createDeskChannel(s *discordgo.Session, guildID string, userID string, name string, deskCategoryId string) error {
	_, err := s.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name: name,
		Type: discordgo.ChannelTypeGuildVoice,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{
				ID:    userID,
				Type:  discordgo.PermissionOverwriteTypeMember,
				Allow: USER_DESK_PERMISSIONS,
			},
			{
				ID:    s.State.User.ID,
				Type:  discordgo.PermissionOverwriteTypeMember,
				Allow: BOT_DESK_PERMISSIONS,
			},
			{
				ID:   guildID, // The `@everyone` role ID matches the guild ID
				Type: discordgo.PermissionOverwriteTypeRole,
				Deny: discordgo.PermissionViewChannel,
			},
		},
		ParentID: deskCategoryId,
		Position: 0,
	})
	return err
}

func findUserDeskChannel(channels []*discordgo.Channel, deskCategoryId any, userID string, botID string) *discordgo.Channel {
	for _, channel := range channels {
		if channel.ParentID == deskCategoryId && userID == getChannelOwner(channel, botID) {
			return channel
		}
	}
	return nil
}

func getChannelOwner(channel *discordgo.Channel, botID string) string {
	for _, permission := range channel.PermissionOverwrites {
		if permission.Type == discordgo.PermissionOverwriteTypeMember &&
			permission.ID != botID &&
			permission.Allow&discordgo.PermissionManageChannels != 0 {
			return permission.ID
		}
	}
	return ""
}

func resetDeskPermissions(s *discordgo.Session, channel *discordgo.Channel, userId string) error {
	var userPermissions int64
	var botPermissions int64

	for _, permission := range channel.PermissionOverwrites {
		if permission.Type == discordgo.PermissionOverwriteTypeMember {
			if permission.ID == userId {
				userPermissions = permission.Allow
			}
			if permission.ID == s.State.User.ID {
				botPermissions = permission.Allow
			}
		}
	}

	if (userPermissions&USER_DESK_PERMISSIONS == USER_DESK_PERMISSIONS) &&
		(botPermissions&BOT_DESK_PERMISSIONS == BOT_DESK_PERMISSIONS) {
		return nil
	}

	_, err := s.ChannelEdit(channel.ID, &discordgo.ChannelEdit{
		PermissionOverwrites: append(
			channel.PermissionOverwrites,
			&discordgo.PermissionOverwrite{
				ID:    userId,
				Type:  discordgo.PermissionOverwriteTypeMember,
				Allow: userPermissions | USER_DESK_PERMISSIONS,
			},
			&discordgo.PermissionOverwrite{
				ID:    s.State.User.ID,
				Type:  discordgo.PermissionOverwriteTypeMember,
				Allow: botPermissions | BOT_DESK_PERMISSIONS,
			},
			&discordgo.PermissionOverwrite{
				ID:   channel.GuildID,
				Type: discordgo.PermissionOverwriteTypeRole,
				Deny: discordgo.PermissionViewChannel,
			},
		),
	})
	return err
}

package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/bwmarrin/discordgo"
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

	discord.AddHandler(ready)
	discord.AddHandler(guildCreate)
	discord.AddHandler(guildMemberAdd)
	discord.AddHandler(voiceStateUpdate)

	discord.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMembers | discordgo.IntentsGuildVoiceStates

	// Open the websocket and begin listening.
	err = discord.Open()
	if err != nil {
		fmt.Println("Error opening Discord session: ", err)
		return
	}

	fmt.Println("Deskbot is now running.  Press CTRL-C to exit.")

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	fmt.Println("Closing discord session...")
	discord.Close()
}

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
		if member.User.Bot {
			continue
		}
		if member.User.System {
			continue
		}

		deskChannel := findUserDeskChannel(event.Channels, deskCategoryId, member.User.ID, s.State.User.ID)
		if deskChannel == nil {
			fmt.Printf("Missing desk channel for user %s\n", member.DisplayName())
			err := createDeskChannel(s, event.ID, member.User.ID, member.DisplayName(), deskCategoryId)
			if err != nil {
				fmt.Printf("Failed to create desk channel for user %s: %v\n", member.DisplayName(), err)
			}
		} else {
			if err := resetDeskPermissions(s, deskChannel, member.User.ID); err != nil {
				fmt.Printf("Failed to reset desk permissions for user %s: %v\n", member.DisplayName(), err)
			}
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

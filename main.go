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

var (
	token               string
	guildToDeskCategory *sync.Map
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

	discord.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMembers

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
		fmt.Printf("Deskbot found the DESKS category in %v and is ready\n", event.Name)
	} else {
		fmt.Printf("Deskbot failed to find the DESKS category in %v\n", event.Name)
	}
}

func guildMemberAdd(s *discordgo.Session, event *discordgo.GuildMemberAdd) {
	name := event.Nick
	if len(name) == 0 {
		name = event.User.Username
	}

	fmt.Println("guildMemberAdd", name)

	deskCategoryId, ok := guildToDeskCategory.Load(event.GuildID)
	if !ok {
		fmt.Println("Failed to find deskCategory for guildId", event.GuildID)
		return
	}

	channels, err := s.GuildChannels(event.GuildID)
	if err != nil {
		fmt.Println("Failed to fetch channels", err)
		return
	}

	var existingDeskChannelId string
	for _, channel := range channels {
		if channel.ParentID == deskCategoryId && (channel.Name == name) {
			existingDeskChannelId = channel.ID
			break
		}
	}

	if existingDeskChannelId != "" {
		return
	}

	_, err = s.GuildChannelCreateComplex(event.GuildID, discordgo.GuildChannelCreateData{
		Name: name,
		Type: discordgo.ChannelTypeGuildVoice,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{
				ID:    event.User.ID,
				Type:  discordgo.PermissionOverwriteTypeMember,
				Allow: discordgo.PermissionManageChannels,
			},
		},
		ParentID: deskCategoryId.(string),
		Position: 0,
	})
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

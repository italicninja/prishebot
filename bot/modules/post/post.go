// Package post provides a /post slash command that lets server admins
// publish announcements, rules, or info posts under Prishe's name.
//
// Flow:
//
//	/post create channel:#general [mention:@role]
//	    → opens a modal: Title (optional) + Content (paragraph)
//	    → on submit, sends the message to the chosen channel
//
//	/post edit channel:#general message_id:123 [mention:@role]
//	    → fetches the existing message, opens the modal pre-filled with
//	      its title (if any) and content
//	    → on submit, edits the message in place
//
// When the modal's Title field is empty, the post is plain text. When it's
// set, the post becomes an embed with that title and the content as the
// description - better for longer rules/info posts.
package post

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/user/discord-bot-skeleton/bot"
)

// embedColor matches the dashboard accent - Prishe-purple.
const embedColor = 0x9B59B6

// Module implements bot.Module for the /post slash command.
type Module struct{}

// New constructs the module. No state: every interaction carries the data
// it needs (channel + message IDs) in the modal customID, so we don't need
// a per-guild store.
func New() *Module { return &Module{} }

func (m *Module) Name() string           { return "post" }
func (m *Module) Description() string    { return "Lets admins publish announcements, rules, or info posts as Prishe." }
func (m *Module) Category() bot.Category { return bot.CategoryFunctional }

func (m *Module) OnLoad(_ *discordgo.Session) error {
	log.Println("[post] module loaded")
	return nil
}

func (m *Module) OnUnload(_ *discordgo.Session) error {
	log.Println("[post] module unloaded")
	return nil
}

func (m *Module) Commands() []*discordgo.ApplicationCommand {
	textChannel := []discordgo.ChannelType{discordgo.ChannelTypeGuildText}
	return []*discordgo.ApplicationCommand{
		{
			Name:        "post",
			Description: "Publish an announcement as Prishe (rules, info, etc.)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "create",
					Description: "Compose a new post",
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:         discordgo.ApplicationCommandOptionChannel,
							Name:         "channel",
							Description:  "Channel to post in",
							Required:     true,
							ChannelTypes: textChannel,
						},
						{
							Type:        discordgo.ApplicationCommandOptionRole,
							Name:        "mention",
							Description: "Role to ping with the post (optional)",
							Required:    false,
						},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "edit",
					Description: "Edit an existing post Prishe sent",
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:         discordgo.ApplicationCommandOptionChannel,
							Name:         "channel",
							Description:  "Channel containing the message",
							Required:     true,
							ChannelTypes: textChannel,
						},
						{
							Type:        discordgo.ApplicationCommandOptionString,
							Name:        "message_id",
							Description: "Message ID (right-click → Copy Message ID - needs developer mode)",
							Required:    true,
						},
						{
							Type:        discordgo.ApplicationCommandOptionRole,
							Name:        "mention",
							Description: "Role to ping with the edited post (optional)",
							Required:    false,
						},
					},
				},
			},
		},
	}
}

// HandleInteraction dispatches by interaction type. /post create | edit open
// a modal; the modal submit (custom_id starts with "post:") arrives later as
// a separate interaction and is routed back here by the bot's prefix lookup.
func (m *Module) HandleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		data := i.ApplicationCommandData()
		if len(data.Options) == 0 {
			return
		}
		switch data.Options[0].Name {
		case "create":
			m.openCreateModal(s, i, data.Options[0].Options)
		case "edit":
			m.openEditModal(s, i, data.Options[0].Options)
		}
	case discordgo.InteractionModalSubmit:
		m.handleModalSubmit(s, i)
	}
}

// ── Slash → modal ────────────────────────────────────────────────────────────

func (m *Module) openCreateModal(s *discordgo.Session, i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption) {
	channelID, mentionID := readCommonOpts(opts)
	if channelID == "" {
		respondEphemeral(s, i, "Pick a channel to post in.")
		return
	}
	m.showModal(s, i, "post:create:"+channelID+":"+mentionID, "New post", "", "")
}

func (m *Module) openEditModal(s *discordgo.Session, i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption) {
	channelID, mentionID := readCommonOpts(opts)
	var messageID string
	for _, o := range opts {
		if o.Name == "message_id" {
			messageID = strings.TrimSpace(o.StringValue())
		}
	}
	if channelID == "" || messageID == "" {
		respondEphemeral(s, i, "Pick a channel and provide the message ID.")
		return
	}

	// Fetch the original to pre-fill the modal. If we can't read it (bot
	// lacks perms, message gone, wrong channel), surface an ephemeral error
	// instead of opening an empty modal that would silently fail on submit.
	msg, err := s.ChannelMessage(channelID, messageID)
	if err != nil {
		respondEphemeral(s, i, "Couldn't fetch that message - check the ID and channel, and make sure Prishe has access.")
		return
	}
	if msg.Author == nil || msg.Author.ID != s.State.User.ID {
		respondEphemeral(s, i, "That message wasn't posted by Prishe, so I can't edit it.")
		return
	}

	title, content := extractPostFields(msg)
	m.showModal(s, i, "post:edit:"+channelID+":"+messageID+":"+mentionID, "Edit post", title, content)
}

// showModal opens the title+content modal. customID encodes all the state
// needed by handleModalSubmit (action, channel, optional message ID,
// optional mention role) so the submit handler is fully stateless.
func (m *Module) showModal(s *discordgo.Session, i *discordgo.InteractionCreate, customID, modalTitle, prefillTitle, prefillContent string) {
	resp := &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: customID,
			Title:    modalTitle,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.TextInput{
						CustomID:  "title",
						Label:     "Title (optional - renders as an embed)",
						Style:     discordgo.TextInputShort,
						MaxLength: 256,
						Required:  false,
						Value:     prefillTitle,
					},
				}},
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.TextInput{
						CustomID:  "content",
						Label:     "Content",
						Style:     discordgo.TextInputParagraph,
						MinLength: 1,
						MaxLength: 4000,
						Required:  true,
						Value:     prefillContent,
					},
				}},
			},
		},
	}
	if err := s.InteractionRespond(i.Interaction, resp); err != nil {
		log.Printf("[post] open modal: %v", err)
	}
}

// ── Modal → message ──────────────────────────────────────────────────────────

func (m *Module) handleModalSubmit(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ModalSubmitData()
	parts := strings.Split(data.CustomID, ":")
	if len(parts) < 3 || parts[0] != "post" {
		return
	}

	title, content := readModalInputs(data)
	if strings.TrimSpace(content) == "" {
		respondEphemeral(s, i, "Content can't be empty.")
		return
	}

	switch parts[1] {
	case "create":
		// post:create:<channelID>[:<mentionID>]
		channelID := parts[2]
		mentionID := ""
		if len(parts) > 3 {
			mentionID = parts[3]
		}
		m.sendCreate(s, i, channelID, mentionID, title, content)
	case "edit":
		// post:edit:<channelID>:<messageID>[:<mentionID>]
		if len(parts) < 4 {
			respondEphemeral(s, i, "Couldn't recover the message ID - please try /post edit again.")
			return
		}
		channelID, messageID := parts[2], parts[3]
		mentionID := ""
		if len(parts) > 4 {
			mentionID = parts[4]
		}
		m.sendEdit(s, i, channelID, messageID, mentionID, title, content)
	}
}

func (m *Module) sendCreate(s *discordgo.Session, i *discordgo.InteractionCreate, channelID, mentionID, title, content string) {
	send := buildMessageSend(mentionID, title, content)
	msg, err := s.ChannelMessageSendComplex(channelID, send)
	if err != nil {
		log.Printf("[post] send to %s: %v", channelID, err)
		respondEphemeral(s, i, "Couldn't post - make sure Prishe has permission to send messages in that channel.")
		return
	}
	respondEphemeral(s, i, fmt.Sprintf("✅ Posted in <#%s>. Message ID `%s` - use `/post edit` with that ID to update it later.",
		channelID, msg.ID))
}

func (m *Module) sendEdit(s *discordgo.Session, i *discordgo.InteractionCreate, channelID, messageID, mentionID, title, content string) {
	edit := buildMessageEdit(channelID, messageID, mentionID, title, content)
	if _, err := s.ChannelMessageEditComplex(edit); err != nil {
		log.Printf("[post] edit %s/%s: %v", channelID, messageID, err)
		respondEphemeral(s, i, "Couldn't edit that message - check the ID and make sure Prishe still has access.")
		return
	}
	respondEphemeral(s, i, fmt.Sprintf("✅ Updated the post in <#%s>.", channelID))
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// readCommonOpts pulls the channel and (optional) mention role ID out of a
// subcommand option list. Returns "" for either when missing.
func readCommonOpts(opts []*discordgo.ApplicationCommandInteractionDataOption) (channelID, mentionID string) {
	for _, o := range opts {
		switch o.Name {
		case "channel":
			channelID = o.ChannelValue(nil).ID
		case "mention":
			if r := o.RoleValue(nil, ""); r != nil {
				mentionID = r.ID
			}
		}
	}
	return channelID, mentionID
}

// readModalInputs walks the modal's Components tree and pulls "title" /
// "content" by CustomID. Discord nests each TextInput inside an ActionsRow,
// so we unpack one level before reading values.
func readModalInputs(data discordgo.ModalSubmitInteractionData) (title, content string) {
	for _, row := range data.Components {
		ar, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, c := range ar.Components {
			ti, ok := c.(*discordgo.TextInput)
			if !ok {
				continue
			}
			switch ti.CustomID {
			case "title":
				title = strings.TrimSpace(ti.Value)
			case "content":
				content = ti.Value // preserve leading/trailing whitespace in content
			}
		}
	}
	return title, content
}

// extractPostFields recovers the title + content from an existing Prishe-
// posted message so we can pre-fill the edit modal. An embed-style post
// uses Embed.Title + Embed.Description; a plain post uses Content.
func extractPostFields(msg *discordgo.Message) (title, content string) {
	if len(msg.Embeds) > 0 && msg.Embeds[0] != nil {
		e := msg.Embeds[0]
		// If a mention was prepended (e.g. "<@&...> <embed>"), strip role
		// mentions from the leading content so the user doesn't see them
		// in the title slot.
		return e.Title, e.Description
	}
	return "", stripLeadingRoleMentions(msg.Content)
}

// stripLeadingRoleMentions removes any leading "<@&id>" tokens and the space
// that follows them. Used when pre-filling the content of a plain message
// that was originally posted with a mention - the mention is reconstructed
// on submit from the per-edit "mention" option, so we shouldn't show it
// twice in the editor.
func stripLeadingRoleMentions(s string) string {
	trimmed := strings.TrimLeft(s, " \t")
	for strings.HasPrefix(trimmed, "<@&") {
		end := strings.Index(trimmed, ">")
		if end < 0 {
			break
		}
		// Validate the inner text is digits - otherwise it's some other
		// kind of mention or just text that happens to start with "<@&".
		inner := trimmed[3:end]
		if _, err := strconv.ParseUint(inner, 10, 64); err != nil {
			break
		}
		trimmed = strings.TrimLeft(trimmed[end+1:], " \t")
	}
	return trimmed
}

// buildMessageSend assembles a MessageSend for create. Title set → embed.
// Mention set → prepend "<@&id>" and scope AllowedMentions to that role.
func buildMessageSend(mentionID, title, content string) *discordgo.MessageSend {
	send := &discordgo.MessageSend{}
	if title != "" {
		send.Embeds = []*discordgo.MessageEmbed{{
			Title:       title,
			Description: content,
			Color:       embedColor,
		}}
	} else {
		send.Content = content
	}
	if mentionID != "" {
		// Prepend the mention to whichever field carries the body so the
		// ping fires on the actual message, not just the embed.
		send.Content = "<@&" + mentionID + ">" + leadingSpacer(send.Content, send.Embeds)
		send.AllowedMentions = &discordgo.MessageAllowedMentions{Roles: []string{mentionID}}
	}
	return send
}

// buildMessageEdit mirrors buildMessageSend for the edit endpoint. discordgo's
// edit struct needs pointers to opt into modifying Content / Embeds.
func buildMessageEdit(channelID, messageID, mentionID, title, content string) *discordgo.MessageEdit {
	edit := &discordgo.MessageEdit{Channel: channelID, ID: messageID}
	var body string
	if title != "" {
		embeds := []*discordgo.MessageEmbed{{
			Title:       title,
			Description: content,
			Color:       embedColor,
		}}
		edit.Embeds = &embeds
	} else {
		// Clear any prior embed by sending an empty embed list.
		empty := []*discordgo.MessageEmbed{}
		edit.Embeds = &empty
		body = content
	}
	if mentionID != "" {
		body = "<@&" + mentionID + ">" + leadingSpacer(body, edit.Embeds)
		edit.AllowedMentions = &discordgo.MessageAllowedMentions{Roles: []string{mentionID}}
	}
	edit.Content = &body
	return edit
}

// leadingSpacer adds a separator after the mention so the body content
// doesn't run into the mention token. Returns " " when there's text to
// follow, "" when only an embed follows (the mention can stand alone).
func leadingSpacer(content string, embeds any) string {
	if content != "" {
		return " " + content
	}
	switch v := embeds.(type) {
	case []*discordgo.MessageEmbed:
		if len(v) > 0 {
			return ""
		}
	case *[]*discordgo.MessageEmbed:
		if v != nil && len(*v) > 0 {
			return ""
		}
	}
	return ""
}

// respondEphemeral sends a private confirmation visible only to the invoker.
// Used for both success and error paths after a modal - Discord requires the
// modal-submit interaction to be acknowledged, and ephemeral is the right
// channel for "post sent" feedback that nobody else needs to see.
func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("[post] ephemeral respond: %v", err)
	}
}


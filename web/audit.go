// Audit-log posts: when an admin/moderator changes server config via the
// dashboard, post an embed describing the change to the channel the server
// owner picked (if any). Empty audit channel = feature disabled for the guild.
package web

import (
	"log"
	"time"

	"github.com/bwmarrin/discordgo"
)

// auditColor is the embed accent for audit posts. 0x9B59B6 matches the rest
// of the dashboard's purple-ish accent.
const auditColor = 0x9B59B6

// postAudit sends an audit embed for a config change to the guild's
// configured audit channel. No-op when auditing isn't enabled for the guild
// or when the bot can't post (channel deleted, missing permissions, etc. —
// logged but never bubbled up to the user, since the config save itself
// already succeeded by the time this runs).
func (s *Server) postAudit(guildID string, sess *Session, action, summary string) {
	channelID := s.bot.AuditChannels().Get(guildID)
	if channelID == "" {
		return
	}
	embed := &discordgo.MessageEmbed{
		Title:       action,
		Description: summary,
		Color:       auditColor,
		Author: &discordgo.MessageEmbedAuthor{
			Name:    sess.Username,
			IconURL: sess.AvatarURL,
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: "Dashboard change · user ID " + sess.UserID,
		},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	// Suppress pings — the diff description embeds <@&roleID> / <#channelID>
	// references for readability, and we don't want every save to ping
	// everyone in those roles. Empty Parse means "no mention types are
	// allowed to ping" without disabling mention rendering itself.
	if _, err := s.bot.Session().ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	}); err != nil {
		log.Printf("[audit] post to %s in guild %s: %v", channelID, guildID, err)
	}
}

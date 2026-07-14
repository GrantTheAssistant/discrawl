package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrArchiveInvalidRequest = errors.New("invalid archive request")

// ArchivePageOptions is the intentionally small, stable API surface used by
// remote archive readers. Discord snowflakes are used as the page boundary so
// callers never need to trust or reproduce SQLite offsets.
type ArchivePageOptions struct {
	GuildID              string
	ChannelID            string
	BeforeID             string
	Limit                int
	ExposeAttachmentURLs bool
}

type ArchiveAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
	URL         string `json:"url,omitempty"`
	ProxyURL    string `json:"proxy_url,omitempty"`
}

type ArchiveMessage struct {
	MessageID      string              `json:"message_id"`
	GuildID        string              `json:"guild_id"`
	ChannelID      string              `json:"channel_id"`
	AuthorID       string              `json:"author_id,omitempty"`
	AuthorName     string              `json:"author_name"`
	AuthorPhotoURL string              `json:"author_photo_url,omitempty"`
	RoleColor      string              `json:"role_color,omitempty"`
	Content        string              `json:"content"`
	CreatedAt      time.Time           `json:"created_at"`
	EditedAt       time.Time           `json:"edited_at,omitzero"`
	DeletedAt      time.Time           `json:"deleted_at,omitzero"`
	Deleted        bool                `json:"deleted"`
	Bot            bool                `json:"bot"`
	Webhook        bool                `json:"webhook"`
	Pinned         bool                `json:"pinned"`
	ReplyToID      string              `json:"reply_to_message_id,omitempty"`
	Attachments    []ArchiveAttachment `json:"attachments"`
}

type ArchivePage struct {
	Messages     []ArchiveMessage `json:"messages"`
	HasMore      bool             `json:"has_more"`
	NextBeforeID string           `json:"next_before_id,omitempty"`
}

type archiveRawMessage struct {
	Author struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
		Avatar     string `json:"avatar"`
		Bot        bool   `json:"bot"`
	} `json:"author"`
	Member struct {
		Nick   string   `json:"nick"`
		Avatar string   `json:"avatar"`
		Roles  []string `json:"roles"`
	} `json:"member"`
	WebhookID string `json:"webhook_id"`
}

// ListArchiveMessages returns one descending Discord-history page, rendered in
// chronological order for clients. The boundary message must belong to the
// requested guild/channel; a foreign snowflake cannot become a cross-scope
// timing oracle.
func (s *Store) ListArchiveMessages(ctx context.Context, opts ArchivePageOptions) (ArchivePage, error) {
	opts.GuildID = strings.TrimSpace(opts.GuildID)
	opts.ChannelID = strings.TrimSpace(opts.ChannelID)
	opts.BeforeID = strings.TrimSpace(opts.BeforeID)
	if opts.GuildID == "" || opts.ChannelID == "" {
		return ArchivePage{}, fmt.Errorf("%w: guild and channel are required", ErrArchiveInvalidRequest)
	}
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	if opts.Limit > 100 {
		return ArchivePage{}, fmt.Errorf("%w: archive page limit exceeds 100", ErrArchiveInvalidRequest)
	}

	args := []any{opts.GuildID, opts.ChannelID}
	boundary := ""
	if opts.BeforeID != "" {
		// The product union cursor can name a website-origin Discord message
		// that has not reached SQLite yet. Snowflakes are monotonic decimal
		// identifiers, so an absent but validated boundary is still safe.
		boundary = `and (length(m.id) < length(?) or (length(m.id) = length(?) and m.id < ?))`
		args = append(args, opts.BeforeID, opts.BeforeID, opts.BeforeID)
	}
	args = append(args, opts.Limit+1)

	rows, err := s.db.QueryContext(ctx, `
		select
			m.id, m.guild_id, m.channel_id, coalesce(m.author_id, ''),
			coalesce(nullif(mem.display_name, ''), nullif(mem.nick, ''),
				nullif(mem.global_name, ''), nullif(mem.username, ''), ''),
			coalesce(m.content, ''),
			m.created_at, coalesce(m.edited_at, ''), coalesce(m.deleted_at, ''),
			m.pinned, coalesce(m.reply_to_message_id, ''), m.raw_json,
			coalesce(mem.role_ids_json, '[]'), coalesce(g.raw_json, '{}')
		from messages m
		left join members mem on mem.guild_id = m.guild_id and mem.user_id = m.author_id
		join guilds g on g.id = m.guild_id
		where m.guild_id = ? and m.channel_id = ? `+boundary+`
		order by length(m.id) desc, m.id desc
		limit ?
	`, args...)
	if err != nil {
		return ArchivePage{}, err
	}
	defer func() { _ = rows.Close() }()

	desc := make([]ArchiveMessage, 0, opts.Limit+1)
	for rows.Next() {
		var (
			message                                             ArchiveMessage
			created, edited, deleted, raw, roleIDsRaw, guildRaw string
			pinned                                              int
		)
		if err := rows.Scan(
			&message.MessageID, &message.GuildID, &message.ChannelID, &message.AuthorID,
			&message.AuthorName, &message.Content, &created, &edited, &deleted, &pinned,
			&message.ReplyToID, &raw, &roleIDsRaw, &guildRaw,
		); err != nil {
			return ArchivePage{}, err
		}
		message.CreatedAt = parseTime(created)
		message.EditedAt = parseTime(edited)
		message.DeletedAt = parseTime(deleted)
		message.Deleted = deleted != ""
		message.Pinned = pinned == 1
		message.Attachments = []ArchiveAttachment{}
		var payload archiveRawMessage
		if json.Unmarshal([]byte(raw), &payload) == nil {
			message.Bot = payload.Author.Bot
			message.Webhook = strings.TrimSpace(payload.WebhookID) != ""
			if message.AuthorID == "" {
				message.AuthorID = payload.Author.ID
			}
			if message.AuthorName == "" {
				message.AuthorName = archiveFirstNonEmpty(payload.Member.Nick, payload.Author.GlobalName, payload.Author.Username, "Discord User")
			}
			message.AuthorPhotoURL = discordArchiveAvatarURL(message.GuildID, message.AuthorID, payload.Member.Avatar, payload.Author.Avatar)
			roles := payload.Member.Roles
			if len(roles) == 0 {
				_ = json.Unmarshal([]byte(roleIDsRaw), &roles)
			}
			message.RoleColor = archiveRoleColorFromIDs(roles, guildRaw)
		}
		if message.Deleted {
			// A delete is a body-free tombstone even though the restricted archive
			// deliberately retains evidence. Preserve author/presentation metadata
			// to match the existing product contract.
			message.Content = ""
		}
		desc = append(desc, message)
	}
	if err := rows.Err(); err != nil {
		return ArchivePage{}, err
	}

	hasMore := len(desc) > opts.Limit
	if hasMore {
		desc = desc[:opts.Limit]
	}
	if err := s.attachArchiveMessageFiles(ctx, desc, opts.ExposeAttachmentURLs); err != nil {
		return ArchivePage{}, err
	}
	page := ArchivePage{Messages: make([]ArchiveMessage, len(desc)), HasMore: hasMore}
	for i := range desc {
		page.Messages[len(desc)-1-i] = desc[i]
	}
	if hasMore && len(desc) > 0 {
		page.NextBeforeID = desc[len(desc)-1].MessageID
	}
	return page, nil
}

func (s *Store) attachArchiveMessageFiles(ctx context.Context, messages []ArchiveMessage, exposeURLs bool) error {
	if len(messages) == 0 {
		return nil
	}
	args := make([]any, 0, len(messages))
	byID := make(map[string]int, len(messages))
	for i := range messages {
		args = append(args, messages[i].MessageID)
		byID[messages[i].MessageID] = i
	}
	rows, err := s.db.QueryContext(ctx, `
		select a.message_id, a.attachment_id, a.filename, coalesce(a.content_type, ''), a.size,
			coalesce(url, ''), coalesce(proxy_url, '')
		from message_attachments a
		join messages m on m.id = a.message_id
		where a.message_id in (`+placeholders(len(args))+`) and m.deleted_at is null
		order by a.message_id, a.attachment_id
	`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var messageID string
		var attachment ArchiveAttachment
		if err := rows.Scan(&messageID, &attachment.ID, &attachment.Filename, &attachment.ContentType,
			&attachment.Size, &attachment.URL, &attachment.ProxyURL); err != nil {
			return err
		}
		if index, ok := byID[messageID]; ok {
			if len(messages[index].Attachments) >= 10 {
				continue
			}
			if !exposeURLs {
				attachment.URL = ""
				attachment.ProxyURL = ""
			}
			messages[index].Attachments = append(messages[index].Attachments, attachment)
		}
	}
	return rows.Err()
}

func archiveFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func discordArchiveAvatarURL(guildID, userID, guildAvatar, userAvatar string) string {
	avatar := strings.TrimSpace(guildAvatar)
	if avatar != "" && guildID != "" && userID != "" {
		ext := "png"
		if strings.HasPrefix(avatar, "a_") {
			ext = "gif"
		}
		return fmt.Sprintf("https://cdn.discordapp.com/guilds/%s/users/%s/avatars/%s.%s?size=128", guildID, userID, avatar, ext)
	}
	avatar = strings.TrimSpace(userAvatar)
	if avatar == "" || userID == "" {
		return ""
	}
	ext := "png"
	if strings.HasPrefix(avatar, "a_") {
		ext = "gif"
	}
	return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.%s?size=128", userID, avatar, ext)
}

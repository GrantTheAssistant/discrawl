package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ProjectionCursor struct {
	UpdatedAt time.Time `json:"updated_at"`
	MessageID string    `json:"message_id"`
}

type ProjectionMessage struct {
	ArchiveMessage
	UpdatedAt time.Time `json:"updated_at"`
}

type ProjectionTombstone struct {
	MessageID string    `json:"message_id"`
	GuildID   string    `json:"guild_id"`
	ChannelID string    `json:"channel_id"`
	DeletedAt time.Time `json:"deleted_at"`
}

// ListProjectionMessages is the sole outbound projection scan. The guild is a
// mandatory predicate, and the stable (updated_at, Discord snowflake) cursor
// makes replays idempotent without exposing SQL or an admin sync endpoint.
func (s *Store) ListProjectionMessages(ctx context.Context, guildID string, cursor ProjectionCursor, limit int) ([]ProjectionMessage, error) {
	guildID = strings.TrimSpace(guildID)
	if guildID == "" {
		return nil, fmt.Errorf("%w: guild is required", ErrArchiveInvalidRequest)
	}
	if limit < 1 || limit > 250 {
		return nil, fmt.Errorf("%w: projection limit must be between 1 and 250", ErrArchiveInvalidRequest)
	}
	updated := ""
	messageID := strings.TrimSpace(cursor.MessageID)
	if !cursor.UpdatedAt.IsZero() {
		updated = cursor.UpdatedAt.UTC().Format(timeLayout)
	}
	rows, err := s.db.QueryContext(ctx, `
		select
			m.id, m.guild_id, m.channel_id, coalesce(m.author_id, ''),
			coalesce(nullif(mem.display_name, ''), nullif(mem.nick, ''),
				nullif(mem.global_name, ''), nullif(mem.username, ''), ''),
			case when m.deleted_at is not null then '' else coalesce(m.content, '') end,
			m.created_at, coalesce(m.edited_at, ''), coalesce(m.deleted_at, ''),
			m.pinned, coalesce(m.reply_to_message_id, ''), m.raw_json, m.updated_at,
			coalesce(mem.role_ids_json, '[]'), coalesce(g.raw_json, '{}')
		from messages m
		left join members mem on mem.guild_id = m.guild_id and mem.user_id = m.author_id
		join guilds g on g.id = m.guild_id
		where m.guild_id = ?
		  and (? = '' or m.updated_at > ? or
			(m.updated_at = ? and cast(m.id as integer) > cast(? as integer)))
		order by m.updated_at, cast(m.id as integer)
		limit ?
	`, guildID, updated, updated, updated, messageID, limit)
	if err != nil {
		return nil, err
	}
	return s.scanProjectionRows(ctx, rows, limit)
}

func (s *Store) scanProjectionRows(ctx context.Context, rows *sql.Rows, capacity int) ([]ProjectionMessage, error) {
	defer func() { _ = rows.Close() }()
	messages := make([]ProjectionMessage, 0, capacity)
	for rows.Next() {
		var message ProjectionMessage
		var created, edited, deleted, raw, rowUpdated, roleIDsRaw, guildRaw string
		var pinned int
		if err := rows.Scan(
			&message.MessageID, &message.GuildID, &message.ChannelID, &message.AuthorID,
			&message.AuthorName, &message.Content, &created, &edited, &deleted, &pinned,
			&message.ReplyToID, &raw, &rowUpdated, &roleIDsRaw, &guildRaw,
		); err != nil {
			return nil, err
		}
		message.CreatedAt = parseTime(created)
		message.EditedAt = parseTime(edited)
		message.DeletedAt = parseTime(deleted)
		message.UpdatedAt = parseTime(rowUpdated)
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
			message.Content = ""
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	base := make([]ArchiveMessage, len(messages))
	for i := range messages {
		base[i] = messages[i].ArchiveMessage
	}
	if err := s.attachArchiveMessageFiles(ctx, base, true); err != nil {
		return nil, err
	}
	for i := range messages {
		messages[i].Attachments = base[i].Attachments
	}
	return messages, nil
}

func (s *Store) ListRecentProjectionMessages(ctx context.Context, guildID, channelID string, createdAfter time.Time, limit int) ([]ProjectionMessage, error) {
	if strings.TrimSpace(guildID) == "" || strings.TrimSpace(channelID) == "" || limit < 1 || limit > 2000 {
		return nil, fmt.Errorf("%w: invalid recent projection scope", ErrArchiveInvalidRequest)
	}
	rows, err := s.db.QueryContext(ctx, `
		select
			m.id, m.guild_id, m.channel_id, coalesce(m.author_id, ''),
			coalesce(nullif(mem.display_name, ''), nullif(mem.nick, ''),
				nullif(mem.global_name, ''), nullif(mem.username, ''), ''),
			case when m.deleted_at is not null then '' else coalesce(m.content, '') end,
			m.created_at, coalesce(m.edited_at, ''), coalesce(m.deleted_at, ''),
			m.pinned, coalesce(m.reply_to_message_id, ''), m.raw_json, m.updated_at,
			coalesce(mem.role_ids_json, '[]'), coalesce(g.raw_json, '{}')
		from messages m
		left join members mem on mem.guild_id = m.guild_id and mem.user_id = m.author_id
		join guilds g on g.id = m.guild_id
		where m.guild_id = ? and m.channel_id = ? and m.created_at >= ?
		order by m.created_at desc, cast(m.id as integer) desc
		limit ?
	`, guildID, channelID, createdAfter.UTC().Format(timeLayout), limit)
	if err != nil {
		return nil, err
	}
	return s.scanProjectionRows(ctx, rows, limit)
}

func (s *Store) ListProjectionTombstones(ctx context.Context, guildID string, after time.Time, afterID string, limit int) ([]ProjectionTombstone, error) {
	if strings.TrimSpace(guildID) == "" || limit < 1 || limit > 250 {
		return nil, fmt.Errorf("%w: invalid tombstone projection scope", ErrArchiveInvalidRequest)
	}
	rawAfter := ""
	if !after.IsZero() {
		rawAfter = after.UTC().Format(timeLayout)
	}
	rows, err := s.db.QueryContext(ctx, `
		select message_id, guild_id, channel_id, deleted_at
		from message_tombstones
		where guild_id = ? and (? = '' or deleted_at > ? or
			(deleted_at = ? and cast(message_id as integer) > cast(? as integer)))
		order by deleted_at, cast(message_id as integer)
		limit ?
	`, guildID, rawAfter, rawAfter, rawAfter, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]ProjectionTombstone, 0, limit)
	for rows.Next() {
		var row ProjectionTombstone
		var deleted string
		if err := rows.Scan(&row.MessageID, &row.GuildID, &row.ChannelID, &deleted); err != nil {
			return nil, err
		}
		row.DeletedAt = parseTime(deleted)
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListRecentProjectionTombstones returns only the newest deletion markers for
// a newly added or retargeted product binding. The projection is deliberately
// bounded; older history remains canonical in SQLite.
func (s *Store) ListRecentProjectionTombstones(ctx context.Context, guildID, channelID string, deletedAfter time.Time, limit int) ([]ProjectionTombstone, error) {
	if strings.TrimSpace(guildID) == "" || strings.TrimSpace(channelID) == "" || limit < 1 || limit > 2000 {
		return nil, fmt.Errorf("%w: invalid recent tombstone projection scope", ErrArchiveInvalidRequest)
	}
	rows, err := s.db.QueryContext(ctx, `
		select message_id, guild_id, channel_id, deleted_at
		from message_tombstones
		where guild_id = ? and channel_id = ? and deleted_at >= ?
		order by deleted_at desc, cast(message_id as integer) desc
		limit ?
	`, guildID, channelID, deletedAfter.UTC().Format(timeLayout), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]ProjectionTombstone, 0, limit)
	for rows.Next() {
		var row ProjectionTombstone
		var deleted string
		if err := rows.Scan(&row.MessageID, &row.GuildID, &row.ChannelID, &deleted); err != nil {
			return nil, err
		}
		row.DeletedAt = parseTime(deleted)
		out = append(out, row)
	}
	return out, rows.Err()
}

func archiveRoleColorFromIDs(roleIDs []string, guildRaw string) string {
	allowed := make(map[string]struct{}, len(roleIDs))
	for _, id := range roleIDs {
		allowed[id] = struct{}{}
	}
	var guild struct {
		Roles []struct {
			ID       string `json:"id"`
			Color    int    `json:"color"`
			Position int    `json:"position"`
		} `json:"roles"`
	}
	if json.Unmarshal([]byte(guildRaw), &guild) != nil {
		return ""
	}
	bestColor, bestPosition := 0, -1
	for _, role := range guild.Roles {
		if _, ok := allowed[role.ID]; !ok || role.Color <= 0 || role.Color > 0xffffff {
			continue
		}
		if role.Position > bestPosition {
			bestColor, bestPosition = role.Color, role.Position
		}
	}
	if bestColor == 0 {
		return ""
	}
	return fmt.Sprintf("#%06x", bestColor)
}

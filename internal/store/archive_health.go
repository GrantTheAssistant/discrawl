package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type ScopedArchiveStatus struct {
	GuildID         string    `json:"guild_id"`
	ChannelCount    int       `json:"channel_count"`
	ThreadCount     int       `json:"thread_count"`
	MessageCount    int       `json:"message_count"`
	MemberCount     int       `json:"member_count"`
	LastSyncAt      time.Time `json:"last_sync_at,omitzero"`
	WriterHeartbeat time.Time `json:"writer_heartbeat_at,omitzero"`
	Stale           bool      `json:"stale"`
	Degraded        bool      `json:"degraded"`
}

// ValidateSingleGuild prevents accidentally pointing a tenant deployment at a
// shared archive. Scope injection on every query is defense in depth; startup
// still refuses any database containing zero, two, or a different guild.
func (s *Store) ValidateSingleGuild(ctx context.Context, guildID string) error {
	rows, err := s.db.QueryContext(ctx, `select id from guilds order by id limit 2`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) != 1 || ids[0] != guildID {
		return fmt.Errorf("archive must contain exactly configured guild %s", guildID)
	}
	return nil
}

// ArchiveReady is intentionally cheap. Readiness means the process can query
// the expected schema and isolated guild. Stale history remains queryable and
// is reported by /v1/status instead of creating a restart loop.
func (s *Store) ArchiveReady(ctx context.Context, guildID string, _ time.Time, _ time.Duration) (bool, error) {
	var found string
	err := s.db.QueryRowContext(ctx, `
		select g.id
		from guilds g
		where g.id = ?
		  and exists(select 1 from sqlite_master where type = 'table' and name = 'messages')
		  and not exists(select 1 from guilds other where other.id <> g.id)
	`, guildID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return found == guildID, err
}

func (s *Store) ScopedArchiveStatus(ctx context.Context, guildID string, now time.Time, staleAfter time.Duration) (ScopedArchiveStatus, error) {
	status := ScopedArchiveStatus{GuildID: guildID}
	row := s.db.QueryRowContext(ctx, `
		select
			(select count(*) from channels where guild_id = ?),
			(select count(*) from channels where guild_id = ? and kind = 'thread'),
			(select count(*) from messages where guild_id = ?),
			(select count(*) from members where guild_id = ?)
	`, guildID, guildID, guildID, guildID)
	if err := row.Scan(&status.ChannelCount, &status.ThreadCount, &status.MessageCount, &status.MemberCount); err != nil {
		return ScopedArchiveStatus{}, err
	}
	status.LastSyncAt = s.syncUpdatedAt(ctx, "sync:last_success")
	status.WriterHeartbeat = s.syncUpdatedAt(ctx, "tail:heartbeat")
	freshest := status.LastSyncAt
	if status.WriterHeartbeat.After(freshest) {
		freshest = status.WriterHeartbeat
	}
	status.Stale = freshest.IsZero() || now.Sub(freshest) > staleAfter
	status.Degraded = status.Stale
	return status, nil
}

func (s *Store) syncUpdatedAt(ctx context.Context, scope string) time.Time {
	var raw string
	if err := s.db.QueryRowContext(ctx, `select updated_at from sync_state where scope = ?`, scope).Scan(&raw); err != nil {
		return time.Time{}
	}
	return parseTime(raw)
}

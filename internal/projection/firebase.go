package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/db"
	"github.com/openclaw/discrawl/internal/store"
	"google.golang.org/api/iterator"
)

const maxBindings = 250

type FirebaseConfig struct {
	ProjectID   string
	OrgID       string
	DatabaseURL string
}

type FirebaseSink struct {
	projectID string
	orgID     string
	fs        *firestore.Client
	rtdb      *db.Client
}

func NewFirebaseSink(ctx context.Context, cfg FirebaseConfig) (*FirebaseSink, error) {
	if strings.TrimSpace(cfg.ProjectID) == "" || strings.TrimSpace(cfg.OrgID) == "" ||
		cfg.DatabaseURL != "https://"+cfg.ProjectID+"-default-rtdb.firebaseio.com" {
		return nil, errors.New("invalid tenant-local Firebase projection configuration")
	}
	fs, err := firestore.NewClient(ctx, cfg.ProjectID)
	if err != nil {
		return nil, err
	}
	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: cfg.ProjectID, DatabaseURL: cfg.DatabaseURL})
	if err != nil {
		_ = fs.Close()
		return nil, err
	}
	realtime, err := app.Database(ctx)
	if err != nil {
		_ = fs.Close()
		return nil, err
	}
	return &FirebaseSink{projectID: cfg.ProjectID, orgID: cfg.OrgID, fs: fs, rtdb: realtime}, nil
}

func (s *FirebaseSink) Close() error { return s.fs.Close() }

func (s *FirebaseSink) Bindings(ctx context.Context, guildID string) ([]Binding, error) {
	iter := s.fs.Collection("orgs").Doc(s.orgID).Collection("chatBindings").
		Where("enabled", "==", true).Limit(maxBindings + 1).Documents(ctx)
	defer iter.Stop()
	bindings := make([]Binding, 0, 32)
	for {
		doc, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		data := doc.Data()
		binding := Binding{
			ID: doc.Ref.ID, GuildID: stringField(data, "guildId"),
			ChannelID: stringField(data, "channelId"), ThreadID: stringField(data, "threadId"),
		}
		if binding.GuildID != guildID {
			return nil, errors.New("enabled binding escaped configured guild")
		}
		bindings = append(bindings, binding)
	}
	if len(bindings) > maxBindings {
		return nil, errors.New("enabled binding count exceeds 250")
	}
	return bindings, nil
}

func (s *FirebaseSink) ApplyMessages(ctx context.Context, bindings []Binding, messages []store.ProjectionMessage) (int, error) {
	byTarget := bindingTargets(bindings)
	type projected struct {
		binding Binding
		message store.ProjectionMessage
	}
	selected := make([]projected, 0, len(messages))
	for _, message := range messages {
		binding, ok := byTarget[message.ChannelID]
		if !ok || message.GuildID != binding.GuildID {
			continue
		}
		if !validSnowflake(message.MessageID) || !validSnowflake(message.GuildID) || !validSnowflake(message.ChannelID) {
			return 0, errors.New("projection message contains invalid Discord scope")
		}
		selected = append(selected, projected{binding: binding, message: message})
	}
	if len(selected) == 0 {
		return 0, nil
	}
	refs := make([]*firestore.DocumentRef, 0, len(selected)*2)
	for i := range selected {
		refs = append(refs, s.messageRef(selected[i].message.MessageID))
	}
	for i := range selected {
		refs = append(refs, s.tombstoneRef(selected[i].message.MessageID))
	}
	selectedBindings := map[string]struct{}{}
	for _, item := range selected {
		selectedBindings[item.binding.ID] = struct{}{}
	}
	changed := 0
	err := s.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		attemptChanged := 0
		snaps, err := tx.GetAll(refs)
		if err != nil {
			return err
		}
		for i, item := range selected {
			existing := map[string]any{}
			if snaps[i].Exists() {
				existing = snaps[i].Data()
			}
			candidate := projectionDocument(item.binding, item.message, existing)
			ledger := map[string]any{}
			if snaps[len(selected)+i].Exists() {
				ledger = snaps[len(selected)+i].Data()
				if stringField(ledger, "guildId") != item.message.GuildID ||
					stringField(ledger, "channelId") != item.message.ChannelID {
					return errors.New("durable tombstone scope mismatch")
				}
				if _, ok := firestoreTime(ledger["deletedAt"]); !ok {
					return errors.New("durable tombstone has invalid deletion time")
				}
			}
			candidate = preserveTerminalDelete(candidate, existing, ledger)
			equal, err := mapsEqual(existing, candidate)
			if err != nil {
				return err
			}
			if equal {
				continue
			}
			if err := tx.Set(refs[i], candidate, firestore.MergeAll); err != nil {
				return err
			}
			attemptChanged++
		}
		changed = attemptChanged
		return nil
	})
	if err != nil {
		return 0, err
	}
	// Re-tick selected bindings even on an unchanged replay. This makes a
	// Firestore-commit/RTDB-failure safe: the cursor is not advanced and the
	// replay repairs the missed invalidation without rewriting Firestore.
	if err := s.writeTicks(ctx, selectedBindings); err != nil {
		return changed, err
	}
	return changed, nil
}

func (s *FirebaseSink) ApplyTombstones(ctx context.Context, bindings []Binding, tombstones []store.ProjectionTombstone) (int, error) {
	_ = bindings // Deletes intentionally outlive the active binding snapshot.
	if len(tombstones) == 0 {
		return 0, nil
	}
	for _, row := range tombstones {
		if !validSnowflake(row.MessageID) || !validSnowflake(row.GuildID) || !validSnowflake(row.ChannelID) || row.DeletedAt.IsZero() {
			return 0, errors.New("projection tombstone contains invalid Discord scope or deletion time")
		}
	}
	refs := make([]*firestore.DocumentRef, 0, len(tombstones)*2)
	for i := range tombstones {
		refs = append(refs, s.messageRef(tombstones[i].MessageID))
	}
	for i := range tombstones {
		refs = append(refs, s.tombstoneRef(tombstones[i].MessageID))
	}
	selectedBindings := map[string]struct{}{}
	changed := 0
	err := s.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		attemptChanged := 0
		attemptBindings := map[string]struct{}{}
		snaps, err := tx.GetAll(refs)
		if err != nil {
			return err
		}
		for i, row := range tombstones {
			ledgerExisting := map[string]any{}
			if snaps[len(tombstones)+i].Exists() {
				ledgerExisting = snaps[len(tombstones)+i].Data()
			}
			ledger, ledgerChanged, err := durableTombstoneDocument(ledgerExisting, row)
			if err != nil {
				return err
			}
			if ledgerChanged {
				if err := tx.Set(refs[len(tombstones)+i], ledger); err != nil {
					return err
				}
				attemptChanged++
			}
			// The append-only ledger is written even when no projected message or
			// active binding exists. This prevents an older SQLite restore from
			// resurrecting a post-backup Discord deletion.
			if !snaps[i].Exists() {
				continue
			}
			existing := snaps[i].Data()
			candidate, bindingID, ok := tombstoneDocument(existing, row)
			if !ok {
				continue
			}
			if validBindingID(bindingID) {
				attemptBindings[bindingID] = struct{}{}
			}
			equal, err := mapsEqual(existing, candidate)
			if err != nil {
				return err
			}
			if equal {
				continue
			}
			if err := tx.Set(refs[i], candidate, firestore.MergeAll); err != nil {
				return err
			}
			attemptChanged++
		}
		changed = attemptChanged
		selectedBindings = attemptBindings
		return nil
	})
	if err != nil {
		return 0, err
	}
	if err := s.writeTicks(ctx, selectedBindings); err != nil {
		return changed, err
	}
	return changed, nil
}

// SanitizeAttachmentURLs exhaustively walks the tenant's projected messages in
// stable document-ID order. The query chooses the page, then the transaction
// re-reads every document before updating it so a concurrent metadata change is
// retried instead of overwritten. Product writers remove these fields before
// this sweep is started, closing the race behind the persistent cursor.
func (s *FirebaseSink) SanitizeAttachmentURLs(ctx context.Context, afterID string, limit int) (string, int, bool, error) {
	if limit < 1 || limit > 250 || !validAttachmentSweepCursor(afterID) {
		return afterID, 0, false, errors.New("invalid attachment URL sanitation cursor or limit")
	}
	query := s.fs.Collection("orgs").Doc(s.orgID).Collection("chatMessages").
		OrderBy(firestore.DocumentID, firestore.Asc).Limit(limit)
	if afterID != "" {
		query = query.StartAfter(afterID)
	}
	snaps, err := query.Documents(ctx).GetAll()
	if err != nil {
		return afterID, 0, false, err
	}
	if len(snaps) == 0 {
		return afterID, 0, true, nil
	}
	refs := make([]*firestore.DocumentRef, len(snaps))
	for i := range snaps {
		refs[i] = snaps[i].Ref
	}
	changed := 0
	if err := s.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		attemptChanged := 0
		current, err := tx.GetAll(refs)
		if err != nil {
			return err
		}
		for i := range current {
			if !current[i].Exists() {
				continue
			}
			attachments, ok := current[i].Data()["attachments"]
			if !ok {
				continue
			}
			sanitized, scrubbed := sanitizeProjectedAttachments(attachments)
			if !scrubbed {
				continue
			}
			if err := tx.Update(refs[i], []firestore.Update{{Path: "attachments", Value: sanitized}}); err != nil {
				return err
			}
			attemptChanged++
		}
		changed = attemptChanged
		return nil
	}); err != nil {
		return afterID, 0, false, err
	}
	next := snaps[len(snaps)-1].Ref.ID
	return next, changed, len(snaps) < limit, nil
}

func validAttachmentSweepCursor(value string) bool {
	return len(value) <= 1500 && !strings.Contains(value, "/")
}

func sanitizeProjectedAttachments(raw any) (any, bool) {
	urlKey := func(key string) bool {
		return key == "url" || key == "proxyUrl" || key == "proxy_url" || key == "proxyURL"
	}
	var sanitize func(any) (any, bool)
	sanitize = func(value any) (any, bool) {
		switch typed := value.(type) {
		case []any:
			out := append([]any(nil), typed...)
			changed := false
			for i := range typed {
				var scrubbed bool
				out[i], scrubbed = sanitize(typed[i])
				changed = changed || scrubbed
			}
			return out, changed
		case []map[string]any:
			out := append([]map[string]any(nil), typed...)
			changed := false
			for i := range typed {
				sanitized, scrubbed := sanitize(typed[i])
				out[i] = sanitized.(map[string]any)
				changed = changed || scrubbed
			}
			return out, changed
		case map[string]any:
			out := cloneMap(typed)
			changed := false
			for key, child := range typed {
				if urlKey(key) {
					delete(out, key)
					changed = true
					continue
				}
				sanitized, scrubbed := sanitize(child)
				if scrubbed {
					out[key] = sanitized
					changed = true
				}
			}
			return out, changed
		default:
			return value, false
		}
	}
	return sanitize(raw)
}

func tombstoneDocument(existing map[string]any, row store.ProjectionTombstone) (map[string]any, string, bool) {
	guildID := stringField(existing, "guildId")
	channelID := stringField(existing, "channelId")
	threadID := stringField(existing, "threadId")
	targetID := channelID
	if threadID != "" {
		targetID = threadID
	}
	if guildID != row.GuildID || targetID != row.ChannelID {
		return nil, "", false
	}
	candidate := cloneMap(existing)
	candidate["discordMessageId"] = row.MessageID
	candidate["discordSortKey"] = snowflakeSortKey(row.MessageID)
	candidate["content"] = ""
	candidate["attachments"] = []map[string]any{}
	candidate["deleted"] = true
	candidate["deletedAt"] = row.DeletedAt
	candidate["updatedAt"] = row.DeletedAt
	return candidate, stringField(existing, "bindingId"), true
}

func (s *FirebaseSink) Status(ctx context.Context, status Status) error {
	_, err := s.fs.Collection("orgs").Doc(s.orgID).Collection("chatRuntime").Doc("discrawlProjection").Set(ctx, status, firestore.MergeAll)
	return err
}

func (s *FirebaseSink) messageRef(id string) *firestore.DocumentRef {
	return s.fs.Collection("orgs").Doc(s.orgID).Collection("chatMessages").Doc(id)
}

func (s *FirebaseSink) tombstoneRef(id string) *firestore.DocumentRef {
	return s.fs.Collection("orgs").Doc(s.orgID).Collection("chatTombstones").Doc(id)
}

func (s *FirebaseSink) writeTicks(ctx context.Context, bindingIDs map[string]struct{}) error {
	ids := make([]string, 0, len(bindingIDs))
	for id := range bindingIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := s.rtdb.NewRef(fmt.Sprintf("chatTicks/%s/%s", s.orgID, id)).Set(ctx, map[string]any{
			"updatedAt": time.Now().UnixMilli(),
		}); err != nil {
			return err
		}
	}
	return nil
}

func bindingTargets(bindings []Binding) map[string]Binding {
	out := make(map[string]Binding, len(bindings))
	for _, binding := range bindings {
		out[binding.TargetID()] = binding
	}
	return out
}

func projectionDocument(binding Binding, message store.ProjectionMessage, existing map[string]any) map[string]any {
	doc := map[string]any{
		"discordMessageId": message.MessageID,
		"discordSortKey":   snowflakeSortKey(message.MessageID),
		"bindingId":        binding.ID,
		"guildId":          binding.GuildID,
		"channelId":        binding.ChannelID,
		"threadId":         binding.ThreadID,
		"content":          message.Content,
		"authorDiscordId":  message.AuthorID,
		"authorName":       message.AuthorName,
		"authorPhoto":      message.AuthorPhotoURL,
		"roleColor":        message.RoleColor,
		"isBot":            message.Bot,
		"isWebhook":        message.Webhook,
		"attachments":      projectionAttachments(message.Attachments),
		"source":           "discord",
		"websiteOrigin":    false,
		"deleted":          message.Deleted,
		"createdAt":        message.CreatedAt,
		"editedAt":         nil,
		"updatedAt":        message.UpdatedAt,
	}
	if !message.EditedAt.IsZero() {
		doc["editedAt"] = message.EditedAt
	}
	if message.ReplyToID != "" {
		doc["replyToMessageId"] = message.ReplyToID
	}
	if message.Deleted {
		doc["content"] = ""
		doc["attachments"] = []map[string]any{}
		doc["deletedAt"] = message.DeletedAt
	}
	if existing["websiteOrigin"] == true {
		for _, key := range []string{"source", "websiteOrigin", "authorDiscordId", "authorName", "authorPhoto", "roleColor", "isBot", "isWebhook"} {
			if value, ok := existing[key]; ok {
				doc[key] = value
			}
		}
	}
	return preserveTerminalDelete(doc, existing, nil)
}

func preserveTerminalDelete(candidate, existing, ledger map[string]any) map[string]any {
	terminal := false
	var deletedAt any
	if existing["deleted"] == true {
		terminal = true
		deletedAt = existing["deletedAt"]
		if deletedAt == nil {
			deletedAt = existing["updatedAt"]
		}
		if deletedAt == nil {
			deletedAt = candidate["updatedAt"]
		}
	}
	if ledger != nil {
		if value, ok := ledger["deletedAt"]; ok {
			terminal = true
			if deletedAt == nil || firestoreTimeBefore(value, deletedAt) {
				deletedAt = value
			}
		}
	}
	if !terminal {
		return candidate
	}
	candidate["content"] = ""
	candidate["attachments"] = []map[string]any{}
	candidate["deleted"] = true
	candidate["deletedAt"] = deletedAt
	if existingUpdated, ok := existing["updatedAt"]; ok {
		candidate["updatedAt"] = existingUpdated
	}
	return candidate
}

func durableTombstoneDocument(existing map[string]any, row store.ProjectionTombstone) (map[string]any, bool, error) {
	if !validSnowflake(row.MessageID) || !validSnowflake(row.GuildID) || !validSnowflake(row.ChannelID) || row.DeletedAt.IsZero() {
		return nil, false, errors.New("invalid durable tombstone scope or deletion time")
	}
	if len(existing) > 0 && (stringField(existing, "guildId") != row.GuildID || stringField(existing, "channelId") != row.ChannelID) {
		return nil, false, errors.New("durable tombstone scope mismatch")
	}
	deletedAt := row.DeletedAt.UTC()
	if len(existing) > 0 {
		current, ok := firestoreTime(existing["deletedAt"])
		if !ok {
			return nil, false, errors.New("durable tombstone has invalid deletion time")
		}
		if current.Before(deletedAt) {
			deletedAt = current
		}
	}
	candidate := map[string]any{
		"discordMessageId": row.MessageID,
		"guildId":          row.GuildID,
		"channelId":        row.ChannelID,
		"deletedAt":        deletedAt,
	}
	return candidate, !reflect.DeepEqual(existing, candidate), nil
}

func firestoreTimeBefore(left, right any) bool {
	l, lok := firestoreTime(left)
	r, rok := firestoreTime(right)
	return lok && rok && l.Before(r)
}

func firestoreTime(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC(), !typed.IsZero()
	case *time.Time:
		if typed != nil && !typed.IsZero() {
			return typed.UTC(), true
		}
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, typed); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func projectionAttachments(rows []store.ArchiveAttachment) []map[string]any {
	out := make([]map[string]any, 0, min(10, len(rows)))
	for _, row := range rows {
		if len(out) >= 10 {
			break
		}
		id := strings.TrimSpace(row.ID)
		filename := strings.TrimSpace(row.Filename)
		if id == "" || filename == "" {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "filename": filename, "contentType": strings.ToLower(strings.TrimSpace(row.ContentType)),
			"size": row.Size,
		})
	}
	return out
}

func projectionFingerprint(doc map[string]any) (string, error) {
	keys := []string{
		"discordMessageId", "discordSortKey", "bindingId", "guildId", "channelId", "threadId",
		"content", "authorDiscordId", "authorName", "authorPhoto", "roleColor", "isBot", "isWebhook",
		"attachments", "source", "websiteOrigin", "deleted", "createdAt", "editedAt", "deletedAt", "replyToMessageId",
	}
	values := make(map[string]any, len(keys))
	for _, key := range keys {
		values[key] = doc[key]
	}
	body, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("marshal projection fingerprint: %w", err)
	}
	return string(body), nil
}

func mapsEqual(a, b map[string]any) (bool, error) {
	aFingerprint, err := projectionFingerprint(a)
	if err != nil {
		return false, err
	}
	bFingerprint, err := projectionFingerprint(b)
	if err != nil {
		return false, err
	}
	return aFingerprint == bFingerprint, nil
}

func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func stringField(input map[string]any, key string) string {
	value, _ := input[key].(string)
	return strings.TrimSpace(value)
}

func snowflakeSortKey(id string) string {
	if len(id) >= 20 {
		return id
	}
	return strings.Repeat("0", 20-len(id)) + id
}

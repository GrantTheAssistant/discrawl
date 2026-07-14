package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
		selected = append(selected, projected{binding: binding, message: message})
	}
	if len(selected) == 0 {
		return 0, nil
	}
	refs := make([]*firestore.DocumentRef, len(selected))
	for i := range selected {
		refs[i] = s.messageRef(selected[i].message.MessageID)
	}
	selectedBindings := map[string]struct{}{}
	for _, item := range selected {
		selectedBindings[item.binding.ID] = struct{}{}
	}
	changed := 0
	err := s.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
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
			if mapsEqual(existing, candidate) {
				continue
			}
			if err := tx.Set(refs[i], candidate, firestore.MergeAll); err != nil {
				return err
			}
			changed++
		}
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
	byTarget := bindingTargets(bindings)
	type selectedTombstone struct {
		binding Binding
		row     store.ProjectionTombstone
	}
	selected := make([]selectedTombstone, 0, len(tombstones))
	for _, row := range tombstones {
		binding, ok := byTarget[row.ChannelID]
		if ok && row.GuildID == binding.GuildID {
			selected = append(selected, selectedTombstone{binding: binding, row: row})
		}
	}
	if len(selected) == 0 {
		return 0, nil
	}
	refs := make([]*firestore.DocumentRef, len(selected))
	for i := range selected {
		refs[i] = s.messageRef(selected[i].row.MessageID)
	}
	selectedBindings := map[string]struct{}{}
	for _, item := range selected {
		selectedBindings[item.binding.ID] = struct{}{}
	}
	changed := 0
	err := s.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		snaps, err := tx.GetAll(refs)
		if err != nil {
			return err
		}
		for i, item := range selected {
			// A marker-only delete blanks an existing website-origin projection,
			// but never creates an identity-free row for an unknown message.
			if !snaps[i].Exists() {
				continue
			}
			existing := snaps[i].Data()
			candidate := cloneMap(existing)
			candidate["discordMessageId"] = item.row.MessageID
			candidate["discordSortKey"] = snowflakeSortKey(item.row.MessageID)
			candidate["bindingId"] = item.binding.ID
			candidate["guildId"] = item.binding.GuildID
			candidate["channelId"] = item.binding.ChannelID
			candidate["threadId"] = item.binding.ThreadID
			candidate["content"] = ""
			candidate["attachments"] = []map[string]any{}
			candidate["deleted"] = true
			candidate["deletedAt"] = item.row.DeletedAt
			candidate["updatedAt"] = item.row.DeletedAt
			if mapsEqual(existing, candidate) {
				continue
			}
			if err := tx.Set(refs[i], candidate, firestore.MergeAll); err != nil {
				return err
			}
			changed++
		}
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

func (s *FirebaseSink) Status(ctx context.Context, status Status) error {
	_, err := s.fs.Collection("orgs").Doc(s.orgID).Collection("chatRuntime").Doc("discrawlProjection").Set(ctx, status, firestore.MergeAll)
	return err
}

func (s *FirebaseSink) messageRef(id string) *firestore.DocumentRef {
	return s.fs.Collection("orgs").Doc(s.orgID).Collection("chatMessages").Doc(id)
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
	return doc
}

func projectionAttachments(rows []store.ArchiveAttachment) []map[string]any {
	out := make([]map[string]any, 0, min(10, len(rows)))
	for _, row := range rows {
		if len(out) >= 10 {
			break
		}
		attachmentURL := safeHTTPSURL(row.URL)
		if attachmentURL == "" {
			continue
		}
		out = append(out, map[string]any{
			"id": row.ID, "filename": row.Filename, "contentType": strings.ToLower(row.ContentType),
			"size": row.Size, "url": attachmentURL, "proxyUrl": safeHTTPSURL(row.ProxyURL),
		})
	}
	return out
}

func safeHTTPSURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" ||
		(parsed.Hostname() != "cdn.discordapp.com" && parsed.Hostname() != "media.discordapp.net") ||
		!strings.HasPrefix(parsed.EscapedPath(), "/attachments/") {
		return ""
	}
	return parsed.String()
}

func projectionFingerprint(doc map[string]any) string {
	keys := []string{
		"discordMessageId", "discordSortKey", "bindingId", "guildId", "channelId", "threadId",
		"content", "authorDiscordId", "authorName", "authorPhoto", "roleColor", "isBot", "isWebhook",
		"attachments", "source", "websiteOrigin", "deleted", "createdAt", "editedAt", "deletedAt", "replyToMessageId",
	}
	values := make(map[string]any, len(keys))
	for _, key := range keys {
		values[key] = doc[key]
	}
	body, _ := json.Marshal(values)
	return string(body)
}

func mapsEqual(a, b map[string]any) bool { return projectionFingerprint(a) == projectionFingerprint(b) }

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

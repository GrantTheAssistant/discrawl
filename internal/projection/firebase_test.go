package projection

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStatusDocumentIsMergeAllCompatibleMap(t *testing.T) {
	now := time.Date(2026, 7, 15, 17, 30, 0, 0, time.UTC)
	doc := statusDocument(Status{
		State: "healthy", GuildID: "1309020691965677648",
		TombstoneSweepComplete: true, AttachmentURLSweepComplete: true,
		LastSuccessAt: now, BindingCount: 3, ProjectedChanges: 12, SchemaVersion: 2,
	})

	require.Equal(t, "healthy", doc["state"])
	require.Equal(t, "1309020691965677648", doc["guildId"])
	require.Equal(t, now, doc["lastSuccessAt"])
	require.Equal(t, time.Time{}, doc["lastFailureAt"])
	require.Equal(t, 2, doc["schemaVersion"])
	require.NotContains(t, statusDocument(Status{State: "degraded"}), "lastSuccessAt")
}

package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSQLCSchemaMirrorsRuntimeTables(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtimeStore, err := Open(ctx, filepath.Join(t.TempDir(), "runtime.db"))
	require.NoError(t, err)
	defer func() { _ = runtimeStore.Close() }()

	schemaDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "sqlc.db"))
	require.NoError(t, err)
	defer func() { _ = schemaDB.Close() }()

	body, err := os.ReadFile(filepath.Join("sqlc", "schema.sql"))
	require.NoError(t, err)
	for stmt := range strings.SplitSeq(string(body), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" || strings.HasPrefix(stmt, "--") {
			continue
		}
		_, err := schemaDB.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	for _, table := range []string{
		"guilds",
		"channels",
		"members",
		"messages",
		"message_events",
		"message_attachments",
		"message_tombstones",
		"mention_events",
		"sync_state",
		"embedding_jobs",
		"message_embeddings",
	} {
		require.Equal(t, tableColumns(ctx, t, runtimeStore.DB(), table), tableColumns(ctx, t, schemaDB, table), table)
	}
}

func tableColumns(ctx context.Context, t *testing.T, db *sql.DB, table string) []string {
	t.Helper()

	rows, err := db.QueryContext(ctx, `pragma table_info(`+table+`)`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var columns []string
	for rows.Next() {
		var cid int
		var name string
		var columnType string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		require.NoError(t, rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk))
		columns = append(columns, name)
	}
	require.NoError(t, rows.Err())
	return columns
}

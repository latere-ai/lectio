// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/store/postgres/migrations"
)

// shape reads what a schema is made of: every function of the control plane
// with its whole definition, and every column of every table with its type
// and its default. The table the migrator keeps its own version in is left
// out.
func shape(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	const read = `
SELECT 'function ' || p.proname || ' ' || pg_get_functiondef(p.oid)
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = current_schema() AND p.proname LIKE 'lectio_%'
UNION ALL
SELECT 'column ' || table_name || '.' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '')
  FROM information_schema.columns
 WHERE table_schema = current_schema() AND table_name <> 'schema_migrations'
UNION ALL
SELECT 'index ' || indexdef FROM pg_indexes WHERE schemaname = current_schema() AND tablename <> 'schema_migrations'`
	rows, err := conn.Query(context.Background(), read)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// migration reads one migration file.
func migration(t *testing.T, name string) string {
	t.Helper()
	raw, err := fs.ReadFile(migrations.FS, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestTheLastMigrationIsUndoneByItsDownFile: the newest migration's down
// file leaves the schema the migrations before it made, function by
// function and column by column, with work of the kinds it added queued,
// and its up file then applies again. A down file that restored a function
// from the wrong migration, or left a table behind, would show here and not
// on the day a release is rolled back.
func TestTheLastMigrationIsUndoneByItsDownFile(t *testing.T) {
	srv := server(t)
	t.Parallel()
	ctx := context.Background()
	highest, err := migrations.Highest()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var earlier []string
	last := ""
	for _, e := range entries {
		name, found := strings.CutSuffix(e.Name(), ".up.sql")
		if !found {
			continue
		}
		if number, _, _ := strings.Cut(name, "_"); strings.TrimLeft(number, "0") == strconv.FormatUint(uint64(highest), 10) {
			last = name
			continue
		}
		earlier = append(earlier, e.Name())
	}
	if last == "" || len(earlier) == 0 {
		t.Fatalf("the migrations hold no newest one after others: %q, %v", last, earlier)
	}

	// One database holds the migrations before the newest, each applied as
	// the migrator applies it.
	before, err := pgx.Connect(ctx, database(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = before.Close(ctx) }()
	for _, name := range earlier {
		if _, err := before.Exec(ctx, migration(t, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	// The other holds them all, with an extraction and a figure queued,
	// which are tasks of kinds the newest migration added.
	dsn := database(t, srv)
	if err := postgres.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	store, err := postgres.Open(ctx, dsn, postgres.Options{Settings: withDescribers()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	after, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = after.Close(ctx) }()
	h := &harness{t: t, store: store, admin: after, now: epoch}
	store.SetClock(h.now)
	h.submit(postgres.Submission{Parse: "prs_a", Owner: "alice"})
	w := h.worker()
	h.through(w, "prs_a", 1)
	h.field("prs_a", "invoice", "")
	h.figures("prs_a", "", false, "1.2")
	if n := value[int](h, `SELECT count(*) FROM tasks WHERE kind IN ('extract', 'figure') AND state = 'queued'`); n != 2 {
		t.Fatalf("%d tasks of the new kinds are queued, want 2", n)
	}

	if _, err := after.Exec(ctx, migration(t, last+".down.sql")); err != nil {
		t.Fatalf("the down file: %v", err)
	}
	want, got := shape(t, before), shape(t, after)
	if !slices.Equal(want, got) {
		for _, line := range got {
			if !slices.Contains(want, line) {
				t.Errorf("after the down file the schema holds what the earlier migrations did not make:\n%.400s", line)
			}
		}
		for _, line := range want {
			if !slices.Contains(got, line) {
				t.Errorf("after the down file the schema lacks what the earlier migrations made:\n%.400s", line)
			}
		}
	}
	// The tasks of the kinds that are gone went with it, and the counters
	// of queued tasks with them.
	h.consistent()
	if n := value[int](h, `SELECT count(*) FROM tasks WHERE kind IN ('extract', 'figure')`); n != 0 {
		t.Fatalf("%d tasks of the kinds the migration added are left", n)
	}
	if _, err := after.Exec(ctx, migration(t, last+".up.sql")); err != nil {
		t.Fatalf("the up file, applied again: %v", err)
	}
}

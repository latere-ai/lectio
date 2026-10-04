// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// TestMigrateRefusesAURLThatIsNotPostgres keeps a misconfigured deployment
// from starting, and keeps the URL, which holds a password, out of the
// error.
func TestMigrateRefusesAURLThatIsNotPostgres(t *testing.T) {
	for name, dsn := range map[string]string{
		"not a URL":      "://s3cret-nope",
		"another scheme": "mysql://user:s3cret@localhost:3306/lectio",
	} {
		t.Run(name, func(t *testing.T) {
			err := postgres.Migrate(t.Context(), dsn)
			if err == nil {
				t.Fatalf("%q was accepted", dsn)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Fatalf("the error quotes the URL: %v", err)
			}
		})
	}
	// A server that does not answer is an error and not a hang.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := postgres.Migrate(ctx, "postgres://lectio:lectio@127.0.0.1:1/lectio?sslmode=disable&connect_timeout=2"); err == nil {
		t.Fatal("migrating against a closed port succeeded")
	}
}

// TestMigrateIsSafeToRepeat: every replica migrates at start, and all but
// the first find nothing to apply.
func TestMigrateIsSafeToRepeat(t *testing.T) {
	dsn := database(t, server(t))
	for range 2 {
		if err := postgres.Migrate(t.Context(), dsn); err != nil {
			t.Fatalf("migrating: %v", err)
		}
	}
}

// TestASchemaThisBinaryDoesNotKnowIsRefused: a schema a failed migration
// left halfway, or one a newer binary wrote, stops the migrator and the
// store, which would otherwise call functions that mean something else.
func TestASchemaThisBinaryDoesNotKnowIsRefused(t *testing.T) {
	srv := server(t)
	for _, tc := range []struct{ name, statement, want string }{
		{"ahead of the binary", `UPDATE schema_migrations SET version = 9999, dirty = false`, "9999"},
		{"dirty", `UPDATE schema_migrations SET dirty = true`, "dirty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := open(t, srv, modes[0], defaults())
			dsn := h.admin.Config().ConnString()
			h.exec(tc.statement)
			if err := postgres.Migrate(t.Context(), dsn); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Migrate = %v, want an error naming %q", err, tc.want)
			}
			if _, err := postgres.Open(t.Context(), dsn, postgres.Options{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open = %v, want an error naming %q", err, tc.want)
			}
			// The case leaves the schema as the next step expects it.
			h.exec(`UPDATE schema_migrations SET version = 1, dirty = false`)
		})
	}
}

// TestADatabaseTheMigratorCannotUseIsAnError: a version table that is not
// the migrator's, and a schema that collides with the one it applies, each
// stop the start with the step that failed named. A database whose functions
// are not the ones this binary calls stops the store.
func TestADatabaseTheMigratorCannotUseIsAnError(t *testing.T) {
	srv := server(t)
	run := func(dsn, statement string) {
		t.Helper()
		conn, err := pgx.Connect(t.Context(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close(context.Background()) }()
		if _, err := conn.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}

	foreign := database(t, srv)
	run(foreign, `CREATE TABLE schema_migrations (version text, dirty boolean); INSERT INTO schema_migrations VALUES ('one', false)`)
	if err := postgres.Migrate(t.Context(), foreign); err == nil || !strings.Contains(err.Error(), "reading the schema version") {
		t.Fatalf("Migrate over a version table that is not the migrator's = %v", err)
	}

	taken := database(t, srv)
	run(taken, `CREATE TABLE settings (one integer)`)
	if err := postgres.Migrate(t.Context(), taken); err == nil || !strings.Contains(err.Error(), "applying the schema") {
		t.Fatalf("Migrate over a schema that collides = %v", err)
	}

	altered := database(t, srv)
	if err := postgres.Migrate(t.Context(), altered); err != nil {
		t.Fatal(err)
	}
	run(altered, `DROP FUNCTION lectio_configure(text)`)
	if _, err := postgres.Open(t.Context(), altered, postgres.Options{}); err == nil || !strings.Contains(err.Error(), "writing the settings") {
		t.Fatalf("Open of a database without the store's functions = %v", err)
	}
}

// TestOpenRefusesWhatItCannotRunWith: a URL that is not Postgres, settings
// no store can run with, a database nobody migrated, and a server that does
// not answer are each an error at start.
func TestOpenRefusesWhatItCannotRunWith(t *testing.T) {
	_, err := postgres.Open(t.Context(), "mysql://user:s3cret@pooler/lectio", postgres.Options{})
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("Open of a URL that is not Postgres = %v", err)
	}
	_, err = postgres.Open(t.Context(), "postgres://localhost/lectio", postgres.Options{
		Settings: tasks.Settings{ReadChain: []string{"gone"}},
	})
	if err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("Open with a chain that names no pool = %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := postgres.Open(ctx, "postgres://lectio:lectio@127.0.0.1:1/lectio?sslmode=disable&connect_timeout=2", postgres.Options{}); err == nil {
		t.Fatal("opening against a closed port succeeded")
	}

	fresh := database(t, server(t))
	if _, err := postgres.Open(t.Context(), fresh, postgres.Options{}); err == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("Open of a database nobody migrated = %v", err)
	}
}

// TestThePoolIsBounded: the pool never grows past its ceiling, whatever is
// asked for.
func TestThePoolIsBounded(t *testing.T) {
	for asked, want := range map[int32]int32{0: postgres.DefaultMaxConns, -3: postgres.DefaultMaxConns, 1: 1, 1000: postgres.MaxMaxConns} {
		if got := postgres.Bound(asked); got != want {
			t.Errorf("a pool of %d is opened with %d connections, want %d", asked, got, want)
		}
	}
}

// TestOpenWritesTheSettings: the store writes what it was configured with,
// so the functions read it from the database. A pool that was there keeps
// its breaker state, and the pool of a reader that left the configuration
// goes with its scopes.
func TestOpenWritesTheSettings(t *testing.T) {
	everywhere(t, tasks.Settings{
		Lease: 90 * time.Second, Attempts: 7, InteractiveWeight: 9, BatchWeight: 2, KeysPerGroup: true,
		Pools:     []tasks.Pool{{Reader: "small", MaxInFlight: 16}, {Reader: "large", MaxInFlight: 4, Cost: 5}},
		ReadChain: []string{"small", "large"},
	}, func(t *testing.T, h *harness) {
		type row struct {
			Lease      string   `json:"lease"`
			Attempts   int      `json:"attempts"`
			Expiries   int      `json:"expiries"`
			ByGroup    bool     `json:"scope_by_group"`
			ReadChain  []string `json:"read_chain"`
			ExtractAll []string `json:"extract_chain"`
		}
		var got row
		if err := h.store.Decode(t.Context(), &got, `SELECT to_jsonb(s)::text FROM settings s`); err != nil {
			t.Fatal(err)
		}
		if got.Lease != "00:01:30" || got.Attempts != 7 || got.Expiries != tasks.DefaultExpiries || !got.ByGroup ||
			strings.Join(got.ReadChain, ",") != "small,large" || got.ExtractAll == nil || len(got.ExtractAll) != 0 {
			t.Fatalf("the settings row is %+v", got)
		}
		if w := value[string](h, `SELECT string_agg(weight::text, ',' ORDER BY class) FROM class_service`); w != "9,2" {
			t.Fatalf("the class weights are %s", w)
		}
		if p := value[string](h, `SELECT string_agg(reader || ':' || max_in_flight || ':' || cost, ',' ORDER BY reader) FROM pools`); p != "large:4:5,small:16:1" {
			t.Fatalf("the pools are %s", p)
		}

		// The breaker of a pool that stays is kept across a restart.
		h.exec(`UPDATE pools SET failures = 2 WHERE reader = 'small'`)
		h.exec(`INSERT INTO pool_scopes (reader, scope, ceiling) VALUES ('large', 'acme', 2), ('small', 'acme', 8)`)
		again, err := postgres.Open(t.Context(), h.admin.Config().ConnString(), postgres.Options{Settings: tasks.Settings{
			Pools: []tasks.Pool{{Reader: "small", MaxInFlight: 32, Cost: 2}}, ReadChain: []string{"small"},
		}})
		if err != nil {
			t.Fatalf("opening again: %v", err)
		}
		defer again.Close()
		if p := value[string](h, `SELECT string_agg(reader || ':' || max_in_flight || ':' || cost || ':' || failures, ',') FROM pools`); p != "small:32:2:2" {
			t.Fatalf("after a restart with other readers the pools are %s", p)
		}
		if s := value[string](h, `SELECT string_agg(reader, ',') FROM pool_scopes`); s != "small" {
			t.Fatalf("after a restart the scopes are of %s", s)
		}
		if lease := value[string](h, `SELECT lease::text FROM settings`); lease != "00:01:00" {
			t.Fatalf("the last store to open did not decide the lease: %s", lease)
		}
	})
}

// TestSubmitRefusesWhatIsNotAParse: a parse has an id, an owner, a class the
// queue knows, and a deadline.
func TestSubmitRefusesWhatIsNotAParse(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ok := postgres.Submission{Parse: "prs_a", Owner: "alice", Deadline: time.Hour}
		for name, change := range map[string]func(*postgres.Submission){
			"no id":       func(s *postgres.Submission) { s.Parse = "" },
			"no owner":    func(s *postgres.Submission) { s.Owner = "" },
			"a class":     func(s *postgres.Submission) { s.Class = 2 },
			"no deadline": func(s *postgres.Submission) { s.Deadline = 0 },
		} {
			sub := ok
			change(&sub)
			if _, _, err := h.store.Submit(t.Context(), sub); fault.CodeOf(err) != fault.InvalidRequest {
				t.Errorf("%s: Submit = %v, want invalid_request", name, err)
			}
		}
		if n := value[int64](h, `SELECT count(*) FROM parses`); n != 0 {
			t.Fatalf("a refused submit wrote %d parses", n)
		}
	})
}

// TestSubmitRefreshesTheGroupAndTheProject: the group is the owner when the
// limits name none, a parse joins the group's own project when they name
// none, and every submit carries the weights and bounds in force, so a
// change reaches the queue with the tenant's next parse. A submit repeated
// under one id writes nothing the second time.
func TestSubmitRefreshesTheGroupAndTheProject(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := t.Context()
		h.submit(postgres.Submission{Parse: "prs_a", Owner: "alice", Class: tasks.Batch, Priority: 3})
		p := h.parse("prs_a")
		if p.Group != "alice" || p.Project != "" || p.Class != tasks.Batch || p.Priority != 3 || p.Owner != "alice" || p.Pin != "" {
			t.Fatalf("the parse is %+v", p)
		}
		if got := h.task("prs_a", tasks.PrepareID); got.State != tasks.Queued || got.Class != tasks.Batch || got.Priority != 3 || got.Seq >= 0 {
			t.Fatalf("the prepare task is %+v", got)
		}

		h.submit(postgres.Submission{
			Parse: "prs_b", Owner: "bob", Group: "acme", Project: "search", Weight: 4, ProjectWeight: 2,
			MaxRunning: 10, MaxQueued: 20, MaxPriority: 5, Pin: "large",
		})
		h.submit(postgres.Submission{Parse: "prs_c", Owner: "carol", Group: "acme", Project: "search", Weight: 5000, ProjectWeight: 7})
		_, created, err := h.store.Submit(ctx, filled(postgres.Submission{Parse: "prs_c", Owner: "carol", Group: "acme", Project: "search", Weight: 1}))
		if err != nil || created {
			t.Fatalf("a submit repeated under one id: created %t, %v", created, err)
		}

		queue, err := h.store.Queue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(queue) != 2 || queue[0].Group != "acme" || queue[1].Group != "alice" {
			t.Fatalf("the queue is %+v", queue)
		}
		acme := queue[0]
		// The repeated submit still carried the limits in force: the weight
		// is the last one given, held to its range, and a bound of 0 is none.
		if acme.Weight != 1 || acme.MaxRunning != 0 || acme.Parses != 2 || len(acme.Projects) != 1 ||
			acme.Projects[0].Project != "search" || acme.Projects[0].Weight != 1 ||
			len(acme.Classes) != 1 || acme.Classes[0] != (postgres.ClassQueue{Class: tasks.Interactive, Queued: 2}) ||
			acme.Projects[0].Classes[0].Queued != 2 {
			t.Fatalf("the queue of acme is %+v", acme)
		}
		if alice := queue[1]; alice.Weight != 1 || alice.Parses != 1 || alice.Projects[0].Project != "" ||
			alice.Classes[0] != (postgres.ClassQueue{Class: tasks.Batch, Queued: 1}) {
			t.Fatalf("the queue of alice is %+v", alice)
		}
		if w := value[int](h, `SELECT weight FROM groups WHERE group_id = 'acme'`); w != 1 {
			t.Fatalf("the group's weight is %d", w)
		}
		if h.parse("prs_b").Pin != "large" {
			t.Fatal("the parse lost the reader it named")
		}
	})
}

// TestAGroupAtMaxQueuedIsRefused: a submit is refused with queue_full while
// the group holds max_queued parses that have not ended, and admitted again
// when one ends.
func TestAGroupAtMaxQueuedIsRefused(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := t.Context()
		for _, id := range []string{"prs_1", "prs_2"} {
			h.submit(postgres.Submission{Parse: id, Group: "acme", MaxQueued: 2})
		}
		next := filled(postgres.Submission{Parse: "prs_3", Group: "acme", MaxQueued: 2})
		if _, _, err := h.store.Submit(ctx, next); fault.CodeOf(err) != fault.QueueFull {
			t.Fatalf("a submit past max_queued = %v", err)
		}
		// Another group is not held to this one's bound.
		h.submit(postgres.Submission{Parse: "prs_other", Group: "globex", MaxQueued: 2})

		if err := h.store.Cancel(ctx, "prs_1"); err != nil {
			t.Fatal(err)
		}
		if _, created, err := h.store.Submit(ctx, next); err != nil || !created {
			t.Fatalf("a submit after a parse ended: created %t, %v", created, err)
		}
	})
}

// TestReadsOfWhatIsNotThere: a parse that does not exist is not found, and
// has no tasks.
func TestReadsOfWhatIsNotThere(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		if _, err := h.store.Parse(t.Context(), "prs_none"); fault.CodeOf(err) != fault.ParseNotFound {
			t.Fatalf("Parse of nothing = %v", err)
		}
		rows, err := h.store.Tasks(t.Context(), "prs_none")
		if err != nil || len(rows) != 0 {
			t.Fatalf("Tasks of nothing = %v, %v", rows, err)
		}
		queue, err := h.store.Queue(t.Context())
		if err != nil || len(queue) != 0 {
			t.Fatalf("the queue of an empty store = %v, %v", queue, err)
		}
	})
}

// TestAnExchangeTheStoreCannotReadIsAnError: a request the protocol does not
// allow is refused before it is sent, a worker the store never registered is
// answered as given up, and a document that is not JSON is an error and not
// a silent zero.
func TestAnExchangeTheStoreCannotReadIsAnError(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := t.Context()
		w := h.worker()
		if _, err := h.store.Exchange(ctx, w.id, tasks.Request{Free: -1}); err == nil {
			t.Fatal("an exchange asking for a number of tasks below zero was sent")
		}
		broken := tasks.Settle{Parse: "prs_a", Task: tasks.PrepareID, Outcome: tasks.Done,
			Prepare: &tasks.Prepared{Manifest: json.RawMessage(`{"pages_total":`)}}
		if _, err := h.store.Exchange(ctx, w.id, tasks.Request{Settles: []tasks.Settle{broken}}); err == nil {
			t.Fatal("a manifest that is not JSON was sent")
		}
		reply, err := h.store.Exchange(ctx, "wrk_never_registered", tasks.Request{
			Free: 1, Settles: []tasks.Settle{{Parse: "prs_a", Task: "page-1", Outcome: tasks.Done}},
			Held: []tasks.Held{{Parse: "prs_a", Task: "page-2", Token: 1}},
		})
		if err != nil || !reply.Gone || len(reply.Refused) != 1 || len(reply.Lost) != 1 {
			t.Fatalf("the exchange of an unknown worker = %+v, %v", reply, err)
		}

		var into struct{}
		if err := h.store.Decode(ctx, &into, `SELECT 'not a document'`); err == nil {
			t.Fatal("an answer that is not JSON was read")
		}
	})
}

// TestAStoreWithNoDatabaseReturnsErrors: every call of a store whose pool is
// closed, or whose context has ended, is an error the caller sees.
func TestAStoreWithNoDatabaseReturnsErrors(t *testing.T) {
	h := open(t, server(t), modes[0], defaults())
	w := h.worker()
	h.submit(postgres.Submission{Parse: "prs_a", Group: "acme"})
	h.store.Close()

	ctx := t.Context()
	if _, err := h.store.Register(ctx); err == nil {
		t.Error("Register on a closed store succeeded")
	}
	if _, err := h.store.Exchange(ctx, w.id, tasks.Request{}); err == nil {
		t.Error("Exchange on a closed store succeeded")
	}
	if _, _, err := h.store.Submit(ctx, filled(postgres.Submission{Parse: "prs_b", Group: "acme"})); err == nil || fault.CodeOf(err) != fault.Internal {
		t.Errorf("Submit on a closed store = %v", err)
	}
	if err := h.store.Cancel(ctx, "prs_a"); err == nil || fault.CodeOf(err) != fault.Internal {
		t.Errorf("Cancel on a closed store = %v", err)
	}
	if _, err := h.store.Parse(ctx, "prs_a"); err == nil || fault.CodeOf(err) != fault.Internal {
		t.Errorf("Parse on a closed store = %v", err)
	}
	if _, err := h.store.Tasks(ctx, "prs_a"); err == nil {
		t.Error("Tasks on a closed store succeeded")
	}
	if _, err := h.store.Queue(ctx); err == nil {
		t.Error("Queue on a closed store succeeded")
	}
	for name, call := range map[string]func() error{
		"Ping":    func() error { return h.store.Ping(ctx) },
		"Task":    func() error { _, _, err := h.store.Task(ctx, "prs_a", "prepare"); return err },
		"ParseOf": func() error { _, err := h.store.ParseOf(ctx, "acme", "prs_a"); return err },
		"Parses": func() error {
			_, _, err := h.store.Parses(ctx, []string{"acme"}, postgres.Filter{}, "", 10)
			return err
		},
		"DeleteParse":   func() error { return h.store.DeleteParse(ctx, "acme", "prs_a") },
		"File":          func() error { _, err := h.store.File(ctx, "fil_1"); return err },
		"FileByContent": func() error { _, _, err := h.store.FileByContent(ctx, "acme", "aa"); return err },
		"InsertFile":    func() error { _, _, err := h.store.InsertFile(ctx, file("acme", "fil_1", "aa")); return err },
		"DeleteFile":    func() error { _, err := h.store.DeleteFile(ctx, "acme", "fil_1"); return err },
		"ForgetFile":    func() error { return h.store.ForgetFile(ctx, "fil_1") },
	} {
		if err := call(); err == nil || fault.CodeOf(err) != fault.Internal {
			t.Errorf("%s on a closed store = %v", name, err)
		}
	}
}

// TestMigrationURL: the migrator's URL is the direct one under the scheme its
// driver registers, with everything else kept.
func TestMigrationURL(t *testing.T) {
	got, err := postgres.MigrationURL("postgresql://lectio:pw@db.example:5432/lectio?sslmode=require")
	if err != nil || got != "pgx5://lectio:pw@db.example:5432/lectio?sslmode=require" {
		t.Fatalf("MigrationURL = %q, %v", got, err)
	}
}

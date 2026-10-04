// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/durable"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/internal/testservers"
	"latere.ai/x/lectio/internal/worker"
)

// TestMain removes the containers the durable run started.
func TestMain(m *testing.M) { testservers.Main(m) }

// durableBench is the durable control plane a case runs over: a database of
// its own on the suite's Postgres, an object store, and a worker in the
// test's process that claims from the task store as a worker process does.
type durableBench struct {
	srv testservers.Postgres
}

// start makes the server durable: its backend is the task store and an
// object store, and a worker runs the tasks with what the case set on the
// runner. The stop it returns ends the worker.
func (d *durableBench) start(ctx context.Context, t *testing.T, s *Server, runner *run.Runner) (stop func()) {
	t.Helper()
	// The database is the case's and not the server's: it is dropped when
	// the case ends, after the server's context has.
	dsn := testservers.Database(t, d.srv) //nolint:contextcheck
	if err := postgres.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	// A chain may name a reader that is not configured, which the memory
	// runner passes over; the task store is opened with the ones that are.
	settings := tasks.Settings{
		Lease: 2 * time.Second, SweepInterval: 200 * time.Millisecond, Attempts: 3,
		BackoffBase: time.Millisecond, BackoffCap: time.Millisecond,
	}
	if runner.Attempts > 0 {
		settings.Attempts = runner.Attempts
	}
	for _, name := range slices.Sorted(mapKeys(runner.Readers)) {
		settings.Pools = append(settings.Pools, tasks.Pool{Reader: name, MaxInFlight: 64})
	}
	for _, name := range runner.Chain {
		if runner.Readers[name] != nil {
			settings.ReadChain = append(settings.ReadChain, name)
		}
	}
	st, err := postgres.Open(ctx, dsn, postgres.Options{Settings: settings})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	objects := blob.NewMemory()
	s.Backend = &durable.Backend{
		Store: st, Objects: objects, Readers: runner.Readers, Chain: settings.ReadChain,
		MaxDeadline: s.MaxDeadline, Poll: 5 * time.Millisecond, Log: slog.New(slog.DiscardHandler),
	}
	w := &worker.Worker{
		Store: st, Objects: objects, Pipeline: runner.Pipeline, Readers: runner.Readers,
		Slots: runner.Workers, Lease: settings.Lease, Flush: 2 * time.Millisecond, Poll: 5 * time.Millisecond,
		Grace: 50 * time.Millisecond, CacheBytes: 64 << 20, Log: slog.New(slog.DiscardHandler),
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	return func() {
		if err := <-done; err != nil {
			t.Errorf("the worker stopped with %v", err)
		}
		st.Close()
	}
}

func mapKeys[V any](m map[string]V) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// TestTheContractHoldsOverTheDurableBackend runs the cases of the contract
// over the durable backend: the same handlers, the same requests and the
// same assertions, with the parses run by a worker that claims their tasks
// from Postgres and every response held to api/openapi.yaml.
func TestTheContractHoldsOverTheDurableBackend(t *testing.T) {
	srv, err := testservers.StartPostgres()
	if err != nil {
		t.Skipf("no container runtime answered, so the durable run of the contract did not happen: %v", err)
	}
	over = &durableBench{srv: srv}
	defer func() { over = nil }()

	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"a file is parsed and read", TestAFileIsParsedAndRead},
		{"a native file is parsed with no reader", TestANativeFileIsParsedWithNoReader},
		{"a parse is read while it runs and can be canceled", TestAParseIsReadWhileItRunsAndCanBeCanceled},
		{"a submit can be held until the parse ends", TestASubmitCanBeHeldUntilTheParseEnds},
		{"a failed page fails the parse unless allowed", TestAFailedPageFailsTheParseUnlessAllowed},
		{"a document is rendered as it is read", TestADocumentIsRenderedAsItIsRead},
		{"a submit is checked against the contract", TestASubmitIsCheckedAgainstTheContract},
		{"a submit is safe to repeat with a key", TestASubmitIsSafeToRepeatWithAKey},
		{"parses are listed newest first", TestParsesAreListedNewestFirst},
		{"a file is fetched from a URL", TestAFileIsFetchedFromAURL},
		{"uploads are checked", TestUploadsAreChecked},
		{"a caller is known and sees only its own", TestACallerIsKnownAndSeesOnlyItsOwn},
		{"readers are listed with the default first", TestReadersAreListedWithTheDefaultFirst},
		{"what is not routed answers in the same shape", TestWhatIsNotRoutedAnswersInTheSameShape},
	} {
		t.Run(tc.name, tc.run)
	}

	// Describing figures is not built over the durable control plane: the
	// request is routed, authenticated and answered 501, and the listing
	// shows the figures with no run.
	t.Run("figures are listed and not described", func(t *testing.T) {
		e := serve(t, nil)
		pid := e.parsed(e.upload("scan.png", sheet(t)), "")["id"].(string)
		if got := e.do("POST", "/parses/"+pid+"/figures", nil); got.status != http.StatusNotImplemented || got.code(t) != "not_implemented" {
			t.Fatalf("describing figures: %d %s", got.status, got.body)
		}
		listed := e.do("GET", "/parses/"+pid+"/figures", nil)
		if listed.status != http.StatusOK || listed.json(t)["run"] != nil || listed.json(t)["figures"] == nil {
			t.Fatalf("the figures of a parse: %d %s", listed.status, listed.body)
		}
	})
}

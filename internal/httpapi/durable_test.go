// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/durable"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/objects"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/internal/testservers"
	"latere.ai/x/lectio/internal/worker"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// TestMain removes the containers the durable run started.
func TestMain(m *testing.M) { testservers.Main(m) }

// durableBench is the durable control plane a case runs over: a database of
// its own on the suite's Postgres, an object store, and a worker in the
// test's process that claims from the task store as a worker process does.
type durableBench struct {
	srv testservers.Postgres
	// idle starts no worker: the case claims the tasks itself, to see the
	// order they are dispatched in.
	idle bool

	// extractors are what fills a schema in the servers a case starts, by
	// name, and extractChain the routing policy's order of them. The
	// in-process runner extracts nothing, so a case sets them here.
	extractors   map[string]reader.Extractor
	extractChain []string

	// objects, when a case sets it, is put around the object store the
	// server and its worker share, so the case sees what each of them
	// writes and removes.
	objects func(blob.Store) blob.Store
}

// durably runs the servers a test starts over the durable backend, until
// the test ends. The test skips where no container runtime answers.
func durably(t *testing.T, idle bool) {
	t.Helper()
	srv, err := testservers.StartPostgres()
	if err != nil {
		t.Skipf("no container runtime answered, so the durable run did not happen: %v", err)
	}
	over = &durableBench{srv: srv, idle: idle}
	t.Cleanup(func() { over = nil })
}

// storeOf is the task store behind a server of a durable run.
func storeOf(t *testing.T, e *env) *postgres.Store {
	t.Helper()
	b, ok := e.server.Backend.(*durable.Backend)
	if !ok {
		t.Fatal("the server is not over the durable backend")
	}
	return b.Store
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
	// A reader, a describer and an extractor of one name share one pool.
	names := slices.Collect(mapKeys(runner.Readers))
	names = slices.AppendSeq(names, mapKeys(runner.Describers))
	names = slices.AppendSeq(names, mapKeys(d.extractors))
	slices.Sort(names)
	for _, name := range slices.Compact(names) {
		settings.Pools = append(settings.Pools, tasks.Pool{Reader: name, MaxInFlight: 64})
	}
	for _, name := range runner.Chain {
		if runner.Readers[name] != nil {
			settings.ReadChain = append(settings.ReadChain, name)
		}
	}
	for _, name := range runner.DescribeChain {
		if runner.Describers[name] != nil {
			settings.DescribeChain = append(settings.DescribeChain, name)
		}
	}
	settings.ExtractChain = d.extractChain
	st, err := postgres.Open(ctx, dsn, postgres.Options{Settings: settings})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	var objects blob.Store = blob.NewMemory()
	if d.objects != nil {
		objects = d.objects(objects)
	}
	s.Backend = &durable.Backend{
		Store: st, Objects: objects, Readers: runner.Readers, Chain: settings.ReadChain,
		Describers: runner.Describers, DescribeChain: settings.DescribeChain,
		Extractors: d.extractors, ExtractChain: settings.ExtractChain,
		MaxDeadline: s.MaxDeadline, Poll: 5 * time.Millisecond, Log: slog.New(slog.DiscardHandler),
	}
	if d.idle {
		return st.Close
	}
	w := &worker.Worker{
		Store: st, Objects: objects, Pipeline: runner.Pipeline, Readers: runner.Readers,
		Describers: runner.Describers, Extractors: d.extractors,
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
		{"every route asks its action", TestEveryRouteAsksItsAction},
		{"what a question carries", TestWhatAQuestionCarries},
		{"a deny and an outage answer before anything is done", TestADenyAndAnOutageAnswerBeforeAnythingIsDone},
		{"a block's image is its region of the page", TestABlocksImageIsItsRegionOfThePage},
		{"figures are described on request", TestFiguresAreDescribedOnRequest},
		{"a figure run is one at a time and says what it lost", TestAFigureRunIsOneAtATimeAndSaysWhatItLost},
	} {
		t.Run(tc.name, tc.run)
	}
}

// failing is an object store whose calls fail when a case says so.
type failing struct {
	blob.Store
	down bool
}

var errObjects = errors.New("the object store does not answer")

func (f *failing) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if f.down {
		return errObjects
	}
	return f.Store.Put(ctx, key, data, contentType)
}

func (f *failing) Get(ctx context.Context, key string) ([]byte, string, error) {
	if f.down {
		return nil, "", errObjects
	}
	return f.Store.Get(ctx, key)
}

func (f *failing) Delete(ctx context.Context, key string) error {
	if f.down {
		return errObjects
	}
	return f.Store.Delete(ctx, key)
}

func (f *failing) List(ctx context.Context, prefix string) ([]string, error) {
	if f.down {
		return nil, errObjects
	}
	return f.Store.List(ctx, prefix)
}

// versionless is a reader that promises nothing about its results.
type versionless struct{ stub.Reader }

func (*versionless) Describe() reader.Description { return reader.Description{Name: "versionless"} }

// TestTheDurableBackendSaysWhatItsStoresDoNotAnswer: what the task store or
// the object store fails is an error the handlers answer as internal, and
// never a page or a document made of nothing. A page is found where its
// parse's state says it is: through the manifest for a native format,
// through its task's row while the parse runs, and through the index after.
func TestTheDurableBackendSaysWhatItsStoresDoNotAnswer(t *testing.T) {
	srv, err := testservers.StartPostgres()
	if err != nil {
		t.Skipf("no container runtime answered, so the durable backend did not run: %v", err)
	}
	ctx := context.Background()
	dsn := testservers.Database(t, srv)
	if err := postgres.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	settings := tasks.Settings{Pools: []tasks.Pool{{Reader: "stub", MaxInFlight: 8}}, ReadChain: []string{"stub"}}
	st, err := postgres.Open(ctx, dsn, postgres.Options{Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	objs := &failing{Store: blob.NewMemory()}
	b := &durable.Backend{
		Store: st, Objects: objs, Poll: time.Millisecond,
		Readers: map[string]reader.Reader{"stub": &stub.Reader{}, "versionless": &versionless{}}, Chain: []string{"gone", "stub"},
	}
	failed := func(what string, err error) {
		t.Helper()
		if err == nil || fault.CodeOf(err) != fault.Internal {
			t.Errorf("%s: %v", what, err)
		}
	}

	// A file whose row cannot be written leaves no object behind, and says
	// so when the object cannot be removed either.
	first, created, err := b.PutFile(ctx, store.File{ID: "fil_1", Owner: "alice", SHA256: "aa", MediaType: "image/png", Data: []byte("x")})
	if err != nil || !created {
		t.Fatalf("storing a file: %v", err)
	}
	if _, _, err := b.PutFile(ctx, store.File{ID: "fil_1", Owner: "bob", SHA256: "bb", MediaType: "image/png", Data: []byte("y")}); err == nil {
		t.Fatal("a file under an id that is taken was stored")
	}
	if keys, err := objs.List(ctx, "sources/"); err != nil || len(keys) != 1 {
		t.Fatalf("after a row that could not be written the bucket holds %v, %v", keys, err)
	}
	objs.down = true
	_, _, err = b.PutFile(ctx, store.File{ID: "fil_2", Owner: "alice", SHA256: "cc", MediaType: "image/png", Data: []byte("z")})
	failed("storing a file with the object store down", err)
	failed("deleting a file with the object store down", b.DeleteFile(ctx, "alice", first.ID))
	objs.down = false

	// What a read of a page means has no name without the file's digest,
	// or with a reader that promises nothing; a reader the chain names and
	// nobody configured is passed over.
	submit := func(id string, p store.Parse) store.Parse {
		t.Helper()
		p.ID, p.Owner, p.CreatedAt = id, "alice", time.Now()
		stored, _, err := b.Submit(ctx, p, store.Admission{}, "", "")
		if err != nil {
			t.Fatalf("submitting %s: %v", id, err)
		}
		return stored
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	base := func(id string) (out string) {
		if err := admin.QueryRow(ctx, `SELECT coalesce(read_base, '') FROM parses WHERE parse_id = $1`, id).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	submit("prs_nosha", store.Parse{})
	submit("prs_versionless", store.Parse{ContentSHA: "aa", Reader: "versionless"})
	running := submit("prs_run", store.Parse{ContentSHA: "aa"})
	if base("prs_nosha") != "" || base("prs_versionless") != "" || len(base("prs_run")) != 64 {
		t.Fatalf("the read bases are %q, %q and %q", base("prs_nosha"), base("prs_versionless"), base("prs_run"))
	}
	for _, id := range []string{"prs_nosha", "prs_versionless"} {
		if _, err := b.Cancel(ctx, "alice", id); err != nil {
			t.Fatal(err)
		}
	}

	// The running parse: one page read, one failed, one with a summary that
	// is none, one still queued.
	worker, err := st.Register(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// held is what the worker says it still runs.
	var held []tasks.Held
	exchange := func(free int, settles ...tasks.Settle) []tasks.Claim {
		t.Helper()
		reply, err := st.Exchange(ctx, worker, tasks.Request{Settles: settles, Held: held, Free: free, Idle: true})
		if err != nil || len(reply.Refused) != 0 {
			t.Fatalf("the exchange: %+v, %v", reply, err)
		}
		return reply.Claims
	}
	prepare := exchange(1)[0]
	manifest := `{"media_type":"image/tiff","pages_total":4,"selected":[1,2,3,4],"source":"reader","work":"sources/x","token":1}`
	exchange(0, tasks.Settle{Parse: prepare.Parse, Task: prepare.Task, Token: prepare.Token, Outcome: tasks.Done,
		Prepare: &tasks.Prepared{Manifest: json.RawMessage(manifest), Pages: []int{1, 2, 3, 4}}})
	claims := exchange(3)
	read := tasks.Settle{Parse: claims[0].Parse, Task: claims[0].Task, Token: claims[0].Token, Outcome: tasks.Done,
		Output: "parses/prs_run/pages/1.1.json", Result: json.RawMessage(`{"blocks":2,"source":"reader"}`)}
	lost := tasks.Settle{Parse: claims[1].Parse, Task: claims[1].Task, Token: claims[1].Token, Outcome: tasks.Permanent,
		Error: &tasks.Error{Code: "page_unreadable", Detail: "no"}}
	odd := tasks.Settle{Parse: claims[2].Parse, Task: claims[2].Task, Token: claims[2].Token, Outcome: tasks.Done,
		Output: "parses/prs_run/pages/3.1.json", Result: json.RawMessage(`"not a summary"`)}
	if err := objects.PutPage(ctx, objs, read.Output, objects.Page{Revision: 1, Image: "parses/prs_run/pages/1.1.png", Page: document.Page{Number: 1, State: document.PageSucceeded}}); err != nil {
		t.Fatal(err)
	}
	held = []tasks.Held{{Parse: claims[2].Parse, Task: claims[2].Task, Token: claims[2].Token}}
	exchange(0, read, lost)
	held = nil
	running, err = b.Parse(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := b.Summaries(ctx, running)
	if err != nil || len(summaries) != 2 || summaries[0].Blocks != 2 || summaries[1].State != document.PageFailed || summaries[1].Error.Code != "page_unreadable" {
		t.Fatalf("the pages of a running parse are %+v, %v", summaries, err)
	}
	if page, ok, err := b.Page(ctx, running, 2); err != nil || !ok || page.State != document.PageFailed {
		t.Fatalf("a page that failed while its parse runs: %+v, %t, %v", page, ok, err)
	}
	if _, ok, err := b.Page(ctx, running, 4); err != nil || ok {
		t.Fatalf("a page that is still queued: %t, %v", ok, err)
	}
	// The page names an image that is not in the bucket: it has none.
	if _, ok, err := b.Image(ctx, running, 1); err != nil || ok {
		t.Fatalf("a page whose image is gone: %t, %v", ok, err)
	}
	exchange(0, odd)
	_, err = b.Summaries(ctx, running)
	failed("a row whose summary is none", err)
	_, err = b.Pages(ctx, running)
	failed("a page whose object is gone", err)
	if _, ok, err := b.Document(ctx, running); err != nil || ok {
		t.Fatalf("the document of a running parse: %t, %v", ok, err)
	}

	// A held submit stops waiting when its caller does.
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	b.Wait(stopped, "alice", running.ID, time.Minute)
	b.WaitFigures(ctx, running.ID, time.Minute)

	// A native format before assemble: the pages are where prepare wrote
	// them.
	native := running
	native.ID, native.ManifestToken = "prs_native", 3
	native.Manifest = &parse.Manifest{MediaType: "text/csv", PagesTotal: 1, Selected: []int{1}, Source: document.SourceNative}
	if err := objects.PutPage(ctx, objs, blob.PageKey("prs_native", 1, 3), objects.Page{Revision: 1, Page: document.Page{Number: 1, State: document.PageSucceeded, Source: document.SourceNative}}); err != nil {
		t.Fatal(err)
	}
	if pages, err := b.Pages(ctx, native); err != nil || len(pages) != 1 || pages[0].Source != document.SourceNative {
		t.Fatalf("the pages of a native parse before assemble: %+v, %v", pages, err)
	}
	if listed, err := b.Summaries(ctx, native); err != nil || len(listed) != 1 {
		t.Fatalf("its page list: %+v, %v", listed, err)
	}

	// An index: a page it lists with no key is one the parse did not read,
	// and an index that is gone is an error.
	indexed := running
	indexed.IndexKey = "parses/prs_run/document.9.json"
	_, err = b.Pages(ctx, indexed)
	failed("the pages of a parse whose index is gone", err)
	_, _, err = b.Document(ctx, indexed)
	failed("the document of a parse whose index is gone", err)
	if err := objects.PutIndex(ctx, objs, indexed.IndexKey, objects.Index{Keys: []objects.Entry{{Number: 1, Key: read.Output}, {Number: 2}}}); err != nil {
		t.Fatal(err)
	}
	if pages, err := b.Pages(ctx, indexed); err != nil || len(pages) != 2 || pages[1].State != document.PageSkipped {
		t.Fatalf("the pages of an index with a page it has no key for: %+v, %v", pages, err)
	}

	// The object store stops answering.
	objs.down = true
	_, _, err = b.Image(ctx, indexed, 1)
	failed("an image with the object store down", err)
	_, _, err = b.Page(ctx, indexed, 1)
	failed("a page with the object store down", err)
	_, err = b.Summaries(ctx, native)
	failed("a page list with the object store down", err)
	if _, err := b.Cancel(ctx, "alice", running.ID); err != nil {
		t.Fatal(err)
	}
	ended, err := b.Parse(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = b.Document(ctx, ended)
	failed("a document made from rows with the object store down", err)
	failed("deleting a parse with the object store down", b.DeleteParse(ctx, "alice", running.ID))
	objs.down = false
	objs.Store = &undeletable{Store: objs.Store}
	failed("deleting a parse whose objects cannot be removed", b.DeleteParse(ctx, "alice", running.ID))

	// The task store stops answering.
	st.Close()
	_, _, err = b.PutFile(ctx, store.File{ID: "fil_3", Owner: "alice", SHA256: "dd", Data: []byte("x")})
	failed("storing a file with the task store down", err)
	_, _, err = b.Parses(ctx, []string{"alice"}, store.Filter{}, "", 10)
	failed("listing parses with the task store down", err)
	_, err = b.Summaries(ctx, running)
	failed("a page list with the task store down", err)
	_, _, err = b.Page(ctx, running, 1)
	failed("a page with the task store down", err)
	_, err = b.Pages(ctx, running)
	failed("the pages with the task store down", err)
	_, _, err = b.Document(ctx, ended)
	failed("a document with the task store down", err)
	_, err = b.Retry(ctx, "alice", running.ID)
	failed("a retry with the task store down", err)
	_, err = b.Events(ctx, running.ID, 0, 10)
	failed("the events of a parse with the task store down", err)
	_, err = b.Usage(ctx, store.UsageQuery{By: "group", Interval: "hour", From: time.Now().Add(-time.Hour), To: time.Now()})
	failed("the meters with the task store down", err)
	_, err = b.Queue(ctx, nil)
	failed("the queue with the task store down", err)
}

// undeletable is an object store that lists and cannot delete.
type undeletable struct{ blob.Store }

func (undeletable) Delete(context.Context, string) error { return errObjects }

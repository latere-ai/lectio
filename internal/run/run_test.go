// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package run

import (
	"context"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// drawn renders every page as an image that is not blank, so a file of
// several pages can be read without a real renderer.
type drawn struct{}

func (drawn) Render(_ context.Context, _ []byte, _ string, n int, _ reader.Description) (render.Image, error) {
	return render.Image{Data: []byte{byte(n)}, MediaType: "image/png", Width: 100, Height: 100}, nil
}

func noWait(int) time.Duration { return 0 }

// start returns a running runner over an empty store. The runner is stopped
// when the test ends.
func start(t *testing.T, r *Runner) *Runner {
	t.Helper()
	r.Store = store.NewMemory()
	if r.Pipeline == nil {
		r.Pipeline = &parse.Pipeline{Limits: pages.DefaultLimits(), Renderer: drawn{}}
	}
	if r.Backoff == nil {
		r.Backoff = noWait
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	t.Cleanup(func() { cancel(); r.Wait() })
	return r
}

// submit stores a fixture and a parse of it, and starts the parse.
func submit(t *testing.T, r *Runner, p store.Parse, fixture string) {
	t.Helper()
	data := testfixtures.Read(t, fixture)
	f, _ := r.Store.PutFile(store.File{ID: "fil_" + p.ID, Owner: "alice", Name: path.Base(fixture), SHA256: p.ID, Data: data})
	p.Owner, p.State, p.Stage = "alice", store.StateQueued, store.StageQueued
	if p.File == "" {
		p.File = f.ID
	}
	if p.Class == "" {
		p.Class = store.ClassInteractive
	}
	if _, _, err := r.Store.CreateParse(p, "", ""); err != nil {
		t.Fatal(err)
	}
	r.Submit(p)
}

// run submits a parse and returns it as it ended.
func run(t *testing.T, r *Runner, p store.Parse, fixture string) store.Parse {
	t.Helper()
	submit(t, r, p, fixture)
	return ended(t, r, p.ID)
}

func ended(t *testing.T, r *Runner, id string) store.Parse {
	t.Helper()
	select {
	case <-r.Done(id):
	case <-time.After(10 * time.Second):
		t.Fatalf("parse %s did not end", id)
	}
	p, err := r.Store.Parse("alice", id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 5000 {
		if ok() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("never happened: %s", what)
}

func code(e *document.Error) string {
	if e == nil {
		return ""
	}
	return e.Code
}

func TestAParseIsReadPageByPage(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	var seen reader.Page
	rd := &stub.Reader{Fail: func(p reader.Page) error {
		if p.Number == 3 {
			seen = p
			close(reached)
			<-release
		}
		return nil
	}}
	r := start(t, &Runner{
		Readers: map[string]reader.Reader{"stub": rd}, Chain: []string{"stub"}, Workers: 1,
		Credential: func(owner string) reader.Credential { return reader.NewCredential("key of " + owner) },
	})
	submit(t, r, store.Parse{ID: "prs_1", Languages: []string{"de"}}, testfixtures.MultiTIFF)

	// With the third page still being read, the first two are there to read.
	<-reached
	mid, _ := r.Store.Parse("alice", "prs_1")
	if mid.State != store.StateRunning || mid.Stage != store.StageReading || mid.PagesTotal != 3 || mid.PagesDone != 2 {
		t.Fatalf("mid-run: %+v", mid)
	}
	if got := r.Store.Pages("prs_1"); len(got) != 2 || got[1].Number != 2 || got[1].State != document.PageSucceeded {
		t.Fatalf("pages mid-run: %+v", got)
	}
	if _, ok := r.Store.Document("prs_1"); ok {
		t.Fatal("no document before the pages are read")
	}
	close(release)

	p := ended(t, r, "prs_1")
	if p.State != store.StateSucceeded || p.Stage != store.StageDone || p.PagesDone != 3 || p.PagesFailed != 0 || p.Error != nil {
		t.Fatalf("ended: %+v", p)
	}
	if p.StartedAt == nil || p.FinishedAt == nil || p.Manifest == nil || p.Manifest.PagesTotal != 3 || p.Usage.Pages != 3 {
		t.Fatalf("ended: %+v", p)
	}
	if seen.Credential.Reveal() != "key of alice" || len(seen.Languages) != 1 || seen.Languages[0] != "de" {
		t.Fatalf("the reader was called with %+v", seen)
	}

	doc, ok := r.Store.Document("prs_1")
	if !ok || len(doc.Pages) != 3 || doc.Usage.Pages != 3 || strings.Join(doc.Renderings, ",") != "markdown,text" {
		t.Fatalf("document: %+v, %v", doc, ok)
	}
	for n := 1; n <= 3; n++ {
		page, ok := r.Store.Page("prs_1", n)
		if !ok || page.Attempts != 1 || page.Reader != stub.Name || len(page.Blocks) != 3 {
			t.Fatalf("page %d: %+v, %v", n, page, ok)
		}
		if _, ok := r.Store.Image("prs_1", n); !ok {
			t.Fatalf("page %d has no image", n)
		}
		// Assembly ran over the stored pages: the stub's first line is the
		// same on every page, so it is a running header, repeated after the
		// first.
		if first := page.Blocks[0]; first.Kind != document.KindPageHeader || first.Repeated != (n > 1) {
			t.Fatalf("page %d starts with %+v", n, first)
		}
	}
}

func TestANativeFileNeedsNoReader(t *testing.T) {
	r := start(t, &Runner{})
	p := run(t, r, store.Parse{ID: "prs_1"}, testfixtures.CSV)
	if p.State != store.StateSucceeded || p.PagesTotal != 1 || p.PagesDone != 1 {
		t.Fatalf("ended: %+v", p)
	}
	page, ok := r.Store.Page("prs_1", 1)
	if !ok || page.Source != document.SourceNative || len(page.Blocks) != 1 || page.Blocks[0].Kind != document.KindTable {
		t.Fatalf("page: %+v, %v", page, ok)
	}
}

// failing makes page 2 fail: the first n calls, or every call when n is 0.
func failing(n int, err error) *stub.Reader {
	var mu sync.Mutex
	calls := 0
	return &stub.Reader{Fail: func(p reader.Page) error {
		if p.Number != 2 {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if calls++; n == 0 || calls <= n {
			return err
		}
		return nil
	}}
}

func TestAPageIsRetriedByWhatWentWrong(t *testing.T) {
	limited := &reader.Error{Class: reader.RateLimited, RetryAfter: time.Nanosecond}
	for name, tc := range map[string]struct {
		first, second *stub.Reader
		pinned        string
		firstCalls    int
		secondCalls   int
		attempts      int
		pageCode      string
	}{
		"a rate limit is waited out and spends no attempt": {first: failing(2, limited), firstCalls: 3, attempts: 1},
		"a rate limit that never lifts":                    {first: failing(0, limited), firstCalls: maxWaits + 1, pageCode: "reader_unavailable"},
		"a spent budget is final":                          {first: failing(0, reader.Errorf(reader.Budget, "spent")), firstCalls: 1, attempts: 1, pageCode: "budget_exhausted"},
		"a refusal is final":                               {first: failing(0, reader.Errorf(reader.Permanent, "no")), firstCalls: 1, attempts: 1, pageCode: "page_unreadable"},
		"a transient failure is tried again":               {first: failing(1, reader.Errorf(reader.Retryable, "reset")), firstCalls: 2, attempts: 2},
		"a transient failure, every attempt":               {first: failing(0, reader.Errorf(reader.Retryable, "reset")), firstCalls: 3, attempts: 3, pageCode: "reader_unavailable"},
		"an unusable reply, with one reader":               {first: failing(0, reader.Errorf(reader.Invalid, "loop")), firstCalls: 3, attempts: 3, pageCode: "page_unreadable"},
		"two unusable replies go to the next reader": {
			first: failing(0, reader.Errorf(reader.Invalid, "loop")), second: &stub.Reader{}, firstCalls: 2, secondCalls: 1, attempts: 1,
		},
		"the next reader is tried once, with its own attempts": {
			first: failing(0, reader.Errorf(reader.Invalid, "loop")), second: failing(0, reader.Errorf(reader.Invalid, "loop")),
			firstCalls: 2, secondCalls: 3, attempts: 3, pageCode: "page_unreadable",
		},
		"a parse that named its reader stays with it": {
			first: failing(0, reader.Errorf(reader.Invalid, "loop")), second: &stub.Reader{}, pinned: "first",
			firstCalls: 3, attempts: 3, pageCode: "page_unreadable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			readers, chain := map[string]reader.Reader{"first": tc.first}, []string{"first"}
			if tc.second != nil {
				readers["second"], chain = tc.second, append(chain, "second")
			}
			r := start(t, &Runner{Readers: readers, Chain: chain})
			p := run(t, r, store.Parse{ID: "prs_1", Reader: tc.pinned}, testfixtures.MultiTIFF)

			page, ok := r.Store.Page("prs_1", 2)
			if !ok || code(page.Error) != tc.pageCode || page.Attempts != tc.attempts {
				t.Fatalf("page 2: %+v (%v)", page, page.Error)
			}
			if got := tc.first.Calls(2); got != tc.firstCalls {
				t.Errorf("the first reader read page 2 %d times, want %d", got, tc.firstCalls)
			}
			if tc.second != nil && tc.second.Calls(2) != tc.secondCalls {
				t.Errorf("the second reader read page 2 %d times, want %d", tc.second.Calls(2), tc.secondCalls)
			}

			if tc.pageCode == "" {
				if p.State != store.StateSucceeded || p.PagesDone != 3 || page.State != document.PageSucceeded {
					t.Fatalf("ended: %+v", p)
				}
				return
			}
			if p.State != store.StateFailed || code(p.Error) != "page_unreadable" || p.PagesDone != 2 || p.PagesFailed != 1 {
				t.Fatalf("ended: %+v (%v)", p, p.Error)
			}
			if page.State != document.PageFailed || len(page.Blocks) != 0 {
				t.Fatalf("a failed page holds no block: %+v", page)
			}
			// The pages that were read stay readable.
			if one, ok := r.Store.Page("prs_1", 1); !ok || one.State != document.PageSucceeded {
				t.Fatalf("page 1: %+v, %v", one, ok)
			}
		})
	}
}

func TestAFileTheRendererRefusesFailsEachPageOnce(t *testing.T) {
	rd := &stub.Reader{}
	r := start(t, &Runner{
		Pipeline: &parse.Pipeline{Limits: pages.DefaultLimits(), Renderer: render.Images{}},
		Readers:  map[string]reader.Reader{"stub": rd}, Chain: []string{"stub"},
	})
	p := run(t, r, store.Parse{ID: "prs_1"}, testfixtures.MultipagePDF)
	if p.State != store.StateFailed || p.PagesFailed != 3 || code(p.Error) != "page_unreadable" {
		t.Fatalf("ended: %+v (%v)", p, p.Error)
	}
	if p.Error.Detail != "3 of 3 pages could not be read" {
		t.Fatalf("detail: %q", p.Error.Detail)
	}
	page, _ := r.Store.Page("prs_1", 1)
	if code(page.Error) != string(fault.UnsupportedMediaType) || page.Attempts != 1 || rd.Calls(1) != 0 {
		t.Fatalf("page 1: %+v (%v), %d calls", page, page.Error, rd.Calls(1))
	}
}

func TestFailedPagesAreAllowedUpToTheNumberAsked(t *testing.T) {
	for allowed, want := range map[int]string{0: store.StateFailed, 1: store.StateSucceeded} {
		r := start(t, &Runner{
			Readers: map[string]reader.Reader{"stub": failing(0, reader.Errorf(reader.Permanent, "no"))}, Chain: []string{"stub"},
		})
		p := run(t, r, store.Parse{ID: "prs_1", AllowFailedPages: allowed}, testfixtures.MultiTIFF)
		if p.State != want || p.PagesFailed != 1 || p.PagesDone != 2 {
			t.Fatalf("allowing %d: %+v", allowed, p)
		}
		// The document is written either way, and says which page failed.
		doc, ok := r.Store.Document("prs_1")
		if !ok || len(doc.Pages) != 3 || doc.Pages[1].State != document.PageFailed || code(doc.Pages[1].Error) != "page_unreadable" {
			t.Fatalf("allowing %d: document %+v, %v", allowed, doc, ok)
		}
	}
}

func TestCancelStopsBetweenPages(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	rd := &stub.Reader{Fail: func(p reader.Page) error {
		if p.Number == 2 {
			close(reached)
			<-release
		}
		return nil
	}}
	r := start(t, &Runner{Readers: map[string]reader.Reader{"stub": rd}, Chain: []string{"stub"}, Workers: 1})
	submit(t, r, store.Parse{ID: "prs_1"}, testfixtures.MultiTIFF)
	<-reached
	if !r.Cancel("prs_1") {
		t.Fatal("a running parse can be canceled")
	}
	close(release)

	p := ended(t, r, "prs_1")
	if p.State != store.StateCanceled || p.Error != nil || p.FinishedAt == nil || p.PagesDone != 1 {
		t.Fatalf("ended: %+v", p)
	}
	// The page that was being read is dropped and the one after is not read.
	if got := r.Store.Pages("prs_1"); len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("pages: %+v", got)
	}
	if rd.Calls(3) != 0 {
		t.Fatal("page 3 was read after the cancel")
	}
	if _, ok := r.Store.Document("prs_1"); ok {
		t.Fatal("a canceled parse has no document")
	}
	if r.Cancel("prs_1") || r.Cancel("prs_none") {
		t.Fatal("a parse that is not running is not canceled here")
	}
}

func TestCancelEndsAWait(t *testing.T) {
	for name, err := range map[string]error{
		"for a rate limit to lift": &reader.Error{Class: reader.RateLimited, RetryAfter: time.Hour},
		"before the next attempt":  reader.Errorf(reader.Retryable, "reset"),
	} {
		reached := make(chan struct{})
		var once sync.Once
		rd := &stub.Reader{Fail: func(reader.Page) error {
			once.Do(func() { close(reached) })
			return err
		}}
		r := start(t, &Runner{
			Readers: map[string]reader.Reader{"stub": rd}, Chain: []string{"stub"}, Workers: 1,
			Backoff: func(int) time.Duration { return time.Hour },
		})
		submit(t, r, store.Parse{ID: "prs_1"}, testfixtures.PNG)
		<-reached
		r.Cancel("prs_1")
		if p := ended(t, r, "prs_1"); p.State != store.StateCanceled || len(r.Store.Pages("prs_1")) != 0 {
			t.Fatalf("%s: %+v", name, p)
		}
	}
}

func TestAParsePastItsDeadlineFails(t *testing.T) {
	rd := &stub.Reader{}
	r := start(t, &Runner{Readers: map[string]reader.Reader{"stub": rd}, Chain: []string{"stub"}})
	past := time.Now().Add(-time.Minute)
	p := run(t, r, store.Parse{ID: "prs_1", DeadlineAt: &past}, testfixtures.MultiTIFF)
	if p.State != store.StateFailed || code(p.Error) != "deadline_exceeded" {
		t.Fatalf("ended: %+v (%v)", p, p.Error)
	}
	if rd.Calls(1) != 0 || len(r.Store.Pages("prs_1")) != 0 {
		t.Fatal("no page is read past the deadline")
	}

	future := time.Now().Add(time.Hour)
	if p := run(t, r, store.Parse{ID: "prs_2", DeadlineAt: &future}, testfixtures.MultiTIFF); p.State != store.StateSucceeded {
		t.Fatalf("a parse within its deadline: %+v", p)
	}
}

func TestAParseThatCannotStartFails(t *testing.T) {
	rd := &stub.Reader{}
	for name, tc := range map[string]struct {
		parse   store.Parse
		fixture string
		chain   []string
		want    fault.Code
	}{
		"its file is gone":                {store.Parse{File: "fil_none"}, testfixtures.PNG, []string{"stub"}, fault.FileNotFound},
		"a format this build cannot read": {store.Parse{}, testfixtures.DOCX, []string{"stub"}, fault.UnsupportedMediaType},
		"a selection naming no page":      {store.Parse{Pages: "9"}, testfixtures.MultiTIFF, []string{"stub"}, fault.InvalidPages},
		"no reader is configured":         {store.Parse{}, testfixtures.PNG, nil, fault.ReaderUnavailable},
		"the reader it named is not here": {store.Parse{Reader: "other"}, testfixtures.PNG, []string{"stub"}, fault.ReaderUnavailable},
	} {
		r := start(t, &Runner{Readers: map[string]reader.Reader{"stub": rd}, Chain: tc.chain})
		tc.parse.ID = "prs_1"
		p := run(t, r, tc.parse, tc.fixture)
		if p.State != store.StateFailed || code(p.Error) != string(tc.want) || p.Error.Detail == "" || p.FinishedAt == nil {
			t.Errorf("%s: %+v (%v)", name, p, p.Error)
		}
	}
}

// TestPagesAreDispatchedByClassThenPriority holds the one worker on a page,
// queues four parses behind it, and reads the order they are read in.
func TestPagesAreDispatchedByClassThenPriority(t *testing.T) {
	var mu sync.Mutex
	var order []string
	labeled := func(label string) *stub.Reader {
		return &stub.Reader{Fail: func(p reader.Page) error {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, label+string(rune('0'+p.Number)))
			return nil
		}}
	}
	reached, release := make(chan struct{}), make(chan struct{})
	gate := &stub.Reader{Fail: func(reader.Page) error {
		close(reached)
		<-release
		return nil
	}}
	r := start(t, &Runner{Workers: 1, Readers: map[string]reader.Reader{
		"gate": gate, "a": labeled("a"), "b": labeled("b"), "h": labeled("h"), "i": labeled("i"),
	}})

	submit(t, r, store.Parse{ID: "prs_0", Reader: "gate"}, testfixtures.PNG)
	<-reached
	submit(t, r, store.Parse{ID: "prs_1", Reader: "a", Class: store.ClassBatch}, testfixtures.MultiTIFF)
	submit(t, r, store.Parse{ID: "prs_2", Reader: "b", Class: store.ClassBatch}, testfixtures.MultiTIFF)
	submit(t, r, store.Parse{ID: "prs_3", Reader: "h", Class: store.ClassBatch, Priority: 5}, testfixtures.MultiTIFF)
	submit(t, r, store.Parse{ID: "prs_4", Reader: "i", Class: store.ClassInteractive}, testfixtures.MultiTIFF)
	eventually(t, "twelve pages wait", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.queue) == 12
	})
	close(release)
	for _, id := range []string{"prs_0", "prs_1", "prs_2", "prs_3", "prs_4"} {
		if p := ended(t, r, id); p.State != store.StateSucceeded {
			t.Fatalf("%s: %+v", id, p)
		}
	}

	// Interactive first, then the higher priority, then two parses of the
	// same standing page by page, so both advance.
	if got, want := strings.Join(order, " "), "i1 i2 i3 h1 h2 h3 a1 b1 a2 b2 a3 b3"; got != want {
		t.Fatalf("read in the order\n  %s\nwant\n  %s", got, want)
	}
}

func TestStoppingTheRunnerEndsWhatIsRunning(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	rd := &stub.Reader{Fail: func(reader.Page) error {
		close(reached)
		<-release
		return nil
	}}
	r := &Runner{
		Store: store.NewMemory(), Pipeline: &parse.Pipeline{Limits: pages.DefaultLimits(), Renderer: drawn{}},
		Readers: map[string]reader.Reader{"stub": rd}, Chain: []string{"stub"}, Workers: 1,
	}
	ctx, stop := context.WithCancel(context.Background())
	r.Start(ctx)
	submit(t, r, store.Parse{ID: "prs_1"}, testfixtures.MultiTIFF)
	<-reached
	stop()
	close(release)
	r.Wait()

	p, _ := r.Store.Parse("alice", "prs_1")
	if p.State != store.StateCanceled || len(r.Store.Pages("prs_1")) != 0 {
		t.Fatalf("ended: %+v", p)
	}

	// A parse submitted to a stopped runner ends at once, with no page queued.
	submit(t, r, store.Parse{ID: "prs_2"}, testfixtures.MultiTIFF)
	r.Wait()
	if p, _ := r.Store.Parse("alice", "prs_2"); p.State != store.StateCanceled {
		t.Fatalf("submitted after the stop: %+v", p)
	}
	if len(r.queue) != 0 {
		t.Fatalf("%d pages were queued for no worker", len(r.queue))
	}
}

func TestBackoffAndSleep(t *testing.T) {
	r := &Runner{}
	for attempt, want := range map[int]time.Duration{1: 200 * time.Millisecond, 2: 400 * time.Millisecond, 6: 5 * time.Second, 40: 5 * time.Second} {
		if got := r.backoff(attempt); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
	if !sleep(context.Background(), 0) || !sleep(context.Background(), time.Millisecond) {
		t.Fatal("a wait that ran its time reports so")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleep(ctx, 0) || sleep(ctx, time.Hour) {
		t.Fatal("a wait cut short reports so")
	}
}

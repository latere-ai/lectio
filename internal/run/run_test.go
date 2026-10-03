// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package run

import (
	"context"
	"path"
	"strconv"
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
		// Assembly ran over the stored pages. The stub's first line differs
		// from page to page by its number, so it is content and stays a
		// title; its last line is the page's number.
		if first, last := page.Blocks[0], page.Blocks[2]; first.Kind != document.KindTitle || first.Repeated || last.Kind != document.KindPageNumber {
			t.Fatalf("page %d holds %+v and %+v", n, first, last)
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
	// The page that was being read is dropped and the one after is not
	// read: both are recorded as skipped, and nothing went wrong with them.
	got := r.Store.Pages("prs_1")
	if len(got) != 3 || got[0].State != document.PageSucceeded || got[1].State != document.PageSkipped || got[2].State != document.PageSkipped || got[1].Error != nil {
		t.Fatalf("pages: %+v", got)
	}
	if rd.Calls(3) != 0 {
		t.Fatal("page 3 was read after the cancel")
	}
	// What was read is a document all the same: a caller reads the one
	// page as Markdown and does not have to fetch it block by block.
	doc, ok := r.Store.Document("prs_1")
	if !ok || len(doc.Pages) != 3 || doc.Pages[0].Blocks != 3 || doc.Pages[2].State != document.PageSkipped {
		t.Fatalf("a canceled parse's document: %+v, %v", doc, ok)
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
		p := ended(t, r, "prs_1")
		if got := r.Store.Pages("prs_1"); p.State != store.StateCanceled || len(got) != 1 || got[0].State != document.PageSkipped {
			t.Fatalf("%s: %+v, pages %+v", name, p, got)
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
	if got := r.Store.Pages("prs_1"); rd.Calls(1) != 0 || len(got) != 3 || got[0].State != document.PageSkipped {
		t.Fatalf("no page is read past the deadline, and each is skipped: %+v", got)
	}
	if _, ok := r.Store.Document("prs_1"); !ok {
		t.Fatal("a parse that ran out of time still has a document")
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
		return r.queue.len() == 12
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
	if got := r.Store.Pages("prs_1"); p.State != store.StateCanceled || len(got) != 3 || got[0].State != document.PageSkipped {
		t.Fatalf("ended: %+v, pages %+v", p, got)
	}

	// A parse submitted to a stopped runner ends at once, with no page queued.
	submit(t, r, store.Parse{ID: "prs_2"}, testfixtures.MultiTIFF)
	r.Wait()
	if p, _ := r.Store.Parse("alice", "prs_2"); p.State != store.StateCanceled {
		t.Fatalf("submitted after the stop: %+v", p)
	}
	if r.queue.len() != 0 {
		t.Fatalf("%d pages were queued for no worker", r.queue.len())
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

// A reader that cannot be the one to read a page passes it down the chain:
// one that declined the content, and one whose endpoint rejects the
// request itself. Neither is retried where it failed.
func TestAPageMovesDownTheChainWhenAReaderCannotReadIt(t *testing.T) {
	declined, rejected := reader.Errorf(reader.Refused, "no"), reader.Errorf(reader.Misconfigured, "400")
	for name, tc := range map[string]struct {
		chain    []*stub.Reader
		pinned   string
		calls    []int
		pageCode string
		reader   string
	}{
		"declined, and the next reads it":          {chain: []*stub.Reader{failing(0, declined), {}}, calls: []int{1, 1}},
		"declined by two, read by the third":       {chain: []*stub.Reader{failing(0, declined), failing(0, declined), {}}, calls: []int{1, 1, 1}},
		"declined, with no other reader":           {chain: []*stub.Reader{failing(0, declined)}, calls: []int{1}, pageCode: "page_unreadable"},
		"declined, and the parse named its reader": {chain: []*stub.Reader{failing(0, declined), {}}, pinned: "r0", calls: []int{1, 0}, pageCode: "page_unreadable"},
		"rejected, and the next reads it":          {chain: []*stub.Reader{failing(0, rejected), {}}, calls: []int{1, 1}},
		"rejected, with no other reader":           {chain: []*stub.Reader{failing(0, rejected)}, calls: []int{1}, pageCode: "reader_unavailable"},
		"unusable replies still move it only once": {
			chain: []*stub.Reader{failing(0, reader.Errorf(reader.Invalid, "loop")), failing(0, reader.Errorf(reader.Invalid, "loop")), {}},
			calls: []int{2, 3, 0}, pageCode: "page_unreadable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			readers, chain := map[string]reader.Reader{}, []string{}
			for i, rd := range tc.chain {
				name := "r" + strconv.Itoa(i)
				readers[name], chain = rd, append(chain, name)
			}
			r := start(t, &Runner{Readers: readers, Chain: chain})
			p := run(t, r, store.Parse{ID: "prs_1", Reader: tc.pinned}, testfixtures.MultiTIFF)
			page, _ := r.Store.Page("prs_1", 2)
			if code(page.Error) != tc.pageCode {
				t.Fatalf("page 2: %+v (%v)", page, page.Error)
			}
			for i, rd := range tc.chain {
				if got := rd.Calls(2); got != tc.calls[i] {
					t.Errorf("reader %d read page 2 %d times, want %d", i, got, tc.calls[i])
				}
			}
			if (tc.pageCode == "") != (p.State == store.StateSucceeded) {
				t.Fatalf("ended: %+v (%v)", p, p.Error)
			}
		})
	}
}

// A page that an earlier parse of the same owner read whole, from the same
// bytes with the same readers, is not read again. The parse is a new one.
func TestAPageAlreadyReadIsNotReadAgain(t *testing.T) {
	rd := failing(0, reader.Errorf(reader.Permanent, "no"))
	r := start(t, &Runner{Readers: map[string]reader.Reader{"stub": rd}, Chain: []string{"stub"}})
	first := run(t, r, store.Parse{ID: "prs_1", ContentSHA: "sha", Reuse: true, AllowFailedPages: 1}, testfixtures.MultiTIFF)
	if first.PagesDone != 2 || first.PagesReused != 0 || rd.Calls(1) != 1 {
		t.Fatalf("the first parse: %+v", first)
	}

	second := run(t, r, store.Parse{ID: "prs_2", ContentSHA: "sha", Reuse: true, AllowFailedPages: 1, Labels: map[string]string{"batch": "nov"}}, testfixtures.MultiTIFF)
	if second.ID != "prs_2" || second.PagesDone != 2 || second.PagesReused != 2 || second.Labels["batch"] != "nov" {
		t.Fatalf("the second parse is its own, with two pages taken: %+v", second)
	}
	// The pages that were read are not read again; the one that failed is.
	if rd.Calls(1) != 1 || rd.Calls(3) != 1 || rd.Calls(2) != 2 {
		t.Fatalf("calls: %d, %d, %d", rd.Calls(1), rd.Calls(2), rd.Calls(3))
	}
	page, _ := r.Store.Page("prs_2", 3)
	if !page.Reused || page.Blocks[0].Ref != "3.1" || page.Usage.Pages != 1 || page.Usage.InputTokens != 0 {
		t.Fatalf("a page taken from an earlier read: %+v (%+v)", page, page.Usage)
	}
	if _, ok := r.Store.Image("prs_2", 3); !ok {
		t.Fatal("a reused page has the image the reader saw")
	}

	for name, tc := range map[string]struct {
		parse store.Parse
		owner string
	}{
		"a parse that asks for no reuse": {store.Parse{ContentSHA: "sha"}, ""},
		"other bytes":                    {store.Parse{ContentSHA: "other", Reuse: true}, ""},
		"other language hints":           {store.Parse{ContentSHA: "sha", Reuse: true, Languages: []string{"de"}}, ""},
		"a file with no digest":          {store.Parse{Reuse: true}, ""},
	} {
		before := rd.Calls(1)
		tc.parse.ID, tc.parse.AllowFailedPages = "prs_"+name, 1
		if p := run(t, r, tc.parse, testfixtures.MultiTIFF); p.PagesReused != 0 || rd.Calls(1) != before+1 {
			t.Errorf("%s: %d pages reused, %d more calls", name, p.PagesReused, rd.Calls(1)-before)
		}
	}

	// A reader that names no version promises nothing about its results.
	unversioned := start(t, &Runner{Readers: map[string]reader.Reader{"stub": noVersion{&stub.Reader{}}}, Chain: []string{"stub"}})
	run(t, unversioned, store.Parse{ID: "prs_1", ContentSHA: "sha", Reuse: true}, testfixtures.MultiTIFF)
	if p := run(t, unversioned, store.Parse{ID: "prs_2", ContentSHA: "sha", Reuse: true}, testfixtures.MultiTIFF); p.PagesReused != 0 {
		t.Fatalf("pages of a reader with no version were reused: %+v", p)
	}
}

// noVersion is a reader that describes itself without a version.
type noVersion struct{ reader.Reader }

func (n noVersion) Describe() reader.Description {
	d := n.Reader.Describe()
	d.Version = ""
	return d
}

// Owners take turns, one page each. Nothing an owner queues, however much
// or at whatever priority, moves it ahead of another owner.
func TestOwnersTakeTurns(t *testing.T) {
	var q queue
	add := func(owner, id, class string, priority, pages int) {
		for i := range pages {
			q.push(&job{parse: store.Parse{ID: id, Owner: owner, Class: class, Priority: priority}, seq: i})
		}
	}
	order := func() string {
		var out []string
		for q.len() > 0 {
			j := q.pop()
			out = append(out, j.parse.ID+strconv.Itoa(j.seq+1))
		}
		return strings.Join(out, " ")
	}

	// One owner queues six pages at a high priority, another two at none.
	add("alice", "a", store.ClassInteractive, 9, 6)
	add("bob", "b", store.ClassInteractive, 0, 2)
	if got, want := order(), "a1 b1 a2 b2 a3 a4 a5 a6"; got != want {
		t.Fatalf("two owners:\n  %s\nwant\n  %s", got, want)
	}

	// Priority orders an owner's own parses.
	add("alice", "low", store.ClassInteractive, 0, 2)
	add("alice", "high", store.ClassInteractive, 5, 2)
	if got, want := order(), "high1 high2 low1 low2"; got != want {
		t.Fatalf("one owner's priorities:\n  %s\nwant\n  %s", got, want)
	}

	// Interactive goes ahead of batch and does not starve it: one page in
	// five is a batch page while both wait.
	add("alice", "i", store.ClassInteractive, 0, 9)
	add("bob", "b", store.ClassBatch, 0, 3)
	if got, want := order(), "i1 i2 i3 i4 b1 i5 i6 i7 i8 b2 i9 b3"; got != want {
		t.Fatalf("two classes:\n  %s\nwant\n  %s", got, want)
	}

	// With no interactive work, batch takes every slot.
	add("bob", "b", store.ClassBatch, 0, 2)
	if got, want := order(), "b1 b2"; got != want {
		t.Fatalf("batch alone: %s", got)
	}
}

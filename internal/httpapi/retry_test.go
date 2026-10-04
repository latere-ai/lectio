// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// flaky is a reader a case breaks and mends: while broken is set it cannot
// take page 2 of any file, and while a gate is set it holds page 2 until
// the gate is released.
type flaky struct {
	stub.Reader
	broken atomic.Bool

	mu      sync.Mutex
	reached chan struct{}
	release chan struct{}
}

// newFlaky returns the reader, broken.
func newFlaky() *flaky {
	f := &flaky{}
	f.broken.Store(true)
	f.Fail = func(p reader.Page) error {
		if p.Number != 2 {
			return nil
		}
		if f.broken.Load() {
			return reader.Errorf(reader.Permanent, "the page cannot be taken")
		}
		f.mu.Lock()
		reached, release := f.reached, f.release
		f.reached, f.release = nil, nil
		f.mu.Unlock()
		if reached != nil {
			close(reached)
			<-release
		}
		return nil
	}
	return f
}

// hold makes the next read of page 2 wait: reached is closed when the read
// begins, and the read returns when release is closed.
func (f *flaky) hold() (reached, release chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reached, f.release = make(chan struct{}), make(chan struct{})
	return f.reached, f.release
}

// TestARetryReadsOnlyThePagesThatFailed: a parse with one page its reader
// cannot take ends failed, returns the other pages and a document that
// marks the missing one. A retry answers 202 with the parse running and
// that one page open, the pages that were read stay readable while it
// runs, and with the reader mended the parse ends succeeded having called
// the reader once more, for that page alone.
func TestARetryReadsOnlyThePagesThatFailed(t *testing.T) {
	durably(t, false)
	rd := newFlaky()
	e := serve(t, func(_ *Server, r *run.Runner) {
		r.Workers = 2
		r.Readers = map[string]reader.Reader{"stub": rd}
	})
	file := e.upload("scan.tiff", testfixtures.Read(t, testfixtures.MultiTIFF))
	failed := e.parsed(file, `,"reuse":false,"labels":{"batch":"oct"}`)
	pid := failed["id"].(string)
	at2 := "/parses/" + pid
	if failed["state"] != "failed" || at(failed, "error", "code") != "page_unreadable" || at(failed, "progress", "pages_failed") != 1.0 || at(failed, "progress", "pages_done") != 2.0 {
		t.Fatalf("the parse ended %v", failed)
	}
	if doc := e.do("GET", at2+"/document", nil).json(t); at(doc["pages"].([]any)[1], "state") != "failed" || at(doc["pages"].([]any)[2], "state") != "succeeded" {
		t.Fatalf("the document of the failed parse: %v", doc)
	}

	// A retry while the reader is still broken reads the page again, and
	// no other, and ends failed again.
	if got := e.do("POST", at2+"/retry", nil); got.status != http.StatusAccepted {
		t.Fatalf("the first retry: %d %s", got.status, got.body)
	}
	if again := e.ended(pid); again["state"] != "failed" || at(again, "progress", "pages_failed") != 1.0 {
		t.Fatalf("with the reader still broken the retried parse ended %v", again)
	}
	if rd.Calls(1) != 1 || rd.Calls(2) != 2 || rd.Calls(3) != 1 {
		t.Fatalf("after one retry the reader was called %d, %d and %d times for pages 1 to 3", rd.Calls(1), rd.Calls(2), rd.Calls(3))
	}

	// The reader is mended, and holds the page so the parse can be read
	// while the retry runs.
	rd.broken.Store(false)
	reached, release := rd.hold()
	retried := e.do("POST", at2+"/retry", nil)
	if retried.status != http.StatusAccepted {
		t.Fatalf("the retry: %d %s", retried.status, retried.body)
	}
	p := retried.json(t)
	if p["id"] != pid || p["state"] != "running" || p["error"] != nil || p["finished_at"] != nil || at(p, "labels", "batch") != "oct" ||
		at(p, "progress", "stage") != "reading" || at(p, "progress", "pages_total") != 3.0 || at(p, "progress", "pages_done") != 2.0 || at(p, "progress", "pages_failed") != 0.0 {
		t.Fatalf("the parse a retry answers: %v", p)
	}
	<-reached
	// What was read is readable as before, and what is being read again is
	// not ready.
	if got := e.do("GET", at2+"/pages/1", nil); got.status != http.StatusOK || got.json(t)["state"] != "succeeded" {
		t.Fatalf("a page read before the retry: %d %s", got.status, got.body)
	}
	if got := e.do("GET", at2+"/pages/2", nil); got.status != http.StatusConflict || got.code(t) != "page_not_ready" {
		t.Fatalf("the page being read again: %d %s", got.status, got.body)
	}
	if got := e.do("GET", at2+"/document", nil); got.status != http.StatusConflict || got.code(t) != "document_not_ready" {
		t.Fatalf("the document while the retry runs: %d %s", got.status, got.body)
	}
	if list := e.do("GET", at2+"/pages", nil).json(t)["pages"].([]any); len(list) != 3 || at(list[0], "state") != "succeeded" || at(list[1], "state") != "pending" {
		t.Fatalf("the pages while the retry runs: %v", list)
	}
	if got := e.do("POST", at2+"/retry", nil); got.status != http.StatusConflict || got.code(t) != "not_terminal" {
		t.Fatalf("a retry of a parse that has not ended: %d %s", got.status, got.body)
	}
	close(release)

	done := e.ended(pid)
	if done["state"] != "succeeded" || done["error"] != nil || at(done, "progress", "pages_done") != 3.0 || at(done, "progress", "pages_failed") != 0.0 {
		t.Fatalf("the retried parse ended %v", done)
	}
	if rd.Calls(1) != 1 || rd.Calls(2) != 3 || rd.Calls(3) != 1 {
		t.Fatalf("after the retries the reader was called %d, %d and %d times for pages 1 to 3", rd.Calls(1), rd.Calls(2), rd.Calls(3))
	}
	doc := e.do("GET", at2+"/document", nil).json(t)
	for i, page := range doc["pages"].([]any) {
		if at(page, "state") != "succeeded" || at(page, "error") != nil {
			t.Fatalf("page %d of the document after the retry: %v", i+1, page)
		}
	}
	if md := e.do("GET", at2+"/document?format=markdown", nil); md.status != http.StatusOK || strings.Count(string(md.body), "# Page ") != 3 {
		t.Fatalf("the document after the retry: %d %q", md.status, md.body)
	}
	if got := e.do("GET", at2+"/pages/2", nil); got.status != http.StatusOK || got.json(t)["state"] != "succeeded" || len(got.json(t)["blocks"].([]any)) != 3 {
		t.Fatalf("the page read again: %d %s", got.status, got.body)
	}

	// There is nothing left to read again, and nothing to read again of a
	// parse that was canceled or is not there.
	if got := e.do("POST", at2+"/retry", nil); got.status != http.StatusConflict || got.code(t) != "conflict" || !strings.Contains(string(got.body), "no failed page") {
		t.Fatalf("a retry of a parse with every page read: %d %s", got.status, got.body)
	}
	if got := e.do("POST", "/parses/prs_00000000000000000000000000/retry", nil); got.status != http.StatusNotFound || got.code(t) != "parse_not_found" {
		t.Fatalf("a retry of no parse: %d %s", got.status, got.body)
	}
	if got := e.as("bob-token").do("POST", at2+"/retry", nil); got.status != http.StatusNotFound || got.code(t) != "parse_not_found" {
		t.Fatalf("a retry of another caller's parse: %d %s", got.status, got.body)
	}
	// An admin reads every owner's parses and changes none.
	if got := e.as("root-token").do("POST", at2+"/retry", nil); got.status != http.StatusNotFound {
		t.Fatalf("a retry by an admin that is not the owner: %d %s", got.status, got.body)
	}
}

// TestARetryIsHeldToTheBoundsOfItsGroup: a retry asks parse.create about
// the stored parse and stays in the group the parse was admitted to. A
// group that holds as many parses that have not ended as it may refuses the
// retry with queue_full, and one whose day does not hold the failed pages
// with budget_exhausted, and each admits it once there is room.
func TestARetryIsHeldToTheBoundsOfItsGroup(t *testing.T) {
	durably(t, false)
	rd, rec := newFlaky(), &recorder{}
	release := make(chan struct{})
	e := serve(t, func(s *Server, r *run.Runner) {
		s.Authz = access.NewAuthorizer(rec, configured())
		// The reader "other" holds every page until the test ends, so a
		// parse that pins it stays open.
		held := &stub.Reader{Fail: func(reader.Page) error { <-release; return nil }}
		r.Readers = map[string]reader.Reader{"stub": rd, "other": held}
	})
	t.Cleanup(func() { close(release) })
	tiff := e.upload("scan.tiff", testfixtures.Read(t, testfixtures.MultiTIFF))
	scan := e.upload("scan.png", sheet(t))

	// One place in the group, taken by another parse.
	rec.answer = allowing(`{"group": "full", "max_queued": 1}`, nil)
	failed := e.parsed(tiff, `,"reuse":false`)
	if failed["state"] != "failed" {
		t.Fatalf("the parse ended %v", failed)
	}
	rec.take()
	waiting := e.do("POST", "/parses", `{"source":{"file":"`+scan+`"},"reuse":false,"reader":"other"}`)
	if waiting.status != http.StatusAccepted {
		t.Fatalf("the parse that takes the group's place: %d %s", waiting.status, waiting.body)
	}
	rec.take()
	rd.broken.Store(false)
	full := e.do("POST", "/parses/"+failed["id"].(string)+"/retry", nil)
	if full.status != http.StatusTooManyRequests || full.code(t) != "queue_full" || at(full.json(t), "error", "details", "retryable") != true {
		t.Fatalf("a retry into a group that holds as many parses as it may: %d %s", full.status, full.body)
	}
	// The retry asked parse.create about the stored parse, under its owner.
	if asked := rec.take(); len(asked) != 1 || asked[0].Action != "parse.create" || asked[0].Resource.ID != failed["id"] || asked[0].Resource.String("owner") != "alice" {
		t.Fatalf("the retry asked %+v", asked)
	}
	if got := e.do("POST", "/parses/"+waiting.json(t)["id"].(string)+"/cancel", nil); got.status != http.StatusOK {
		t.Fatalf("cancel: %d %s", got.status, got.body)
	}
	if got := e.do("POST", "/parses/"+failed["id"].(string)+"/retry", nil); got.status != http.StatusAccepted {
		t.Fatalf("the retry once the group has room: %d %s", got.status, got.body)
	}
	if done := e.ended(failed["id"].(string)); done["state"] != "succeeded" {
		t.Fatalf("the retried parse ended %v", done)
	}

	// A day of 4 pages: 2 read by the parse that failed, 2 by others.
	rd.broken.Store(true)
	rec.answer = allowing(`{"group": "budgeted", "pages_per_day": 4}`, nil)
	short := e.parsed(tiff, `,"reuse":false`)
	if short["state"] != "failed" || at(short, "progress", "pages_done") != 2.0 {
		t.Fatalf("the parse ended %v", short)
	}
	for range 2 {
		if p := e.parsed(scan, `,"reuse":false`); p["state"] != "succeeded" {
			t.Fatalf("a parse of 1 page ended %v", p)
		}
	}
	rd.broken.Store(false)
	spent := e.do("POST", "/parses/"+short["id"].(string)+"/retry", nil)
	if spent.status != http.StatusPaymentRequired || spent.code(t) != "budget_exhausted" {
		t.Fatalf("a retry of 1 page into a day with none left: %d %s", spent.status, spent.body)
	}
	if p := e.do("GET", "/parses/"+short["id"].(string), nil).json(t); p["state"] != "failed" {
		t.Fatalf("a refused retry left the parse %v", p)
	}
	// The group's next submit carries a larger day, and the retry is held
	// to the day as it then stands.
	rec.answer = allowing(`{"group": "budgeted", "pages_per_day": 6}`, nil)
	if p := e.parsed(scan, `,"reuse":false`); p["state"] != "succeeded" {
		t.Fatalf("a parse of 1 page ended %v", p)
	}
	if got := e.do("POST", "/parses/"+short["id"].(string)+"/retry", nil); got.status != http.StatusAccepted {
		t.Fatalf("the retry once the day has room: %d %s", got.status, got.body)
	}
	if done := e.ended(short["id"].(string)); done["state"] != "succeeded" || at(done, "progress", "pages_done") != 3.0 {
		t.Fatalf("the retried parse ended %v", done)
	}
}

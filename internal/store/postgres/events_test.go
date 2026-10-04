// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// What a stream of a parse's events is read from (specs/003-api.md): the
// count of a parse's changes, and the change that settled each page, which
// outlives the page's row.

// events reads what a stream is told after a change.
func (h *harness) events(parse string, after int64, limit int) postgres.Events {
	h.t.Helper()
	ev, err := h.store.Events(context.Background(), parse, after, limit)
	if err != nil {
		h.t.Fatalf("reading the events of %s: %v", parse, err)
	}
	return ev
}

// settled lists page events as "page:state@change".
func settled(pages []postgres.PageEvent) string {
	out := ""
	for i, p := range pages {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%d:%s@%d", p.Page, p.State, p.Event)
	}
	return out
}

// TestTheChangesOfAParseAreCounted: every change of a parse's state or
// progress raises its count by 1, and a page's row holds the change that
// settled it. What a stream is told after a change is the pages that
// settled after it, oldest first, with the parse as it stands, from one
// statement. What a stream does not report, usage and a lease, changes no
// count. The statement runs on a direct connection, with nothing prepared,
// and behind the pooler.
func TestTheChangesOfAParseAreCounted(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		w := h.worker()
		h.submit(postgres.Submission{Parse: "prs_a", Owner: "alice", Group: "acme"})
		if ev := h.events("prs_a", 0, 10); ev.Parse.Events != 1 || ev.Parse.State != "queued" || len(ev.Pages) != 0 {
			t.Fatalf("a parse that was submitted is at change %d, %s, with %d pages", ev.Parse.Events, ev.Parse.State, len(ev.Pages))
		}
		prepare := w.claim(1, 1)[0]
		if ev := h.events("prs_a", 0, 10); ev.Parse.Events != 2 || ev.Parse.State != "running" {
			t.Fatalf("a parse whose first task was claimed is at change %d, %s", ev.Parse.Events, ev.Parse.State)
		}
		w.settle(prepared(prepare, 4))
		if ev := h.events("prs_a", 0, 10); ev.Parse.Events != 3 || ev.Parse.PagesTotal != 4 || len(ev.Pages) != 0 {
			t.Fatalf("a parse whose pages were counted is at change %d with %d pages settled", ev.Parse.Events, len(ev.Pages))
		}

		// A claim of a page changes nothing a stream reports, and neither
		// does an attempt that goes back to the queue with what it spent.
		claims := w.claim(4, 4)
		again := ended(claims[3], tasks.Retryable, "reader_unavailable")
		again.Usage = tasks.Usage{Calls: 1, InputTokens: 50}
		w.settle(again)
		if ev := h.events("prs_a", 0, 10); ev.Parse.Events != 3 || ev.Parse.Calls != 1 {
			t.Fatalf("after a claim and an attempt that failed the parse is at change %d with %d calls", ev.Parse.Events, ev.Parse.Calls)
		}

		// 2 pages settle in one exchange, in the order of their settles,
		// and a third in the next.
		w.settle(done(claims[1]), ended(claims[0], tasks.Permanent, "page_unreadable"))
		w.settle(done(claims[2]))
		ev := h.events("prs_a", 0, 10)
		if got := settled(ev.Pages); got != "2:succeeded@4 1:failed@5 3:succeeded@6" || ev.Parse.Events != 6 || ev.Parse.PagesDone != 2 || ev.Parse.PagesFailed != 1 {
			t.Fatalf("the pages that settled are %q and the parse is at change %d", got, ev.Parse.Events)
		}
		if ev.Pages[1].Error == nil || ev.Pages[1].Error.Code != "page_unreadable" || ev.Pages[0].Error != nil {
			t.Fatalf("a failed page says why and a read one nothing: %+v", ev.Pages)
		}
		if row := h.task("prs_a", "page-2"); row.Event != 4 {
			t.Fatalf("the row of the page that settled first holds change %d", row.Event)
		}
		// After a change, and at most so many.
		if got := settled(h.events("prs_a", 4, 10).Pages); got != "1:failed@5 3:succeeded@6" {
			t.Fatalf("the pages that settled after change 4 are %q", got)
		}
		if got := settled(h.events("prs_a", 0, 2).Pages); got != "2:succeeded@4 1:failed@5" {
			t.Fatalf("the first 2 pages that settled are %q", got)
		}
		if got := h.events("prs_a", 6, 10).Pages; len(got) != 0 {
			t.Fatalf("after the parse's own change %d pages settled", len(got))
		}

		// The last page comes back from its backoff and fails for good.
		h.advance(5 * time.Second)
		w.settle(ended(w.claim(1, 1)[0], tasks.Permanent, "page_unreadable"))
		ev = h.events("prs_a", 6, 10)
		if got := settled(ev.Pages); got != "4:failed@7" || ev.Parse.PagesOpen != 0 {
			t.Fatalf("the last page settled as %q with the parse at %d pages open", got, ev.Parse.PagesOpen)
		}

		// The claim of assemble changes nothing, and its settle ends the
		// parse: one change more. The parse has failed pages, so its rows
		// stay and every page is still told.
		assemble := w.claim(1, 1)[0]
		if h.events("prs_a", 0, 10).Parse.Events != 7 {
			t.Fatal("the claim of assemble was counted as a change")
		}
		w.settle(done(assemble))
		ev = h.events("prs_a", 0, 10)
		if ev.Parse.Events != 8 || ev.Parse.State != "failed" || len(ev.Pages) != 4 {
			t.Fatalf("the ended parse is at change %d, %s, with %d pages told", ev.Parse.Events, ev.Parse.State, len(ev.Pages))
		}

		// A retry is a change, and a page queued again is not settled: its
		// row holds no change until it settles again, after every other.
		if err := h.store.Retry(context.Background(), "alice", "prs_a"); err != nil {
			t.Fatal(err)
		}
		ev = h.events("prs_a", 0, 10)
		if got := settled(ev.Pages); ev.Parse.Events != 9 || got != "2:succeeded@4 3:succeeded@6" {
			t.Fatalf("after a retry the parse is at change %d with %q told", ev.Parse.Events, got)
		}
		retried := w.claim(2, 2)
		w.settle(done(retried[0]), done(retried[1]))
		if got := settled(h.events("prs_a", 8, 10).Pages); got != "1:succeeded@10 4:succeeded@11" {
			t.Fatalf("the pages read again settled as %q", got)
		}
		w.settle(done(w.claim(1, 1)[0]))
		// Every page was read: the rows are gone, and each page is still
		// told under the change that settled it.
		ev = h.events("prs_a", 0, 10)
		if got := settled(ev.Pages); ev.Parse.Events != 12 || ev.Parse.State != "succeeded" || len(h.tasks("prs_a")) != 0 ||
			got != "2:succeeded@4 3:succeeded@6 1:succeeded@10 4:succeeded@11" {
			t.Fatalf("the parse read whole is at change %d, %s, with %q told", ev.Parse.Events, ev.Parse.State, got)
		}
		if got := settled(h.events("prs_a", 6, 1).Pages); got != "1:succeeded@10" {
			t.Fatalf("the first page that settled after change 6 is %q", got)
		}

		if _, err := h.store.Events(context.Background(), "prs_none", 0, 10); fault.CodeOf(err) != fault.ParseNotFound {
			t.Fatalf("the events of no parse = %v", err)
		}
	})
}

// TestAPageTheStoreFailsItselfHoldsItsChange: a page whose workers died as
// often as a task may end them is failed by the sweep, with no settle of a
// worker. Its row holds the change that failed it as any settled page's
// does, and a retry queues it again as a task that ended no worker.
func TestAPageTheStoreFailsItselfHoldsItsChange(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		// reap lets the live workers find a dead one: a lease passes while
		// they keep exchanging.
		reap := func(live ...*worker) {
			for range 5 {
				h.advance(third - time.Second)
				for _, w := range live {
					w.exchange(0)
				}
			}
		}
		first, second, next, last := h.worker(), h.worker(), h.worker(), h.worker()
		h.reading(first, postgres.Submission{Parse: "prs_a", Owner: "alice", Group: "acme"}, 2)
		claims := first.claim(2, 2)
		first.settle(done(claims[0]))
		before := h.events("prs_a", 0, 10).Parse.Events

		// 3 workers die with the second page: the one it shared and 2 that
		// ran it alone.
		delete(first.held, tasks.Ref{Parse: "prs_a", Task: "page-2"})
		reap(second, next, last)
		second.claim(1, 1)
		reap(next, last)
		next.claim(1, 1)
		reap(last)
		row := h.task("prs_a", "page-2")
		ev := h.events("prs_a", before, 10)
		if row.State != tasks.Failed || row.Expiries != 3 || row.Event != before+1 || settled(ev.Pages) != fmt.Sprintf("2:failed@%d", before+1) || ev.Parse.Events != before+1 {
			t.Fatalf("the page the sweep failed is %+v, told as %q with the parse at change %d", row, settled(ev.Pages), ev.Parse.Events)
		}
		last.settle(done(last.claim(1, 1)[0]))

		// Queued again it has ended no worker, so a worker that runs
		// something else takes it too.
		if err := h.store.Retry(context.Background(), "alice", "prs_a"); err != nil {
			t.Fatal(err)
		}
		h.submit(postgres.Submission{Parse: "prs_b", Owner: "alice", Group: "acme"})
		if got := last.claim(2, 2); got[1].Task != "page-2" || got[1].Alone || got[1].Expiries != 0 {
			t.Fatalf("after the retry the claims are %s, the page %+v", names(got), got[1])
		}
	})
}

// TestAStopAndANativeParseAreChanges: a cancel is one change and tells no
// page of the ones it stopped. A format whose pages prepare wrote has no
// page task: its pages are told one change each, in the order of the
// selection, from the settle of prepare on.
func TestAStopAndANativeParseAreChanges(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_stop", Owner: "alice", Group: "acme"}, 3)
		w.settle(done(w.claim(1, 1)[0]))
		before := h.events("prs_stop", 0, 10)
		if err := h.store.Cancel(ctx, "prs_stop"); err != nil {
			t.Fatal(err)
		}
		after := h.events("prs_stop", 0, 10)
		if after.Parse.Events != before.Parse.Events+1 || after.Parse.State != "canceled" || settled(after.Pages) != settled(before.Pages) || len(after.Pages) != 1 {
			t.Fatalf("a cancel took the parse from change %d to %d, %s, with %q told", before.Parse.Events, after.Parse.Events, after.Parse.State, settled(after.Pages))
		}

		h.submit(postgres.Submission{Parse: "prs_native", Owner: "alice", Group: "acme"})
		native := done(w.claim(1, 1)[0])
		native.Prepare = &tasks.Prepared{Manifest: []byte(`{"media_type":"text/csv","source":"native"}`), Pages: pages(5), Native: true}
		w.settle(native)
		// Change 3 counted the pages, and each page is one change after it,
		// in the order of the selection.
		ev := h.events("prs_native", 0, 10)
		if got := settled(ev.Pages); ev.Parse.Events != 8 || ev.Parse.PagesDone != 5 ||
			got != "1:succeeded@4 2:succeeded@5 3:succeeded@6 4:succeeded@7 5:succeeded@8" {
			t.Fatalf("a native parse after prepare is at change %d with %d pages done and %q told", ev.Parse.Events, ev.Parse.PagesDone, got)
		}
		w.settle(done(w.claim(1, 1)[0]))
		if ev := h.events("prs_native", 6, 10); ev.Parse.Events != 9 || ev.Parse.State != "succeeded" || settled(ev.Pages) != "4:succeeded@7 5:succeeded@8" {
			t.Fatalf("the native parse ended at change %d, %s, with %q told after change 6", ev.Parse.Events, ev.Parse.State, settled(ev.Pages))
		}
		// The page list goes with its parse.
		if err := h.store.DeleteParse(ctx, "alice", "prs_native"); err != nil {
			t.Fatal(err)
		}
		if n := value[int](h, `SELECT count(*) FROM page_events`); n != 0 {
			t.Fatalf("%d page lists outlived their parse", n)
		}
	})
}

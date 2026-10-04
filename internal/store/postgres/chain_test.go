// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"testing"
	"time"

	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// How a page moves down the policy's chain for what a reader answered
// (specs/005-parse-graph.md, the table of reader errors): the store holds
// how far along the chain each page is, and the claim reads it.

// chain is settings with 3 readers in the read chain, each wide enough that
// its room decides nothing.
func chain() tasks.Settings {
	return readers(
		tasks.Pool{Reader: "first", MaxInFlight: 100},
		tasks.Pool{Reader: "second", MaxInFlight: 100, Cost: 3},
		tasks.Pool{Reader: "third", MaxInFlight: 100, Cost: 9},
	)
}

// unusable is a settle of a call the reader answered with a reply that is
// not usable.
func unusable(c tasks.Claim) tasks.Settle {
	s := ended(c, tasks.Retryable, "page_unreadable")
	s.Invalid = true
	return s
}

// declined is a settle of a call whose reader cannot be the one to read the
// page. code is what the page fails with when no reader is left.
func declined(c tasks.Claim, code string) tasks.Settle {
	return ended(c, tasks.Next, code)
}

// TestTheSecondUnusableReplyMovesAPageOnce: an unusable reply spends an
// attempt and waits a backoff. The second from one reader sends the page to
// the next reader in the chain, with no wait and attempts of its own. That
// happens once for a page: unusable replies of the reader it moved to are
// failures like any other, and at the bound of attempts the page fails with
// the error of its last attempt.
func TestTheSecondUnusableReplyMovesAPageOnce(t *testing.T) {
	settings := chain()
	settings.Attempts = 3
	everywhere(t, settings, func(t *testing.T, h *harness) {
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme", AllowFailedPages: 1}, 1)

		c := w.claim(1, 1)[0]
		if c.Reader != "first" {
			t.Fatalf("the page was first claimed for %+v", c)
		}
		w.settle(unusable(c))
		got := h.task("prs_a", "page-1")
		if got.State != tasks.Queued || got.Attempt != 1 || got.Invalid != 1 || got.ChainAt != 0 || got.Lane != "page" || !got.AvailableAt.After(h.now) {
			t.Fatalf("after one unusable reply the page is %+v", got)
		}

		h.advance(2 * time.Second)
		c = w.claim(1, 1)[0]
		if c.Reader != "first" || c.Attempt != 1 {
			t.Fatalf("the second attempt was claimed as %+v", c)
		}
		w.settle(unusable(c))
		got = h.task("prs_a", "page-1")
		if got.State != tasks.Queued || got.ChainAt != 1 || got.Attempt != 0 || got.Invalid != 0 || !got.Escalated ||
			got.Lane != "page@1" || !got.AvailableAt.Equal(h.now) {
			t.Fatalf("after the second unusable reply the page is %+v", got)
		}

		// The reader it moved to has attempts of its own, and its unusable
		// replies move the page no further.
		for attempt := range 3 {
			c = w.claim(1, 1)[0]
			if c.Reader != "second" || c.Attempt != attempt {
				t.Fatalf("attempt %d on the next reader was claimed as %+v", attempt, c)
			}
			if charged := h.task("prs_a", "page-1").Charged; charged != 3 {
				t.Fatalf("a page read by the second reader is charged %d, want its cost", charged)
			}
			w.settle(unusable(c))
			h.advance(5 * time.Second)
		}
		got = h.task("prs_a", "page-1")
		if got.State != tasks.Failed || got.ChainAt != 1 || got.Attempt != 3 || got.Error == nil || got.Error.Code != "page_unreadable" {
			t.Fatalf("at the bound of attempts on the next reader the page is %+v, error %+v", got, got.Error)
		}
		if p := h.parse("prs_a"); p.PagesFailed != 1 || p.PagesOpen != 0 {
			t.Fatalf("the parse counts %+v", p)
		}
	})
}

// TestADeclinedPageMovesDownTheChainAsFarAsItGoes: a reader that cannot be
// the one to read a page sends it to the next, with no attempt spent and no
// limit on how far. With no reader left the page fails with the error its
// last settle carried. A failure that is the reader's own counts against
// that reader's breaker.
func TestADeclinedPageMovesDownTheChainAsFarAsItGoes(t *testing.T) {
	everywhere(t, chain(), func(t *testing.T, h *harness) {
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme", AllowFailedPages: 1}, 1)

		for at, name := range []string{"first", "second", "third"} {
			c := w.claim(1, 1)[0]
			if c.Reader != name || c.Attempt != 0 {
				t.Fatalf("at position %d the page was claimed as %+v", at, c)
			}
			if got := h.task("prs_a", "page-1"); got.ChainAt != at {
				t.Fatalf("at position %d the page is %+v", at, got)
			}
			settle := declined(c, "reader_unavailable")
			settle.Health = tasks.Unhealthy
			w.settle(settle)
			if n := value[int](h, `SELECT failures FROM pools WHERE reader = $1`, name); n != 1 {
				t.Fatalf("a request the endpoint of %s rejected counted %d failures against it", name, n)
			}
		}
		got := h.task("prs_a", "page-1")
		if got.State != tasks.Failed || got.Attempt != 0 || got.Error == nil || got.Error.Code != "reader_unavailable" {
			t.Fatalf("with no reader left the page is %+v, error %+v", got, got.Error)
		}
	})
}

// TestAPinnedPageNeverMoves: a parse that named its reader has a chain of
// one. Its page stays with that reader through unusable replies, and fails
// at once when the reader declines it.
func TestAPinnedPageNeverMoves(t *testing.T) {
	logic(t, chain(), func(t *testing.T, h *harness) {
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme", Pin: "first", AllowFailedPages: 1}, 1)

		for attempt := range 3 {
			c := w.claim(1, 1)[0]
			if c.Reader != "first" || c.Attempt != attempt {
				t.Fatalf("attempt %d of a pinned page was claimed as %+v", attempt, c)
			}
			w.settle(unusable(c))
			got := h.task("prs_a", "page-1")
			if got.State != tasks.Queued || got.ChainAt != 0 || got.Escalated || got.Lane != "page:first" || got.Invalid != attempt+1 {
				t.Fatalf("a pinned page with %d unusable replies is %+v", attempt+1, got)
			}
			h.advance(10 * time.Second)
		}

		w.settle(declined(w.claim(1, 1)[0], "page_unreadable"))
		got := h.task("prs_a", "page-1")
		if got.State != tasks.Failed || got.ChainAt != 0 || got.Error == nil || got.Error.Code != "page_unreadable" {
			t.Fatalf("a pinned page its reader declined is %+v, error %+v", got, got.Error)
		}
	})
}

// TestAPageMovesPastTheReaderThatReadIt: a page that was read by the second
// reader, because the first was paused, moves to the third when the second
// declines it, and is not sent back to the first.
func TestAPageMovesPastTheReaderThatReadIt(t *testing.T) {
	logic(t, chain(), func(t *testing.T, h *harness) {
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 2)
		first := w.claim(1, 1)[0]
		w.settle(limited(first, time.Minute))

		c := w.claim(1, 1)[0]
		if c.Task != "page-2" || c.Reader != "second" {
			t.Fatalf("with the first reader paused the claim is %+v", c)
		}
		w.settle(declined(c, "page_unreadable"))
		if got := h.task("prs_a", "page-2"); got.State != tasks.Queued || got.ChainAt != 2 || got.Lane != "page@2" {
			t.Fatalf("a page the second reader declined is %+v", got)
		}
		if c := w.claim(1, 1)[0]; c.Task != "page-2" || c.Reader != "third" {
			t.Fatalf("the page that moved was claimed as %+v", c)
		}
	})
}

// TestAPageThatMovedWaitsForTheReaderItMovedTo: the pages at one position of
// the chain are a lane of their own, with room when the reader they are with
// has room. A page that moved to a reader that is full waits for it while
// the pages at the head of the chain are read, and the counters of each lane
// equal a recount of its rows.
func TestAPageThatMovedWaitsForTheReaderItMovedTo(t *testing.T) {
	settings := readers(tasks.Pool{Reader: "first", MaxInFlight: 100}, tasks.Pool{Reader: "second", MaxInFlight: 1})
	logic(t, settings, func(t *testing.T, h *harness) {
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 6)
		claims := w.claim(2, 2)
		w.settle(declined(claims[0], "page_unreadable"), declined(claims[1], "page_unreadable"))
		if n := value[int](h, `SELECT queued FROM lane_service WHERE lane = 'page@1'`); n != 2 {
			t.Fatalf("the lane of the second position counts %d queued pages, want 2", n)
		}

		// The second reader takes one of the two, and the 4 pages at the head
		// of the chain are read by the first. The other page that moved
		// waits: the first reader has room, and that page is not sent back.
		got := by(w.claim(10, 5))
		if got["first"] != 4 || got["second"] != 1 {
			t.Fatalf("with one page waiting for the second reader the claims went to %v", got)
		}
		w.claim(10, 0)
		if row := h.task("prs_a", "page-2"); row.State != tasks.Queued || row.ChainAt != 1 {
			t.Fatalf("the page that waits for the second reader is %+v", row)
		}

		// The second reader's slot frees, and the page takes it.
		w.settle(done(w.held[tasks.Ref{Parse: "prs_a", Task: "page-1"}]))
		if c := w.claim(10, 1)[0]; c.Task != "page-2" || c.Reader != "second" {
			t.Fatalf("the slot the second reader freed went to %+v", c)
		}
	})
}

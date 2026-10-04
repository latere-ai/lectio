// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// The criteria of specs/004-durable-tasks.md and specs/005-parse-graph.md
// that hold at the store, each run on a direct connection, in exec mode and
// behind a transaction-mode pooler.

// third is a third of the default lease: the longest a worker may go between
// two exchanges and still return the tasks of a dead one.
const third = tasks.DefaultLease / 3

// TestAParseRunsFromPrepareToItsEnd: the graph of a parse. Prepare writes
// exactly one page task per selected page and no assemble row; the settle of
// the last page writes assemble; the settle of assemble ends the parse,
// keeps its counters and its usage on the parse, and removes the rows of the
// tasks that succeeded.
func TestAParseRunsFromPrepareToItsEnd(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		w := h.worker()
		h.submit(postgres.Submission{Parse: "prs_a", Group: "acme"})
		if p := h.parse("prs_a"); p.State != "queued" || p.StartedAt != nil || !p.DeadlineAt.Equal(epoch.Add(time.Hour)) {
			t.Fatalf("a submitted parse is %+v", p)
		}

		prepare := w.claim(4, 1)[0]
		if prepare.Kind != tasks.Prepare || prepare.Token != 1 || prepare.Reader != "" || prepare.Group != "acme" {
			t.Fatalf("the first claim is %+v", prepare)
		}
		if p := h.parse("prs_a"); p.State != "running" || p.StartedAt == nil {
			t.Fatalf("a parse with a leased task is %+v", p)
		}
		w.settle(prepared(prepare, 3))
		rows := h.tasks("prs_a")
		if len(rows) != 4 || rows[0].ID != tasks.PrepareID || rows[0].State != tasks.Succeeded ||
			rows[1].ID != "page-1" || rows[2].ID != "page-2" || rows[3].ID != "page-3" || rows[3].Seq != 3 {
			t.Fatalf("after prepare the tasks are %+v", rows)
		}
		if p := h.parse("prs_a"); p.PagesTotal != 3 || p.PagesOpen != 3 || string(p.Manifest) == "" {
			t.Fatalf("after prepare the parse is %+v", p)
		}

		claims := w.claim(4, 3)
		for i, c := range claims {
			if c.Kind != tasks.Page || c.Reader != stub || c.Task != tasks.PageID(i+1) {
				t.Fatalf("claim %d is %+v", i, c)
			}
		}
		first := done(claims[0])
		first.Usage = tasks.Usage{Calls: 1, InputTokens: 900, OutputTokens: 300}
		w.settle(first, done(claims[1]))
		if got := h.task("prs_a", "page-1"); got.State != tasks.Succeeded || got.Output != first.Output || got.Calls != 1 || got.InputTokens != 900 {
			t.Fatalf("a settled page is %+v", got)
		}
		if p := h.parse("prs_a"); p.PagesDone != 2 || p.PagesOpen != 1 || len(h.tasks("prs_a")) != 4 {
			t.Fatalf("with a page open the parse is %+v and has %d tasks", p, len(h.tasks("prs_a")))
		}

		// The last page settles as failed: it releases assemble as a
		// succeeded one would.
		w.settle(ended(claims[2], tasks.Permanent, "page_unreadable"))
		assemble := w.claim(4, 1)[0]
		if assemble.Kind != tasks.Assemble || assemble.Reader != "" {
			t.Fatalf("after the last page the claim is %+v", assemble)
		}
		settle := done(assemble)
		w.settle(settle)

		p := h.parse("prs_a")
		if p.State != "failed" || p.Error == nil || p.Error.Code != string(fault.PageUnreadable) || p.Index != settle.Assemble.Index ||
			p.PagesDone != 2 || p.PagesFailed != 1 || p.PagesOpen != 0 || p.Calls != 1 || p.InputTokens != 900 || p.OutputTokens != 300 || p.FinishedAt == nil {
			t.Fatalf("the ended parse is %+v, error %+v", p, p.Error)
		}
		// What was read is kept: the failed page's row stays for a retry, and
		// the rows that succeeded are gone.
		if rows := h.tasks("prs_a"); len(rows) != 1 || rows[0].ID != "page-3" || rows[0].State != tasks.Failed || rows[0].Error.Code != "page_unreadable" {
			t.Fatalf("after the parse ended its tasks are %+v", rows)
		}
		if got := w.exchange(4); len(got.Claims) != 0 {
			t.Fatalf("an ended parse still had %s to claim", names(got.Claims))
		}
	})
}

// TestAParseEndsByWhatItAllows: the same parse with allow_failed_pages 1
// succeeds with the failed page counted, and a failed prepare or assemble
// fails the parse with the task's own error.
func TestAParseEndsByWhatItAllows(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_allowed", Group: "acme", AllowFailedPages: 1}, 2)
		claims := w.claim(2, 2)
		w.settle(done(claims[0]), ended(claims[1], tasks.Permanent, "page_unreadable"))
		w.settle(done(w.claim(1, 1)[0]))
		if p := h.parse("prs_allowed"); p.State != "succeeded" || p.Error != nil || p.PagesFailed != 1 {
			t.Fatalf("a parse that allows one failed page ended %+v", p)
		}

		h.submit(postgres.Submission{Parse: "prs_corrupt", Group: "acme"})
		w.settle(ended(w.claim(1, 1)[0], tasks.Permanent, "document_corrupt"))
		if p := h.parse("prs_corrupt"); p.State != "failed" || p.Error == nil || p.Error.Code != "document_corrupt" || p.FinishedAt == nil {
			t.Fatalf("a parse whose prepare failed is %+v", p)
		}

		h.reading(w, postgres.Submission{Parse: "prs_unassembled", Group: "acme"}, 1)
		w.settle(done(w.claim(1, 1)[0]))
		// A failure with no error of its own is still a failure with a code.
		w.settle(ended(w.claim(1, 1)[0], tasks.Permanent, ""))
		if p := h.parse("prs_unassembled"); p.State != "failed" || p.Error == nil || p.Error.Code != string(fault.Internal) || p.PagesDone != 1 {
			t.Fatalf("a parse whose assemble failed is %+v", p)
		}
	})
}

// TestAFailedParseSaysWhatItsPagesFailedWith: a parse that fails for its
// pages carries the code they failed with when they all failed with one, so
// a parse whose every failed page was refused for budget fails with
// budget_exhausted, and page_unreadable when they failed with several. A
// page that was read does not change the code, and a parse that allows its
// failed pages carries none.
func TestAFailedParseSaysWhatItsPagesFailedWith(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		w := h.worker()
		for _, tc := range []struct {
			id      string
			allowed int
			// codes are what each page fails with, in page order. An empty
			// one is a page that is read.
			codes []string
			want  string
		}{
			{"prs_budget", 0, []string{"budget_exhausted", "budget_exhausted", "budget_exhausted"}, "budget_exhausted"},
			{"prs_reader", 0, []string{"", "reader_unavailable", "reader_unavailable"}, "reader_unavailable"},
			{"prs_one", 0, []string{"", "", "budget_exhausted"}, "budget_exhausted"},
			{"prs_mixed", 0, []string{"budget_exhausted", "page_unreadable", ""}, "page_unreadable"},
			{"prs_three", 0, []string{"budget_exhausted", "reader_unavailable", "internal"}, "page_unreadable"},
			{"prs_allowed", 3, []string{"budget_exhausted", "budget_exhausted", ""}, ""},
		} {
			h.reading(w, postgres.Submission{Parse: tc.id, Group: "acme", AllowFailedPages: tc.allowed}, len(tc.codes))
			var settles []tasks.Settle
			failed := 0
			for i, c := range w.claim(len(tc.codes), len(tc.codes)) {
				if tc.codes[i] == "" {
					settles = append(settles, done(c))
					continue
				}
				failed++
				settles = append(settles, ended(c, tasks.Permanent, tc.codes[i]))
			}
			w.settle(settles...)
			w.settle(done(w.claim(1, 1)[0]))

			p := h.parse(tc.id)
			if tc.want == "" {
				if p.State != "succeeded" || p.Error != nil {
					t.Errorf("%s: a parse that allows its failed pages ended %s with %+v", tc.id, p.State, p.Error)
				}
				continue
			}
			wantDetail := fmt.Sprintf("%d of %d pages could not be read", failed, len(tc.codes))
			if p.State != "failed" || p.Error == nil || p.Error.Code != tc.want || p.Error.Detail != wantDetail {
				t.Errorf("%s: pages that failed with %q ended the parse %s with %+v, want %s", tc.id, tc.codes, p.State, p.Error, tc.want)
			}
		}
	})
}

// TestANativeParseWritesNoPageTask: prepare of a format that carries its own
// structure wrote the pages itself, so its settle counts them done and
// writes assemble, and a selection of no page does the same.
func TestANativeParseWritesNoPageTask(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		w := h.worker()
		h.submit(postgres.Submission{Parse: "prs_native", Group: "acme"})
		settle := done(w.claim(1, 1)[0])
		settle.Prepare = &tasks.Prepared{Pages: pages(40), Native: true}
		w.settle(settle)
		rows := h.tasks("prs_native")
		if len(rows) != 2 || rows[0].ID != tasks.AssembleID || rows[0].State != tasks.Queued || rows[1].ID != tasks.PrepareID {
			t.Fatalf("after a native prepare the tasks are %+v", rows)
		}
		if p := h.parse("prs_native"); p.PagesTotal != 40 || p.PagesDone != 40 || p.PagesOpen != 0 {
			t.Fatalf("after a native prepare the parse is %+v", p)
		}
		w.settle(done(w.claim(1, 1)[0]))
		if p := h.parse("prs_native"); p.State != "succeeded" || len(h.tasks("prs_native")) != 0 {
			t.Fatalf("the native parse ended %+v with %d task rows", p, len(h.tasks("prs_native")))
		}

		h.submit(postgres.Submission{Parse: "prs_empty", Group: "acme"})
		empty := done(w.claim(1, 1)[0])
		empty.Prepare = &tasks.Prepared{}
		w.settle(empty)
		if c := w.claim(1, 1)[0]; c.Kind != tasks.Assemble || c.Parse != "prs_empty" {
			t.Fatalf("a selection of no page did not write assemble: %+v", c)
		}
	})
}

// TestAStaleTokenCannotSettle: every claim raises the task's token, and a
// settle is accepted only under the current one, from the worker that holds
// the lease. A refused settle records nothing.
func TestAStaleTokenCannotSettle(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		w, other := h.worker(), h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 1)

		first := w.claim(1, 1)[0]
		w.settle(ended(first, tasks.Returned, ""))
		second := w.claim(1, 1)[0]
		if first.Token != 1 || second.Token != 2 {
			t.Fatalf("the tokens of two claims are %d and %d", first.Token, second.Token)
		}

		// The first claim's settle arrives late, under the token it was given.
		late := done(first)
		late.Usage = tasks.Usage{Calls: 1, InputTokens: 10}
		// The worker still runs the task under the token of its second claim,
		// and says so in the exchange that carries the late settle.
		reply, err := h.store.Exchange(context.Background(), w.id, tasks.Request{
			Settles: []tasks.Settle{late},
			Held:    []tasks.Held{{Parse: second.Parse, Task: second.Task, Token: second.Token}},
		})
		if err != nil || len(reply.Refused) != 1 || reply.Refused[0] != (tasks.Ref{Parse: "prs_a", Task: "page-1"}) || len(reply.Lost) != 0 {
			t.Fatalf("a settle under a stale token was not refused: %+v, %v", reply, err)
		}

		// Another live worker cannot settle it under the right token either.
		if reply := other.exchange(0, done(second)); len(reply.Refused) != 1 {
			t.Fatalf("a settle from a worker that does not hold the lease was not refused: %+v", reply)
		}
		got := h.task("prs_a", "page-1")
		if got.State != tasks.Leased || got.LeaseToken != 2 || got.Output != "" || got.Calls != 0 || h.parse("prs_a").PagesDone != 0 {
			t.Fatalf("a refused settle wrote to the task: %+v", got)
		}

		w.settle(done(second))
		if got := h.task("prs_a", "page-1"); got.State != tasks.Succeeded || got.Output != done(second).Output {
			t.Fatalf("the settle under the current token was not recorded: %+v", got)
		}
		// A settle that was accepted cannot be sent twice.
		if reply := w.exchange(0, done(second)); len(reply.Refused) != 1 {
			t.Fatalf("a task was settled twice: %+v", reply)
		}
	})
}

// TestAWorkerPastItsLeaseCannotSettle: a worker that stopped exchanging for
// longer than its lease is taken for dead by a live one. Its tasks are back
// in the queue, every settle it sends is refused and records nothing, the
// reply says the fleet gave it up, and it works again only under a new id.
func TestAWorkerPastItsLeaseCannotSettle(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		paused, live := h.worker(), h.worker()
		h.reading(paused, postgres.Submission{Parse: "prs_a", Group: "acme"}, 2)
		claims := paused.claim(2, 2)

		// The live worker keeps exchanging while the paused one says nothing.
		for range 5 {
			h.advance(third - time.Second)
			live.exchange(0)
		}
		for _, c := range claims {
			if got := h.task(c.Parse, c.Task); got.State != tasks.Queued || got.Expiries != 1 || got.Attempt != 0 {
				t.Fatalf("a task of a worker past its lease is %+v", got)
			}
		}

		reply := paused.raw(tasks.Request{Free: 2}, done(claims[0]))
		if !reply.Gone || len(reply.Refused) != 1 || len(reply.Lost) != 1 || len(reply.Claims) != 0 {
			t.Fatalf("the exchange of a worker past its lease answered %+v", reply)
		}
		if got := h.task("prs_a", "page-1"); got.State != tasks.Queued || got.Output != "" || h.parse("prs_a").PagesDone != 0 {
			t.Fatalf("a worker past its lease settled a task: %+v", got)
		}
		// It stays given up for as long as it keeps its id.
		if reply := paused.raw(tasks.Request{Free: 2}); !reply.Gone {
			t.Fatalf("a worker the fleet gave up was taken back under its old id: %+v", reply)
		}
		again := h.worker()
		if again.id == paused.id {
			t.Fatal("a worker registered twice under one id")
		}
		if c := again.claim(2, 1)[0]; !c.Alone || c.Expiries != 1 || c.Token != 2 {
			t.Fatalf("the new registration claimed %+v", c)
		}
	})
}

// TestTheDeadWorkerSweep: the tasks of a worker whose lease ran out return to
// the queue with expiries + 1, their attempts unchanged and their slot
// cleared, and the worker's row is removed. A worker reaps others only when
// its own previous exchange was within the last third of a lease.
func TestTheDeadWorkerSweep(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		dead, away := h.worker(), h.worker()
		h.reading(dead, postgres.Submission{Parse: "prs_a", Group: "acme"}, 2)

		// One page has an attempt behind it, so the sweep is seen to leave
		// the attempts alone.
		failed := dead.claim(1, 1)[0]
		dead.settle(ended(failed, tasks.Retryable, "reader_unavailable"))
		h.advance(2 * time.Second)
		claims := dead.claim(2, 2)
		if got := h.task("prs_a", "page-1"); got.Attempt != 1 || !got.Calling || got.Reader != stub {
			t.Fatalf("a leased page with one attempt spent is %+v", got)
		}

		// Both workers go silent for longer than a lease. The first to come
		// back was away too long to tell a dead worker from a database nobody
		// could reach: it renews itself and reaps no one.
		h.advance(tasks.DefaultLease + 10*time.Second)
		away.exchange(0)
		for _, c := range claims {
			if got := h.task(c.Parse, c.Task); got.State != tasks.Leased || got.Expiries != 0 {
				t.Fatalf("a worker that was away reaped %+v", got)
			}
		}
		if n := value[int64](h, `SELECT count(*) FROM workers`); n != 2 {
			t.Fatalf("%d workers are registered after an exchange that may not reap, want 2", n)
		}

		// Its next exchange is within a third of a lease of its last.
		h.advance(time.Second)
		away.exchange(0)
		for _, c := range claims {
			got := h.task(c.Parse, c.Task)
			if got.State != tasks.Queued || got.Expiries != 1 || got.Calling || got.Reader != "" || got.LeaseOwner != "" {
				t.Fatalf("a reaped task is %+v", got)
			}
		}
		if got := h.task("prs_a", "page-1"); got.Attempt != 1 {
			t.Fatalf("the sweep changed a task's attempts to %d", got.Attempt)
		}
		if n := value[int64](h, `SELECT count(*) FROM workers WHERE worker_id = $1`, dead.id); n != 0 {
			t.Fatal("the dead worker's row is still there")
		}
	})
}

// TestAStallOfTheDatabaseExpiresNothing: after the database was unreachable
// for two lease periods, every worker's lease is past its expiry and no
// worker reaped another. Each renews at its first exchange and reaps no one,
// so no task has its expiries raised and every settle is accepted.
func TestAStallOfTheDatabaseExpiresNothing(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		one, two := h.worker(), h.worker()
		h.reading(one, postgres.Submission{Parse: "prs_a", Group: "acme"}, 4)
		a, b := one.claim(2, 2), two.claim(2, 2)

		h.advance(2 * tasks.DefaultLease)
		if reply := one.exchange(0, done(a[0])); reply.Gone || len(reply.Refused) != 0 || len(reply.Lost) != 0 {
			t.Fatalf("the first exchange after the stall answered %+v", reply)
		}
		h.advance(time.Second)
		if reply := two.exchange(0, done(b[0])); reply.Gone || len(reply.Refused) != 0 || len(reply.Lost) != 0 {
			t.Fatalf("the second worker's exchange after the stall answered %+v", reply)
		}
		// From here both are live again and the sweep finds nothing.
		h.advance(time.Second)
		one.exchange(0)
		for _, row := range h.tasks("prs_a") {
			if row.Expiries != 0 {
				t.Fatalf("the stall raised the expiries of %+v", row)
			}
		}
		if p := h.parse("prs_a"); p.PagesDone != 2 {
			t.Fatalf("after the stall %d pages are done, want 2", p.PagesDone)
		}
		one.settle(done(a[1]))
		two.settle(done(b[1]))
	})
}

// TestATaskThatKillsItsWorkerRunsAloneAndFails: the 8 pages of a worker
// that died each come back with expiries 1 and are then run alone, one per
// idle worker. The page that kills every worker that touches it is failed
// with page_unreadable after 3 workers, and the 7 that shared its first
// worker succeed with expiries 1.
func TestATaskThatKillsItsWorkerRunsAloneAndFails(t *testing.T) {
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
		h.reading(first, postgres.Submission{Parse: "prs_a", Group: "acme", AllowFailedPages: 1}, 8)
		first.claim(8, 8)

		// The first worker dies with all 8.
		reap(second, next, last)
		for _, row := range h.tasks("prs_a") {
			if row.Kind == tasks.Page && (row.State != tasks.Queued || row.Expiries != 1) {
				t.Fatalf("a page that shared the dead worker is %+v", row)
			}
		}

		// An idle worker is handed one of them and nothing more, however many
		// slots it has free, and claims nothing while it holds it.
		killer := second.claim(8, 1)[0]
		if !killer.Alone || killer.Task != "page-1" || killer.Expiries != 1 {
			t.Fatalf("the claim of a task that must run alone is %+v", killer)
		}
		second.claim(7, 0)
		// A worker that runs something is not handed one.
		h.submit(postgres.Submission{Parse: "prs_other", Group: "acme"})
		if c := next.claim(1, 1)[0]; c.Parse != "prs_other" {
			t.Fatalf("a task that must run alone went to %+v", c)
		}
		next.claim(8, 0)
		next.settle(done(next.held[tasks.Ref{Parse: "prs_other", Task: tasks.PrepareID}]))
		next.settle(done(next.claim(1, 1)[0]))

		// The second worker dies running page-1 alone, and then the third.
		reap(next, last)
		if got := h.task("prs_a", "page-1"); got.State != tasks.Queued || got.Expiries != 2 {
			t.Fatalf("after its second worker the page is %+v", got)
		}
		if c := next.claim(8, 1)[0]; c.Task != "page-1" || !c.Alone {
			t.Fatalf("the third worker claimed %+v", c)
		}
		reap(last)
		got := h.task("prs_a", "page-1")
		if got.State != tasks.Failed || got.Expiries != 3 || got.Error == nil || got.Error.Code != string(fault.PageUnreadable) || got.Attempt != 0 {
			t.Fatalf("after its third worker the page is %+v, error %+v", got, got.Error)
		}
		if p := h.parse("prs_a"); p.PagesFailed != 1 || p.PagesOpen != 7 {
			t.Fatalf("the parse counts %+v", p)
		}

		// The other 7 are run alone and succeed.
		for i := range 7 {
			c := last.claim(8, 1)[0]
			if !c.Alone || c.Expiries != 1 {
				t.Fatalf("page %d of the 7 was claimed as %+v", i, c)
			}
			if i == 6 {
				for _, row := range h.tasks("prs_a") {
					if row.Kind == tasks.Page && row.ID != "page-1" && row.Expiries != 1 {
						t.Fatalf("a page that shared the first worker has expiries %d", row.Expiries)
					}
				}
			}
			last.settle(done(c))
		}
		last.settle(done(last.claim(1, 1)[0]))
		if p := h.parse("prs_a"); p.State != "succeeded" || p.PagesDone != 7 || p.PagesFailed != 1 {
			t.Fatalf("the parse ended %+v", p)
		}
	})
}

// TestARetryableFailureBacksOffAndIsBounded: a retryable failure spends an
// attempt and makes the task wait min(cap, base * 2^(attempt-1)) plus jitter
// of at most half of that, and the task fails at the bound of attempts with
// the error of its last attempt.
func TestARetryableFailureBacksOffAndIsBounded(t *testing.T) {
	settings := defaults()
	settings.Attempts = 8
	logic(t, settings, func(t *testing.T, h *harness) {
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme", AllowFailedPages: 1}, 1)
		delays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, tasks.DefaultBackoffCap}
		for attempt, delay := range delays {
			c := w.claim(1, 1)[0]
			if c.Attempt != attempt {
				t.Fatalf("claim %d carries %d attempts", attempt, c.Attempt)
			}
			settle := ended(c, tasks.Retryable, "reader_unavailable")
			settle.Usage = tasks.Usage{Calls: 1, InputTokens: 100}
			w.settle(settle)
			got := h.task("prs_a", "page-1")
			wait := got.AvailableAt.Sub(h.now)
			if got.State != tasks.Queued || got.Attempt != attempt+1 || wait < delay || wait > delay+delay/2 {
				t.Fatalf("after failure %d the task is %s with %d attempts and waits %v, want %v to %v", attempt+1, got.State, got.Attempt, wait, delay, delay+delay/2)
			}
			if got.Error == nil || got.Error.Code != "reader_unavailable" {
				t.Fatalf("a task waiting for its next attempt does not say why: %+v", got.Error)
			}
			// It is not claimed while it waits.
			h.advance(delay - time.Millisecond)
			w.claim(1, 0)
			h.advance(delay/2 + 2*time.Millisecond)
		}
		w.settle(ended(w.claim(1, 1)[0], tasks.Retryable, "reader_unavailable"))
		got := h.task("prs_a", "page-1")
		if got.State != tasks.Failed || got.Attempt != 8 || got.Error.Code != "reader_unavailable" || got.Calls != 7 || got.InputTokens != 700 {
			t.Fatalf("at the bound of attempts the task is %+v", got)
		}
		// What every attempt spent is on the parse, the failed ones included.
		if p := h.parse("prs_a"); p.PagesFailed != 1 || p.Calls != 7 || p.InputTokens != 700 {
			t.Fatalf("the parse counts %+v", p)
		}
	})
}

// TestARateLimitSpendsNoAttempt: a rate-limit reply ends the attempt as a
// wait. The task's attempts are unchanged, it is not looked at until the
// pause ends, and neither is any other task of the scope, by any worker.
func TestARateLimitSpendsNoAttempt(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		w, other := h.worker(), h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 3)
		c := w.claim(1, 1)[0]
		settle := ended(c, tasks.Wait, "")
		settle.RetryAfter = 7 * time.Second
		settle.Usage = tasks.Usage{Calls: 1}
		w.settle(settle)

		got := h.task("prs_a", "page-1")
		if got.State != tasks.Queued || got.Attempt != 0 || !got.AvailableAt.Equal(h.now.Add(7*time.Second)) || got.Calls != 1 {
			t.Fatalf("a task told to wait is %+v", got)
		}
		// For 7 seconds no worker calls the reader with that key, and the
		// reply says when the pause ends.
		h.advance(7*time.Second - time.Millisecond)
		for _, each := range []*worker{w, other} {
			reply := each.exchange(4)
			if len(reply.Claims) != 0 || reply.SleepUntil == nil || !reply.SleepUntil.Equal(h.now.Add(time.Millisecond)) {
				t.Fatalf("during the pause the exchange answered %+v", reply)
			}
		}
		h.advance(time.Millisecond)
		if c := w.claim(1, 1)[0]; c.Attempt != 0 || c.Task != "page-1" {
			t.Fatalf("after the pause the claim is %+v", c)
		}
		for _, row := range h.tasks("prs_a") {
			if row.Attempt != 0 {
				t.Fatalf("the rate limit spent an attempt of %+v", row)
			}
		}

		// A reply that names no wait takes the default pause.
		w.settle(ended(w.held[tasks.Ref{Parse: "prs_a", Task: "page-1"}], tasks.Wait, ""))
		if got := h.task("prs_a", "page-1"); !got.AvailableAt.Equal(h.now.Add(tasks.DefaultPoolPause)) {
			t.Fatalf("a wait with no time given waits until %v, want %v", got.AvailableAt, h.now.Add(tasks.DefaultPoolPause))
		}
	})
}

// TestCancelFences: a cancel moves the parse and its queued and leased tasks
// to canceled in one transaction. A page that finishes afterwards settles
// against a row that is no longer leased: its output is not recorded,
// progress does not advance, and assemble is never written. The worker is
// told at its next exchange, and the slots are free at once.
func TestCancelFences(t *testing.T) {
	settings := defaults()
	settings.Pools[0].MaxInFlight = 2
	everywhere(t, settings, func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 3)
		claims := w.claim(2, 2)

		if err := h.store.Cancel(ctx, "prs_a"); err != nil {
			t.Fatalf("canceling: %v", err)
		}
		if p := h.parse("prs_a"); p.State != "canceled" || p.FinishedAt == nil {
			t.Fatalf("when the cancel returned the parse was %+v", p)
		}
		for _, row := range h.tasks("prs_a") {
			if row.Kind == tasks.Page && (row.State != tasks.Canceled || row.Calling) {
				t.Fatalf("a page of a canceled parse is %+v", row)
			}
		}

		// The page's call returns after the cancel.
		late := done(claims[0])
		late.Usage = tasks.Usage{Calls: 1, InputTokens: 50}
		reply := w.exchange(2, late)
		if len(reply.Refused) != 1 || len(reply.Lost) != 1 || reply.Lost[0].Task != claims[1].Task || len(reply.Claims) != 0 {
			t.Fatalf("the exchange after the cancel answered %+v", reply)
		}
		p := h.parse("prs_a")
		if p.State != "canceled" || p.PagesDone != 0 || p.PagesOpen != 3 || p.Calls != 0 {
			t.Fatalf("a settle after the cancel advanced the parse: %+v", p)
		}
		for _, row := range h.tasks("prs_a") {
			if row.ID == tasks.AssembleID || row.Output != "" && row.Kind == tasks.Page {
				t.Fatalf("after the cancel the parse has %+v", row)
			}
		}

		// The canceled calls hold no slot: a pool of 2 admits 2 again.
		h.reading(w, postgres.Submission{Parse: "prs_b", Group: "acme"}, 2)
		w.claim(2, 2)

		if err := h.store.Cancel(ctx, "prs_a"); fault.CodeOf(err) != fault.AlreadyTerminal {
			t.Fatalf("canceling a parse that has ended: %v", err)
		}
		if err := h.store.Cancel(ctx, "prs_none"); fault.CodeOf(err) != fault.ParseNotFound {
			t.Fatalf("canceling a parse that is not there: %v", err)
		}
	})
}

// TestTheDeadlineSweep: a parse whose pinned reader never admits a call ends
// failed with deadline_exceeded when its deadline passes, and its tasks that
// had not settled are canceled. Nothing waits without bound.
func TestTheDeadlineSweep(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		w := h.worker()
		// The parse names a reader that is not configured: its pages wait.
		h.reading(w, postgres.Submission{Parse: "prs_pinned", Group: "acme", Pin: "gone", Deadline: 10 * time.Minute}, 3)
		h.reading(w, postgres.Submission{Parse: "prs_later", Group: "acme", Deadline: time.Hour}, 1)
		running := w.claim(4, 1)[0]
		if running.Parse != "prs_later" {
			t.Fatalf("a page pinned to a reader that is gone was claimed: %+v", running)
		}

		h.advance(10*time.Minute - time.Second)
		w.exchange(0)
		if p := h.parse("prs_pinned"); p.State != "running" {
			t.Fatalf("before its deadline the parse is %s", p.State)
		}
		h.advance(tasks.DefaultSweepInterval)
		w.exchange(0)
		p := h.parse("prs_pinned")
		if p.State != "failed" || p.Error == nil || p.Error.Code != string(fault.DeadlineExceeded) || p.FinishedAt == nil {
			t.Fatalf("past its deadline the parse is %+v, error %+v", p, p.Error)
		}
		for _, row := range h.tasks("prs_pinned") {
			if row.Kind == tasks.Page && row.State != tasks.Canceled {
				t.Fatalf("a page of a parse past its deadline is %+v", row)
			}
		}
		if p := h.parse("prs_later"); p.State != "running" {
			t.Fatalf("a parse inside its deadline was ended: %+v", p)
		}
		w.settle(done(running))
	})
}

// TestAShutdownExchange: a worker's last exchange settles what finished,
// returns the rest to the queue with no counter changed, and removes its
// row, so a rolling restart costs neither an attempt nor a lease period.
func TestAShutdownExchange(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		w, next := h.worker(), h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 3)

		// One page has an attempt behind it.
		w.settle(ended(w.claim(1, 1)[0], tasks.Retryable, "reader_unavailable"))
		h.advance(2 * time.Second)
		claims := w.claim(3, 3)

		reply := w.raw(tasks.Request{Shutdown: true, Free: 3}, done(claims[2]))
		if reply.Gone || len(reply.Refused) != 0 || len(reply.Lost) != 0 || len(reply.Claims) != 0 {
			t.Fatalf("the shutdown exchange answered %+v", reply)
		}
		for i, want := range []struct {
			state   tasks.State
			attempt int
		}{{tasks.Queued, 1}, {tasks.Queued, 0}, {tasks.Succeeded, 0}} {
			got := h.task("prs_a", tasks.PageID(i+1))
			if got.State != want.state || got.Attempt != want.attempt || got.Expiries != 0 || got.Calling || !got.AvailableAt.Before(h.now.Add(time.Nanosecond)) {
				t.Fatalf("after the shutdown page %d is %+v", i+1, got)
			}
		}
		if n := value[int64](h, `SELECT count(*) FROM workers WHERE worker_id = $1`, w.id); n != 0 {
			t.Fatal("the worker's row outlived its shutdown")
		}
		if reply := w.raw(tasks.Request{Free: 1}); !reply.Gone {
			t.Fatalf("a worker that shut down was answered %+v", reply)
		}

		// Another worker takes them at once, with nothing to run alone.
		for _, c := range next.claim(3, 2) {
			if c.Alone || c.Expiries != 0 {
				t.Fatalf("a task returned at shutdown was claimed as %+v", c)
			}
		}
	})
}

// TestIdlePollsWriteNoTaskRow: with every pool full for 10 minutes and 32
// idle slots polling, no task row is written. Waiting for capacity is not an
// outcome: a task with no room is never claimed, never charged and never
// written.
func TestIdlePollsWriteNoTaskRow(t *testing.T) {
	settings := defaults()
	settings.Pools[0].MaxInFlight = 8
	logic(t, settings, func(t *testing.T, h *harness) {
		holder := h.worker()
		h.reading(holder, postgres.Submission{Parse: "prs_a", Group: "acme"}, 200)
		holder.claim(8, 8)
		idle := []*worker{h.worker(), h.worker(), h.worker(), h.worker()}

		// A row that was written has a new version: its physical address and
		// the transaction that made it change with every update.
		const fingerprint = `SELECT md5(string_agg(ctid::text || '/' || xmin::text, ',' ORDER BY parse_id, task_id)) FROM tasks`
		before := value[string](h, fingerprint)
		charged := value[int64](h, `SELECT sum(vtime) FROM group_service`)

		for range 120 {
			h.advance(5 * time.Second)
			holder.exchange(0)
			for _, w := range idle {
				if reply := w.exchange(8); len(reply.Claims) != 0 {
					t.Fatalf("a full pool admitted %s", names(reply.Claims))
				}
			}
		}
		if after := value[string](h, fingerprint); after != before {
			t.Fatal("a task row was written while every slot waited for capacity")
		}
		if after := value[int64](h, `SELECT sum(vtime) FROM group_service`); after != charged {
			t.Fatalf("waiting was charged: the group's virtual time moved from %d to %d", charged, after)
		}

		// The slots were waiting and not stuck: one that frees is taken.
		holder.settle(done(holder.held[tasks.Ref{Parse: "prs_a", Task: "page-1"}]))
		idle[0].claim(8, 1)
	})
}

// TestTheStoreKeepsNothingOnItsConnection: behind the pooler every statement
// may reach another server connection, and none of them holds a prepared
// statement or an advisory lock when its transaction has ended.
func TestTheStoreKeepsNothingOnItsConnection(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 2)
		w.settle(done(w.claim(1, 1)[0]))
		if err := h.store.Cancel(ctx, "prs_a"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.Queue(ctx); err != nil {
			t.Fatal(err)
		}
		if row, ok, err := h.store.Task(ctx, "prs_a", "page-1"); err != nil || !ok || row.State != tasks.Succeeded {
			t.Fatalf("reading one task: %+v, %t, %v", row, ok, err)
		}
		if err := h.store.Ping(ctx); err != nil {
			t.Fatal(err)
		}
		var prepared, locks int
		if err := h.store.Decode(ctx, &prepared, `SELECT count(*)::text FROM pg_prepared_statements`); err != nil {
			t.Fatal(err)
		}
		if err := h.store.Decode(ctx, &locks, `SELECT count(*)::text FROM pg_locks WHERE locktype = 'advisory'
			   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`); err != nil {
			t.Fatal(err)
		}
		if prepared != 0 || locks != 0 {
			t.Fatalf("a connection the store used holds %d prepared statements and %d advisory locks", prepared, locks)
		}
	})
}

// TestSubmitsCancelsAndExchangesAtOnce: submits, cancels and exchanges of
// the same groups run at once without one waiting on another in a circle.
// Every parse ends, succeeded or canceled, no settle is lost, and the
// counters equal a recount of the rows.
func TestSubmitsCancelsAndExchangesAtOnce(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		const (
			submitters = 4
			each       = 30
			pagesEach  = 5
		)
		ctx := context.Background()
		var wg sync.WaitGroup
		var failed atomic.Pointer[error]
		fail := func(err error) {
			if err != nil {
				failed.CompareAndSwap(nil, &err)
			}
		}
		var submitted atomic.Int64
		for s := range submitters {
			wg.Go(func() {
				for i := range each {
					id := fmt.Sprintf("prs_%d_%03d", s, i)
					_, _, err := h.store.Submit(ctx, filled(postgres.Submission{
						Parse: id, Group: "g" + strconv.Itoa(i%3), Project: "p" + strconv.Itoa(s%2), Class: tasks.Class(i % 2),
					}))
					fail(err)
					submitted.Add(1)
					// Every 7th parse is canceled, at any point of its run.
					if i%7 == 0 {
						if err := h.store.Cancel(ctx, id); err != nil && fault.CodeOf(err) != fault.AlreadyTerminal {
							fail(err)
						}
					}
				}
			})
		}
		open := func() int64 {
			var n int64
			fail(h.store.Decode(ctx, &n, `SELECT count(*)::text FROM parses WHERE state IN ('queued', 'running')`))
			return n
		}
		for range 4 {
			id, err := h.store.Register(ctx)
			if err != nil {
				t.Fatal(err)
			}
			wg.Go(func() {
				var settles []tasks.Settle
				for failed.Load() == nil && (submitted.Load() < submitters*each || len(settles) > 0 || open() > 0) {
					reply, err := h.store.Exchange(ctx, id, tasks.Request{Free: 4, Idle: true, Settles: settles})
					if err != nil {
						fail(err)
						return
					}
					settles = settles[:0]
					for _, c := range reply.Claims {
						settle := done(c)
						if c.Kind == tasks.Prepare {
							settle = prepared(c, pagesEach)
						}
						settles = append(settles, settle)
					}
				}
			})
		}
		wg.Wait()
		if err := failed.Load(); err != nil {
			t.Fatalf("a call failed: %v", *err)
		}
		states := value[string](h, `SELECT string_agg(state || ':' || n, ',' ORDER BY state) FROM (SELECT state, count(*) AS n FROM parses GROUP BY state) s`)
		if n := value[int64](h, `SELECT count(*) FROM parses WHERE state NOT IN ('succeeded', 'canceled')`); n != 0 {
			t.Fatalf("the parses ended %s", states)
		}
		if n := value[int64](h, `SELECT count(*) FROM parses WHERE state = 'succeeded' AND pages_done <> $1`, pagesEach); n != 0 {
			t.Fatalf("%d parses succeeded without all their pages", n)
		}
		if n := value[int64](h, `SELECT count(*) FROM tasks WHERE state IN ('queued', 'leased')`); n != 0 {
			t.Fatalf("%d tasks are left unsettled", n)
		}
	})
}

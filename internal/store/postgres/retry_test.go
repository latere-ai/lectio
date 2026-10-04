// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// Reading again the pages of a parse that failed
// (specs/004-durable-tasks.md, specs/005-parse-graph.md): what a retry does
// to the rows, what it is refused for, and which bounds it is held to.

// failing queues a parse of n pages and ends it with assemble, the pages
// named in lost having failed with the code and every other page read. It
// is called while nothing else can be claimed.
func (h *harness) failing(w *worker, sub postgres.Submission, n int, code string, lost ...int) {
	h.t.Helper()
	h.reading(w, sub, n)
	h.settling(w, sub.Parse, code, lost...)
}

// settling claims the queued pages of a parse, settles those named in lost
// as failed with the code and the others as read, and settles the assemble
// that follows.
func (h *harness) settling(w *worker, parse, code string, lost ...int) {
	h.t.Helper()
	var settles []tasks.Settle
	for _, c := range w.exchange(1000).Claims {
		n, isPage := tasks.PageOf(c.Task)
		if c.Parse != parse || !isPage {
			h.t.Fatalf("claimed %s/%s where only pages of %s were queued", c.Parse, c.Task, parse)
		}
		if slices.Contains(lost, n) {
			settles = append(settles, ended(c, tasks.Permanent, code))
			continue
		}
		read := done(c)
		read.Usage = tasks.Usage{Calls: 1, InputTokens: 100, OutputTokens: 10}
		settles = append(settles, read)
	}
	w.settle(settles...)
	assemble := w.claim(1, 1)[0]
	if assemble.Parse != parse || assemble.Kind != tasks.Assemble {
		h.t.Fatalf("after the pages of %s the claim is %s/%s", parse, assemble.Parse, assemble.Task)
	}
	w.settle(done(assemble))
}

// states lists the task rows of a parse as "task:state/attempt/token".
func (h *harness) states(parse string) string {
	h.t.Helper()
	return value[string](h, `SELECT coalesce(string_agg(task_id || ':' || state || '/' || attempt || '/' || lease_token, ' '
	                                  ORDER BY seq, task_id), '') FROM tasks WHERE parse_id = $1`, parse)
}

// TestARetryQueuesAgainOnlyThePagesThatFailed: a parse that ended with
// failed pages keeps every task row. A retry returns the failed page rows to
// the queue with their attempts and their place in the chain as a new task
// has them and their tokens kept, and the parse to running with only those
// pages open. No other page is claimed again, the assemble that ended the
// parse runs again under the token after its last, and the parse then ends
// succeeded with no task row left. The statement runs on a direct
// connection, with nothing prepared, and behind the pooler.
func TestARetryQueuesAgainOnlyThePagesThatFailed(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.failing(w, postgres.Submission{Parse: "prs_a", Owner: "alice", Group: "acme", Deadline: 10 * time.Minute}, 4, "reader_unavailable", 3, 4)

		ended := h.parse("prs_a")
		if ended.State != "failed" || ended.Error == nil || ended.Error.Code != "reader_unavailable" ||
			ended.PagesDone != 2 || ended.PagesFailed != 2 || ended.Index == "" || ended.FinishedAt == nil {
			t.Fatalf("the parse ended %+v, error %+v", ended, ended.Error)
		}
		// Every row is kept: the pages that were read are found through
		// them when the parse is assembled again.
		if got := h.states("prs_a"); got != "assemble:succeeded/0/1 prepare:succeeded/0/1 page-1:succeeded/0/1 page-2:succeeded/0/1 page-3:failed/0/1 page-4:failed/0/1" {
			t.Fatalf("after the parse ended its tasks are %q", got)
		}

		h.advance(time.Hour)
		if err := h.store.Retry(ctx, "alice", "prs_a"); err != nil {
			t.Fatalf("the retry: %v", err)
		}
		p := h.parse("prs_a")
		if p.State != "running" || p.Error != nil || p.FinishedAt != nil || p.Index != "" || p.ExpiresAt != nil ||
			p.PagesOpen != 2 || p.PagesFailed != 0 || p.PagesDone != 2 || p.PagesTotal != 4 {
			t.Fatalf("after the retry the parse is %+v, error %+v", p, p.Error)
		}
		// It has as long from the retry as its submit gave it, and it
		// started when it first started.
		if !p.DeadlineAt.Equal(h.now.Add(10*time.Minute)) || p.RetriedAt == nil || !p.RetriedAt.Equal(h.now) || !p.StartedAt.Equal(*ended.StartedAt) {
			t.Fatalf("after a retry at %v the deadline is %v, retried at %v, started at %v", h.now, p.DeadlineAt, p.RetriedAt, p.StartedAt)
		}
		if got := h.states("prs_a"); got != "assemble:succeeded/0/1 prepare:succeeded/0/1 page-1:succeeded/0/1 page-2:succeeded/0/1 page-3:queued/0/1 page-4:queued/0/1" {
			t.Fatalf("after the retry the tasks are %q", got)
		}
		if row := h.task("prs_a", "page-3"); row.Error != nil || row.SettledAt != nil || row.ChainAt != 0 || !row.AvailableAt.Equal(h.now) {
			t.Fatalf("a page queued again is %+v", row)
		}

		// Only the 2 pages are there to claim, each under the token after
		// the one its failed attempt held.
		claims := w.claim(8, 2)
		if claims[0].Task != "page-3" || claims[1].Task != "page-4" || claims[0].Token != 2 || claims[0].Attempt != 0 {
			t.Fatalf("the claims after a retry are %s, the first under token %d", names(claims), claims[0].Token)
		}
		w.settle(done(claims[0]))
		if p := h.parse("prs_a"); p.PagesOpen != 1 || p.PagesDone != 3 {
			t.Fatalf("with one page read again the parse is %+v", p)
		}
		w.settle(done(claims[1]))
		assemble := w.claim(8, 1)[0]
		if assemble.Kind != tasks.Assemble || assemble.Token != 2 || assemble.Attempt != 0 {
			t.Fatalf("the assemble after a retry is %+v", assemble)
		}
		settle := done(assemble)
		w.settle(settle)

		p = h.parse("prs_a")
		if p.State != "succeeded" || p.Error != nil || p.PagesDone != 4 || p.PagesFailed != 0 || p.PagesOpen != 0 ||
			p.Index != settle.Assemble.Index || p.Index == ended.Index || p.FinishedAt == nil {
			t.Fatalf("the retried parse ended %+v with the index %q, the first being %q", p, p.Index, ended.Index)
		}
		// The first run made 2 calls and the retry's settles reported none.
		if p.Calls != 2 || p.InputTokens != 200 {
			t.Fatalf("the parse's usage after the retry is %d calls and %d tokens", p.Calls, p.InputTokens)
		}
		if rows := h.tasks("prs_a"); len(rows) != 0 {
			t.Fatalf("a parse with every page read keeps %d task rows", len(rows))
		}
		if err := h.store.Retry(ctx, "alice", "prs_a"); fault.CodeOf(err) != fault.Conflict {
			t.Fatalf("a retry of a parse with no failed page = %v", err)
		}
	})
}

// TestAPageThatFailsAgainCanBeReadAgain: a retry whose page fails again
// ends the parse failed again, with the page's new code, and the parse can
// be retried once more. A parse that allowed its failed pages and succeeded
// with them is read again the same way.
func TestAPageThatFailsAgainCanBeReadAgain(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.failing(w, postgres.Submission{Parse: "prs_a", Owner: "alice", Group: "acme"}, 3, "reader_unavailable", 2)
		for round, code := range []string{"budget_exhausted", "page_unreadable"} {
			if err := h.store.Retry(ctx, "alice", "prs_a"); err != nil {
				t.Fatalf("retry %d: %v", round+1, err)
			}
			h.settling(w, "prs_a", code, 2)
			p := h.parse("prs_a")
			if p.State != "failed" || p.Error.Code != code || p.PagesFailed != 1 || p.PagesDone != 2 {
				t.Fatalf("after retry %d the parse ended %+v, error %+v", round+1, p, p.Error)
			}
			if row := h.task("prs_a", "page-2"); row.State != tasks.Failed || row.LeaseToken != int64(round+2) {
				t.Fatalf("after retry %d the page is %+v", round+1, row)
			}
			if row := h.task("prs_a", "assemble"); row.State != tasks.Succeeded || row.LeaseToken != int64(round+2) {
				t.Fatalf("after retry %d the assemble is %+v", round+1, row)
			}
		}
		if err := h.store.Retry(ctx, "alice", "prs_a"); err != nil {
			t.Fatal(err)
		}
		h.settling(w, "prs_a", "")
		if p := h.parse("prs_a"); p.State != "succeeded" || p.PagesDone != 3 || len(h.tasks("prs_a")) != 0 {
			t.Fatalf("the parse read whole at last is %+v with %d task rows", p, len(h.tasks("prs_a")))
		}

		h.failing(w, postgres.Submission{Parse: "prs_lenient", Owner: "alice", Group: "acme", AllowFailedPages: 1}, 2, "page_unreadable", 1)
		if p := h.parse("prs_lenient"); p.State != "succeeded" || p.PagesFailed != 1 || len(h.tasks("prs_lenient")) != 4 {
			t.Fatalf("a parse that allows its failed page ended %+v with %d task rows", p, len(h.tasks("prs_lenient")))
		}
		if err := h.store.Retry(ctx, "alice", "prs_lenient"); err != nil {
			t.Fatalf("the retry of a parse that succeeded with a failed page: %v", err)
		}
		h.settling(w, "prs_lenient", "")
		if p := h.parse("prs_lenient"); p.State != "succeeded" || p.PagesFailed != 0 || p.PagesDone != 2 {
			t.Fatalf("it ended %+v", p)
		}
	})
}

// TestWhatARetryIsRefusedFor: a parse that has not ended is not_terminal,
// one that is not there or is another owner's is not found, and one with
// nothing to read again is a conflict that says why: no page of it failed,
// it ended before assemble did, its task rows are not all there, or its
// retention has ended. A refused retry changes nothing.
func TestWhatARetryIsRefusedFor(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		w := h.worker()
		refused := func(what, owner, id string, code fault.Code, reason string) {
			t.Helper()
			before := h.states(id)
			err := h.store.Retry(context.Background(), owner, id)
			if fault.CodeOf(err) != code || !strings.Contains(fault.DetailOf(err), reason) {
				t.Errorf("%s: the retry answered %v, want %s that says %q", what, err, code, reason)
			}
			if after := h.states(id); after != before {
				t.Errorf("%s: a refused retry changed the tasks from %q to %q", what, before, after)
			}
		}

		refused("no such parse", "alice", "prs_none", fault.ParseNotFound, "no parse")

		h.failing(w, postgres.Submission{Parse: "prs_clean", Owner: "alice", Group: "acme"}, 2, "")
		refused("every page was read", "alice", "prs_clean", fault.Conflict, "no failed page")

		h.submit(postgres.Submission{Parse: "prs_native", Owner: "alice", Group: "acme"})
		native := done(w.claim(1, 1)[0])
		native.Prepare = &tasks.Prepared{Pages: pages(3), Native: true}
		w.settle(native)
		w.settle(done(w.claim(1, 1)[0]))
		refused("a native format", "alice", "prs_native", fault.Conflict, "no failed page")

		h.submit(postgres.Submission{Parse: "prs_corrupt", Owner: "alice", Group: "acme"})
		w.settle(ended(w.claim(1, 1)[0], tasks.Permanent, "document_corrupt"))
		refused("prepare failed", "alice", "prs_corrupt", fault.Conflict, "before its pages were all read")

		h.reading(w, postgres.Submission{Parse: "prs_open", Owner: "alice", Group: "acme"}, 2)
		refused("the parse is queued or running", "alice", "prs_open", fault.NotTerminal, "has not ended")
		claims := w.claim(2, 2)
		w.settle(ended(claims[0], tasks.Permanent, "page_unreadable"))
		refused("a page is still open", "alice", "prs_open", fault.NotTerminal, "has not ended")
		if err := h.store.Cancel(context.Background(), "prs_open"); err != nil {
			t.Fatal(err)
		}
		w.exchange(0)
		refused("the parse was canceled", "alice", "prs_open", fault.Conflict, "before its pages were all read")

		h.reading(w, postgres.Submission{Parse: "prs_late", Owner: "alice", Group: "acme", Deadline: time.Minute}, 2)
		late := w.claim(2, 2)
		w.settle(ended(late[0], tasks.Permanent, "page_unreadable"))
		h.advance(2 * time.Minute)
		w.exchange(0)
		if p := h.parse("prs_late"); p.State != "failed" || p.Error.Code != "deadline_exceeded" {
			t.Fatalf("a parse past its deadline is %+v", p)
		}
		refused("the parse ran out of time", "alice", "prs_late", fault.Conflict, "before its pages were all read")

		h.failing(w, postgres.Submission{Parse: "prs_mine", Owner: "alice", Group: "acme"}, 2, "page_unreadable", 1)
		refused("another owner's parse", "bob", "prs_mine", fault.ParseNotFound, "no parse")

		// A parse that ended before its rows were kept, or whose rows a
		// later sweep removed, has nothing a retry could queue.
		h.failing(w, postgres.Submission{Parse: "prs_bare", Owner: "alice", Group: "acme"}, 3, "page_unreadable", 2)
		h.exec(`DELETE FROM tasks WHERE parse_id = 'prs_bare' AND state = 'succeeded'`)
		refused("the rows of the pages that were read are gone", "alice", "prs_bare", fault.Conflict, "no longer kept")
		h.exec(`DELETE FROM tasks WHERE parse_id = 'prs_bare'`)
		refused("every row is gone", "alice", "prs_bare", fault.Conflict, "no longer kept")

		h.failing(w, postgres.Submission{Parse: "prs_kept", Owner: "alice", Group: "acme", Retention: time.Hour}, 2, "page_unreadable", 1)
		h.advance(59 * time.Minute)
		if err := h.store.Retry(context.Background(), "alice", "prs_kept"); err != nil {
			t.Fatalf("a retry inside the retention: %v", err)
		}
		h.settling(w, "prs_kept", "page_unreadable", 1)
		h.advance(61 * time.Minute)
		refused("the retention has ended", "alice", "prs_kept", fault.Conflict, "retention")
	})
}

// TestARetryIsHeldToItsGroupsBounds: a retry makes its parse one that has
// not ended, so a group that holds max_queued such parses is refused it
// with queue_full. The failed pages are reserved again, of the day of the
// retry: a day that does not hold them refuses the retry with
// budget_exhausted, and the pages a retry reserved and did not read go back
// to that day when the parse ends.
func TestARetryIsHeldToItsGroupsBounds(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		acme := postgres.Submission{Owner: "alice", Group: "acme", MaxQueued: 1, PagesPerDay: 5}
		first := acme
		first.Parse = "prs_first"
		h.failing(w, first, 3, "reader_unavailable", 2, 3)
		// The 2 pages that failed went back to the day.
		if got := h.reserved("acme"); got != 1 {
			t.Fatalf("after a parse of 3 pages with 2 failed the day holds %d, want 1", got)
		}

		// Another parse of the group is open: the group is at its bound.
		second := acme
		second.Parse = "prs_second"
		h.reading(w, second, 4)
		if got := h.reserved("acme"); got != 5 {
			t.Fatalf("with 4 more pages reserved the day holds %d", got)
		}
		if err := h.store.Retry(ctx, "alice", "prs_first"); fault.CodeOf(err) != fault.QueueFull {
			t.Fatalf("a retry into a group at max_queued = %v", err)
		}
		if p := h.parse("prs_first"); p.State != "failed" {
			t.Fatalf("a refused retry left the parse %s", p.State)
		}

		// The second parse reads its pages: the group has room for a parse
		// again, and its day has none for 2 pages.
		h.settling(w, "prs_second", "")
		if err := h.store.Retry(ctx, "alice", "prs_first"); fault.CodeOf(err) != fault.BudgetExhausted {
			t.Fatalf("a retry of 2 pages into a day with none left = %v", err)
		}
		if got := h.reserved("acme"); got != 5 || h.parse("prs_first").State != "failed" {
			t.Fatalf("a refused retry left the day at %d and the parse %s", got, h.parse("prs_first").State)
		}

		// The next day holds them. The page read on the first day stays
		// counted there.
		yesterday := h.now.UTC().Format(time.DateOnly)
		h.advance(24 * time.Hour)
		if err := h.store.Retry(ctx, "alice", "prs_first"); err != nil {
			t.Fatalf("a retry on a day with room: %v", err)
		}
		if p := h.parse("prs_first"); h.reserved("acme") != 2 || p.Reserved != 3 {
			t.Fatalf("after the retry the day holds %d and the parse %d", h.reserved("acme"), p.Reserved)
		}
		// One of the 2 is read and one fails again: the failed one goes
		// back to the day of the retry.
		h.settling(w, "prs_first", "reader_unavailable", 3)
		if p := h.parse("prs_first"); h.reserved("acme") != 1 || p.Reserved != 2 || p.PagesDone != 2 {
			t.Fatalf("after the retried parse ended the day holds %d and the parse %d of %d pages done", h.reserved("acme"), p.Reserved, p.PagesDone)
		}
		if before := value[int](h, `SELECT reserved FROM group_days WHERE group_id = 'acme' AND day = $1::date`, yesterday); before != 5 {
			t.Fatalf("the day before holds %d, want the 5 it held", before)
		}

		// A group with no bound is refused nothing.
		h.failing(w, postgres.Submission{Parse: "prs_free", Owner: "bob", Group: "free"}, 2, "page_unreadable", 1)
		h.reading(w, postgres.Submission{Parse: "prs_beside", Owner: "bob", Group: "free"}, 1)
		if err := h.store.Retry(ctx, "bob", "prs_free"); err != nil {
			t.Fatalf("a retry in a group with no bound: %v", err)
		}
		if p := h.parse("prs_free"); p.Reserved != 0 {
			t.Fatalf("a parse of a group with no budget holds %d pages of a day", p.Reserved)
		}
	})
}

// TestARetriedParseHasItsTimeFromTheRetry: the deadline of a parse that is
// read again is the retry plus what its submit gave it, however many times
// it is retried, and a retried parse that passes it fails with
// deadline_exceeded as any parse does.
func TestARetriedParseHasItsTimeFromTheRetry(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.failing(w, postgres.Submission{Parse: "prs_a", Owner: "alice", Group: "acme", Deadline: 10 * time.Minute}, 2, "reader_unavailable", 2)
		for _, later := range []time.Duration{3 * time.Hour, 26 * time.Hour} {
			h.advance(later)
			if err := h.store.Retry(ctx, "alice", "prs_a"); err != nil {
				t.Fatal(err)
			}
			if p := h.parse("prs_a"); !p.DeadlineAt.Equal(h.now.Add(10 * time.Minute)) {
				t.Fatalf("a retry at %v set the deadline to %v", h.now, p.DeadlineAt)
			}
			h.settling(w, "prs_a", "reader_unavailable", 2)
		}

		if err := h.store.Retry(ctx, "alice", "prs_a"); err != nil {
			t.Fatal(err)
		}
		h.advance(9 * time.Minute)
		w.exchange(0)
		if p := h.parse("prs_a"); p.State != "running" {
			t.Fatalf("inside its deadline the retried parse is %s", p.State)
		}
		h.advance(2 * time.Minute)
		w.exchange(0)
		p := h.parse("prs_a")
		if p.State != "failed" || p.Error.Code != "deadline_exceeded" {
			t.Fatalf("past its deadline the retried parse is %+v, error %+v", p, p.Error)
		}
		if row := h.task("prs_a", "page-2"); row.State != tasks.Canceled {
			t.Fatalf("its page is %+v", row)
		}
	})
}

// TestRetriesAtOnceQueueThePagesOnce: of several retries of one parse at
// the same instant one queues the pages and the others find a parse that
// has not ended. The counters of queued tasks equal a recount of the rows,
// which every case of the store ends with.
func TestRetriesAtOnceQueueThePagesOnce(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.failing(w, postgres.Submission{Parse: "prs_a", Owner: "alice", Group: "acme"}, 6, "reader_unavailable", 2, 4, 6)

		var retried, open atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				switch err := h.store.Retry(ctx, "alice", "prs_a"); {
				case err == nil:
					retried.Add(1)
				case fault.CodeOf(err) == fault.NotTerminal:
					open.Add(1)
				default:
					t.Errorf("a retry among 8 at once: %v", err)
				}
			})
		}
		wg.Wait()
		if retried.Load() != 1 || open.Load() != 7 {
			t.Fatalf("%d retries queued the pages and %d found the parse open, want 1 and 7", retried.Load(), open.Load())
		}
		if queued := value[int](h, `SELECT queued FROM group_service WHERE group_id = 'acme'`); queued != 3 {
			t.Fatalf("the group counts %d queued tasks, want the 3 pages", queued)
		}
		if claims := w.claim(8, 3); claims[0].Task != "page-2" || claims[2].Task != "page-6" {
			t.Fatalf("the claims after the retries are %s", names(claims))
		}
	})
}

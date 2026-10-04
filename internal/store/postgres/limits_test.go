// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// What an allow holds a parse and its group to where the pages are counted
// (specs/013-limits-and-usage.md): the most pages one parse may select, and
// the pages a group may have read in a day, as a reservation.

// reserved reads what a group's day holds.
func (h *harness) reserved(group string) int {
	h.t.Helper()
	return value[int](h, `SELECT coalesce((SELECT reserved FROM group_days WHERE group_id = $1 AND day = $2::date), 0)`,
		group, h.now.UTC().Format(time.DateOnly))
}

// counted queues a parse and settles its prepare with n pages, and returns
// the parse as that left it.
func (h *harness) counted(w *worker, sub postgres.Submission, n int) postgres.Parse {
	h.t.Helper()
	h.reading(w, sub, n)
	return h.parse(sub.Parse)
}

// TestAParseSelectsNoMorePagesThanItsAllowLets: at the limit a parse reads
// its pages, and one page past it the parse fails in prepare with
// too_many_pages, says how many pages it would have read, and has no page
// task. A parse whose allow names no bound is held to none.
func TestAParseSelectsNoMorePagesThanItsAllowLets(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		w := h.worker()
		for _, tc := range []struct {
			id           string
			limit, pages int
			state        string
		}{
			{"prs_at", 3, 3, "running"},
			{"prs_past", 3, 4, "failed"},
			{"prs_free", 0, 40, "running"},
		} {
			p := h.counted(w, postgres.Submission{Parse: tc.id, Group: "g-" + tc.id, MaxPages: tc.limit}, tc.pages)
			if p.State != tc.state || p.PagesTotal != tc.pages || p.MaxPages != tc.limit {
				t.Fatalf("%s: a parse of %d pages under a limit of %d is %s with %d pages", tc.id, tc.pages, tc.limit, p.State, p.PagesTotal)
			}
			rows := h.tasks(tc.id)
			if tc.state == "failed" {
				if p.Error == nil || p.Error.Code != string(fault.TooManyPages) || len(rows) != 0 && rows[0].Kind == tasks.Page {
					t.Fatalf("%s: the refusal is %+v with tasks %+v", tc.id, p.Error, rows)
				}
				for _, row := range rows {
					if row.Kind == tasks.Page {
						t.Fatalf("%s: a page task was written past the limit", tc.id)
					}
				}
				continue
			}
			if got := len(w.claim(100, tc.pages)); got != tc.pages {
				t.Fatalf("%s: %d pages wait to be read, want %d", tc.id, got, tc.pages)
			}
		}
	})
}

// TestAGroupsPagesForADayAreReserved: a group's pages are promised when a
// parse counts its own, by the statement that refuses a promise past the
// limit. A parse the day does not hold fails in prepare with
// budget_exhausted before a page is read; a group with nothing left is
// refused at the submit; and the next day begins with nothing reserved. The
// limit is the group's and is refreshed by each submit.
func TestAGroupsPagesForADayAreReserved(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		acme := func(id string) postgres.Submission {
			return postgres.Submission{Parse: id, Owner: "alice", Group: "acme", PagesPerDay: 10}
		}
		if p := h.counted(w, acme("prs_a"), 7); p.State != "running" || p.Reserved != 7 || h.reserved("acme") != 7 {
			t.Fatalf("7 of 10: the parse is %s holding %d, and the day holds %d", p.State, p.Reserved, h.reserved("acme"))
		}
		// 4 more would make 11: the parse fails where its pages are counted,
		// and no page of it is queued.
		p := h.counted(w, acme("prs_b"), 4)
		if p.State != "failed" || p.Error == nil || p.Error.Code != string(fault.BudgetExhausted) || p.Reserved != 0 || p.PagesTotal != 4 {
			t.Fatalf("4 past the 3 left: the parse is %+v", p)
		}
		if h.reserved("acme") != 7 {
			t.Fatalf("a refused parse changed the day to %d", h.reserved("acme"))
		}
		// Exactly what is left is reserved, and the day is then full.
		if p := h.counted(w, acme("prs_c"), 3); p.State != "running" || h.reserved("acme") != 10 {
			t.Fatalf("3 of the 3 left: the parse is %s and the day holds %d", p.State, h.reserved("acme"))
		}
		if got := len(w.claim(100, 10)); got != 10 {
			t.Fatalf("%d pages are queued, want the 10 that were reserved", got)
		}
		_, _, err := h.store.Submit(ctx, filled(acme("prs_d")))
		if fault.CodeOf(err) != fault.BudgetExhausted {
			t.Fatalf("a submit of a group with nothing left: %v", err)
		}
		if n := value[int](h, `SELECT count(*) FROM parses WHERE parse_id = 'prs_d'`); n != 0 {
			t.Fatal("a refused submit wrote a parse")
		}
		// Another group has a day of its own, and a group with no budget
		// reserves nothing.
		if p := h.counted(w, postgres.Submission{Parse: "prs_e", Group: "other", PagesPerDay: 10}, 10); p.State != "running" || h.reserved("other") != 10 {
			t.Fatalf("another group's day: the parse is %s and the day holds %d", p.State, h.reserved("other"))
		}
		w.claim(100, 10)
		if p := h.counted(w, postgres.Submission{Parse: "prs_f", Group: "unbounded"}, 50); p.State != "running" || p.Reserved != 0 || h.reserved("unbounded") != 0 {
			t.Fatalf("a group with no budget: the parse is %s holding %d", p.State, p.Reserved)
		}
		w.claim(100, 50)
		// A higher limit on the next submit holds from that submit on.
		raised := acme("prs_g")
		raised.PagesPerDay = 12
		if p := h.counted(w, raised, 2); p.State != "running" || h.reserved("acme") != 12 {
			t.Fatalf("after the limit rose to 12: the parse is %s and the day holds %d", p.State, h.reserved("acme"))
		}
		w.claim(100, 2)
		// The next day holds nothing yet.
		h.advance(24 * time.Hour)
		if p := h.counted(w, acme("prs_h"), 10); p.State != "running" || h.reserved("acme") != 10 {
			t.Fatalf("the next day: the parse is %s and the day holds %d", p.State, h.reserved("acme"))
		}
	})
}

// TestPagesReservedAndNotReadAreGivenBack: a parse that reserved 100 pages
// and was canceled after 30 gives 70 back, and a parse of 70 pages
// submitted next reserves. Pages that failed are given back when the parse
// ends, and a parse that ends on another day gives back to the day it
// reserved on.
func TestPagesReservedAndNotReadAreGivenBack(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		acme := func(id string) postgres.Submission {
			return postgres.Submission{Parse: id, Group: "acme", PagesPerDay: 100, AllowFailedPages: 5}
		}
		if p := h.counted(w, acme("prs_a"), 100); p.Reserved != 100 || h.reserved("acme") != 100 {
			t.Fatalf("the parse holds %d and the day %d", p.Reserved, h.reserved("acme"))
		}
		var settles []tasks.Settle
		for _, c := range w.claim(30, 30) {
			settles = append(settles, done(c))
		}
		w.settle(settles...)
		if err := h.store.Cancel(ctx, "prs_a"); err != nil {
			t.Fatal(err)
		}
		if p := h.parse("prs_a"); p.State != "canceled" || p.PagesDone != 30 || p.Reserved != 30 || h.reserved("acme") != 30 {
			t.Fatalf("after the cancel the parse is %s with %d read, holds %d, and the day holds %d", p.State, p.PagesDone, p.Reserved, h.reserved("acme"))
		}
		if p := h.counted(w, acme("prs_b"), 70); p.State != "running" || h.reserved("acme") != 100 {
			t.Fatalf("70 pages after 70 were given back: the parse is %s and the day holds %d", p.State, h.reserved("acme"))
		}

		// The parse is canceled the day after it began, with its 70 pages
		// claimed and none read: they go back to the day they were reserved
		// on, and the day of the cancel is charged nothing.
		day := h.now
		w.claim(70, 70)
		h.advance(25 * time.Hour)
		if err := h.store.Cancel(ctx, "prs_b"); err != nil {
			t.Fatal(err)
		}
		back := value[int](h, `SELECT reserved FROM group_days WHERE group_id = 'acme' AND day = $1::date`, day.UTC().Format(time.DateOnly))
		if p := h.parse("prs_b"); p.Reserved != 0 || back != 30 {
			t.Fatalf("a parse that read nothing holds %d, and the day it reserved on holds %d, want 0 and 30", p.Reserved, back)
		}
		if h.reserved("acme") != 0 {
			t.Fatalf("the day the parse ended on was charged %d", h.reserved("acme"))
		}

		// A parse that ends by itself gives back the pages that failed.
		fresh := h.worker()
		if p := h.counted(fresh, acme("prs_c"), 5); p.Reserved != 5 {
			t.Fatalf("the parse holds %d", p.Reserved)
		}
		settles = settles[:0]
		for i, c := range fresh.claim(5, 5) {
			if i < 2 {
				settles = append(settles, ended(c, tasks.Permanent, string(fault.PageUnreadable)))
				continue
			}
			settles = append(settles, done(c))
		}
		fresh.settle(settles...)
		fresh.settle(done(fresh.claim(1, 1)[0]))
		if p := h.parse("prs_c"); p.State != "succeeded" || p.PagesFailed != 2 || p.Reserved != 3 || h.reserved("acme") != 3 {
			t.Fatalf("the parse ended %s with %d failed, holds %d, and the day holds %d", p.State, p.PagesFailed, p.Reserved, h.reserved("acme"))
		}
	})
}

// TestFiftyParsesWithTenPagesLeft: a group with 10 pages of its day left
// submits 50 parses of 10 pages at once. Exactly one reserves, 49 fail in
// prepare, and 10 pages are queued to be read. The workers settle the
// prepares at the same time, on a direct connection, in exec mode and
// behind the pooler.
func TestFiftyParsesWithTenPagesLeft(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		const parses, each, workers = 50, 10, 5
		first := h.worker()
		h.reading(first, postgres.Submission{Parse: "prs_first", Group: "acme", PagesPerDay: 100}, 90)
		for i := range parses {
			h.submit(postgres.Submission{Parse: "prs_" + twoDigits(i), Group: "acme", PagesPerDay: 100})
		}
		fleet := make([]*worker, workers)
		for i := range fleet {
			fleet[i] = h.worker()
		}
		var wg sync.WaitGroup
		var settled atomic.Int64
		for _, w := range fleet {
			wg.Go(func() {
				for settled.Load() < parses {
					reply, err := w.send(tasks.Request{Free: 4})
					if err != nil {
						t.Errorf("claiming: %v", err)
						return
					}
					var settles []tasks.Settle
					for _, c := range reply.Claims {
						if c.Kind == tasks.Prepare {
							settles = append(settles, prepared(c, each))
						}
					}
					if len(settles) == 0 {
						continue
					}
					if _, err := w.send(tasks.Request{}, settles...); err != nil {
						t.Errorf("settling: %v", err)
						return
					}
					settled.Add(int64(len(settles)))
				}
			})
		}
		wg.Wait()
		running := value[int](h, `SELECT count(*) FROM parses WHERE parse_id <> 'prs_first' AND state IN ('queued', 'running')`)
		refused := value[int](h, `SELECT count(*) FROM parses WHERE state = 'failed' AND error->>'code' = 'budget_exhausted'`)
		queued := value[int](h, `SELECT count(*) FROM tasks t JOIN parses p USING (parse_id) WHERE t.kind = 'page' AND p.parse_id <> 'prs_first'`)
		if running != 1 || refused != parses-1 || queued != each || h.reserved("acme") != 100 {
			t.Fatalf("%d parses reserved and %d were refused, %d pages are queued, and the day holds %d; want 1, %d, %d and 100",
				running, refused, queued, h.reserved("acme"), parses-1, each)
		}
	})
}

func twoDigits(i int) string { return string(rune('0'+i/10)) + string(rune('0'+i%10)) }

// TestReservationsAtOnceNeverPassTheLimit: the statement that reserves is
// one statement, and holds by itself with nothing else serializing it: of
// 40 reservations of 3 pages against a limit of 50, made at once, 16 are
// kept, the day holds 48, and no interleaving holds 51.
func TestReservationsAtOnceNeverPassTheLimit(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		var wg sync.WaitGroup
		var kept atomic.Int64
		for range 40 {
			wg.Go(func() {
				ok, err := h.store.Reserve(ctx, "acme", h.now, 3, 50)
				if err != nil {
					t.Errorf("reserving: %v", err)
				}
				if ok {
					kept.Add(1)
				}
			})
		}
		wg.Wait()
		if kept.Load() != 16 || h.reserved("acme") != 48 {
			t.Fatalf("%d reservations were kept and the day holds %d, want 16 and 48", kept.Load(), h.reserved("acme"))
		}
		// A first reservation past the limit writes no row.
		if ok, err := h.store.Reserve(ctx, "fresh", h.now, 51, 50); err != nil || ok || h.reserved("fresh") != 0 {
			t.Fatalf("51 of 50 on a new day: %t, %v, and the day holds %d", ok, err, h.reserved("fresh"))
		}
		if ok, err := h.store.Reserve(ctx, "fresh", h.now, 50, 50); err != nil || !ok {
			t.Fatalf("50 of 50 on a new day: %t, %v", ok, err)
		}
	})
}

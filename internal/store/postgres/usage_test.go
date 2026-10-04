// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// The meters (specs/013-limits-and-usage.md): what each settle adds to
// them, what a read of them sums, and that they equal a recount of what the
// settles recorded on the parses.

// used is a settle that read its page with a call that used tokens.
func used(c tasks.Claim, in, out int64) tasks.Settle {
	s := done(c)
	s.Usage = tasks.Usage{Calls: 1, InputTokens: in, OutputTokens: out}
	return s
}

// spent is a settle that failed with a call that used tokens.
func spent(c tasks.Claim, outcome tasks.Outcome, in int64) tasks.Settle {
	s := ended(c, outcome, "reader_unavailable")
	s.Usage = tasks.Usage{Calls: 1, InputTokens: in}
	return s
}

// usage reads the meter and lists its sums as
// "key@start pages/calls/input/output", the start as day and hour.
func (h *harness) usage(q postgres.UsageQuery) string {
	h.t.Helper()
	sums, err := h.store.Usage(context.Background(), q)
	if err != nil {
		h.t.Fatalf("reading the meter: %v", err)
	}
	out := make([]string, len(sums))
	for i, s := range sums {
		out[i] = fmt.Sprintf("%s@%s %d/%d/%d/%d", s.Key, s.Start.UTC().Format("02T15"), s.Pages, s.Calls, s.InputTokens, s.OutputTokens)
	}
	return strings.Join(out, ", ")
}

// recount is what the settles recorded on the parses and what the meter
// holds, each as "pages/calls/input/output" over every row.
func (h *harness) recount() (parses, meter string) {
	h.t.Helper()
	parses = value[string](h, `SELECT coalesce(sum(pages_done), 0) || '/' || coalesce(sum(calls), 0) || '/' ||
	                                  coalesce(sum(input_tokens), 0) || '/' || coalesce(sum(output_tokens), 0) FROM parses`)
	meter = value[string](h, `SELECT coalesce(sum(pages), 0) || '/' || coalesce(sum(calls), 0) || '/' ||
	                                 coalesce(sum(input_tokens), 0) || '/' || coalesce(sum(output_tokens), 0) FROM usage`)
	return parses, meter
}

// metered are the settings of a case that reads pages with 2 readers.
func metered() tasks.Settings {
	return tasks.Settings{
		Pools:     []tasks.Pool{{Reader: "small", MaxInFlight: 100}, {Reader: "large", MaxInFlight: 100}},
		ReadChain: []string{"small", "large"}, ExtractChain: []string{"small"},
	}
}

// TestTheMeterIsWrittenAsTasksSettle: each settle adds what its attempt
// used to the row of its hour, its parse's group and owner, and the reader
// the attempt was claimed for. A call that failed is counted as the call it
// was, a page that moved to the next reader is metered under both, a page
// counts as read when its task succeeds, whether or not a call was made,
// and the pages of a format that needs no reader are pages read under no
// reader. A read of the meter sums by a group, an owner or a reader over
// hours or days, for the intervals that begin in its span, narrowed to the
// owners and the groups it names. The statement runs on a direct
// connection, with nothing prepared, and behind the pooler.
func TestTheMeterIsWrittenAsTasksSettle(t *testing.T) {
	everywhere(t, metered(), func(t *testing.T, h *harness) {
		w := h.worker()
		// The first hour, from 09:00: alice of acme reads 3 pages.
		h.reading(w, postgres.Submission{Parse: "prs_a", Owner: "alice", Group: "acme"}, 3)
		claims := w.claim(3, 3)
		if claims[0].Reader != "small" {
			t.Fatalf("the first reader of the chain is %q", claims[0].Reader)
		}
		// Page 1 is read. Page 2 fails once, with a call that used tokens.
		// Page 3 is declined by the small reader after a call.
		declined := spent(claims[2], tasks.Next, 30)
		w.settle(used(claims[0], 100, 10), spent(claims[1], tasks.Retryable, 40), declined)
		if got := h.usage(postgres.UsageQuery{By: "reader", Interval: "hour", From: epoch, To: epoch.Add(time.Hour)}); got != "small@01T09 1/3/170/10" {
			t.Fatalf("after the first settles the meter by reader is %q", got)
		}
		h.advance(10 * time.Second)
		again := w.claim(2, 2)
		for _, c := range again {
			switch c.Task {
			case "page-2":
				w.settle(used(c, 100, 10))
			case "page-3":
				if c.Reader != "large" {
					t.Fatalf("the page the small reader declined was claimed for %q", c.Reader)
				}
				w.settle(used(c, 200, 20))
			}
		}
		w.settle(done(w.claim(1, 1)[0]))

		// The second hour: bob of acme reads a page with no call, alice a
		// format that needs no reader, and carol of another group a page.
		h.advance(time.Hour)
		h.reading(w, postgres.Submission{Parse: "prs_b", Owner: "bob", Group: "acme"}, 1)
		w.settle(done(w.claim(1, 1)[0]))
		w.settle(done(w.claim(1, 1)[0]))
		h.submit(postgres.Submission{Parse: "prs_native", Owner: "alice", Group: "acme"})
		native := done(w.claim(1, 1)[0])
		native.Prepare = &tasks.Prepared{Pages: pages(5), Native: true}
		w.settle(native)
		w.settle(done(w.claim(1, 1)[0]))
		h.reading(w, postgres.Submission{Parse: "prs_c", Owner: "carol", Group: "other"}, 1)
		w.settle(used(w.claim(1, 1)[0], 7, 1))
		w.settle(done(w.claim(1, 1)[0]))

		day := postgres.UsageQuery{Interval: "hour", From: epoch, To: epoch.Add(24 * time.Hour)}
		for _, tc := range []struct {
			name string
			edit func(*postgres.UsageQuery)
			want string
		}{
			{"by reader", func(q *postgres.UsageQuery) { q.By = "reader" },
				"large@01T09 1/1/200/20, small@01T09 2/4/270/20, @01T10 5/0/0/0, small@01T10 2/1/7/1"},
			{"by group", func(q *postgres.UsageQuery) { q.By = "group" },
				"acme@01T09 3/5/470/40, acme@01T10 6/0/0/0, other@01T10 1/1/7/1"},
			{"by owner", func(q *postgres.UsageQuery) { q.By = "owner" },
				"alice@01T09 3/5/470/40, alice@01T10 5/0/0/0, bob@01T10 1/0/0/0, carol@01T10 1/1/7/1"},
			{"by group over days", func(q *postgres.UsageQuery) { q.By, q.Interval = "group", "day" },
				"acme@01T00 9/5/470/40, other@01T00 1/1/7/1"},
			{"an hour that begins before the span is left out", func(q *postgres.UsageQuery) { q.By, q.From = "group", epoch.Add(time.Minute) },
				"acme@01T10 6/0/0/0, other@01T10 1/1/7/1"},
			{"an hour that begins at the end of the span is left out", func(q *postgres.UsageQuery) { q.By, q.To = "group", epoch.Add(time.Hour) },
				"acme@01T09 3/5/470/40"},
			{"one owner", func(q *postgres.UsageQuery) { q.By, q.Owners = "reader", []string{"alice"} },
				"large@01T09 1/1/200/20, small@01T09 2/4/270/20, @01T10 5/0/0/0"},
			{"2 owners of one group", func(q *postgres.UsageQuery) {
				q.By, q.Owners, q.Groups = "owner", []string{"bob", "carol"}, []string{"acme"}
			},
				"bob@01T10 1/0/0/0"},
			{"one group", func(q *postgres.UsageQuery) { q.By, q.Groups = "owner", []string{"other"} },
				"carol@01T10 1/1/7/1"},
			{"no owner", func(q *postgres.UsageQuery) { q.By, q.Owners = "group", []string{} }, ""},
			{"no group", func(q *postgres.UsageQuery) { q.By, q.Groups = "group", []string{} }, ""},
		} {
			q := day
			tc.edit(&q)
			if got := h.usage(q); got != tc.want {
				t.Errorf("%s: the meter reads %q, want %q", tc.name, got, tc.want)
			}
		}

		// The meter is what the settles recorded on the parses, and it is
		// 5 rows for 10 pages: one per hour, group, owner and reader.
		if parses, meter := h.recount(); parses != meter || meter != "10/6/477/41" {
			t.Fatalf("the parses hold %s and the meter %s", parses, meter)
		}
		if rows := value[int](h, `SELECT count(*) FROM usage`); rows != 5 {
			t.Fatalf("the meter holds %d rows", rows)
		}
	})
}

// TestTheMeterEqualsARecountAfterEveryWayAParseEnds: whatever becomes of a
// parse, what its settles recorded on its row is what the meter holds: a
// page that fails for good after calls, a call that was told to wait, a
// parse canceled with a call spent, a page whose worker died, which records
// nothing anywhere, and a retry that reads a page again.
func TestTheMeterEqualsARecountAfterEveryWayAParseEnds(t *testing.T) {
	logic(t, metered(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		same := func(when string) {
			t.Helper()
			if parses, meter := h.recount(); parses != meter {
				t.Fatalf("%s the parses hold %s and the meter %s", when, parses, meter)
			}
		}

		// A page fails for good after 2 calls, and one is told to wait
		// after a call.
		h.reading(w, postgres.Submission{Parse: "prs_a", Owner: "alice", Group: "acme"}, 3)
		claims := w.claim(3, 3)
		limited := spent(claims[2], tasks.Wait, 5)
		limited.RetryAfter = time.Second
		w.settle(used(claims[0], 100, 10), spent(claims[1], tasks.Retryable, 40), limited)
		same("after a wait and a failed call")
		h.advance(time.Minute)
		for _, c := range w.claim(2, 2) {
			if c.Task == "page-2" {
				w.settle(spent(c, tasks.Permanent, 60))
				continue
			}
			w.settle(used(c, 90, 9))
		}
		w.settle(done(w.claim(1, 1)[0]))
		same("after a parse that failed")
		if got := h.usage(postgres.UsageQuery{By: "owner", Interval: "day", From: epoch, To: epoch.Add(24 * time.Hour)}); got != "alice@01T00 2/5/295/19" {
			t.Fatalf("the meter of the failed parse is %q", got)
		}

		// A retry reads the failed page: one call more and one page more.
		if err := h.store.Retry(ctx, "alice", "prs_a"); err != nil {
			t.Fatal(err)
		}
		w.settle(used(w.claim(1, 1)[0], 80, 8))
		w.settle(done(w.claim(1, 1)[0]))
		same("after a retry")
		if got := h.usage(postgres.UsageQuery{By: "owner", Interval: "day", From: epoch, To: epoch.Add(24 * time.Hour)}); got != "alice@01T00 3/6/375/27" {
			t.Fatalf("after the retry the meter is %q", got)
		}

		// A parse is canceled with a call spent and a page in flight.
		h.reading(w, postgres.Submission{Parse: "prs_b", Owner: "bob", Group: "acme"}, 2)
		held := w.claim(2, 2)
		w.settle(spent(held[0], tasks.Retryable, 11))
		if err := h.store.Cancel(ctx, "prs_b"); err != nil {
			t.Fatal(err)
		}
		// The page in flight settles after the cancel: nothing is recorded.
		late := used(held[1], 500, 50)
		if reply := w.exchange(0, late); len(reply.Refused) != 1 {
			t.Fatalf("a settle after a cancel was accepted: %+v", reply)
		}
		same("after a cancel")

		// A worker dies with a page: its call is recorded nowhere.
		doomed, live := h.worker(), h.worker()
		h.reading(doomed, postgres.Submission{Parse: "prs_c", Owner: "carol", Group: "other"}, 1)
		doomed.claim(1, 1)
		for range 5 {
			h.advance(third - time.Second)
			live.exchange(0)
			w.exchange(0)
		}
		if got := h.task("prs_c", "page-1"); got.State != tasks.Queued || got.Expiries != 1 {
			t.Fatalf("the page of the dead worker is %+v", got)
		}
		same("after a worker died")
		if got := h.usage(postgres.UsageQuery{By: "group", Interval: "day", From: epoch, To: epoch.Add(24 * time.Hour), Groups: []string{"other"}}); got != "" {
			t.Fatalf("a call nobody settled was metered as %q", got)
		}
	})
}

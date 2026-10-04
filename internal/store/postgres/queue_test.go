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

// The view of the queue (specs/006-fairness-and-priority.md,
// specs/007-model-capacity.md): what waits and what runs, per group and per
// project, and where each reader's pool stands.

// queue reads the queue of some groups, or of every group that holds work.
func (h *harness) queue(groups ...string) postgres.Queue {
	h.t.Helper()
	view, err := h.store.Queue(context.Background(), groups)
	if err != nil {
		h.t.Fatalf("reading the queue: %v", err)
	}
	return view
}

// classes lists the queues of classes as "class:queued/running".
func classes(list []postgres.ClassQueue) string {
	out := make([]string, len(list))
	for i, c := range list {
		out[i] = fmt.Sprintf("%s:%d/%d", c.Class, c.Queued, c.Running)
	}
	return strings.Join(out, " ")
}

// depth lists the groups of a view as "group parses [classes] {project
// parses [classes]}".
func depth(view postgres.Queue) string {
	var out []string
	for _, g := range view.Groups {
		line := fmt.Sprintf("%s %d [%s]", g.Group, g.Parses, classes(g.Classes))
		for _, p := range g.Projects {
			line += fmt.Sprintf(" {%q %d [%s]}", p.Project, p.Parses, classes(p.Classes))
		}
		out = append(out, line)
	}
	return strings.Join(out, "; ")
}

// pools lists the pools of a view as "reader in_flight/max breaker", each
// with its scopes as "(scope in_flight/ceiling paused|open)".
func pools(view postgres.Queue) string {
	var out []string
	for _, p := range view.Pools {
		line := fmt.Sprintf("%s %d/%d %s", p.Reader, p.InFlight, p.MaxInFlight, p.Breaker)
		for _, s := range p.Scopes {
			state := "open"
			if s.PausedUntil != nil {
				state = "paused"
			}
			line += fmt.Sprintf(" (%q %d/%d %s)", s.Scope, s.InFlight, s.Ceiling, state)
		}
		out = append(out, line)
	}
	return strings.Join(out, "; ")
}

// viewed are the settings of the cases: a reader that admits 4 calls at
// once and one beside it.
func viewed() tasks.Settings {
	return tasks.Settings{
		Pools:     []tasks.Pool{{Reader: "small", MaxInFlight: 4}, {Reader: "large", MaxInFlight: 8}},
		ReadChain: []string{"small"}, ExtractChain: []string{"small"},
	}
}

// TestTheQueueIsReadAsItStands: with a known set of parses of 2 groups
// queued, the view holds each group's weight, bounds, parses that have not
// ended and queued and running tasks per class, the same per project, and
// each reader's pool. Its numbers follow the claims, a rate limit and the
// breaker. A read of one group holds that group alone, with the calls in
// flight that are its own, and a group whose parses have all ended is in a
// read that names it and in no other. The statement runs on a direct
// connection, with nothing prepared, and behind the pooler.
func TestTheQueueIsReadAsItStands(t *testing.T) {
	everywhere(t, viewed(), func(t *testing.T, h *harness) {
		w := h.worker()
		if got := h.queue(); len(got.Groups) != 0 || pools(got) != "large 0/8 closed; small 0/4 closed" {
			t.Fatalf("the queue of a store with no parse is %q, %q", depth(got), pools(got))
		}

		// acme: a parse of 3 pages in its own project and a batch parse of
		// 2 in the project search. beta: a parse of 1 page, and one that
		// was not prepared yet.
		acme := postgres.Submission{Owner: "alice", Group: "acme", Weight: 4, MaxQueued: 10, MaxRunning: 6}
		a, b := acme, acme
		a.Parse = "prs_a"
		b.Parse, b.Project, b.ProjectWeight, b.Class = "prs_b", "search", 2, tasks.Batch
		h.reading(w, a, 3)
		h.reading(w, b, 2)
		h.reading(w, postgres.Submission{Parse: "prs_c", Owner: "bob", Group: "beta"}, 1)
		h.submit(postgres.Submission{Parse: "prs_d", Owner: "bob", Group: "beta"})

		view := h.queue()
		if got := depth(view); got != `acme 2 [interactive:3/0 batch:2/0] {"" 1 [interactive:3/0]} {"search" 1 [batch:2/0]}; beta 2 [interactive:2/0] {"" 2 [interactive:2/0]}` {
			t.Fatalf("with nothing claimed the queue is %s", got)
		}
		if g := view.Groups[0]; g.Weight != 4 || g.MaxQueued != 10 || g.MaxRunning != 6 || g.Projects[1].Weight != 2 || view.Groups[1].Weight != 1 {
			t.Fatalf("the weights and the bounds are %+v", view.Groups)
		}

		// Every slot is asked for. The small reader admits 4 of the 6
		// pages, and the prepare of the last parse calls no reader.
		claims := w.claim(10, 5)
		view = h.queue()
		queued, running := 0, 0
		for _, g := range view.Groups {
			for _, c := range g.Classes {
				queued, running = queued+c.Queued, running+c.Running
			}
			// Each group's numbers are a recount of its task rows.
			var recount []string
			for class, name := range []string{"interactive", "batch"} {
				q := value[int](h, `SELECT count(*) FROM tasks WHERE group_id = $1 AND class = $2 AND state = 'queued'`, g.Group, class)
				r := value[int](h, `SELECT count(*) FROM tasks WHERE group_id = $1 AND class = $2 AND state = 'leased'`, g.Group, class)
				if q+r > 0 || g.Group == "acme" {
					recount = append(recount, fmt.Sprintf("%s:%d/%d", name, q, r))
				}
			}
			if got := classes(g.Classes); got != strings.Join(recount, " ") {
				t.Errorf("the group %s is viewed as [%s] and its rows count [%s]", g.Group, got, strings.Join(recount, " "))
			}
		}
		if queued != 2 || running != 5 || pools(view) != "large 0/8 closed; small 4/4 closed" {
			t.Fatalf("after the claims %d tasks wait and %d run, with the pools %q", queued, running, pools(view))
		}

		// One group by name: its own numbers, and the calls in flight that
		// are its own.
		mine := 0
		for _, c := range claims {
			if c.Group == "beta" && c.Kind == tasks.Page {
				mine++
			}
		}
		beta := h.queue("beta")
		if len(beta.Groups) != 1 || beta.Groups[0].Group != "beta" || beta.Groups[0].Parses != 2 || pools(beta) != fmt.Sprintf("large 0/8 closed; small %d/4 closed", mine) {
			t.Fatalf("the queue of beta is %s with the pools %q, and %d of its pages are being read", depth(beta), pools(beta), mine)
		}
		if got := h.queue("nobody", "acme"); len(got.Groups) != 1 || got.Groups[0].Group != "acme" {
			t.Fatalf("a read that names a group that is not there holds %s", depth(got))
		}
		// A list that is there and empty is no group, and not every group.
		if got := h.queue([]string{}...); len(got.Groups) != 0 || pools(got) != "large 0/8 closed; small 0/4 closed" {
			t.Fatalf("a read of no group holds %d groups and the pools %q", len(got.Groups), pools(got))
		}

		// A rate limit pauses the key every group shares and halves its
		// ceiling: the pool says so, to every group.
		var page tasks.Claim
		for _, c := range claims {
			if c.Kind == tasks.Page {
				page = c
			}
		}
		limited := ended(page, tasks.Wait, "")
		limited.RetryAfter = 30 * time.Second
		w.settle(limited)
		if got := pools(h.queue()); got != `large 0/8 closed; small 3/4 closed ("" 3/2 paused)` {
			t.Fatalf("after a rate limit the pools are %q", got)
		}
		if got := pools(h.queue("beta")); got != fmt.Sprintf(`large 0/8 closed; small %d/4 closed ("" %d/2 paused)`, mineAfter(mine, page), mineAfter(mine, page)) {
			t.Fatalf("after a rate limit beta's pools are %q", got)
		}
		paused := h.queue().Pools[1].Scopes[0].PausedUntil
		if paused == nil || !paused.Equal(h.now.Add(30*time.Second)) {
			t.Fatalf("the pause ends at %v, want 30s after %v", paused, h.now)
		}
		// The pause ends, and a ceiling that was halved comes back.
		h.advance(time.Minute)
		if got := pools(h.queue()); got != `large 0/8 closed; small 3/4 closed ("" 3/4 open)` {
			t.Fatalf("after the pause the pools are %q", got)
		}

		// 3 failures in a row open the breaker, and the open period ends in
		// a trial.
		var failures []tasks.Settle
		for _, c := range claims {
			if c.Kind == tasks.Page && c.Task != page.Task || c.Kind == tasks.Page && c.Parse != page.Parse {
				s := ended(c, tasks.Retryable, "reader_unavailable")
				s.Health = tasks.Unhealthy
				failures = append(failures, s)
			}
		}
		w.settle(failures...)
		if got := pools(h.queue()); !strings.HasPrefix(got, "large 0/8 closed; small 0/4 open") {
			t.Fatalf("after 3 failures in a row the pools are %q", got)
		}
		h.advance(tasks.DefaultBreakerOpen + time.Second)
		if got := pools(h.queue()); !strings.HasPrefix(got, "large 0/8 closed; small 0/4 trial") {
			t.Fatalf("after the open period the pools are %q", got)
		}

		// A group whose parses have all ended holds no work: it is in a
		// read that names it, with its counters at 0, and in no other.
		for _, id := range []string{"prs_c", "prs_d"} {
			if err := h.store.Cancel(context.Background(), id); err != nil {
				t.Fatal(err)
			}
		}
		w.exchange(0)
		if got := depth(h.queue()); !strings.HasPrefix(got, "acme 2 ") || strings.Contains(got, "beta") {
			t.Fatalf("with beta's parses ended the queue is %s", got)
		}
		if got := depth(h.queue("beta")); got != `beta 0 [interactive:0/0] {"" 0 [interactive:0/0]}` {
			t.Fatalf("named, a group with no work is %s", got)
		}
	})
}

// mineAfter is how many of a group's pages are still being read once a
// page was told to wait: one fewer when the page was the group's.
func mineAfter(mine int, limited tasks.Claim) int {
	if limited.Group == "beta" {
		return mine - 1
	}
	return mine
}

// TestAGroupsOwnKeyIsItsOwnScope: with a key per group, a rate limit
// pauses the scope of the group whose key was limited. The pool lists that
// scope to a read of every group and to a read of that group, and not to a
// read of another.
func TestAGroupsOwnKeyIsItsOwnScope(t *testing.T) {
	settings := viewed()
	settings.KeysPerGroup = true
	logic(t, settings, func(t *testing.T, h *harness) {
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Owner: "alice", Group: "acme"}, 2)
		h.reading(w, postgres.Submission{Parse: "prs_b", Owner: "bob", Group: "beta"}, 1)
		var limited tasks.Settle
		for _, c := range w.claim(3, 3) {
			if c.Group == "acme" && limited.Task == "" {
				if c.Scope != "acme" {
					t.Fatalf("a page of acme was claimed in the scope %q", c.Scope)
				}
				limited = ended(c, tasks.Wait, "")
				limited.RetryAfter = time.Minute
			}
		}
		w.settle(limited)

		if got := pools(h.queue()); got != `large 0/8 closed; small 2/4 closed ("acme" 1/2 paused)` {
			t.Fatalf("the pools of every group are %q", got)
		}
		if got := pools(h.queue("acme")); got != `large 0/8 closed; small 1/4 closed ("acme" 1/2 paused)` {
			t.Fatalf("the pools as acme sees them are %q", got)
		}
		if got := pools(h.queue("beta")); got != "large 0/8 closed; small 1/4 closed" {
			t.Fatalf("the pools as beta sees them are %q", got)
		}
	})
}

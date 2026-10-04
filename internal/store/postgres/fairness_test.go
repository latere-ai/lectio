// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// The criteria of specs/006-fairness-and-priority.md. A row of its table
// that names the dispatch simulation is a case here that runs the simulation
// of simulation_test.go; a row that names a store test or a concurrency test
// runs through the store in every mode.

// simulationSize names the variable that runs the simulations at the size
// the spec states: LECTIO_SIMULATION=full. Without it each runs at a size
// that proves the same property in a tenth of the dispatches, since none of
// the properties depends on the count: a late arrival is held to its share
// after 10,000 dispatches as it is after 100,000.
const simulationSize = "LECTIO_SIMULATION"

// sized is a count of a simulation: whole when the simulations run at the
// spec's size, and small on every other run.
func sized(small, whole int) int {
	if os.Getenv(simulationSize) == "full" {
		return whole
	}
	return small
}

// backlog is more pages than any case dispatches from one parse, so a group
// or a project that holds one always has a task that can run.
//
// The simulation cases run beside each other: each has a database of its own
// and a clock of its own.
var backlog = sized(5000, 20000)

// TestShareFollowsWeight: 3 groups with weights 1, 2 and 4, all backlogged.
// After 700 dispatches each has been served within one task of 100, 200 and
// 400 units.
func TestShareFollowsWeight(t *testing.T) {
	t.Parallel()
	h := direct(t, defaults())
	s := h.simulate()
	for group, weight := range map[string]int{"a": 1, "b": 2, "c": 4} {
		s.queue(postgres.Submission{Group: group, Weight: weight}, 1, backlog)
	}
	got := units(s.run(700), byGroup)
	within(t, "the group of weight 1", got["a"], 100, 1)
	within(t, "the group of weight 2", got["b"], 200, 1)
	within(t, "the group of weight 4", got["c"], 400, 1)
}

// TestALateGroupStartsAtTheClock: a group that joins after 3 others were
// served 10,000 pages each gets its share from then on and no burst for the
// past: over the next 3,000 dispatches the equal groups receive the same
// number each within one task. It starts at the clock, so it is served one
// task before another group is. A group whose id sorts first also wins the
// ties at the clock, and is at most 2 tasks ahead of its share at any point,
// which is the bound of the queue itself and not a debt for its absence.
func TestALateGroupStartsAtTheClock(t *testing.T) {
	t.Parallel()
	h := direct(t, defaults())
	s := h.simulate()
	for _, group := range []string{"a", "b", "c"} {
		s.queue(postgres.Submission{Group: group}, 1, backlog)
	}
	// 3 prepare tasks and as many pages of each: 10,000 at the spec's size.
	each := sized(1001, 10001)
	before := served(s.run(3*each), byGroup)
	for _, group := range []string{"a", "b", "c"} {
		within(t, "before the late groups, group "+group, before[group], each, 1)
	}

	// A group whose id sorts after the others joins.
	s.queue(postgres.Submission{Group: "late"}, 1, backlog)
	after := s.run(3000)
	if after[0].Group != "late" {
		t.Errorf("the late group was not the next served: %v", after[:4])
	}
	if run := streak(after, byGroup, "late"); run != 1 {
		t.Errorf("the late group was served %d tasks in a row, want 1", run)
	}
	for group, n := range served(after, byGroup) {
		within(t, "after the late group joined, group "+group, n, 750, 1)
	}

	// A group whose id sorts before the others joins.
	s.queue(postgres.Submission{Group: "0-late"}, 1, backlog)
	after = s.run(3000)
	if most := ahead(after, byGroup, "0-late", 5); after[0].Group != "0-late" || most > 2 {
		t.Errorf("the late group whose id wins every tie was %d tasks ahead of its share, want at most 2: %v", most, after[:6])
	}
	for group, n := range served(after, byGroup) {
		within(t, "after the second late group joined, group "+group, n, 600, 1)
	}
}

// TestBatchWorkAfterInteractiveOnly: batch work that arrives after 100,000
// interactive-only dispatches receives at most one dispatch in a row, and
// then between 19% and 21% of dispatches. With no interactive work, batch
// receives all. The 100,000 are made at the spec's size, and 10,000 on every
// other run: what batch work is held to does not grow with how long it was
// away.
func TestBatchWorkAfterInteractiveOnly(t *testing.T) {
	t.Parallel()
	h := direct(t, defaults())
	s := h.simulate()
	alone := sized(10000, 100000)
	s.queue(postgres.Submission{Group: "a", Class: tasks.Interactive}, 1, alone+4000)
	if got := served(s.run(alone), byClass); got["interactive"] != alone {
		t.Fatalf("with interactive work alone the dispatches were %v", got)
	}

	s.queue(postgres.Submission{Group: "a", Class: tasks.Batch}, 1, backlog)
	after := s.run(4000)
	if run := streak(after, byClass, "batch"); run != 1 {
		t.Fatalf("batch work that arrived late was served %d tasks in a row, want 1", run)
	}
	batch := served(after, byClass)["batch"]
	if batch < 4000*19/100 || batch > 4000*21/100 {
		t.Fatalf("batch work received %d of 4000 dispatches, want between 19%% and 21%%", batch)
	}

	// The interactive parse runs out: every dispatch after its last is batch
	// work.
	rest := s.run(3000)
	last := -1
	for i, d := range rest {
		if d.Class == tasks.Interactive {
			last = i
		}
	}
	if last < 0 || rest[last].Kind != tasks.Assemble || len(rest)-last < 1000 {
		t.Fatalf("the interactive work ended at dispatch %d of %d", last, len(rest))
	}
	if got := served(rest[last+1:], byClass); got["batch"] != len(rest)-last-1 {
		t.Fatalf("with no interactive work left the dispatches were %v", got)
	}
}

// TestAPausedGroupReturnsWithItsShare: a group whose reader was paused while
// 2 others were served 10,000 pages receives its weight's share from the
// first dispatch after the pause and no more.
func TestAPausedGroupReturnsWithItsShare(t *testing.T) {
	t.Parallel()
	settings := defaults()
	settings.KeysPerGroup = true
	h := direct(t, settings)
	s := h.simulate()
	for _, group := range []string{"a", "b", "paused"} {
		s.queue(postgres.Submission{Group: group}, 1, backlog)
	}
	// The stub pool: the endpoint limited the group's key for an hour.
	h.exec(`INSERT INTO pool_scopes (reader, scope, ceiling, paused_until, halved_at, raised_at) VALUES ($1, 'paused', 1000, $2, $3, $3)`,
		stub, h.now.Add(time.Hour), h.now)

	// Its prepare calls no reader and runs; its pages wait.
	each := sized(1001, 5001)
	during := served(s.run(2*each+1), byGroup)
	if during["paused"] != 1 || during["a"] != each || during["b"] != each {
		t.Fatalf("while the group's reader was paused the dispatches were %v", during)
	}

	h.advance(time.Hour + tasks.DefaultPoolResume)
	after := s.run(3000)
	if run := streak(after, byGroup, "paused"); run != 1 {
		t.Fatalf("the group was served %d tasks in a row after its pause, want 1", run)
	}
	for group, n := range served(after, byGroup) {
		within(t, "after the pause, group "+group, n, 1000, 1)
	}
}

// TestPrepareAndExtractionAreCharged: a group that queues 10,000 prepare
// tasks, or 10,000 extractions, receives its weight's share of dispatches
// and changes no other group's share. No task is free: a group whose tasks
// cost nothing would never advance and would be chosen every time.
func TestPrepareAndExtractionAreCharged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		queue func(s *simulation)
	}{
		{"without the group", func(*simulation) {}},
		{"10,000 prepare tasks", func(s *simulation) {
			s.queue(postgres.Submission{Group: "c"}, 10000, 0)
		}},
		{"10,000 extractions", func(s *simulation) {
			ids := s.queue(postgres.Submission{Group: "c"}, 1, 0)
			s.h.exec(`SELECT lectio_enqueue($1, 'extract-' || n, 'extract', NULL, $2) FROM generate_series(1, 10000) n`, ids[0], s.h.now)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := direct(t, defaults())
			s := h.simulate()
			s.queue(postgres.Submission{Group: "a", Weight: 1}, 1, backlog)
			s.queue(postgres.Submission{Group: "b", Weight: 2}, 1, backlog)
			tc.queue(s)
			got := served(s.run(3000), byGroup)
			if got["c"] == 0 {
				// The others' shares, with the group absent.
				within(t, "group a alone with b", got["a"], 1000, 1)
				within(t, "group b alone with a", got["b"], 2000, 1)
				return
			}
			within(t, "the group that queued them", got["c"], 750, 1)
			within(t, "group a beside it", got["a"], 750, 1)
			within(t, "group b beside it", got["b"], 1500, 1)
		})
	}
}

// TestPriorityAndVolumeMoveNoOtherGroup: a group raising its own priorities,
// or queuing 100,000 tasks, changes no other group's dispatch count. The
// group is chosen before any of its tasks is looked at.
func TestPriorityAndVolumeMoveNoOtherGroup(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, change postgres.Submission, pages int) map[string]int {
		h := direct(t, defaults())
		s := h.simulate()
		s.queue(postgres.Submission{Group: "a"}, 2, backlog)
		s.queue(postgres.Submission{Group: "b", Weight: 3}, 2, backlog)
		change.Group = "c"
		s.queue(change, 2, pages)
		return served(s.run(3000), byGroup)
	}
	base := run(t, postgres.Submission{}, 5000)
	for name, got := range map[string]map[string]int{
		"raising its priorities": run(t, postgres.Submission{Priority: 100, MaxPriority: 100}, 5000),
		// 2 parses: 100,000 tasks at the spec's size.
		"queuing many more tasks": run(t, postgres.Submission{}, sized(10000, 50000)),
	} {
		if got["a"] != base["a"] || got["b"] != base["b"] || got["c"] != base["c"] {
			t.Errorf("%s changed the dispatch counts from %v to %v", name, base, got)
		}
	}
	within(t, "the group of weight 3", base["b"], 1800, 1)
}

// TestBlankPagesEarnNoExtraShare: a group whose 3,000 pages are all blank
// receives its weight's share of dispatches against a group whose pages are
// read, and no more. A blank page is corrected to the floor of 1 and never
// to nothing.
func TestBlankPagesEarnNoExtraShare(t *testing.T) {
	t.Parallel()
	h := direct(t, defaults())
	s := h.simulate()
	s.queue(postgres.Submission{Group: "blank"}, 1, 3000)
	s.queue(postgres.Submission{Group: "read"}, 1, 3000)
	s.blank = []string{"blank"}
	got := served(s.run(4000), byGroup)
	within(t, "the group whose pages are blank", got["blank"], 2000, 1)
	within(t, "the group whose pages are read", got["read"], 2000, 1)

	// Under a reader that costs 5, a blank page costs 1 and a page that is
	// read 5: the two groups are served the same units, which is 5 blank
	// pages to each page read.
	costly := defaults()
	costly.Pools[0].Cost = 5
	h = direct(t, costly)
	s = h.simulate()
	s.queue(postgres.Submission{Group: "blank"}, 1, 3000)
	s.queue(postgres.Submission{Group: "read"}, 1, 3000)
	s.blank = []string{"blank"}
	got = served(s.run(1202), byGroup)
	within(t, "under a reader of cost 5, the group whose pages are blank", got["blank"], 1001, 5)
	within(t, "under a reader of cost 5, the group whose pages are read", got["read"], 201, 1)
}

// TestProjectShareFollowsWeight: one group with projects weighted 1, 2 and
// 4, all backlogged. After 700 dispatches of the group each project has been
// served within one task of 100, 200 and 400 units.
func TestProjectShareFollowsWeight(t *testing.T) {
	t.Parallel()
	h := direct(t, defaults())
	s := h.simulate()
	for project, weight := range map[string]int{"x": 1, "y": 2, "z": 4} {
		s.queue(postgres.Submission{Group: "a", Project: project, ProjectWeight: weight}, 1, backlog)
	}
	got := units(s.run(700), byProject)
	within(t, "the project of weight 1", got["x"], 100, 1)
	within(t, "the project of weight 2", got["y"], 200, 1)
	within(t, "the project of weight 4", got["z"], 400, 1)
}

// TestProjectsDivideOnlyTheirGroup: 2 groups of equal weight, all
// backlogged, one of them with 3 projects. Each group receives half of the
// dispatches within one task, and the count is the same when that group's
// work is queued in one project.
func TestProjectsDivideOnlyTheirGroup(t *testing.T) {
	t.Parallel()
	run := func(projects map[string]int) map[string]int {
		h := direct(t, defaults())
		s := h.simulate()
		s.queue(postgres.Submission{Group: "a"}, 3, backlog)
		for project, weight := range projects {
			s.queue(postgres.Submission{Group: "b", Project: project, ProjectWeight: weight}, 1, backlog)
		}
		return served(s.run(2000), byGroup)
	}
	divided := run(map[string]int{"x": 1, "y": 5, "z": 1000})
	single := run(map[string]int{"": 1})
	within(t, "the group with 3 projects", divided["b"], 1000, 1)
	within(t, "the group beside it", divided["a"], 1000, 1)
	if divided["a"] != single["a"] || divided["b"] != single["b"] {
		t.Fatalf("dividing a group's work among projects changed the counts from %v to %v", single, divided)
	}
}

// TestAnIdleProjectsShareStaysInItsGroup: the dispatches of a group go to
// its projects that have work, in the ratio of their weights, and no other
// group's dispatch count changes whether or not the idle project has work.
func TestAnIdleProjectsShareStaysInItsGroup(t *testing.T) {
	t.Parallel()
	run := func(idle bool) (groups, projects map[string]int) {
		h := direct(t, defaults())
		s := h.simulate()
		s.queue(postgres.Submission{Group: "other", Weight: 2}, 1, backlog)
		s.queue(postgres.Submission{Group: "a", Project: "x", ProjectWeight: 1}, 1, backlog)
		s.queue(postgres.Submission{Group: "a", Project: "y", ProjectWeight: 2}, 1, backlog)
		if !idle {
			s.queue(postgres.Submission{Group: "a", Project: "z", ProjectWeight: 4}, 1, backlog)
		}
		ds := s.run(2100)
		var own []dispatch
		for _, d := range ds {
			if d.Group == "a" {
				own = append(own, d)
			}
		}
		return served(ds, byGroup), served(own, byProject)
	}
	busyGroups, busy := run(false)
	idleGroups, idle := run(true)
	// The group is served 700 of 2100 either way.
	if busyGroups["other"] != idleGroups["other"] || busyGroups["a"] != idleGroups["a"] {
		t.Fatalf("a project with nothing to run changed the groups' counts from %v to %v", busyGroups, idleGroups)
	}
	within(t, "the group", idleGroups["a"], 700, 1)
	within(t, "with all 3 at work, the project of weight 1", busy["x"], 100, 1)
	within(t, "with all 3 at work, the project of weight 4", busy["z"], 400, 1)
	// With z idle its share goes to x and y, 1 to 2, and to no other group.
	if idle["z"] != 0 {
		t.Fatalf("the idle project was served %d", idle["z"])
	}
	within(t, "with z idle, the project of weight 1", idle["x"], 233, 1)
	within(t, "with z idle, the project of weight 2", idle["y"], 467, 1)
}

// TestALateProjectStartsAtItsGroupsClock: a project that joins after its
// group's other projects were served 10,000 pages receives its weight's
// share of the group's dispatches from then on and no burst for the past. It
// starts at its group's clock, so it is served one task before another
// project of the group is, and one whose id wins every tie is at most 2
// tasks ahead of its share at any point.
func TestALateProjectStartsAtItsGroupsClock(t *testing.T) {
	t.Parallel()
	h := direct(t, defaults())
	s := h.simulate()
	s.queue(postgres.Submission{Group: "a", Project: "m"}, 1, backlog)
	s.queue(postgres.Submission{Group: "a", Project: "n"}, 1, backlog)
	each := sized(1001, 5001)
	if got := served(s.run(2*each), byProject); got["m"] != each || got["n"] != each {
		t.Fatalf("before the late projects the dispatches were %v", got)
	}
	// A project whose id sorts after the others joins.
	s.queue(postgres.Submission{Group: "a", Project: "z-late"}, 1, backlog)
	after := s.run(3000)
	if run := streak(after, byProject, "z-late"); run != 1 || after[0].Project != "z-late" {
		t.Errorf("the late project was served %d tasks in a row, want 1 at its arrival: %v", run, after[:4])
	}
	for project, n := range served(after, byProject) {
		within(t, "after the late project joined, project "+project, n, 1000, 1)
	}

	// A project whose id sorts before the others joins.
	s.queue(postgres.Submission{Group: "a", Project: "a-late"}, 1, backlog)
	after = s.run(3000)
	if most := ahead(after, byProject, "a-late", 4); after[0].Project != "a-late" || most > 2 {
		t.Errorf("the late project whose id wins every tie was %d tasks ahead of its share, want at most 2: %v", most, after[:6])
	}
	for project, n := range served(after, byProject) {
		within(t, "after the second late project joined, project "+project, n, 750, 1)
	}
}

// TestAProjectMovesNoOtherGroup: a project raising its weight from 1 to
// 1000, raising its priorities, or queuing 100,000 tasks changes no other
// group's dispatch count. A project divides its group's share and nothing
// else.
func TestAProjectMovesNoOtherGroup(t *testing.T) {
	t.Parallel()
	run := func(change postgres.Submission, pages int) map[string]int {
		h := direct(t, defaults())
		s := h.simulate()
		s.queue(postgres.Submission{Group: "other", Weight: 2}, 1, backlog)
		s.queue(postgres.Submission{Group: "a", Project: "y"}, 1, backlog)
		change.Group, change.Project = "a", "x"
		s.queue(change, 2, pages)
		return served(s.run(3000), byGroup)
	}
	base := run(postgres.Submission{}, 5000)
	within(t, "the other group", base["other"], 2000, 1)
	for name, got := range map[string]map[string]int{
		"raising its weight to 1000": run(postgres.Submission{ProjectWeight: 1000}, 5000),
		"raising its priorities":     run(postgres.Submission{Priority: 100, MaxPriority: 100}, 5000),
		// 2 parses: 100,000 tasks at the spec's size.
		"queuing many more tasks": run(postgres.Submission{}, sized(10000, 50000)),
	} {
		if got["other"] != base["other"] || got["a"] != base["a"] {
			t.Errorf("a project %s changed the groups' counts from %v to %v", name, base, got)
		}
	}
}

// TestTheChargeFollowsCostAndUse: a page read by a reader of cost 5 advances
// its group's virtual time 5 times as far as one of cost 1. A blank page
// under that reader is corrected to 1, and a page that moved to a second
// reader to the sum of both readers' costs, on the project, the group and
// the class, each by its own weight.
func TestTheChargeFollowsCostAndUse(t *testing.T) {
	settings := readers(tasks.Pool{Reader: "small", MaxInFlight: 100}, tasks.Pool{Reader: "large", MaxInFlight: 100, Cost: 5})
	logic(t, settings, func(t *testing.T, h *harness) {
		w := h.worker()
		// The pages pinned to the small reader go first, by priority.
		h.readingAll(w, 4,
			postgres.Submission{Parse: "prs_small", Group: "acme", Project: "x", Weight: 2, ProjectWeight: 5, Pin: "small"},
			postgres.Submission{Parse: "prs_large", Group: "acme", Project: "x", Weight: 2, ProjectWeight: 5, Pin: "large", Priority: -1})
		vtime := func() (group, project, class int64) {
			return value[int64](h, `SELECT vtime FROM group_service WHERE group_id = 'acme'`),
				value[int64](h, `SELECT vtime FROM project_service WHERE group_id = 'acme'`),
				value[int64](h, `SELECT vtime FROM class_service WHERE class = 0`)
		}
		// moved claims one task, settles it with the units a case gives, and
		// answers how far the 3 virtual times moved, claim and settle
		// together. The charge is made at the claim, when the work is
		// chosen: the reader's cost, or 1 for a task that calls no model.
		moved := func(reader string, cost, units int) (group, project, class int64) {
			g0, p0, c0 := vtime()
			c := w.claim(1, 1)[0]
			if c.Reader != reader {
				t.Fatalf("the claim took a slot of %s, want %s", c.Reader, reader)
			}
			if g, _, _ := vtime(); g-g0 != int64(cost)*500000 || h.task(c.Parse, c.Task).Charged != cost {
				t.Fatalf("the claim of a task of cost %d moved the group's virtual time by %d and recorded %d units",
					cost, g-g0, h.task(c.Parse, c.Task).Charged)
			}
			settle := done(c)
			settle.Units = units
			w.settle(settle)
			g1, p1, c1 := vtime()
			return g1 - g0, p1 - p0, c1 - c0
		}

		// Weights: group 2, project 5, class 4. A unit is 1,000,000.
		if g, p, c := moved("small", 1, 1); g != 500000 || p != 200000 || c != 250000 {
			t.Fatalf("a page of cost 1 moved the virtual times by %d, %d, %d", g, p, c)
		}
		// A page that was read by the small reader and then by the large one
		// used both: 1 + 5.
		if g, p, c := moved("small", 1, 6); g != 3000000 || p != 1200000 || c != 1500000 {
			t.Fatalf("a page that used 6 units moved the virtual times by %d, %d, %d", g, p, c)
		}
		// The parse's other pages and its assemble, which costs 1 like any
		// task that calls no model.
		w.settle(done(w.claim(1, 1)[0]), done(w.claim(1, 1)[0]))
		if g, p, c := moved("", 1, 1); g != 500000 || p != 200000 || c != 250000 {
			t.Fatalf("an assemble task moved the virtual times by %d, %d, %d", g, p, c)
		}

		if g, p, c := moved("large", 5, 5); g != 2500000 || p != 1000000 || c != 1250000 {
			t.Fatalf("a page of cost 5 moved the virtual times by %d, %d, %d", g, p, c)
		}
		// A blank page under the reader of cost 5 used nothing, and is
		// corrected to the floor of 1.
		if g, p, c := moved("large", 5, 0); g != 500000 || p != 200000 || c != 250000 {
			t.Fatalf("a blank page under a reader of cost 5 moved the virtual times by %d, %d, %d", g, p, c)
		}
		if got := h.task("prs_large", "page-2"); got.Charged != 1 {
			t.Fatalf("the blank page's task records %d units, want 1", got.Charged)
		}
	})
}

// TestAGroupAtMaxRunningIsSkipped: a group that holds max_running leased
// tasks is skipped and others proceed, and it resumes when a task of its own
// settles.
func TestAGroupAtMaxRunningIsSkipped(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_capped", Group: "capped", MaxRunning: 2}, 10)
		h.reading(w, postgres.Submission{Parse: "prs_open", Group: "open"}, 10)

		claims := w.claim(6, 6)
		got := map[string]int{}
		var capped tasks.Claim
		for _, c := range claims {
			if got[c.Group]++; c.Group == "capped" {
				capped = c
			}
		}
		if got["capped"] != 2 || got["open"] != 4 {
			t.Fatalf("with one group capped at 2 the claims went to %v", got)
		}
		// At its cap it is not charged for waiting.
		before := value[int64](h, `SELECT vtime FROM group_service WHERE group_id = 'capped'`)
		for _, c := range w.claim(2, 2) {
			if c.Group != "open" {
				t.Fatalf("a group at max_running was handed %+v", c)
			}
		}
		if after := value[int64](h, `SELECT vtime FROM group_service WHERE group_id = 'capped'`); after != before {
			t.Fatalf("a group at max_running was charged: %d to %d", before, after)
		}

		w.settle(done(capped))
		if c := w.claim(1, 1)[0]; c.Group != "capped" {
			t.Fatalf("after a task of the capped group settled the claim went to %+v", c)
		}
		// It is at its cap again, and the other group takes what is free.
		for _, c := range w.claim(4, 4) {
			if c.Group != "open" {
				t.Fatalf("a group back at max_running was handed %+v", c)
			}
		}
	})
}

// TestConcurrentClaims: 16 workers claiming at once from 100 groups. No
// task is claimed twice, and what each group is served stays within 5% of
// its weight's share.
func TestConcurrentClaims(t *testing.T) {
	const (
		groups   = 100
		workers  = 16
		dispatch = 12500
	)
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		s := h.simulate()
		weights, total := map[string]int{}, 0
		for i := range groups {
			group := "g" + string(rune('a'+i/26)) + string(rune('a'+i%26))
			weights[group] = 1 + i%4
			total += weights[group]
			s.queue(postgres.Submission{Group: group, Weight: weights[group]}, 1, 400)
		}

		var (
			mu      sync.Mutex
			claimed = map[tasks.Ref]int{}
			got     = map[string]int{}
			count   atomic.Int64
			wg      sync.WaitGroup
			failed  atomic.Pointer[error]
		)
		for range workers {
			id, err := h.store.Register(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			wg.Go(func() {
				var settles []tasks.Settle
				for count.Load() < dispatch && failed.Load() == nil {
					reply, err := h.store.Exchange(context.Background(), id, tasks.Request{Free: 4, Idle: true, Settles: settles})
					if err != nil {
						failed.Store(&err)
						return
					}
					settles = settles[:0]
					mu.Lock()
					for _, c := range reply.Claims {
						// Every dispatch is counted up to the number the
						// case set out to make, and none past it.
						if count.Add(1) <= dispatch {
							got[c.Group]++
						}
						claimed[tasks.Ref{Parse: c.Parse, Task: c.Task}]++
						settle := done(c)
						if c.Kind == tasks.Prepare {
							settle = prepared(c, s.pages[c.Parse])
						}
						settles = append(settles, settle)
					}
					mu.Unlock()
				}
				if _, err := h.store.Exchange(context.Background(), id, tasks.Request{Settles: settles}); err != nil {
					failed.Store(&err)
				}
			})
		}
		wg.Wait()
		if err := failed.Load(); err != nil {
			t.Fatalf("an exchange failed: %v", *err)
		}
		for ref, n := range claimed {
			if n != 1 {
				t.Fatalf("%v was claimed %d times", ref, n)
			}
		}
		for group, weight := range weights {
			want := dispatch * weight / total
			if slack := max(1, want*5/100); got[group] < want-slack || got[group] > want+slack {
				t.Errorf("group %s of weight %d was served %d, want %d within 5%%", group, weight, got[group], want)
			}
		}
	})
}

// TestConcurrentSubmitsAtMaxQueued: 50 submits of one group at once, with
// the group 10 below max_queued, admit exactly 10. The submits are
// serialized on the group's row, so none counts from a snapshot another has
// already passed.
func TestConcurrentSubmitsAtMaxQueued(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		const bound = 25
		for i := range bound - 10 {
			h.submit(postgres.Submission{Parse: "prs_before_" + string(rune('a'+i)), Group: "acme", MaxQueued: bound})
		}
		var admitted, refused atomic.Int64
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Go(func() {
				sub := filled(postgres.Submission{Parse: "prs_at_once_" + string(rune('A'+i)), Group: "acme", MaxQueued: bound})
				_, created, err := h.store.Submit(context.Background(), sub)
				switch {
				case err == nil && created:
					admitted.Add(1)
				case fault.CodeOf(err) == fault.QueueFull:
					refused.Add(1)
				default:
					t.Errorf("a submit answered created %t, %v", created, err)
				}
			})
		}
		wg.Wait()
		if admitted.Load() != 10 || refused.Load() != 40 {
			t.Fatalf("%d submits were admitted and %d refused, want 10 and 40", admitted.Load(), refused.Load())
		}
		if n := value[int64](h, `SELECT count(*) FROM parses WHERE group_id = 'acme' AND state IN ('queued', 'running')`); n != bound {
			t.Fatalf("the group holds %d parses, want its bound of %d", n, bound)
		}
	})
}

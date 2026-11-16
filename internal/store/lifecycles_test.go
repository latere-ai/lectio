// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"latere.ai/x/pkg/statemachine"
	"latere.ai/x/pkg/statemachine/statemachinetest"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/tasks"
)

// pathTo is the shortest run of steps of the table from one of its initial
// states to s, the initial state first. Every state of a table has one.
func pathTo[S ~string](t *testing.T, table *statemachine.Table[S], s S) []S {
	t.Helper()
	var best []S
	var walk func(path []S, seen map[S]bool)
	walk = func(path []S, seen map[S]bool) {
		at := path[len(path)-1]
		if at == s {
			if best == nil || len(path) < len(best) {
				best = append([]S(nil), path...)
			}
			return
		}
		for _, next := range table.Next(at) {
			if !seen[next] {
				seen[next] = true
				walk(append(path, next), seen)
				delete(seen, next)
			}
		}
	}
	for _, start := range table.Initial() {
		walk([]S{start}, map[S]bool{start: true})
	}
	if best == nil {
		t.Fatalf("%s: no path to %s", table.Name(), s)
	}
	return best
}

// refused maps an answer of the memory store that is a refusal to the error
// the drivers report: a parse that is not there, one removed before it ended,
// or a figure run started over one that is running is the store saying no.
func refused(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, statemachine.ErrIllegalTransition):
		return err
	case fault.CodeOf(err) == fault.NotTerminal, fault.CodeOf(err) == fault.Conflict, fault.CodeOf(err) == fault.ParseNotFound:
		return fmt.Errorf("%w: %w", statemachine.ErrIllegalTransition, err)
	}
	return err
}

// memoryParse is a driver of tasks.ParseTable over the in-memory store. Move
// is UpdateParse, the one writer of a parse's state, and DeleteParse for the
// parse's removal.
func memoryParse(t *testing.T) statemachinetest.Driver[tasks.ParseState] {
	m := NewMemory()
	var n atomic.Int64
	return statemachinetest.Driver[tasks.ParseState]{
		Enter: func(t *testing.T, s tasks.ParseState) string {
			id := fmt.Sprintf("prs_%d", n.Add(1))
			if _, _, err := m.CreateParse(Parse{ID: id, Owner: "owner", State: StateQueued, Stage: StageQueued}, "", ""); err != nil {
				t.Fatalf("creating %s: %v", id, err)
			}
			for _, to := range pathTo(t, tasks.ParseTable, s)[1:] {
				if to == tasks.ParseGone {
					if err := m.DeleteParse("owner", id); err != nil {
						t.Fatalf("removing %s: %v", id, err)
					}
					continue
				}
				if _, err := m.UpdateParse(id, func(p *Parse) { p.State = to }); err != nil {
					t.Fatalf("moving %s to %s: %v", id, to, err)
				}
			}
			return id
		},
		Move: func(id string, to tasks.ParseState) error {
			if to == tasks.ParseGone {
				return refused(m.DeleteParse("owner", id))
			}
			return refused(setState(m, id, to))
		},
		State: func(t *testing.T, id string) tasks.ParseState {
			p, err := m.ParseByID(id)
			if fault.CodeOf(err) == fault.ParseNotFound {
				return tasks.ParseGone
			}
			if err != nil {
				t.Fatalf("reading %s: %v", id, err)
			}
			return p.State
		},
	}
}

// setState is UpdateParse setting a parse's state. A write that leaves the
// state as it was moves nothing, so it is refused as a step the table lacks.
func setState(m *Memory, id string, to tasks.ParseState) error {
	var unchanged bool
	_, err := m.UpdateParse(id, func(p *Parse) {
		unchanged = p.State == to
		p.State = to
	})
	if err == nil && unchanged {
		return fmt.Errorf("parse %s is %s already: %w", id, to, statemachine.ErrIllegalTransition)
	}
	return err
}

func TestParseLifecycle(t *testing.T) {
	d := memoryParse(t)
	statemachinetest.Exhaustive(t, tasks.ParseTable, d)
	statemachinetest.Race(t, tasks.ParseTable, d)
}

// TestStageLifecycle holds the stage an in-memory parse reports to its table.
func TestStageLifecycle(t *testing.T) {
	m := NewMemory()
	var n atomic.Int64
	d := statemachinetest.Driver[tasks.ParseStage]{
		Enter: func(t *testing.T, s tasks.ParseStage) string {
			id := fmt.Sprintf("prs_%d", n.Add(1))
			if _, _, err := m.CreateParse(Parse{ID: id, Owner: "owner", State: StateQueued, Stage: StageQueued}, "", ""); err != nil {
				t.Fatalf("creating %s: %v", id, err)
			}
			for _, to := range pathTo(t, tasks.StageTable, s)[1:] {
				if _, err := m.UpdateParse(id, func(p *Parse) { p.Stage = to }); err != nil {
					t.Fatalf("moving %s to %s: %v", id, to, err)
				}
			}
			return id
		},
		Move: func(id string, to tasks.ParseStage) error {
			var unchanged bool
			_, err := m.UpdateParse(id, func(p *Parse) {
				unchanged = p.Stage == to
				p.Stage = to
			})
			if err == nil && unchanged {
				return fmt.Errorf("parse %s is in stage %s already: %w", id, to, statemachine.ErrIllegalTransition)
			}
			return refused(err)
		},
		State: func(t *testing.T, id string) tasks.ParseStage {
			p, err := m.ParseByID(id)
			if err != nil {
				t.Fatalf("reading %s: %v", id, err)
			}
			return p.Stage
		},
	}
	statemachinetest.Exhaustive(t, tasks.StageTable, d)
	statemachinetest.Race(t, tasks.StageTable, d)
}

// TestRunLifecycle holds the figure run of the in-memory store to its table:
// StartFigureRun writes a run, and UpdateFigureRun ends it.
func TestRunLifecycle(t *testing.T) {
	m := NewMemory()
	var n atomic.Int64
	d := statemachinetest.Driver[tasks.RunState]{
		Enter: func(t *testing.T, s tasks.RunState) string {
			id := fmt.Sprintf("prs_%d", n.Add(1))
			path := pathTo(t, tasks.RunTable, s)
			if err := m.StartFigureRun(id, FigureRun{State: path[0]}); err != nil {
				t.Fatalf("starting the run of %s: %v", id, err)
			}
			for _, to := range path[1:] {
				if err := m.StartFigureRun(id, FigureRun{State: to}); err != nil {
					t.Fatalf("moving the run of %s to %s: %v", id, to, err)
				}
			}
			return id
		},
		Move: func(id string, to tasks.RunState) error {
			run, ok := m.FigureRun(id)
			if ok && run.State == RunRunning {
				// A run that is running is ended by its own writer. A change
				// that leaves the state where it was moves nothing.
				var unchanged bool
				err := m.UpdateFigureRun(id, func(r *FigureRun) {
					unchanged = r.State == to
					r.State = to
				})
				if err == nil && unchanged {
					return fmt.Errorf("the run of %s is %s already: %w", id, to, statemachine.ErrIllegalTransition)
				}
				return refused(err)
			}
			return refused(m.StartFigureRun(id, FigureRun{State: to}))
		},
		State: func(t *testing.T, id string) tasks.RunState {
			run, ok := m.FigureRun(id)
			if !ok {
				t.Fatalf("the run of %s is gone", id)
			}
			return run.State
		},
	}
	statemachinetest.Exhaustive(t, tasks.RunTable, d)
	statemachinetest.Race(t, tasks.RunTable, d)
}

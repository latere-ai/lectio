// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"latere.ai/x/pkg/statemachine"
	"latere.ai/x/pkg/statemachine/statemachinetest"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// The lifecycles of the work on a parse that has ended and of the rows
// around it: an extraction, a figure and the run that describes figures, a
// file, and a reader's breaker. Each is driven through the writers the
// store exposes, on a database of its own for every record, so the claims
// and the clock one record needs never reach another.
//
// Two shapes keep a driver honest. A writer that answers whether it moved
// the record, a settle that is refused or a fault that says conflict, is
// mapped to statemachine.ErrIllegalTransition from that answer. A writer
// that answers nothing, a sweep or a delete of a row that may not be
// there, is run with the record's other serialized writers held off, and
// is refused when the state was already the target or did not become it.
// Every writer takes the one lock the exchange takes, so the database
// orders them whatever the driver does; the mutex only lets the driver
// tell which writer moved the record.

// lcBLate is how long after the epoch the sweeps of a record run: past the
// hour every deadline and retention of these cases is set to.
const lcBLate = 2 * time.Hour

// lcBRec is one record of a driver: its database, the claims its writers
// settle under, and the clock its sweeps run at.
type lcBRec struct {
	h       *harness
	w       *worker
	sweeper *worker
	claims  []tasks.Claim
	parse   string
	file    postgres.File
	mu      sync.Mutex
}

// lcBWorld keeps the records of one driver by key.
type lcBWorld struct {
	srv      servers
	settings tasks.Settings
	mu       sync.Mutex
	recs     map[string]*lcBRec
}

func newLcBWorld(srv servers, settings tasks.Settings) *lcBWorld {
	return &lcBWorld{srv: srv, settings: settings, recs: map[string]*lcBRec{}}
}

// begin opens a database for a new record.
func (w *lcBWorld) begin(t *testing.T) *lcBRec {
	t.Helper()
	h := open(t, w.srv, modes[0], w.settings)
	return &lcBRec{h: h, w: h.worker(), sweeper: h.worker(), parse: "prs_a"}
}

// keep stores the record and returns its key.
func (w *lcBWorld) keep(r *lcBRec) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := fmt.Sprintf("rec-%d", len(w.recs)+1)
	w.recs[key] = r
	return key
}

func (w *lcBWorld) rec(key string) *lcBRec {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.recs[key]
}

// lcBIllegal is the refusal of a step the lifecycle does not have.
func lcBIllegal(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), statemachine.ErrIllegalTransition)
}

// lcBSettle sends settles from the worker that holds the claims, naming the
// claims it does not settle as still held so the exchange does not return
// them to the queue. A refused settle is a refused step.
func (r *lcBRec) settle(settles ...tasks.Settle) error {
	settled := map[string]bool{}
	for _, s := range settles {
		settled[s.Task] = true
	}
	req := tasks.Request{Kinds: tasks.Kinds, Settles: settles}
	for _, c := range r.claims {
		if !settled[c.Task] {
			req.Held = append(req.Held, tasks.Held{Parse: c.Parse, Task: c.Task, Token: c.Token})
		}
	}
	reply, err := r.h.store.Exchange(context.Background(), r.w.id, req)
	switch {
	case err != nil:
		return err
	case reply.Gone:
		return fmt.Errorf("the fleet gave %s up", r.w.id)
	case len(reply.Refused) > 0:
		return lcBIllegal("the settle of %v was refused", reply.Refused)
	}
	return nil
}

// sweep moves the clock past every deadline of the record and has the
// sweeper make an exchange, which runs the sweeps that are due. The clock
// is set to one fixed instant, so racing sweeps agree on it.
func (r *lcBRec) sweep() error {
	r.h.store.SetClock(epoch.Add(lcBLate))
	reply, err := r.sweeper.send(tasks.Request{Idle: true})
	if err != nil {
		return err
	}
	if reply.Gone {
		return fmt.Errorf("the fleet gave %s up", r.sweeper.id)
	}
	return nil
}

// lcBChanged runs write with the record's other serialized writers held
// off and answers nil only when the record was not in to before and is in
// it after. It is for a writer that answers nothing about whether it moved
// the record.
func lcBChanged[S ~string](r *lcBRec, state func() (S, error), to S, write func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	before, err := state()
	if err != nil {
		return err
	}
	if err := write(); err != nil {
		return err
	}
	after, err := state()
	if err != nil {
		return err
	}
	if before == to || after != to {
		return lcBIllegal("the write left the record in %s, which was %s", after, before)
	}
	return nil
}

// lcBConflict maps the fault a writer answers for a record that is not in a
// state it may leave to a refused step.
func lcBConflict(err error) error {
	if err != nil && fault.CodeOf(err) == fault.Conflict {
		return lcBIllegal("%v", err)
	}
	return err
}

// lcBRefs are the figures of the runs of these cases.
var lcBRefs = []string{"1.1", "1.2"}

// lcBStart starts a run over the refs, with an hour to end in.
func (r *lcBRec) start(redo bool, refs ...string) error {
	run := postgres.FigureStart{Parse: r.parse, Redo: redo, Deadline: time.Hour}
	for _, ref := range refs {
		run.Figures = append(run.Figures, postgres.FigureAsk{
			Ref: ref, Page: 1, PageKey: "parses/" + r.parse + "/pages/1.1.json", FigureKey: "what-" + ref + "-shows",
		})
	}
	return lcBConflict(r.h.store.StartFigures(context.Background(), run))
}

// lcBEnded is the parse of a record run to its end: one page read.
func lcBEnded(h *harness, w *worker) {
	h.readThrough(w, postgres.Submission{Parse: "prs_a", Group: "acme", Owner: "alice"}, 1)
}

// --- Field ---

func lcBFieldState(r *lcBRec) func() (document.FieldState, error) {
	return func() (document.FieldState, error) {
		f, ok, err := r.h.store.Field(context.Background(), r.parse, "f")
		if err != nil {
			return "", fmt.Errorf("reading the extraction: %w", err)
		}
		if !ok {
			return "", errors.New("reading the extraction: it is not there")
		}
		return f.State, nil
	}
}

// lcBFieldDriver drives an extraction. A pending one has its task claimed
// by the record's worker, so a settle can end it under that claim; sweeps
// says the extraction fails by its deadline and not by a settle.
func lcBFieldDriver(w *lcBWorld, sweeps bool) statemachinetest.Driver[document.FieldState] {
	return statemachinetest.Driver[document.FieldState]{
		Enter: func(t *testing.T, s document.FieldState) string {
			r := w.begin(t)
			lcBEnded(r.h, r.w)
			r.h.field(r.parse, "f", "")
			r.claims = r.w.claim(1, 1)
			switch s {
			case document.FieldSucceeded:
				r.w.settle(filledBy(r.claims[0]))
			case document.FieldFailed:
				r.w.settle(ended(r.claims[0], tasks.Permanent, "internal"))
			case document.FieldPending:
			}
			return w.keep(r)
		},
		Move: func(key string, to document.FieldState) error {
			r := w.rec(key)
			switch to {
			case document.FieldSucceeded:
				return r.settle(filledBy(r.claims[0]))
			case document.FieldFailed:
				if !sweeps {
					return r.settle(ended(r.claims[0], tasks.Permanent, "internal"))
				}
				return lcBChanged(r, lcBFieldState(r), to, r.sweep)
			case document.FieldPending:
				// An extraction that has not ended is refused with conflict.
				_, err := r.h.store.AskFieldAgain(context.Background(), postgres.FieldRequest{
					Parse: r.parse, Name: "f", Request: asked, Deadline: time.Hour,
				})
				return lcBConflict(err)
			}
			return lcBIllegal("no writer moves an extraction to %s", to)
		},
		State: func(t *testing.T, key string) document.FieldState {
			t.Helper()
			s, err := lcBFieldState(w.rec(key))()
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
}

func TestFieldLifecycle(t *testing.T) {
	srv := server(t)
	t.Parallel()
	for _, sweeps := range []bool{false, true} {
		t.Run(map[bool]string{false: "settled", true: "deadline"}[sweeps], func(t *testing.T) {
			d := lcBFieldDriver(newLcBWorld(srv, withDescribers()), sweeps)
			statemachinetest.Exhaustive(t, tasks.FieldTable, d)
			statemachinetest.Race(t, tasks.FieldTable, d)
		})
	}
}

// --- Figure ---

func lcBFigureState(r *lcBRec) func() (tasks.FigureState, error) {
	return func() (tasks.FigureState, error) {
		got, started, err := r.h.store.Figures(context.Background(), r.parse)
		if err != nil {
			return "", fmt.Errorf("reading the figure: %w", err)
		}
		if !started || len(got.Figures) != 1 {
			return "", fmt.Errorf("reading the figure: started %t, %d figures", started, len(got.Figures))
		}
		return got.Figures[0].State, nil
	}
}

// lcBFigureDriver drives the one figure of a run. A pending figure has its
// task claimed. A failed figure is one a redo run lost that kept the
// description of the run before, which is the failed figure the start of a
// next run moves to succeeded; a figure that kept none is removed by that
// start and has no state to read.
func lcBFigureDriver(w *lcBWorld, sweeps bool) statemachinetest.Driver[tasks.FigureState] {
	return statemachinetest.Driver[tasks.FigureState]{
		Enter: func(t *testing.T, s tasks.FigureState) string {
			r := w.begin(t)
			lcBEnded(r.h, r.w)
			r.h.figures(r.parse, "", false, "1.1")
			r.claims = r.w.claim(1, 1)
			switch s {
			case tasks.FigureSucceeded:
				r.w.settle(described(r.claims[0]))
			case tasks.FigureFailed:
				r.w.settle(described(r.claims[0]))
				r.h.figures(r.parse, "", true, "1.1")
				r.claims = r.w.claim(1, 1)
				r.w.settle(ended(r.claims[0], tasks.Permanent, "figure_unreadable"))
			case tasks.FigurePending:
			}
			return w.keep(r)
		},
		Move: func(key string, to tasks.FigureState) error {
			r := w.rec(key)
			switch to {
			case tasks.FigureSucceeded:
				err := r.settle(described(r.claims[0]))
				if !errors.Is(err, statemachine.ErrIllegalTransition) {
					return err
				}
				// The next run that takes no figure describes a figure that
				// kept a description. One that kept none is removed by that
				// run, which is not a step to succeeded, so it is not made.
				return lcBChanged(r, lcBFigureState(r), to, func() error {
					got, _, err := r.h.store.Figures(context.Background(), r.parse)
					if err != nil {
						return err
					}
					if len(got.Figures) != 1 || got.Figures[0].Output == "" {
						return lcBIllegal("the figure kept no description")
					}
					return r.start(false)
				})
			case tasks.FigureFailed:
				if !sweeps {
					return r.settle(ended(r.claims[0], tasks.Permanent, "figure_unreadable"))
				}
				return lcBChanged(r, lcBFigureState(r), to, r.sweep)
			case tasks.FigurePending:
				return r.start(false, "1.1")
			}
			return lcBIllegal("no writer moves a figure to %s", to)
		},
		State: func(t *testing.T, key string) tasks.FigureState {
			t.Helper()
			s, err := lcBFigureState(w.rec(key))()
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
}

func TestFigureLifecycle(t *testing.T) {
	srv := server(t)
	t.Parallel()
	for _, sweeps := range []bool{false, true} {
		t.Run(map[bool]string{false: "settled", true: "deadline"}[sweeps], func(t *testing.T) {
			d := lcBFigureDriver(newLcBWorld(srv, withDescribers()), sweeps)
			statemachinetest.Exhaustive(t, tasks.FigureTable, d)
			statemachinetest.Race(t, tasks.FigureTable, d)
		})
	}
}

// --- Run ---

func lcBRunState(r *lcBRec) func() (tasks.RunState, error) {
	return func() (tasks.RunState, error) {
		got, started, err := r.h.store.Figures(context.Background(), r.parse)
		if err != nil {
			return "", fmt.Errorf("reading the run: %w", err)
		}
		if !started {
			return "", errors.New("reading the run: none was started")
		}
		return got.Run.State, nil
	}
}

// lcBRunDriver drives the figure run of a parse. A running run has two
// figures, both claimed, and ends when both settle.
func lcBRunDriver(w *lcBWorld, sweeps bool) statemachinetest.Driver[tasks.RunState] {
	all := func(r *lcBRec, settle func(tasks.Claim) tasks.Settle) []tasks.Settle {
		var out []tasks.Settle
		for _, c := range r.claims {
			out = append(out, settle(c))
		}
		return out
	}
	lost := func(c tasks.Claim) tasks.Settle { return ended(c, tasks.Permanent, "figure_unreadable") }
	return statemachinetest.Driver[tasks.RunState]{
		Enter: func(t *testing.T, s tasks.RunState) string {
			r := w.begin(t)
			lcBEnded(r.h, r.w)
			r.h.figures(r.parse, "", false, lcBRefs...)
			r.claims = r.w.claim(2, 2)
			switch s {
			case tasks.RunSucceeded:
				r.w.settle(all(r, described)...)
			case tasks.RunFailed:
				r.w.settle(all(r, lost)...)
			case tasks.RunRunning:
			}
			return w.keep(r)
		},
		Move: func(key string, to tasks.RunState) error {
			r := w.rec(key)
			switch to {
			case tasks.RunRunning:
				return r.start(false, lcBRefs...)
			case tasks.RunSucceeded:
				err := r.settle(all(r, described)...)
				if !errors.Is(err, statemachine.ErrIllegalTransition) {
					return err
				}
				// A run of no figure succeeds as it is written, and replaces
				// a run that ended.
				return lcBChanged(r, lcBRunState(r), to, func() error { return r.start(false) })
			case tasks.RunFailed:
				if !sweeps {
					return r.settle(all(r, lost)...)
				}
				return lcBChanged(r, lcBRunState(r), to, r.sweep)
			}
			return lcBIllegal("no writer moves a run to %s", to)
		},
		State: func(t *testing.T, key string) tasks.RunState {
			t.Helper()
			s, err := lcBRunState(w.rec(key))()
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
}

func TestRunLifecycle(t *testing.T) {
	srv := server(t)
	t.Parallel()
	for _, sweeps := range []bool{false, true} {
		t.Run(map[bool]string{false: "settled", true: "deadline"}[sweeps], func(t *testing.T) {
			d := lcBRunDriver(newLcBWorld(srv, withDescribers()), sweeps)
			statemachinetest.Exhaustive(t, tasks.RunTable, d)
			statemachinetest.Race(t, tasks.RunTable, d)
		})
	}
}

// --- File ---

func lcBFileState(r *lcBRec) func() (tasks.FileState, error) {
	return func() (tasks.FileState, error) {
		var deleting bool
		err := r.h.admin.QueryRow(context.Background(), `SELECT deleted_at IS NOT NULL FROM files WHERE file_id = $1`, r.file.ID).Scan(&deleting)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return tasks.FileGone, nil
		case err != nil:
			return "", err
		case deleting:
			return tasks.FileDeleting, nil
		}
		return tasks.FileLive, nil
	}
}

// lcBFileDriver drives a file. Its delete begins by its owner's delete, or
// by the retention sweep when sweeps says so, which finds the file past its
// retention.
func lcBFileDriver(w *lcBWorld, sweeps bool) statemachinetest.Driver[tasks.FileState] {
	return statemachinetest.Driver[tasks.FileState]{
		Enter: func(t *testing.T, s tasks.FileState) string {
			r := w.begin(t)
			f := file("alice", "fil_1", "aa")
			f.Retention = time.Hour
			r.file = r.h.stored(f)
			ctx := context.Background()
			if s == tasks.FileDeleting || s == tasks.FileGone {
				if _, err := r.h.store.DeleteFile(ctx, "alice", f.ID); err != nil {
					t.Fatalf("deleting %s: %v", f.ID, err)
				}
			}
			if s == tasks.FileGone {
				if err := r.h.store.ForgetFile(ctx, f.ID); err != nil {
					t.Fatalf("forgetting %s: %v", f.ID, err)
				}
			}
			return w.keep(r)
		},
		Move: func(key string, to tasks.FileState) error {
			r := w.rec(key)
			ctx := context.Background()
			switch to {
			case tasks.FileDeleting:
				if sweeps {
					return lcBChanged(r, lcBFileState(r), to, func() error {
						r.h.store.SetClock(epoch.Add(lcBLate))
						_, err := r.h.store.Expired(ctx, 100)
						return err
					})
				}
				// A file that is gone for every caller is refused as one that
				// is not there.
				_, err := r.h.store.DeleteFile(ctx, "alice", r.file.ID)
				if err != nil && fault.CodeOf(err) == fault.FileNotFound {
					return lcBIllegal("%v", err)
				}
				return err
			case tasks.FileGone:
				return lcBChanged(r, lcBFileState(r), to, func() error { return r.h.store.ForgetFile(ctx, r.file.ID) })
			case tasks.FileLive:
			}
			return lcBIllegal("no writer moves a file to %s", to)
		},
		State: func(t *testing.T, key string) tasks.FileState {
			t.Helper()
			s, err := lcBFileState(w.rec(key))()
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
}

func TestFileLifecycle(t *testing.T) {
	srv := server(t)
	t.Parallel()
	for _, sweeps := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted", true: "swept"}[sweeps], func(t *testing.T) {
			d := lcBFileDriver(newLcBWorld(srv, defaults()), sweeps)
			statemachinetest.Exhaustive(t, tasks.FileTable, d)
			statemachinetest.Race(t, tasks.FileTable, d)
		})
	}
}

// --- Breaker ---

func lcBBreakerState(r *lcBRec) func() (tasks.BreakerState, error) {
	return func() (tasks.BreakerState, error) {
		var open bool
		err := r.h.admin.QueryRow(context.Background(), `SELECT opened_at IS NOT NULL FROM pools WHERE reader = $1`, stub).Scan(&open)
		if err != nil {
			return "", err
		}
		if open {
			return tasks.BreakerOpen, nil
		}
		return tasks.BreakerClosed, nil
	}
}

// lcBBreakerDriver drives the breaker of the one reader. A record holds two
// leased page tasks: the failure of the first opens the breaker, whose bound
// is one failure, and the success of the second closes it. Each settle is
// accepted once, so two writers of one step cannot both move the record.
func lcBBreakerDriver(w *lcBWorld) statemachinetest.Driver[tasks.BreakerState] {
	return statemachinetest.Driver[tasks.BreakerState]{
		Enter: func(t *testing.T, s tasks.BreakerState) string {
			r := w.begin(t)
			r.h.reading(r.w, postgres.Submission{Parse: r.parse, Group: "acme"}, 2)
			r.claims = r.w.claim(2, 2)
			if s == tasks.BreakerOpen {
				r.w.settle(lcBFailure(r.claims[0]))
			}
			return w.keep(r)
		},
		Move: func(key string, to tasks.BreakerState) error {
			r := w.rec(key)
			switch to {
			case tasks.BreakerOpen:
				return r.settle(lcBFailure(r.claims[0]))
			case tasks.BreakerClosed:
				return r.settle(done(r.claims[1]))
			}
			return lcBIllegal("no writer moves a breaker to %s", to)
		},
		State: func(t *testing.T, key string) tasks.BreakerState {
			t.Helper()
			s, err := lcBBreakerState(w.rec(key))()
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
}

// lcBFailure is a settle of a call that failed in a way the next call may
// fail too.
func lcBFailure(c tasks.Claim) tasks.Settle {
	s := ended(c, tasks.Retryable, "reader_unavailable")
	s.Health = tasks.Unhealthy
	return s
}

func TestBreakerLifecycle(t *testing.T) {
	srv := server(t)
	t.Parallel()
	settings := defaults()
	settings.BreakerFailures = 1
	settings.BreakerOpen = time.Hour
	d := lcBBreakerDriver(newLcBWorld(srv, settings))
	statemachinetest.Exhaustive(t, tasks.BreakerTable, d)
	statemachinetest.Race(t, tasks.BreakerTable, d)
}

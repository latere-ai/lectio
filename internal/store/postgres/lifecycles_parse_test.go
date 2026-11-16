// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/statemachine"
	"latere.ai/x/pkg/statemachine/statemachinetest"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// The drivers of tasks.ParseTable and tasks.TaskTable. Every record the
// checks enter lives on a database of its own, so the claim, which picks by
// fairness and not by id, can only ever see that record's tasks. A move calls
// the writer the product calls: the exchange (claim, settle, the sweep of
// dead workers, the sweep of deadlines, a worker's last exchange), Cancel,
// Retry, DeleteParse and ExpireParse. A writer that took nothing, or that was
// refused, answers with an error that wraps statemachine.ErrIllegalTransition,
// as does a target no writer moves a record to.

// lcAIllegal is the refusal of a move the writers do not make.
func lcAIllegal(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, statemachine.ErrIllegalTransition)...)
}

// lcARefusal reports whether err is one of the store's answers that a request
// was refused (a parse that is missing or has ended, a retry with nothing to
// retry), as opposed to a failure of the store.
func lcARefusal(err error) bool {
	var f *fault.Error
	return errors.As(err, &f)
}

// lcAEnv is one record's world: a database, the workers that act on it, and
// the leases they hold.
type lcAEnv struct {
	h     *harness
	owner string
	parse string
	// worker holds the leases Enter took; mover is a second worker the
	// sweeps run by.
	worker, mover string

	// task is the task the record is, and claim the lease that was taken
	// on it. A claim with a stale or zero token is what a writer is tried
	// with for a task that is not leased.
	task  string
	claim tasks.Claim
	// page is the lease on the parse's page task, when one is held.
	page tasks.Claim
	// entered is the state the record was entered in.
	entered string

	// hmu guards held, the leases each worker holds, which every request of
	// the worker names so that the exchange does not return them.
	hmu  sync.Mutex
	held map[string]map[tasks.Ref]tasks.Claim

	// serial serializes the moves whose writer is a sweep: a sweep answers
	// nothing about what it moved, so one at a time is the only way to say
	// which of two writers took the step.
	serial sync.Mutex
}

// lcAEnvs hands out the environments by the key Enter returns.
type lcAEnvs struct {
	srv servers
	set tasks.Settings

	mu  sync.Mutex
	n   atomic.Int64
	env map[string]*lcAEnv
}

func lcANewEnvs(t *testing.T, set tasks.Settings) *lcAEnvs {
	t.Helper()
	return &lcAEnvs{srv: server(t), set: set, env: map[string]*lcAEnv{}}
}

// enter opens a database with a store and two registered workers, and a key
// for them.
func (es *lcAEnvs) enter(t *testing.T) (string, *lcAEnv) {
	t.Helper()
	h := open(t, es.srv, modes[0], es.set)
	e := &lcAEnv{h: h, owner: "owner", parse: "prs_lc", held: map[string]map[tasks.Ref]tasks.Claim{}}
	var err error
	if e.worker, err = h.store.Register(context.Background()); err != nil {
		t.Fatalf("registering a worker: %v", err)
	}
	if e.mover, err = h.store.Register(context.Background()); err != nil {
		t.Fatalf("registering a worker: %v", err)
	}
	key := strconv.FormatInt(es.n.Add(1), 10)
	es.mu.Lock()
	es.env[key] = e
	es.mu.Unlock()
	return key, e
}

func (es *lcAEnvs) of(key string) *lcAEnv {
	es.mu.Lock()
	defer es.mu.Unlock()
	return es.env[key]
}

// send makes an exchange as worker. It names every lease the worker holds
// that the request does not settle, and keeps the worker's leases in step
// with the reply.
func (e *lcAEnv) send(worker string, req tasks.Request, settles ...tasks.Settle) (tasks.Reply, error) {
	if req.Kinds == nil {
		req.Kinds = tasks.Kinds
	}
	req.Settles = settles
	settled := map[tasks.Ref]bool{}
	for _, s := range settles {
		settled[tasks.Ref{Parse: s.Parse, Task: s.Task}] = true
	}
	e.hmu.Lock()
	req.Held = nil
	for ref, c := range e.held[worker] {
		if !settled[ref] {
			req.Held = append(req.Held, tasks.Held{Parse: ref.Parse, Task: ref.Task, Token: c.Token})
		}
	}
	e.hmu.Unlock()
	req.Idle = len(req.Held) == 0
	reply, err := e.h.store.Exchange(context.Background(), worker, req)
	if err != nil {
		return reply, err
	}
	e.hmu.Lock()
	defer e.hmu.Unlock()
	if e.held[worker] == nil {
		e.held[worker] = map[tasks.Ref]tasks.Claim{}
	}
	for ref := range settled {
		delete(e.held[worker], ref)
	}
	for _, ref := range reply.Lost {
		delete(e.held[worker], ref)
	}
	for _, c := range reply.Claims {
		e.held[worker][tasks.Ref{Parse: c.Parse, Task: c.Task}] = c
	}
	return reply, nil
}

// mustSend is send for Enter, where a failure fails the case.
func (e *lcAEnv) mustSend(worker string, req tasks.Request, settles ...tasks.Settle) tasks.Reply {
	e.h.t.Helper()
	reply, err := e.send(worker, req, settles...)
	if err != nil {
		e.h.t.Fatalf("the exchange of %s: %v", worker, err)
	}
	if reply.Gone || len(reply.Refused) != 0 {
		e.h.t.Fatalf("the exchange of %s was refused: %+v", worker, reply)
	}
	return reply
}

// claimed is the claim of kind in a reply, and false when there is none.
func lcAClaimed(reply tasks.Reply, task string) (tasks.Claim, bool) {
	for _, c := range reply.Claims {
		if c.Task == task {
			return c, true
		}
	}
	return tasks.Claim{}, false
}

// submit queues the parse, kept for an hour after it ends so that its
// retention can be over by a delete.
func (e *lcAEnv) submit(allowFailed int) {
	e.h.t.Helper()
	e.h.submit(postgres.Submission{
		Parse: e.parse, Owner: e.owner, Group: "grp", AllowFailedPages: allowFailed, Retention: time.Hour,
	})
}

// start submits the parse, and has the worker claim its prepare task.
func (e *lcAEnv) start(allowFailed int) tasks.Claim {
	e.h.t.Helper()
	e.submit(allowFailed)
	c, ok := lcAClaimed(e.mustSend(e.worker, tasks.Request{Free: 1}), tasks.PrepareID)
	if !ok {
		e.h.t.Fatal("the prepare task was not claimed")
	}
	return c
}

// reading has the worker prepare a parse of one page and claim the page.
func (e *lcAEnv) reading(allowFailed int) {
	e.h.t.Helper()
	prep := e.start(allowFailed)
	e.page = lcAPage(e, e.mustSend(e.worker, tasks.Request{Free: 1}, prepared(prep, 1)))
}

func lcAPage(e *lcAEnv, reply tasks.Reply) tasks.Claim {
	e.h.t.Helper()
	c, ok := lcAClaimed(reply, tasks.PageID(1))
	if !ok {
		e.h.t.Fatal("the page task was not claimed")
	}
	return c
}

// ended runs the parse to its end: the page is read, or fails for good, and
// assemble ends the parse. A parse whose page failed has its task rows and its
// index, so it can be retried.
func (e *lcAEnv) ended(allowFailed int, pageFails bool) {
	e.h.t.Helper()
	e.reading(allowFailed)
	settle := done(e.page)
	if pageFails {
		settle = ended(e.page, tasks.Permanent, "page_unreadable")
	}
	asm, ok := lcAClaimed(e.mustSend(e.worker, tasks.Request{Free: 1}, settle), tasks.AssembleID)
	if !ok {
		e.h.t.Fatal("the assemble task was not claimed")
	}
	e.mustSend(e.worker, tasks.Request{}, done(asm))
	e.claim = asm
}

// parseState reads the parse's state, and ParseGone for a parse with no row.
func (e *lcAEnv) parseState(t testing.TB) tasks.ParseState {
	t.Helper()
	var state string
	err := e.h.admin.QueryRow(context.Background(),
		`SELECT coalesce((SELECT state FROM parses WHERE parse_id = $1), 'gone')`, e.parse).Scan(&state)
	if err != nil {
		t.Fatalf("reading the state of %s: %v", e.parse, err)
	}
	return tasks.ParseState(state)
}

// storedParse is the parse as the store answers it, and false for a parse
// with no row.
func (e *lcAEnv) storedParse() (postgres.Parse, bool, error) {
	p, err := e.h.store.Parse(context.Background(), e.parse)
	switch {
	case lcARefusal(err):
		return postgres.Parse{}, false, nil
	case err != nil:
		return postgres.Parse{}, false, err
	}
	return p, true, nil
}

// jump moves the clock past the parse's deadline and the lease of every
// worker, which the clock of the store and of the harness follow together.
func (e *lcAEnv) jump(d time.Duration) {
	e.hmu.Lock()
	defer e.hmu.Unlock()
	e.h.advance(d)
}

// ParseLifecycle

func TestParseLifecycle(t *testing.T) {
	es := lcANewEnvs(t, defaults())
	d := statemachinetest.Driver[tasks.ParseState]{
		Enter: func(t *testing.T, s tasks.ParseState) string {
			key, e := es.enter(t)
			lcAEnterParse(t, e, s)
			return key
		},
		Move: func(key string, to tasks.ParseState) error { return lcAMoveParse(es.of(key), to) },
		State: func(t *testing.T, key string) tasks.ParseState {
			return es.of(key).parseState(t)
		},
	}
	statemachinetest.Exhaustive(t, tasks.ParseTable, d)
	statemachinetest.Race(t, tasks.ParseTable, d)
}

// lcAEnterParse puts the parse in state s. A parse that ended succeeded has a
// failed page its allow lets it have, and one that ended failed had a failed
// page past its allow: both were ended by assemble, so both have the index a
// retry needs, and succeeded to running and failed to running are the steps
// of a retry. A parse that failed by its deadline, or canceled, has none, and
// the retry of one is refused: the steps to those states are tried from
// queued and running, and the steps from them are the ones a writer makes.
func lcAEnterParse(t *testing.T, e *lcAEnv, s tasks.ParseState) {
	t.Helper()
	e.entered = string(s)
	switch s {
	case tasks.ParseQueued:
		e.submit(0)
	case tasks.ParseRunning:
		e.reading(0)
	case tasks.ParseSucceeded:
		e.ended(1, true)
	case tasks.ParseFailed:
		e.ended(0, true)
	case tasks.ParseCanceled:
		e.submit(0)
		if err := e.h.store.Cancel(context.Background(), e.parse); err != nil {
			t.Fatalf("canceling: %v", err)
		}
	case tasks.ParseGone:
		e.ended(0, false)
		if err := lcADelete(e); err != nil {
			t.Fatalf("deleting: %v", err)
		}
	}
	if got := e.parseState(t); got != s {
		t.Fatalf("entered %s and the parse is %s", s, got)
	}
}

// lcADelete is the owner's delete of a parse that ended, and the removal of
// its rows once its objects are gone.
func lcADelete(e *lcAEnv) error {
	ctx := context.Background()
	if err := e.h.store.DeleteParse(ctx, e.owner, e.parse); err != nil {
		if lcARefusal(err) {
			return lcAIllegal("deleting %s: %v", e.parse, err)
		}
		return err
	}
	removed, err := e.h.store.ExpireParse(ctx, e.parse)
	if err != nil {
		return err
	}
	if !removed {
		return lcAIllegal("the rows of %s were not removed", e.parse)
	}
	return nil
}

// lcAMoveParse runs the writer that takes the parse to to, and holds the
// answer to the state it left the parse in.
func lcAMoveParse(e *lcAEnv, to tasks.ParseState) error {
	ctx := context.Background()
	var err error
	switch to {
	case tasks.ParseQueued:
		// A parse is queued when it is written and never again.
		return lcAIllegal("no writer queues a parse")
	case tasks.ParseRunning:
		err = lcAStartParse(e)
	case tasks.ParseSucceeded:
		err = lcAAssemble(e, e.page)
	case tasks.ParseFailed:
		err = lcAPastDeadline(e, to)
	case tasks.ParseCanceled:
		err = e.h.store.Cancel(ctx, e.parse)
		if lcARefusal(err) {
			err = lcAIllegal("canceling %s: %v", e.parse, err)
		}
	case tasks.ParseGone:
		err = lcADelete(e)
	}
	if err != nil {
		return err
	}
	p, ok, err := e.storedParse()
	switch {
	case err != nil:
		return err
	case !ok && to == tasks.ParseGone, ok && p.State == to:
		return nil
	}
	return lcAIllegal("%s did not become %s", e.parse, to)
}

// lcAStartParse is the claim of a parse's first task, for a parse that is
// queued, and the retry of its failed pages, for one that ended.
func lcAStartParse(e *lcAEnv) error {
	p, ok, err := e.storedParse()
	switch {
	case err != nil:
		return err
	case !ok:
		return lcAIllegal("%s has no row", e.parse)
	case p.State == tasks.ParseQueued:
		worker, err := e.h.store.Register(context.Background())
		if err != nil {
			return err
		}
		reply, err := e.send(worker, tasks.Request{Free: 1})
		if err != nil {
			return err
		}
		if _, ok := lcAClaimed(reply, tasks.PrepareID); !ok {
			return lcAIllegal("no task of %s was claimed", e.parse)
		}
		return nil
	case !p.Terminal():
		return lcAIllegal("%s is %s", e.parse, p.State)
	}
	err = e.h.store.Retry(context.Background(), e.owner, e.parse)
	if lcARefusal(err) {
		return lcAIllegal("retrying %s: %v", e.parse, err)
	}
	return err
}

// lcAAssemble settles the page, claims the assemble task that settle queued,
// and settles it: the writer that ends a parse that was read. page is the
// lease the page was read under, which is stale or zero for a parse that is
// not reading.
func lcAAssemble(e *lcAEnv, page tasks.Claim) error {
	if page.Task == "" {
		page = tasks.Claim{Parse: e.parse, Task: tasks.PageID(1), Kind: tasks.Page}
	}
	reply, err := e.send(e.worker, tasks.Request{}, done(page))
	if err != nil {
		return err
	}
	if reply.Gone || len(reply.Refused) != 0 {
		return lcAIllegal("the settle of %s/%s was refused", page.Parse, page.Task)
	}
	worker, err := e.h.store.Register(context.Background())
	if err != nil {
		return err
	}
	reply, err = e.send(worker, tasks.Request{Free: 1})
	if err != nil {
		return err
	}
	asm, ok := lcAClaimed(reply, tasks.AssembleID)
	if !ok {
		return lcAIllegal("assemble of %s was not claimed", e.parse)
	}
	reply, err = e.send(worker, tasks.Request{}, done(asm))
	if err != nil {
		return err
	}
	if len(reply.Refused) != 0 {
		return lcAIllegal("the settle of assemble of %s was refused", e.parse)
	}
	return nil
}

// lcAPastDeadline moves the clock past the parse's deadline and has a worker
// exchange, which runs the sweep of deadlines. One writer at a time, and the
// parse must not have been in to before: the sweep answers nothing, so which
// of two sweeps ended the parse is told by the state before.
func lcAPastDeadline(e *lcAEnv, to tasks.ParseState) error {
	e.serial.Lock()
	defer e.serial.Unlock()
	p, ok, err := e.storedParse()
	switch {
	case err != nil:
		return err
	case !ok:
		return lcAIllegal("%s has no row", e.parse)
	case p.State == to:
		return lcAIllegal("%s is already %s", e.parse, to)
	}
	e.jump(2 * time.Hour)
	if _, err := e.send(e.mover, tasks.Request{}); err != nil {
		return err
	}
	return nil
}

// TaskLifecycle

// lcATaskMode is which writers the check of the task table uses for the
// steps that have more than one writer.
type lcATaskMode struct {
	name string
	set  tasks.Settings
	// queued and failed name the writer of a leased task's step to queued and
	// to failed: the worker's own settle, or the sweep of dead workers
	// ("reap"), or the worker's last exchange ("shutdown").
	queued, failed string
}

func TestTaskLifecycle(t *testing.T) {
	reaping := defaults()
	reaping.Expiries = 1
	for _, m := range []lcATaskMode{
		{name: "settle", set: defaults(), queued: "settle", failed: "settle"},
		{name: "reap_requeues", set: defaults(), queued: "reap", failed: "settle"},
		{name: "reap_fails", set: reaping, queued: "settle", failed: "reap"},
		{name: "shutdown", set: defaults(), queued: "shutdown", failed: "settle"},
	} {
		t.Run(m.name, func(t *testing.T) {
			es := lcANewEnvs(t, m.set)
			d := statemachinetest.Driver[tasks.State]{
				Enter: func(t *testing.T, s tasks.State) string {
					key, e := es.enter(t)
					lcAEnterTask(t, e, s)
					return key
				},
				Move: func(key string, to tasks.State) error { return lcAMoveTask(es.of(key), m, to) },
				State: func(t *testing.T, key string) tasks.State {
					return es.of(key).taskState(t)
				},
			}
			statemachinetest.Exhaustive(t, tasks.TaskTable, d)
			statemachinetest.Race(t, tasks.TaskTable, d)
		})
	}
}

// taskState reads the state of the record's task.
func (e *lcAEnv) taskState(t testing.TB) tasks.State {
	t.Helper()
	var state string
	err := e.h.admin.QueryRow(context.Background(),
		`SELECT state FROM tasks WHERE parse_id = $1 AND task_id = $2`, e.parse, e.task).Scan(&state)
	if err != nil {
		t.Fatalf("reading the state of %s/%s: %v", e.parse, e.task, err)
	}
	return tasks.State(state)
}

// storedTask is the record's task as the store answers it.
func (e *lcAEnv) storedTask() (postgres.Task, error) {
	rows, err := e.h.store.Tasks(context.Background(), e.parse)
	if err != nil {
		return postgres.Task{}, err
	}
	for _, row := range rows {
		if row.ID == e.task {
			return row, nil
		}
	}
	return postgres.Task{}, fmt.Errorf("%s has no task %s", e.parse, e.task)
}

// lcAEnterTask puts the record's task in state s. Each state is entered on
// the task that can take its steps:
//
//   - queued, leased and canceled: the parse's prepare task.
//   - failed: the page task of a parse whose page failed for good and whose
//     parse ended, which a retry queues again.
//   - succeeded: the assemble task of a parse that ended with a failed page
//     and was retried, which the last page of the retry queues again.
func lcAEnterTask(t *testing.T, e *lcAEnv, s tasks.State) {
	t.Helper()
	e.entered = string(s)
	e.task = tasks.PrepareID
	switch s {
	case tasks.Queued:
		e.submit(0)
		e.claim = tasks.Claim{Parse: e.parse, Task: tasks.PrepareID, Kind: tasks.Prepare}
	case tasks.Leased:
		e.claim = e.start(0)
	case tasks.Canceled:
		e.submit(0)
		e.claim = tasks.Claim{Parse: e.parse, Task: tasks.PrepareID, Kind: tasks.Prepare}
		if err := e.h.store.Cancel(context.Background(), e.parse); err != nil {
			t.Fatalf("canceling: %v", err)
		}
	case tasks.Failed:
		e.ended(0, true)
		e.task = tasks.PageID(1)
		e.claim = e.page
	case tasks.Succeeded:
		e.ended(1, true)
		if err := e.h.store.Retry(context.Background(), e.owner, e.parse); err != nil {
			t.Fatalf("retrying: %v", err)
		}
		e.task = tasks.AssembleID
	}
	if got := e.taskState(t); got != s {
		t.Fatalf("entered %s and the task is %s", s, got)
	}
}

// lcAMoveTask runs the writer that takes the record's task to to, and holds
// the answer to the state it left the task in.
func lcAMoveTask(e *lcAEnv, m lcATaskMode, to tasks.State) error {
	ctx := context.Background()
	var err error
	switch to {
	case tasks.Leased:
		err = lcAClaimTask(e)
	case tasks.Queued:
		err = lcAQueueTask(e, m)
	case tasks.Succeeded:
		err = lcASettleTask(e, done(e.claim))
	case tasks.Failed:
		if m.failed == "reap" && e.entered == string(tasks.Leased) {
			err = lcAReap(e, to)
			break
		}
		err = lcASettleTask(e, ended(e.claim, tasks.Permanent, "page_unreadable"))
	case tasks.Canceled:
		err = e.h.store.Cancel(ctx, e.parse)
		if lcARefusal(err) {
			err = lcAIllegal("canceling %s: %v", e.parse, err)
		}
	}
	if err != nil {
		return err
	}
	row, err := e.storedTask()
	if err != nil {
		return err
	}
	if row.State != to {
		return lcAIllegal("%s/%s did not become %s", e.parse, e.task, to)
	}
	return nil
}

// lcAClaimTask is a claim, by a worker of its own, that must take the
// record's task.
func lcAClaimTask(e *lcAEnv) error {
	worker, err := e.h.store.Register(context.Background())
	if err != nil {
		return err
	}
	reply, err := e.send(worker, tasks.Request{Free: 1})
	if err != nil {
		return err
	}
	if _, ok := lcAClaimed(reply, e.task); !ok {
		return lcAIllegal("%s/%s was not claimed", e.parse, e.task)
	}
	return nil
}

// lcASettleTask is the settle of the lease the record's task was claimed
// under. A refused settle is a task that is no longer leased under it.
func lcASettleTask(e *lcAEnv, s tasks.Settle) error { return lcASettleAs(e, e.worker, s) }

// lcASettleAs is lcASettleTask for the lease another worker holds.
func lcASettleAs(e *lcAEnv, worker string, s tasks.Settle) error {
	reply, err := e.send(worker, tasks.Request{}, s)
	if err != nil {
		return err
	}
	if reply.Gone || len(reply.Refused) != 0 {
		return lcAIllegal("the settle of %s/%s was refused", s.Parse, s.Task)
	}
	return nil
}

// lcAQueueTask is the step to queued: a leased task is given back by its
// worker, found by the sweep of dead workers, or returned by the worker's
// last exchange; a failed page is queued again by a retry; the assemble task
// of a parse that is read again is queued by the page that ends the read.
func lcAQueueTask(e *lcAEnv, m lcATaskMode) error {
	ctx := context.Background()
	switch {
	case e.task == tasks.AssembleID:
		worker, err := e.h.store.Register(ctx)
		if err != nil {
			return err
		}
		// Only pages are asked for: a writer that lost the page must not
		// claim the assemble task the winner's settle queued.
		reply, err := e.send(worker, tasks.Request{Free: 1, Kinds: []tasks.Kind{tasks.Page}})
		if err != nil {
			return err
		}
		page, ok := lcAClaimed(reply, tasks.PageID(1))
		if !ok {
			return lcAIllegal("the page of %s was not claimed", e.parse)
		}
		return lcASettleAs(e, worker, done(page))
	case e.entered == string(tasks.Failed):
		err := e.h.store.Retry(ctx, e.owner, e.parse)
		if lcARefusal(err) {
			return lcAIllegal("retrying %s: %v", e.parse, err)
		}
		return err
	case e.entered == string(tasks.Leased) && m.queued == "reap":
		return lcAReap(e, tasks.Queued)
	case e.entered == string(tasks.Leased) && m.queued == "shutdown":
		return lcAShutdown(e)
	}
	return lcASettleTask(e, ended(e.claim, tasks.Returned, ""))
}

// lcAReap has the worker holding the lease go past its lease, and another
// worker exchange twice: the first renews it, and the second finds the lease
// ran out and returns the task. One at a time, and a task that was not
// leased before is refused: the sweep answers nothing about what it moved.
func lcAReap(e *lcAEnv, to tasks.State) error {
	e.serial.Lock()
	defer e.serial.Unlock()
	before, err := e.storedTask()
	if err != nil {
		return err
	}
	if before.State != tasks.Leased {
		return lcAIllegal("%s/%s is %s, not leased", e.parse, e.task, before.State)
	}
	e.jump(2 * tasks.DefaultLease)
	if _, err := e.send(e.mover, tasks.Request{}); err != nil {
		return err
	}
	e.jump(time.Second)
	if _, err := e.send(e.mover, tasks.Request{}); err != nil {
		return err
	}
	return nil
}

// lcAShutdown is the last exchange of the worker that holds the lease.
func lcAShutdown(e *lcAEnv) error {
	e.serial.Lock()
	defer e.serial.Unlock()
	before, err := e.storedTask()
	if err != nil {
		return err
	}
	if before.State != tasks.Leased {
		return lcAIllegal("%s/%s is %s, not leased", e.parse, e.task, before.State)
	}
	reply, err := e.send(e.worker, tasks.Request{Shutdown: true})
	if err != nil {
		return err
	}
	if reply.Gone {
		return lcAIllegal("the worker was given up")
	}
	return nil
}

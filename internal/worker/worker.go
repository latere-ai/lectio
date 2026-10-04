// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package worker is a worker process of the durable control plane: it
// claims tasks from the task store, runs each through the steps of
// internal/parse and internal/assemble, writes what a task produces to the
// object store, and settles it. The design is specs/004-durable-tasks.md
// and specs/005-parse-graph.md.
//
// A worker talks to the task store through one call, the exchange, and
// makes it at most once per flush interval however many tasks it runs, so
// the statements a fleet issues follow the number of its processes and not
// the pages it reads. Everything a task needs of its parse rides on its
// claim. Everything a task writes goes under a key that carries the claim's
// token, before the settle that names the key, so a worker that was taken
// for dead and wrote late wrote where nothing points.
//
// A worker holds nothing that has to survive it. When it stops it gives
// back what it runs; when it dies the store returns its tasks after its
// lease.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/reader"
)

// Store is the part of the task store a worker uses.
type Store interface {
	// Register records the process and returns its id.
	Register(ctx context.Context) (worker string, err error)
	// Exchange renews the worker's lease, settles, reports and claims.
	Exchange(ctx context.Context, worker string, req tasks.Request) (tasks.Reply, error)
	// Tasks returns the task rows of a parse, which is what assemble reads
	// the pages' keys from.
	Tasks(ctx context.Context, parseID string) ([]postgres.Task, error)
}

// The defaults of a worker's settings, each the value its spec gives.
const (
	DefaultSlots = 8
	DefaultFlush = 200 * time.Millisecond
	DefaultPoll  = time.Second
	DefaultGrace = 25 * time.Second

	// pollBackoff is how far an idle worker's poll interval grows: to 5
	// times the interval it starts at.
	pollBackoff = 5

	// stopWait is how long a stopping worker waits for a task it told to
	// stop before it gives the task back anyway.
	stopWait = 5 * time.Second
)

// Worker runs tasks.
type Worker struct {
	Store   Store
	Objects blob.Store

	// Pipeline holds the steps of a parse.
	Pipeline *parse.Pipeline

	// Readers are the configured readers by name, and Costs what one call
	// to each is charged against a tenant's share. A reader with no cost
	// costs 1.
	Readers map[string]reader.Reader
	Costs   map[string]int

	// Credential returns the key a group's pages are read with. Nil means
	// every call is made without one.
	Credential func(group, owner, parseID string) reader.Credential

	// Slots is how many tasks the process runs at once. Zero takes
	// DefaultSlots.
	Slots int

	// Lease is the length of the worker's lease, as the store is run with.
	// Flush is the shortest time between two exchanges, Poll the time an
	// idle worker waits before it asks again, and Grace how long running
	// tasks get to finish when the worker stops. Zero takes the default.
	Lease time.Duration
	Flush time.Duration
	Poll  time.Duration
	Grace time.Duration

	// CacheBytes bounds the working copies the worker holds between the
	// pages it reads from them. Zero holds one at a time.
	CacheBytes int64

	// Log takes one line per settled task. Nil takes slog.Default.
	Log *slog.Logger

	once  sync.Once
	cache *cache

	// id is the worker's registration and seen the instant of its last
	// exchange the store answered, in nanoseconds: what the readiness probe
	// reads from another goroutine.
	id   atomic.Pointer[string]
	seen atomic.Int64
	// gone counts how often the fleet gave this process up.
	gone atomic.Int64
}

// running is a task the process runs.
type running struct {
	claim  tasks.Claim
	cancel context.CancelFunc
	// lost says the store took the task away: what it returns is dropped.
	lost bool
}

// finished is the end of a task's run. settle is nil for a run that was
// told to stop.
type finished struct {
	ref    tasks.Ref
	settle *tasks.Settle
}

func (w *Worker) init() {
	w.once.Do(func() {
		if w.Slots <= 0 {
			w.Slots = DefaultSlots
		}
		if w.Lease <= 0 {
			w.Lease = tasks.DefaultLease
		}
		if w.Flush <= 0 {
			w.Flush = DefaultFlush
		}
		if w.Poll <= 0 {
			w.Poll = DefaultPoll
		}
		if w.Grace <= 0 {
			w.Grace = DefaultGrace
		}
		if w.Log == nil {
			w.Log = slog.Default()
		}
		w.cache = newCache(w.CacheBytes)
	})
}

// renewal is the longest a worker goes between two exchanges: a quarter of
// its lease. The store asks for one per third, which is also what lets a
// worker return the tasks of a dead one, so the margin is what a slow round
// trip may take.
func (w *Worker) renewal() time.Duration { return w.Lease / 4 }

// Ready reports whether the worker holds a live registration: the store
// answered an exchange of it, or its registration, within a third of its
// lease.
func (w *Worker) Ready() error {
	if w.id.Load() == nil {
		return errors.New("the worker is not registered")
	}
	if since := time.Since(time.Unix(0, w.seen.Load())); since > w.Lease/3 {
		return fmt.Errorf("the task store last answered the worker %s ago, over a third of its lease", since.Round(time.Millisecond))
	}
	return nil
}

// ID is the worker's registration, empty before it has one.
func (w *Worker) ID() string {
	if id := w.id.Load(); id != nil {
		return *id
	}
	return ""
}

// Abandoned is how often the fleet gave this process up and it registered
// again: its lease ran out while it was away.
func (w *Worker) Abandoned() int64 { return w.gone.Load() }

// Run runs tasks until ctx ends, and then stops: it claims nothing more,
// lets the tasks it runs finish for the grace period, settles what finished,
// and gives the rest back with no counter changed. It returns the error of
// its last exchange, when that one failed.
func (w *Worker) Run(ctx context.Context) error {
	w.init()
	// A task outlives the signal that stops the worker: it has the grace
	// period to finish.
	base := context.WithoutCancel(ctx)
	l := &loop{
		w: w, base: base, held: map[tasks.Ref]*running{},
		done: make(chan finished, w.Slots), idle: w.Poll,
	}
	if !l.register(ctx) {
		return nil
	}
	for ctx.Err() == nil {
		l.wait(ctx, l.due())
		if ctx.Err() != nil {
			break
		}
		l.exchange(ctx)
	}
	return l.stop()
}

// loop is the state of a running worker. One goroutine owns it; a task's
// goroutine reaches it only through done.
type loop struct {
	w    *Worker
	base context.Context
	id   string

	held map[tasks.Ref]*running
	done chan finished
	// settles are the tasks that finished since the last exchange.
	settles []tasks.Settle

	// last is when the last exchange was sent, idle the poll interval of a
	// worker with a free slot and nothing to claim, wake when the earliest
	// pause that held a claim back ends, and failures how many exchanges in
	// a row the store did not answer.
	last     time.Time
	idle     time.Duration
	wake     *time.Time
	failures int
}

// register records the process with the store, trying until the store
// answers. It reports false when ctx ended first.
func (l *loop) register(ctx context.Context) bool {
	for attempt := 0; ; attempt++ {
		id, err := l.w.Store.Register(ctx)
		if err == nil {
			l.id, l.last = id, time.Now()
			l.w.id.Store(&id)
			l.w.seen.Store(l.last.UnixNano())
			l.w.Log.InfoContext(ctx, "the worker is registered", "worker", id, "slots", l.w.Slots)
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		l.w.Log.WarnContext(ctx, "the worker could not register", "error", err)
		if !sleep(ctx, min(l.w.Flush<<min(attempt, 5), l.w.renewal())) {
			return false
		}
	}
}

// free is how many tasks the worker can take. A task that must run alone
// leaves none until it has settled. A task that was taken away and has not
// returned yet still holds its slot.
func (l *loop) free() int {
	for _, r := range l.held {
		if r.claim.Alone && !r.lost {
			return 0
		}
	}
	return max(0, l.w.Slots-len(l.held))
}

// due is when the next exchange is sent: as soon as a task has finished,
// when the poll interval of a free slot has passed or the pause that held a
// claim back has ended, and at the latest when the lease is to be renewed;
// never sooner than a flush interval after the last one.
func (l *loop) due() time.Time {
	at := l.last.Add(l.w.renewal())
	if l.free() > 0 {
		poll := l.last.Add(l.idle)
		if l.wake != nil && l.wake.Before(poll) {
			poll = *l.wake
		}
		if poll.Before(at) {
			at = poll
		}
	}
	if len(l.settles) > 0 {
		at = l.last
	}
	if l.failures > 0 {
		// The store did not answer: it is asked again after a pause that
		// grows, and before the lease has to be renewed.
		at = l.last.Add(min(l.w.Flush<<min(l.failures, 5), l.w.renewal()))
	}
	if earliest := l.last.Add(l.w.Flush); at.Before(earliest) {
		at = earliest
	}
	return at
}

// wait takes what the tasks finish until at. A task that finishes moves the
// next exchange forward, so the wait ends when one is due.
func (l *loop) wait(ctx context.Context, at time.Time) {
	for {
		timer := time.NewTimer(time.Until(at))
		select {
		case f := <-l.done:
			timer.Stop()
			l.finish(f)
			if next := l.due(); next.Before(at) {
				at = next
			}
		case <-timer.C:
			return
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// finish records the end of a task's run.
func (l *loop) finish(f finished) {
	r, ok := l.held[f.ref]
	if !ok {
		return
	}
	delete(l.held, f.ref)
	r.cancel()
	if f.settle != nil && !r.lost {
		l.settles = append(l.settles, *f.settle)
		// A slot is free and may be filled at once.
		l.idle = l.w.Poll
	}
}

// request is what the worker says in an exchange.
func (l *loop) request() tasks.Request {
	req := tasks.Request{Settles: l.settles, Held: []tasks.Held{}, Free: l.free(), Idle: len(l.held) == 0}
	for ref, r := range l.held {
		if !r.lost {
			req.Held = append(req.Held, tasks.Held{Parse: ref.Parse, Task: ref.Task, Token: r.claim.Token})
		}
	}
	return req
}

// exchange makes one exchange and acts on the reply.
func (l *loop) exchange(ctx context.Context) {
	req := l.request()
	l.last = time.Now()
	// The exchange is not ended by the signal that stops the worker: a
	// statement cut off after the store ran it would leave claims in a
	// reply nobody read.
	bound, cancel := context.WithTimeout(l.base, max(l.w.renewal(), time.Second))
	reply, err := l.w.Store.Exchange(bound, l.id, req)
	cancel()
	if err != nil {
		// The settles are kept and sent again: one the store did record is
		// refused then, and nothing is recorded twice. The tasks keep
		// running: a store that does not answer expires no lease.
		l.failures++
		if ctx.Err() == nil {
			l.w.Log.WarnContext(ctx, "the task store did not answer an exchange", "worker", l.id, "error", err)
		}
		return
	}
	l.failures, l.settles = 0, nil
	l.w.seen.Store(time.Now().UnixNano())

	if reply.Gone {
		// The fleet gave this process up and returned its tasks to the
		// queue: it stops every one of them, writes nothing further for
		// them, and works again under a new id.
		l.w.gone.Add(1)
		l.w.Log.WarnContext(ctx, "the fleet gave the worker up: its lease ran out", "worker", l.id, "tasks", len(l.held))
		for _, r := range l.held {
			r.lost = true
			r.cancel()
		}
		l.w.id.Store(nil)
		l.register(ctx)
		return
	}
	for _, ref := range reply.Refused {
		// The task was canceled, reissued, or settled before: what this
		// process wrote for it is under a key nothing points at.
		l.w.Log.InfoContext(ctx, "a settle was refused", "parse", ref.Parse, "task", ref.Task)
	}
	for _, ref := range reply.Lost {
		if r, ok := l.held[ref]; ok {
			r.lost = true
			r.cancel()
		}
	}
	for _, c := range reply.Claims {
		l.start(c)
	}

	l.wake = reply.SleepUntil
	switch {
	case len(reply.Claims) > 0:
		l.idle = l.w.Poll
	case req.Free > 0:
		// Nothing to claim: the worker asks less often, up to 5 times its
		// poll interval, with jitter so that idle workers do not ask
		// together.
		l.idle = min(l.idle*2, pollBackoff*l.w.Poll)
		l.idle += time.Duration(rand.Int64N(int64(l.idle)/4 + 1))
	}
}

// start runs a claimed task on a goroutine of its own.
func (l *loop) start(c tasks.Claim) {
	ctx, cancel := context.WithCancel(l.base)
	ref := tasks.Ref{Parse: c.Parse, Task: c.Task}
	l.held[ref] = &running{claim: c, cancel: cancel}
	go func() {
		began := time.Now()
		settle := l.w.run(ctx, c)
		if ctx.Err() != nil {
			// The run was told to stop: whatever it made is not settled.
			l.done <- finished{ref: ref}
			return
		}
		l.w.Log.InfoContext(ctx, "task ended", "parse", c.Parse, "task", c.Task, "kind", c.Kind,
			"outcome", settle.Outcome, "attempt", c.Attempt, "reader", c.Reader, "duration", time.Since(began))
		l.done <- finished{ref: ref, settle: &settle}
	}()
}

// stop ends the worker: the tasks it runs get the grace period to finish,
// and what finishes is settled as it does. Then the rest are told to stop
// and given back, and the last exchange removes the worker's registration.
func (l *loop) stop() error {
	ctx := l.base
	l.w.Log.InfoContext(ctx, "the worker is stopping", "worker", l.id, "tasks", len(l.held))
	grace := time.NewTimer(l.w.Grace)
	defer grace.Stop()
	for expired := false; len(l.held) > 0 && !expired; {
		select {
		case f := <-l.done:
			l.finish(f)
		case <-grace.C:
			expired = true
		}
	}
	// What did not finish is told to stop, and has a moment to return.
	for _, r := range l.held {
		r.cancel()
	}
	returned := []tasks.Ref{}
	wait := time.NewTimer(stopWait)
	defer wait.Stop()
	for expired := false; len(l.held) > 0 && !expired; {
		select {
		case f := <-l.done:
			if r, ok := l.held[f.ref]; ok && !r.lost && f.settle == nil {
				returned = append(returned, f.ref)
				f.settle = &tasks.Settle{Parse: f.ref.Parse, Task: f.ref.Task, Token: r.claim.Token, Outcome: tasks.Returned}
			}
			l.finish(f)
		case <-wait.C:
			expired = true
		}
	}
	// A task that has still not returned is given back by the exchange
	// itself: the store returns everything the worker holds.
	if l.w.id.Load() == nil {
		return nil
	}
	final, cancel := context.WithTimeout(ctx, max(l.w.renewal(), time.Second))
	defer cancel()
	req := tasks.Request{Settles: l.settles, Held: []tasks.Held{}, Shutdown: true}
	if _, err := l.w.Store.Exchange(final, l.id, req); err != nil {
		return fmt.Errorf("worker: the last exchange of %s: %w", l.id, err)
	}
	l.w.id.Store(nil)
	l.w.Log.InfoContext(ctx, "the worker stopped", "worker", l.id, "returned", len(returned))
	return nil
}

// sleep waits for d, or until ctx ends. It reports whether it waited the
// whole time.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

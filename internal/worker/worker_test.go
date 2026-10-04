// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// queued puts n page claims of a parse in the scripted store's queue, each
// reading the one-page file the bench holds.
func (b *bench) queued(parseID string, n int) {
	b.t.Helper()
	b.put("sources/o/aa/fil_1", sheet(b.t, false), "image/png")
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	for i := range n {
		// Every claim reads page 1 of the file, as the task of another page.
		c := page(parseID, 1, int64(i+1), "sources/o/aa/fil_1", "image/png")
		c.Parse = parseID + "_" + string(rune('a'+i))
		b.store.queue = append(b.store.queue, c)
	}
}

// TestTheLoopClaimsRunsAndSettles: a worker registers, claims as many tasks
// as it has slots, runs them, and settles each. It exchanges no more often
// than its flush interval, asks less often while there is nothing to claim,
// and its last exchange says it is stopping.
func TestTheLoopClaimsRunsAndSettles(t *testing.T) {
	b := newBench(t, nil)
	b.queued("prs", 6)
	if err := b.w.Ready(); err == nil {
		t.Fatal("a worker that has not registered is ready")
	}
	stop := b.run()
	eventually(t, "6 tasks were settled", func() bool { return len(b.store.settles()) == 6 })
	if err := b.w.Ready(); err != nil || b.w.ID() != "wrk_1" || b.w.Abandoned() != 0 {
		t.Fatalf("a worker that exchanges is not ready: %v, id %q", err, b.w.ID())
	}
	for _, s := range b.store.settles() {
		if s.Outcome != tasks.Done || s.Output == "" {
			t.Fatalf("a task was settled as %+v", s)
		}
	}
	// With nothing left to claim the worker keeps exchanging, and no sooner
	// than a flush interval after the exchange before.
	before := len(b.store.seen())
	eventually(t, "the idle worker asked again", func() bool { return len(b.store.seen()) >= before+3 })
	if err := stop(); err != nil {
		t.Fatalf("stopping: %v", err)
	}

	requests := b.store.seen()
	first, last := requests[0], requests[len(requests)-1]
	if first.Free != 4 || !first.Idle || first.Shutdown || len(first.Held) != 0 {
		t.Fatalf("the first exchange said %+v", first)
	}
	if !last.Shutdown || len(last.Held) != 0 {
		t.Fatalf("the last exchange said %+v", last)
	}
	// Every exchange names the kinds of task the worker runs, all 5: a
	// request that named none would be handed no extraction and no figure.
	for i, req := range requests {
		if !slices.Equal(req.Kinds, []tasks.Kind{tasks.Prepare, tasks.Page, tasks.Assemble, tasks.Extract, tasks.Figure}) {
			t.Fatalf("exchange %d names the kinds %v", i, req.Kinds)
		}
	}
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	// The last exchange is the one that says the worker stops, and waits
	// for nothing.
	for i := 1; i < len(b.store.at)-1; i++ {
		// The timer that spaces two exchanges is allowed its granularity.
		if gap := b.store.at[i].Sub(b.store.at[i-1]); gap < b.w.Flush-time.Millisecond {
			t.Fatalf("exchanges %d and %d are %v apart, under the flush interval of %v", i-1, i, gap, b.w.Flush)
		}
	}
	if b.w.ID() != "" || b.w.Ready() == nil {
		t.Fatal("a worker that stopped still holds a registration")
	}
}

// TestATaskTheStoreTookAwayIsStoppedAndNotSettled: a task the reply names as
// lost has its call ended, nothing is settled for it, and the worker says it
// runs nothing only once the task has returned.
func TestATaskTheStoreTookAwayIsStoppedAndNotSettled(t *testing.T) {
	g := newGate()
	b := newBench(t, map[string]reader.Reader{"stub": g})
	b.queued("prs", 1)
	b.store.script = func(n int, req tasks.Request, reply *tasks.Reply) error {
		// The second exchange is told the task it holds is canceled.
		if n == 2 && len(req.Held) == 1 {
			reply.Lost = []tasks.Ref{{Parse: req.Held[0].Parse, Task: req.Held[0].Task}}
		}
		return nil
	}
	stop := b.run()
	<-g.entered
	eventually(t, "the worker runs nothing again", func() bool {
		seen := b.store.seen()
		return len(seen) > 2 && seen[len(seen)-1].Idle && len(seen[len(seen)-1].Held) == 0
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got := b.store.settles(); len(got) != 0 {
		t.Fatalf("a task that was taken away was settled: %+v", got)
	}
	requests := b.store.seen()
	if held := requests[1].Held; len(held) != 1 || held[0].Token != 1 || requests[1].Idle || requests[1].Free != 3 {
		t.Fatalf("while it ran the task the worker said %+v", requests[1])
	}
}

// TestAWorkerTheFleetGaveUpRegistersAgain: an exchange answered as gone ends
// every task the worker runs, with nothing settled for them, and the worker
// works again under a new id.
func TestAWorkerTheFleetGaveUpRegistersAgain(t *testing.T) {
	g := newGate()
	b := newBench(t, map[string]reader.Reader{"stub": g})
	b.queued("prs", 2)
	b.store.script = func(n int, _ tasks.Request, reply *tasks.Reply) error {
		if n == 2 {
			*reply = tasks.Reply{Gone: true}
		}
		return nil
	}
	stop := b.run()
	<-g.entered
	eventually(t, "the worker registered again", func() bool { return b.w.ID() == "wrk_2" })
	if b.w.Abandoned() != 1 {
		t.Fatalf("the worker counts %d times it was given up", b.w.Abandoned())
	}
	eventually(t, "the worker runs nothing again", func() bool {
		seen := b.store.seen()
		return seen[len(seen)-1].Idle
	})
	close(g.release)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got := b.store.settles(); len(got) != 0 {
		t.Fatalf("a worker the fleet gave up settled %+v", got)
	}
}

// TestATaskThatMustRunAloneRunsAlone: a task whose worker died before is
// handed to a worker that runs nothing, and the worker takes nothing more
// until it has settled it.
func TestATaskThatMustRunAloneRunsAlone(t *testing.T) {
	g := newGate()
	b := newBench(t, map[string]reader.Reader{"stub": g})
	b.queued("prs", 3)
	b.store.queue[0].Alone = true
	// A worker with no slot to fill exchanges to renew its lease, a quarter
	// of the lease apart.
	b.w.Lease = 200 * time.Millisecond
	stop := b.run()
	<-g.entered
	eventually(t, "the worker exchanged while it ran the task", func() bool { return len(b.store.seen()) >= 4 })
	for i, req := range b.store.seen()[1:] {
		if req.Free != 0 || len(req.Held) != 1 {
			t.Fatalf("exchange %d while the task ran alone said %+v", i+1, req)
		}
	}
	close(g.release)
	eventually(t, "the 3 tasks were settled", func() bool { return len(b.store.settles()) == 3 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// TestAnExchangeTheStoreDidNotAnswerIsSentAgain: the settles of an exchange
// that failed are kept and sent again, the tasks keep running, and a worker
// that could not register keeps trying.
func TestAnExchangeTheStoreDidNotAnswerIsSentAgain(t *testing.T) {
	b := newBench(t, nil)
	b.queued("prs", 2)
	b.store.refuse = 2
	failed, heard := 0, map[string]bool{}
	b.store.script = func(_ int, req tasks.Request, _ *tasks.Reply) error {
		// The first 2 exchanges that carry a settle are not answered.
		if len(req.Settles) > 0 && failed < 2 {
			failed++
			return errors.New("the database does not answer")
		}
		for _, s := range req.Settles {
			heard[s.Parse] = true
		}
		return nil
	}
	stop := b.run()
	eventually(t, "the settles arrived", func() bool {
		b.store.mu.Lock()
		defer b.store.mu.Unlock()
		return failed == 2 && len(heard) == 2
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	// A settle the store did not answer was sent again until it was heard,
	// and not once more after that.
	counts := map[string]int{}
	for _, s := range b.store.settles() {
		counts[s.Parse]++
	}
	if len(counts) != 2 || counts["prs_a"] < 2 || counts["prs_a"] > 3 || counts["prs_b"] < 2 || counts["prs_b"] > 3 || counts["prs_a"]+counts["prs_b"] < 5 {
		t.Fatalf("the settles were sent %v times", counts)
	}
}

// TestAStoppingWorkerSettlesWhatFinishesAndReturnsTheRest: on its way down a
// worker claims nothing, lets its tasks finish for the grace period and
// settles those that do, and gives the rest back as returned in its last
// exchange.
func TestAStoppingWorkerSettlesWhatFinishesAndReturnsTheRest(t *testing.T) {
	g := newGate()
	quick := &stub.Reader{}
	b := newBench(t, map[string]reader.Reader{"stub": quick, "slow": g})
	b.queued("prs", 3)
	b.store.queue[1].Reader = "slow"
	// The quick tasks finish as the worker stops: each waits a moment.
	quick.Fail = func(reader.Page) error { time.Sleep(60 * time.Millisecond); return nil }

	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- b.w.Run(ctx) }()
	<-g.entered
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("stopping: %v", err)
	}

	requests := b.store.seen()
	last := requests[len(requests)-1]
	outcomes := map[string]tasks.Outcome{}
	for _, s := range b.store.settles() {
		outcomes[s.Parse] = s.Outcome
	}
	if !last.Shutdown || last.Free != 0 || outcomes["prs_a"] != tasks.Done || outcomes["prs_b"] != tasks.Returned || outcomes["prs_c"] != tasks.Done {
		t.Fatalf("the worker stopped with %v, last exchange %+v", outcomes, last)
	}
	for _, s := range last.Settles {
		if s.Outcome == tasks.Returned && s.Token != 2 {
			t.Fatalf("a task was given back under the token %d, want its claim's", s.Token)
		}
	}

	// A last exchange the store does not answer is the error Run returns,
	// and a worker that never registered stops with none.
	failing := newBench(t, nil)
	failing.store.script = func(_ int, req tasks.Request, _ *tasks.Reply) error {
		if req.Shutdown {
			return errors.New("the database does not answer")
		}
		return nil
	}
	stop := failing.run()
	eventually(t, "the worker exchanged", func() bool { return len(failing.store.seen()) > 0 })
	if err := stop(); err == nil {
		t.Fatal("a last exchange that failed was not returned")
	}
	never := newBench(t, nil)
	never.store.refuse = 1 << 30
	stop = never.run()
	time.Sleep(20 * time.Millisecond)
	if err := stop(); err != nil || never.w.ID() != "" || len(never.store.seen()) != 0 {
		t.Fatalf("a worker that never registered stopped with %v after %d exchanges", err, len(never.store.seen()))
	}
}

// TestAWorkerIsReadyWhileTheStoreAnswersIt: the readiness probe's question.
// A worker whose last answered exchange is over a third of its lease ago
// holds no registration it can count on.
func TestAWorkerIsReadyWhileTheStoreAnswersIt(t *testing.T) {
	b := newBench(t, nil)
	b.w.Lease = 90 * time.Millisecond
	down := false
	b.store.script = func(int, tasks.Request, *tasks.Reply) error {
		if down {
			return errors.New("the database does not answer")
		}
		return nil
	}
	stop := b.run()
	eventually(t, "the worker is ready", func() bool { return b.w.Ready() == nil })
	b.store.mu.Lock()
	down = true
	b.store.mu.Unlock()
	eventually(t, "the worker is no longer ready", func() bool { return b.w.Ready() != nil })
	b.store.mu.Lock()
	down = false
	b.store.mu.Unlock()
	eventually(t, "the worker is ready again", func() bool { return b.w.Ready() == nil })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// TestTheDefaultsOfAWorker: a worker that is given no setting runs with the
// specs' own.
func TestTheDefaultsOfAWorker(t *testing.T) {
	w := &Worker{}
	w.init()
	if w.Slots != DefaultSlots || w.Lease != tasks.DefaultLease || w.Flush != DefaultFlush || w.Poll != DefaultPoll || w.Grace != DefaultGrace || w.Log == nil {
		t.Fatalf("the defaults are %+v", w)
	}
	if w.renewal() != 15*time.Second || w.cost("any") != 1 {
		t.Fatalf("a lease of a minute is renewed every %v, and a reader with no cost costs %d", w.renewal(), w.cost("any"))
	}
}

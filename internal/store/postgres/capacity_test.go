// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// The criteria of specs/007-model-capacity.md that hold at the store: a
// reader's room is decided in the claim, from the rows, on the clock the
// claim is given.

// readers are settings with the named pools, the first of which is the read
// chain's first candidate.
func readers(pools ...tasks.Pool) tasks.Settings {
	s := tasks.Settings{Pools: pools}
	for _, p := range pools {
		s.ReadChain = append(s.ReadChain, p.Reader)
	}
	return s
}

// limited is a settle of a call the endpoint refused with a rate limit.
func limited(c tasks.Claim, wait time.Duration) tasks.Settle {
	s := ended(c, tasks.Wait, "")
	s.RetryAfter = wait
	return s
}

// unhealthy is a settle of a call that failed in a way the next call to the
// reader may too.
func unhealthy(c tasks.Claim) tasks.Settle {
	s := ended(c, tasks.Retryable, "reader_unavailable")
	s.Health = tasks.Unhealthy
	return s
}

// by counts claims per reader.
func by(claims []tasks.Claim) map[string]int {
	out := map[string]int{}
	for _, c := range claims {
		out[c.Reader]++
	}
	return out
}

// TestAPoolAdmitsNoMoreThanItsBound: with max_in_flight 8 and 32 slots
// across 4 worker processes exchanging at once, no more than 8 calls are
// ever in flight, and the pool's whole bound is used.
func TestAPoolAdmitsNoMoreThanItsBound(t *testing.T) {
	everywhere(t, readers(tasks.Pool{Reader: "only", MaxInFlight: 8}), func(t *testing.T, h *harness) {
		const total = 240
		h.reading(h.worker(), postgres.Submission{Parse: "prs_a", Group: "acme"}, total)

		var flying, peak, read atomic.Int64
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for range 4 {
			id, err := h.store.Register(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			wg.Go(func() {
				var settles []tasks.Settle
				for read.Load() < total {
					reply, err := h.store.Exchange(context.Background(), id, tasks.Request{Free: 8, Idle: true, Settles: settles})
					if err != nil || len(reply.Refused) != 0 {
						errs <- err
						return
					}
					settles = settles[:0]
					now := flying.Add(int64(len(reply.Claims)))
					for {
						seen := peak.Load()
						if now <= seen || peak.CompareAndSwap(seen, now) {
							break
						}
					}
					// The calls are in flight for as long as a worker holds
					// them, and end before the settle that frees the slots.
					// They last several exchanges, so calls that another
					// worker was handed meanwhile would be counted with them.
					time.Sleep(15 * time.Millisecond)
					flying.Add(-int64(len(reply.Claims)))
					for _, c := range reply.Claims {
						if c.Kind == tasks.Page {
							read.Add(1)
						}
						settles = append(settles, done(c))
					}
				}
				if _, err := h.store.Exchange(context.Background(), id, tasks.Request{Settles: settles}); err != nil {
					errs <- err
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("an exchange failed or a settle was refused: %v", err)
		}
		if peak.Load() != 8 {
			t.Fatalf("at most %d calls were in flight, want the pool's bound of 8 and no more", peak.Load())
		}
		if p := h.parse("prs_a"); p.PagesDone != total || p.PagesOpen != 0 {
			t.Fatalf("the parse read %d of %d pages", p.PagesDone, total)
		}
	})
}

// TestAPageGoesToTheNextReaderWithRoom: the slot of a page is the first
// candidate of the policy's chain with room. A first reader that is paused,
// open or full sends pages to the second, and a parse that named its reader
// waits for that reader: none of its pages is read by another.
func TestAPageGoesToTheNextReaderWithRoom(t *testing.T) {
	settings := readers(tasks.Pool{Reader: "first", MaxInFlight: 4}, tasks.Pool{Reader: "second", MaxInFlight: 100, Cost: 5})
	everywhere(t, settings, func(t *testing.T, h *harness) {
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_free", Group: "acme"}, 40)
		h.reading(w, postgres.Submission{Parse: "prs_pinned", Group: "acme", Pin: "first", Priority: 1}, 6)

		// The pinned parse's pages go first, by priority, and fill the first
		// reader. With the first reader full, the pinned pages wait and the
		// others are read by the second.
		claims := w.claim(8, 8)
		if got := by(claims); got["first"] != 4 || got["second"] != 4 {
			t.Fatalf("with the first reader full the claims went to %v", got)
		}
		for _, c := range claims {
			if (c.Parse == "prs_pinned") != (c.Reader == "first") || c.Scope != "" {
				t.Fatalf("a claim took the slot %+v", c)
			}
			if c.Reader == "second" && h.task(c.Parse, c.Task).Charged != 5 {
				t.Fatalf("a page read by the second reader is charged %d, want its cost", h.task(c.Parse, c.Task).Charged)
			}
		}
		var pinned []tasks.Claim
		for _, c := range claims {
			if c.Parse == "prs_pinned" {
				pinned = append(pinned, c)
			}
		}

		// The first reader's endpoint limits its key: it is paused.
		w.settle(limited(pinned[0], 20*time.Second), done(pinned[1]), done(pinned[2]), done(pinned[3]))
		for _, c := range w.claim(4, 4) {
			if c.Reader != "second" || c.Parse != "prs_free" {
				t.Fatalf("while the first reader is paused a claim took %+v", c)
			}
		}
		if n := value[int64](h, `SELECT count(*) FROM tasks WHERE parse_id = 'prs_pinned' AND state = 'queued'`); n != 3 {
			t.Fatalf("%d pinned pages wait, want 3", n)
		}

		// The pause ends: the pinned pages are read by the reader they named.
		// Its ceiling was halved to 2 by the pause and one quiet interval
		// gave 1 back, so it admits the 3.
		h.advance(40 * time.Second)
		for _, c := range w.claim(3, 3) {
			if c.Reader != "first" || c.Parse != "prs_pinned" {
				t.Fatalf("after the pause a claim took %+v", c)
			}
		}

		// 3 failures open the first reader's breaker.
		var failing []tasks.Settle
		for _, c := range w.held {
			if c.Reader == "first" {
				failing = append(failing, unhealthy(c))
			}
		}
		w.settle(failing...)
		h.advance(2 * time.Second)
		for _, c := range w.claim(6, 6) {
			if c.Reader != "second" || c.Parse != "prs_free" {
				t.Fatalf("while the first reader's breaker is open a claim took %+v", c)
			}
		}
	})
}

// TestOnePauseHalvesTheCeilingOnce: 40 calls in flight that all return a
// rate limit halve the scope's ceiling once, from 40 to 20, and stop every
// worker's calls in that scope until the longest wait among them has passed.
// When the pause ends the scope admits a share of its ceiling that grows to
// all of it over the resume period, and each quiet recovery interval gives a
// tenth of the pool back.
func TestOnePauseHalvesTheCeilingOnce(t *testing.T) {
	everywhere(t, readers(tasks.Pool{Reader: "only", MaxInFlight: 40}), func(t *testing.T, h *harness) {
		w, other := h.worker(), h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 300)
		claims := w.claim(100, 40)

		// All 40 come back refused within one second, the first naming the
		// longest wait.
		h.advance(500 * time.Millisecond)
		w.settle(limited(claims[0], 7*time.Second))
		for i, c := range claims[1:] {
			if i%13 == 0 {
				h.advance(30 * time.Millisecond)
			}
			w.settle(limited(c, time.Second))
		}
		paused := epoch.Add(500*time.Millisecond + 7*time.Second)
		if c := value[int](h, `SELECT ceiling FROM pool_scopes WHERE reader = 'only' AND scope = ''`); c != 20 {
			t.Fatalf("40 replies of one pause left the ceiling at %d, want 20", c)
		}
		if until := value[time.Time](h, `SELECT paused_until FROM pool_scopes WHERE reader = 'only'`); !until.Equal(paused) {
			t.Fatalf("the scope is paused until %v, want %v", until, paused)
		}
		if n := value[int64](h, `SELECT count(*) FROM tasks WHERE attempt <> 0`); n != 0 {
			t.Fatalf("the rate limit spent an attempt of %d tasks", n)
		}

		// Nothing is admitted in the scope, by any worker, until the pause
		// ends.
		h.advance(paused.Sub(h.now) - time.Millisecond)
		w.claim(100, 0)
		other.claim(100, 0)

		// One second into the resume period the scope admits a tenth of its
		// ceiling, and the whole of it once the period is over.
		h.advance(time.Millisecond + time.Second)
		first := w.claim(100, 2)
		other.claim(100, 0)
		h.advance(4 * time.Second)
		w.claim(100, 8)
		h.advance(tasks.DefaultPoolResume)
		w.claim(100, 10)
		other.claim(100, 0)

		// A quiet recovery interval, counted from the last rate-limit reply,
		// raises the ceiling by a tenth of the pool: 4 of 40.
		quiet := epoch.Add(500*time.Millisecond + 90*time.Millisecond)
		h.advance(quiet.Add(tasks.DefaultPoolRecovery).Sub(h.now))
		other.claim(100, 4)
		h.advance(4 * tasks.DefaultPoolRecovery)
		other.claim(100, 16)
		if c := value[int](h, `SELECT ceiling FROM pool_scopes WHERE reader = 'only'`); c != 40 {
			t.Fatalf("after 5 quiet intervals the ceiling is %d, want the pool's 40", c)
		}

		// A call claimed after the halving that is refused says something
		// new: it halves again.
		w.settle(limited(first[0], time.Second))
		if c := value[int](h, `SELECT ceiling FROM pool_scopes WHERE reader = 'only'`); c != 20 {
			t.Fatalf("a refusal of a call claimed after the halving left the ceiling at %d, want 20", c)
		}
	})
}

// TestACeilingDrivenToOneRecovers: a scope whose ceiling was driven to 1 by
// repeated pauses returns to a max_in_flight of 200 within 10 quiet recovery
// intervals.
func TestACeilingDrivenToOneRecovers(t *testing.T) {
	everywhere(t, readers(tasks.Pool{Reader: "only", MaxInFlight: 200}), func(t *testing.T, h *harness) {
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 300)
		// Each call is claimed after the last halving and refused: 200, 100,
		// 50, 25, 12, 6, 3, 1.
		for range 8 {
			w.settle(limited(w.claim(1, 1)[0], time.Second))
			h.advance(time.Second)
		}
		if c := value[int](h, `SELECT ceiling FROM pool_scopes WHERE reader = 'only'`); c != 1 {
			t.Fatalf("8 pauses left the ceiling at %d, want 1", c)
		}
		h.advance(9*tasks.DefaultPoolRecovery - time.Second)
		w.claim(250, 181)
		h.advance(tasks.DefaultPoolRecovery)
		w.claim(250, 19)
		if n := value[int64](h, `SELECT count(*) FROM tasks WHERE state = 'leased' AND calling`); n != 200 {
			t.Fatalf("%d calls are in flight after 10 quiet intervals, want the pool's 200", n)
		}
	})
}

// TestARateLimitPausesItsKeyAndNoOther: with a key per group, a rate limit
// on one group's key pauses that group's calls to the reader and no other
// group's.
func TestARateLimitPausesItsKeyAndNoOther(t *testing.T) {
	settings := readers(tasks.Pool{Reader: "only", MaxInFlight: 20})
	settings.KeysPerGroup = true
	everywhere(t, settings, func(t *testing.T, h *harness) {
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 20)
		h.reading(w, postgres.Submission{Parse: "prs_g", Group: "globex"}, 20)
		claims := w.claim(2, 2)
		if claims[0].Group != "acme" || claims[0].Scope != "acme" || claims[1].Group != "globex" || claims[1].Scope != "globex" {
			t.Fatalf("with keys per group the slots are %+v", claims)
		}
		w.settle(limited(claims[0], 30*time.Second), done(claims[1]))
		for _, c := range w.claim(6, 6) {
			if c.Group != "globex" {
				t.Fatalf("a call of the paused group was admitted: %+v", c)
			}
		}
		reply := w.exchange(20)
		if len(reply.Claims) != 13 || reply.SleepUntil == nil || !reply.SleepUntil.Equal(h.now.Add(30*time.Second)) {
			t.Fatalf("with one group paused the exchange claimed %d and sleeps until %v", len(reply.Claims), reply.SleepUntil)
		}
		if scopes := value[string](h, `SELECT string_agg(scope, ',') FROM pool_scopes`); scopes != "acme" {
			t.Fatalf("the scopes with a row are %q, want the limited one alone", scopes)
		}
	})
}

// TestTheBreaker: 3 consecutive failures of a reader's calls open its
// breaker for every worker. After the open period exactly one call is
// admitted as a trial, and no other while it is in flight. Its success
// closes the breaker and its failure opens it again. A success between
// failures resets the count, and a trial that ends without saying anything
// about the reader leaves the next claim to be the trial.
func TestTheBreaker(t *testing.T) {
	everywhere(t, readers(tasks.Pool{Reader: "only", MaxInFlight: 50}), func(t *testing.T, h *harness) {
		w, other := h.worker(), h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme", AllowFailedPages: 100}, 60)
		pool := func() (failures int, open bool) {
			return value[int](h, `SELECT failures FROM pools`), value[bool](h, `SELECT opened_at IS NOT NULL FROM pools`)
		}

		// Two failures, a success, two failures: never 3 in a row.
		claims := w.claim(5, 5)
		w.settle(unhealthy(claims[0]), unhealthy(claims[1]))
		w.settle(done(claims[2]))
		w.settle(unhealthy(claims[3]), unhealthy(claims[4]))
		if n, open := pool(); n != 2 || open {
			t.Fatalf("after 2 failures, a success and 2 failures the pool counts %d and is open: %t", n, open)
		}
		// A failure that says nothing about the reader does not count.
		c := w.claim(1, 1)[0]
		w.settle(ended(c, tasks.Permanent, "page_unreadable"))
		if n, open := pool(); n != 2 || open {
			t.Fatalf("a failure of the page itself moved the breaker: %d, %t", n, open)
		}

		// The third in a row opens it, for every worker.
		inflight := w.claim(3, 3)
		w.settle(unhealthy(inflight[0]))
		if n, open := pool(); n != 3 || !open {
			t.Fatalf("after 3 failures in a row the pool counts %d and is open: %t", n, open)
		}
		h.advance(tasks.DefaultBreakerOpen - time.Second)
		w.claim(10, 0)
		other.claim(10, 0)
		// A task that calls no model is not held back by a breaker.
		h.submit(postgres.Submission{Parse: "prs_b", Group: "acme"})
		if c := other.claim(10, 1)[0]; c.Kind != tasks.Prepare {
			t.Fatalf("with the breaker open the claim is %+v", c)
		}

		// After the open period: exactly one trial, whoever asks and however
		// many slots are free.
		h.advance(time.Second)
		trial := other.claim(10, 1)[0]
		w.claim(10, 0)
		other.claim(10, 0)
		if at := value[time.Time](h, `SELECT trial_at FROM pools`); !at.Equal(h.now) {
			t.Fatalf("the trial was admitted at %v, the pool recorded %v", h.now, at)
		}

		// The trial fails: open again, from now.
		h.advance(time.Second)
		other.settle(unhealthy(trial))
		h.advance(tasks.DefaultBreakerOpen - time.Second)
		w.claim(10, 0)
		h.advance(time.Second)
		trial = w.claim(10, 1)[0]
		other.claim(10, 0)

		// The trial fails for the page's own reason, which says nothing about
		// the reader: the breaker stays open and the next claim is the trial.
		w.settle(ended(trial, tasks.Permanent, "page_unreadable"))
		trial = other.claim(10, 1)[0]
		w.claim(10, 0)

		// A call that was in flight when the breaker opened fails late: it is
		// no trial and moves nothing.
		before := value[time.Time](h, `SELECT opened_at FROM pools`)
		w.settle(unhealthy(inflight[1]))
		if after := value[time.Time](h, `SELECT opened_at FROM pools`); !after.Equal(before) {
			t.Fatalf("a late failure of an earlier call moved the breaker from %v to %v", before, after)
		}

		// The trial succeeds: the breaker closes and everything is admitted.
		other.settle(done(trial))
		if n, open := pool(); n != 0 || open {
			t.Fatalf("after a trial that succeeded the pool counts %d and is open: %t", n, open)
		}
		if got := len(w.exchange(10).Claims) + len(other.exchange(10).Claims); got != 20 {
			t.Fatalf("with the breaker closed %d calls were admitted, want 20", got)
		}
	})
}

// TestATrialWhoseWorkerDiesIsNotTheLastTrial: a trial that never reports,
// because its worker died, leaves the breaker open with no trial in flight
// once its task is back in the queue, and the next claim is the trial.
func TestATrialWhoseWorkerDiesIsNotTheLastTrial(t *testing.T) {
	everywhere(t, readers(tasks.Pool{Reader: "only", MaxInFlight: 50}), func(t *testing.T, h *harness) {
		dying, live := h.worker(), h.worker()
		h.reading(dying, postgres.Submission{Parse: "prs_a", Group: "acme"}, 10)
		claims := dying.claim(3, 3)
		dying.settle(unhealthy(claims[0]), unhealthy(claims[1]), unhealthy(claims[2]))
		h.advance(tasks.DefaultBreakerOpen + 10*time.Second)
		live.exchange(0)
		dying.claim(5, 1)

		// The worker that holds the trial goes silent, and a live one finds
		// it dead.
		for range 5 {
			h.advance(third - time.Second)
			live.claim(5, 0)
		}
		if got := h.task("prs_a", "page-1"); got.State != tasks.Queued || got.Expiries != 1 {
			t.Fatalf("the trial of a dead worker is %+v", got)
		}
		if c := live.claim(5, 1)[0]; c.Reader != "only" || !c.Alone {
			t.Fatalf("after the trial's worker died the next claim is %+v", c)
		}
	})
}

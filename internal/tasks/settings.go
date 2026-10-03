// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// The defaults of the settings a task store runs with. Each is the value its
// spec gives, and the variable an operator overrides it with is named beside
// it where there is one.
const (
	DefaultLease         = 60 * time.Second // LECTIO_TASK_LEASE
	DefaultSweepInterval = 30 * time.Second // LECTIO_SWEEP_INTERVAL
	DefaultAttempts      = 5                // LECTIO_TASK_ATTEMPTS
	DefaultExpiries      = 3                // LECTIO_TASK_EXPIRIES

	// A retry waits min(DefaultBackoffCap, DefaultBackoffBase * 2^(attempt-1))
	// plus jitter uniform in half of that.
	DefaultBackoffBase = time.Second
	DefaultBackoffCap  = 60 * time.Second

	// LECTIO_CLASS_WEIGHTS: when both classes have work, interactive
	// receives 4 units in 5.
	DefaultInteractiveWeight = 4
	DefaultBatchWeight       = 1

	DefaultPoolRecovery = 30 * time.Second // LECTIO_POOL_RECOVERY
	DefaultPoolResume   = 10 * time.Second // LECTIO_POOL_RESUME
	// DefaultPoolPause is the pause of a rate-limit reply that names none.
	DefaultPoolPause = 5 * time.Second

	DefaultBreakerFailures = 3
	DefaultBreakerOpen     = 30 * time.Second
)

// MaxWeight is the largest weight a class, a group or a project carries.
const MaxWeight = 1000

// Pool is one reader as the queue sees it: how many calls it may have in
// flight across the fleet, and what one call to it is charged against a
// tenant's share.
type Pool struct {
	Reader      string
	MaxInFlight int
	// Cost is the fairness charge of one call. Zero takes 1.
	Cost int
}

// Settings are what a task store is run with. The zero value of a member
// takes its default, so the zero Settings are the specs' own with no reader
// configured.
type Settings struct {
	// Lease is the length of a worker's lease, and SweepInterval how often
	// the sweeps for dead workers and for deadlines run.
	Lease         time.Duration
	SweepInterval time.Duration

	// Attempts is how many retryable failures fail a task, and Expiries how
	// many dead workers do.
	Attempts int
	Expiries int

	BackoffBase time.Duration
	BackoffCap  time.Duration

	// InteractiveWeight and BatchWeight are the weights of the two classes.
	InteractiveWeight int
	BatchWeight       int

	// PoolRecovery is the quiet interval after which a limited scope's
	// ceiling rises, PoolResume the ramp after a pause ends, and PoolPause
	// the pause of a rate-limit reply that names none.
	PoolRecovery time.Duration
	PoolResume   time.Duration
	PoolPause    time.Duration

	// BreakerFailures consecutive failures open a reader's breaker for
	// BreakerOpen.
	BreakerFailures int
	BreakerOpen     time.Duration

	// KeysPerGroup says each group's pages are read with a key of its own,
	// so a rate limit pauses that group's calls to the reader and no other
	// group's. False is one key for every group: a rate limit pauses the
	// reader for all.
	KeysPerGroup bool

	// Pools are the configured readers. ReadChain is the routing policy's
	// order for a page, and ExtractChain its order for an extraction; a
	// task whose parse named a reader uses that reader alone.
	Pools        []Pool
	ReadChain    []string
	ExtractChain []string
}

// WithDefaults returns the settings with every zero member at its default.
func (s Settings) WithDefaults() Settings {
	duration := func(d *time.Duration, def time.Duration) {
		if *d == 0 {
			*d = def
		}
	}
	number := func(n *int, def int) {
		if *n == 0 {
			*n = def
		}
	}
	duration(&s.Lease, DefaultLease)
	duration(&s.SweepInterval, DefaultSweepInterval)
	number(&s.Attempts, DefaultAttempts)
	number(&s.Expiries, DefaultExpiries)
	duration(&s.BackoffBase, DefaultBackoffBase)
	duration(&s.BackoffCap, DefaultBackoffCap)
	number(&s.InteractiveWeight, DefaultInteractiveWeight)
	number(&s.BatchWeight, DefaultBatchWeight)
	duration(&s.PoolRecovery, DefaultPoolRecovery)
	duration(&s.PoolResume, DefaultPoolResume)
	duration(&s.PoolPause, DefaultPoolPause)
	number(&s.BreakerFailures, DefaultBreakerFailures)
	duration(&s.BreakerOpen, DefaultBreakerOpen)
	s.Pools = slices.Clone(s.Pools)
	for i := range s.Pools {
		number(&s.Pools[i].Cost, 1)
	}
	return s
}

// Validate reports everything in the settings a store cannot run with. It
// reads the settings as they are, so a caller applies WithDefaults first.
func (s Settings) Validate() error {
	var errs []error
	positive := func(name string, d time.Duration) {
		// A millisecond is the unit the store keeps a duration in.
		if d < time.Millisecond {
			errs = append(errs, fmt.Errorf("tasks: %s is below a millisecond", name))
		}
	}
	atLeast := func(name string, n, least int) {
		if n < least {
			errs = append(errs, fmt.Errorf("tasks: %s is %d, below %d", name, n, least))
		}
	}
	weight := func(name string, n int) {
		if n < 1 || n > MaxWeight {
			errs = append(errs, fmt.Errorf("tasks: %s is %d, outside 1 to %d", name, n, MaxWeight))
		}
	}
	positive("the lease", s.Lease)
	positive("the sweep interval", s.SweepInterval)
	atLeast("the attempts bound", s.Attempts, 1)
	atLeast("the expiries bound", s.Expiries, 1)
	positive("the backoff base", s.BackoffBase)
	if s.BackoffCap < s.BackoffBase {
		errs = append(errs, errors.New("tasks: the backoff cap is below the backoff base"))
	}
	weight("the interactive weight", s.InteractiveWeight)
	weight("the batch weight", s.BatchWeight)
	positive("the pool recovery interval", s.PoolRecovery)
	positive("the pool resume period", s.PoolResume)
	positive("the pool pause", s.PoolPause)
	atLeast("the breaker's failure count", s.BreakerFailures, 1)
	positive("the breaker's open period", s.BreakerOpen)

	names := map[string]bool{}
	for _, p := range s.Pools {
		switch {
		case p.Reader == "":
			errs = append(errs, errors.New("tasks: a pool names no reader"))
			continue
		case names[p.Reader]:
			errs = append(errs, fmt.Errorf("tasks: the reader %q has two pools", p.Reader))
		}
		names[p.Reader] = true
		atLeast(fmt.Sprintf("the reader %q's bound of calls in flight", p.Reader), p.MaxInFlight, 1)
		atLeast(fmt.Sprintf("the reader %q's cost", p.Reader), p.Cost, 1)
	}
	for _, chain := range []struct {
		name    string
		readers []string
	}{{"read", s.ReadChain}, {"extract", s.ExtractChain}} {
		for _, name := range chain.readers {
			if !names[name] {
				errs = append(errs, fmt.Errorf("tasks: the %s chain names %q, which has no pool", chain.name, name))
			}
		}
	}
	return errors.Join(errs...)
}

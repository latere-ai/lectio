// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"context"
	"errors"
	"sync"
	"time"
)

// A check is counted before it is made, and a schema whose check would
// cost too much is refused or its reply not checked (cost.go). That count
// is a model of a validator this package did not write, and a validation
// that began cannot be stopped. So a check is also made where being wrong
// about it costs a bounded amount: off the goroutine of the task that asked
// for it, for a bounded time, and among a bounded number of checks.
const (
	// MaxChecking is how many checks a process makes at once, the ones it
	// stopped waiting for included. A check that never ends holds 1 of
	// them and 1 processor, and with every one held the process holds no
	// reply to a schema until one ends.
	MaxChecking = 2

	// CheckDeadline is how long a task waits for a check to be made. The
	// count allows a check about 0.1 seconds, so one that takes 50 times
	// that is one the count was wrong about.
	CheckDeadline = 5 * time.Second
)

// ErrBusy is what a check is answered with when none could begin: for as
// long as a task waits for a check, every check of the process was taken.
var ErrBusy = errors.New("extract: every check of this process is taken")

// Bench makes the checks of a process. The zero value is ready.
type Bench struct {
	// Deadline is how long a check is waited for, to begin and then to
	// end. Zero is CheckDeadline.
	Deadline time.Duration
	// Hold is what holds an object to a schema. Nil is (*Schema).Check; a
	// test puts a slow one in its place.
	Hold func(s *Schema, data []byte, part bool) []Finding
	// Left is told when a check passed its deadline and was left to run.
	Left func()

	once  sync.Once
	slots chan struct{}

	mu   sync.Mutex
	late int
}

// Late is how many checks passed their deadline and still run.
func (b *Bench) Late() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.late
}

// Check holds an object to a schema as (*Schema).Check does, on a goroutine
// of its own. It waits up to the deadline for one of the process's checks
// to be free and answers ErrBusy when none is. It then waits up to the
// deadline for the check: one that has not ended by then is left to run,
// holding its place until it does, and the answer is the finding of an
// object that was not held to its schema. A context that ends answers its
// error, and a check that had begun is left the same way.
func (b *Bench) Check(ctx context.Context, s *Schema, data []byte, part bool) ([]Finding, error) {
	// A task that was taken away begins no check.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.once.Do(func() { b.slots = make(chan struct{}, MaxChecking) })
	deadline, hold := b.Deadline, b.Hold
	if deadline <= 0 {
		deadline = CheckDeadline
	}
	if hold == nil {
		hold = (*Schema).Check
	}

	free := time.NewTimer(deadline)
	defer free.Stop()
	select {
	case b.slots <- struct{}{}:
	case <-free.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// ended and left are of this check, and are read and written under the
	// bench's lock, so a check that ends as its task stops waiting is
	// counted late and then not, or not at all.
	var ended, left bool
	done := make(chan []Finding, 1)
	go func() {
		findings := hold(s, data, part)
		b.mu.Lock()
		ended = true
		if left {
			b.late--
		}
		b.mu.Unlock()
		done <- findings
		<-b.slots
	}()

	wait := time.NewTimer(deadline)
	defer wait.Stop()
	select {
	case findings := <-done:
		return findings, nil
	case <-wait.C:
	case <-ctx.Done():
	}
	b.mu.Lock()
	if ended {
		b.mu.Unlock()
		return <-done, nil
	}
	left = true
	b.late++
	b.mu.Unlock()
	if b.Left != nil {
		b.Left()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return costs("the check did not end in " + deadline.String()), nil
}

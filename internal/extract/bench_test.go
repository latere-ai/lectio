// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/lectio/reader"
)

// Take and Empty are the bench's, with a bench of their own, for the cases
// that are about what a reply does to an extraction and not about how long
// a check takes.
func Take(schema *Schema, in Input, p *Progress, res reader.ExtractResult) (Step, Result, []Finding) {
	step, result, findings, err := (&Bench{}).Take(context.Background(), schema, in, p, res)
	if err != nil {
		panic(err)
	}
	return step, result, findings
}

func Empty(schema *Schema) (Step, Result, []Finding) {
	step, result, findings, err := (&Bench{}).Empty(context.Background(), schema)
	if err != nil {
		panic(err)
	}
	return step, result, findings
}

// soon waits for something another goroutine brings about.
func soon(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("it did not happen: %s", what)
}

// TestACheckThatDoesNotEndGivesItsTaskBack: a validation cannot be stopped,
// so a check runs on a goroutine of its own and its task waits for it a
// bounded time. A check that has not ended by then is left to run and the
// object is answered as one that was not held to its schema. A process
// makes 2 checks at once: with both left running, a third is not begun,
// and is answered as busy after the same wait. When a check that was left
// ends, its place is free again. An extraction whose check was not made
// says so and moves nowhere.
func TestACheckThatDoesNotEndGivesItsTaskBack(t *testing.T) {
	s := compiled(t, invoice)
	release := make(chan struct{})
	var began, left atomic.Int32
	b := &Bench{Deadline: 30 * time.Millisecond, Left: func() { left.Add(1) }}
	b.Hold = func(s *Schema, data []byte, part bool) []Finding {
		began.Add(1)
		<-release
		return s.Check(data, part)
	}
	ctx := context.Background()
	in := Input{Extractor: "text", Windows: []Window{{Text: "[1.1] Invoice INV-0042", Refs: []string{"1.1"}}}}
	good := answer(`{"number":"INV-0042"}`, nil)

	for i := range MaxChecking {
		start := time.Now()
		var p Progress
		step, _, findings, err := b.Take(ctx, s, in, &p, good)
		took := time.Since(start)
		if err != nil || step != Unsatisfied || !unchecked(findings) || p.Repairs != 0 || took < b.Deadline || took > 5*time.Second {
			t.Fatalf("check %d that does not end: step %d, %+v, %v after %s", i+1, step, findings, err, took)
		}
		if got := Broken(findings); got != "the object was not held to the schema, which costs too much to check it against: the check did not end in 30ms" {
			t.Fatalf("the field would say %q", got)
		}
	}
	if b.Late() != MaxChecking || left.Load() != MaxChecking || began.Load() != MaxChecking {
		t.Fatalf("%d checks began, %d were left and %d are late", began.Load(), left.Load(), b.Late())
	}

	// No third check begins while both run.
	if _, err := b.Check(ctx, s, []byte(`{}`), false); !errors.Is(err, ErrBusy) {
		t.Fatalf("a third check: %v", err)
	}
	if _, _, _, err := b.Empty(ctx, s); !errors.Is(err, ErrBusy) {
		t.Fatalf("an extraction of a document with no text: %v", err)
	}
	var p Progress
	if _, _, _, err := b.Take(ctx, s, in, &p, good); !errors.Is(err, ErrBusy) || len(p.Parts) != 0 {
		t.Fatalf("an extraction with no check to be had: %v, %+v", err, p)
	}
	if began.Load() != MaxChecking {
		t.Fatalf("%d checks began", began.Load())
	}
	// A task that is taken away while it waits for a check stops waiting.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := b.Check(gone, s, []byte(`{}`), false); !errors.Is(err, context.Canceled) {
		t.Fatalf("a check for a task that was taken away: %v", err)
	}

	// The checks that were left end, and the bench checks again.
	close(release)
	soon(t, "the checks that were left ended", func() bool { return b.Late() == 0 })
	var again Progress
	step, result, findings, err := b.Take(ctx, s, in, &again, good)
	if err != nil || step != Filled || findings != nil || string(result.Data) != `{"number":"INV-0042"}` {
		t.Fatalf("a check after the others ended: step %d, %+v, %v", step, findings, err)
	}
	if step, _, findings, err := b.Empty(ctx, s); err != nil || step != Unsatisfied || len(findings) != 1 {
		t.Fatalf("a document with no text, held to a schema that requires a member: step %d, %+v, %v", step, findings, err)
	}

	// A check whose task is taken away while it runs is left as one that
	// passed its deadline is, and counted until it ends.
	hold := make(chan struct{})
	slow := &Bench{Hold: func(*Schema, []byte, bool) []Finding { <-hold; return nil }}
	gone, cancel = context.WithCancel(ctx)
	time.AfterFunc(10*time.Millisecond, cancel)
	if _, err := slow.Check(gone, s, []byte(`{}`), false); !errors.Is(err, context.Canceled) || slow.Late() != 1 {
		t.Fatalf("a check whose task was taken away: %v, %d late", err, slow.Late())
	}
	close(hold)
	soon(t, "the check ended", func() bool { return slow.Late() == 0 })

	// A document in windows whose merged object cannot be checked says so.
	windows := Input{Extractor: "text", Windows: []Window{{Text: "[1.1] a", Refs: []string{"1.1"}}, {Text: "[2.1] b", Refs: []string{"2.1"}}}}
	calls := 0
	merging := &Bench{Hold: func(s *Schema, data []byte, part bool) []Finding {
		if calls++; calls == 3 {
			return costs("the check did not end in 30ms")
		}
		return s.Check(data, part)
	}}
	var parts Progress
	if step, _, _, err := merging.Take(ctx, s, windows, &parts, good); err != nil || step != Next {
		t.Fatalf("the first window: step %d, %v", step, err)
	}
	if step, _, findings, err := merging.Take(ctx, s, windows, &parts, good); err != nil || step != Unsatisfied || !unchecked(findings) {
		t.Fatalf("a merged object that was not checked: step %d, %+v, %v", step, findings, err)
	}
	// A task taken away between the check of its last window and the
	// check of the merged object makes no second check.
	gone, cancel = context.WithCancel(ctx)
	checks := 0
	taken := &Bench{Hold: func(s *Schema, data []byte, part bool) []Finding {
		checks++
		cancel()
		return s.Check(data, part)
	}}
	parts = Progress{Parts: []Part{{Data: []byte(`{}`)}}}
	if _, _, _, err := taken.Take(gone, s, windows, &parts, good); !errors.Is(err, context.Canceled) || checks != 1 {
		t.Fatalf("a merged object of a task that was taken away: %v after %d checks", err, checks)
	}
}

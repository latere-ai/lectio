// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// ledger is an object store that records what is written under the fields
// and the figures of a parse and what is removed, in order, and fails the
// removal of a key a case names.
type ledger struct {
	blob.Store
	mu         sync.Mutex
	log        []string
	failDelete string
}

func (l *ledger) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if strings.Contains(key, "/fields/") || strings.Contains(key, "/figures/") {
		l.mu.Lock()
		l.log = append(l.log, "put "+key)
		l.mu.Unlock()
	}
	return l.Store.Put(ctx, key, data, contentType)
}

func (l *ledger) Delete(ctx context.Context, key string) error {
	l.mu.Lock()
	l.log = append(l.log, "delete "+key)
	fails := key == l.failDelete
	l.mu.Unlock()
	if fails {
		return errDown
	}
	return l.Store.Delete(ctx, key)
}

// did lists what the ledger recorded.
func (l *ledger) did() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.log)
}

// left lists the objects under the fields and the figures of a parse.
func (b *bench) left() []string {
	b.t.Helper()
	var out []string
	for _, key := range objectKeys(b.t, b.objects) {
		if strings.Contains(key, "/fields/") || strings.Contains(key, "/figures/") {
			out = append(out, key)
		}
	}
	return out
}

// long is a bench whose extractor reads the document in 4 windows, with a
// claim of the extraction queued and the objects it writes recorded.
func long(t *testing.T, ext *asking) (*bench, *ledger) {
	t.Helper()
	ext.maxInput = 64
	b := extracting(t, ext)
	index := b.assembled("prs_a",
		leaf(1, "Invoice INV-0042", "Item bolt 3"), leaf(2, "Item nut 4", "Item gear 9"),
		leaf(3, "Item bolt 3", "Item cog 1"), leaf(4, "Total: 17", "Paid in full"))
	kept := &ledger{Store: b.objects}
	b.w.Objects = kept
	b.store.queue = append(b.store.queue, extraction("prs_a", "invoice", 5, index, invoiceSchema, ""))
	return b, kept
}

// TestWhatARefusedExtractionWroteIsRemoved: the first claim of a long
// extraction keeps the document as it reads it, with the whole text, and
// what it has so far, under its token. When the store refuses its settle,
// for a parse that was deleted while the call ran, nothing names those
// objects and the delete listed the parse's objects before they were
// written, so the worker removes them. A page's result is left: its parse
// is one that is being read.
func TestWhatARefusedExtractionWroteIsRemoved(t *testing.T) {
	b, kept := long(t, &asking{})
	b.store.script = refusing
	stop := b.run()
	input, progress := blob.FieldInputKey("prs_a", "invoice", 5), blob.FieldProgressKey("prs_a", "invoice", 5)
	eventually(t, "what the extraction wrote is removed", func() bool {
		return slices.Contains(kept.did(), "delete "+blob.FieldKey("prs_a", "invoice", 5))
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	want := []string{"put " + input, "put " + progress, "delete " + input, "delete " + progress, "delete " + blob.FieldKey("prs_a", "invoice", 5)}
	if got := kept.did(); !slices.Equal(got, want) {
		t.Fatalf("the object store was asked\n  %v\nwant\n  %v", got, want)
	}
	if left := b.left(); len(left) != 0 {
		t.Fatalf("of the extraction the object store holds %v", left)
	}

	b = newBench(t, nil)
	b.queued("other", 1)
	kept = &ledger{Store: b.objects}
	b.w.Objects, b.store.script = kept, refusing
	stop = b.run()
	eventually(t, "the page's settle is sent", func() bool { return len(b.store.settles()) == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got := kept.did(); len(got) != 0 || !slices.Contains(objectKeys(t, b.objects), blob.PageKey("other_a", 1, 1)) {
		t.Fatalf("after a refused page settle the object store was asked %v and holds %v", got, objectKeys(t, b.objects))
	}
}

// refusing is a store that refuses every settle it is sent.
func refusing(_ int, req tasks.Request, reply *tasks.Reply) error {
	for _, s := range req.Settles {
		reply.Refused = append(reply.Refused, tasks.Ref{Parse: s.Parse, Task: s.Task})
	}
	return nil
}

// TestWhatATaskTakenAwayMidCallWroteIsRemoved: an extraction that is told
// it lost its task while its call is in flight has written the document as
// it reads it. Its run is stopped, nothing is settled, and what it wrote is
// removed once the run has returned. The same holds for every task of a
// worker the fleet gave up.
func TestWhatATaskTakenAwayMidCallWroteIsRemoved(t *testing.T) {
	for name, told := range map[string]func(req tasks.Request, reply *tasks.Reply){
		"the task is lost": func(req tasks.Request, reply *tasks.Reply) {
			reply.Lost = []tasks.Ref{{Parse: req.Held[0].Parse, Task: req.Held[0].Task}}
		},
		"the worker is given up": func(_ tasks.Request, reply *tasks.Reply) { reply.Gone = true },
	} {
		t.Run(name, func(t *testing.T) {
			// The call waits until its task was taken away.
			entered, gone := make(chan struct{}, 1), make(chan struct{})
			b, kept := long(t, &asking{answers: func(_ int, req reader.ExtractRequest) (reader.ExtractResult, error) {
				entered <- struct{}{}
				<-gone
				return honest(req.Text, false), nil
			}})
			once := sync.Once{}
			b.store.script = func(_ int, req tasks.Request, reply *tasks.Reply) error {
				if len(req.Held) == 1 {
					once.Do(func() { told(req, reply); close(gone) })
				}
				return nil
			}
			stop := b.run()
			<-entered
			input := blob.FieldInputKey("prs_a", "invoice", 5)
			eventually(t, "what the extraction wrote is removed", func() bool { return slices.Contains(kept.did(), "delete "+input) })
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			if got := b.store.settles(); len(got) != 0 {
				t.Fatalf("a task that was taken away was settled: %+v", got)
			}
			if got := kept.did(); got[0] != "put "+input || slices.Index(got, "delete "+input) < slices.Index(got, "put "+blob.FieldProgressKey("prs_a", "invoice", 5)) {
				t.Fatalf("the object store was asked %v", got)
			}
			if left := b.left(); len(left) != 0 {
				t.Fatalf("of the extraction the object store holds %v", left)
			}
		})
	}
}

// TestASettleThatWasSentAgainKeepsWhatItWrote: an exchange the store did
// not answer may have been taken all the same. Its settles are sent again,
// and one that is refused then may be one the store recorded the first
// time: what it wrote is named by a row, and is not removed.
func TestASettleThatWasSentAgainKeepsWhatItWrote(t *testing.T) {
	b, kept := long(t, &asking{})
	failed := false
	b.store.script = func(_ int, req tasks.Request, reply *tasks.Reply) error {
		if len(req.Settles) == 0 {
			return nil
		}
		if !failed {
			failed = true
			return errors.New("the database does not answer")
		}
		reply.Refused = []tasks.Ref{{Parse: req.Settles[0].Parse, Task: req.Settles[0].Task}}
		return nil
	}
	stop := b.run()
	eventually(t, "the settle is sent again and refused", func() bool {
		seen := b.store.seen()
		return len(seen) > 3 && len(seen[len(seen)-1].Settles) == 0 && len(b.store.settles()) == 2
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	want := []string{blob.FieldInputKey("prs_a", "invoice", 5), blob.FieldProgressKey("prs_a", "invoice", 5)}
	if left := b.left(); !slices.Equal(left, want) {
		t.Fatalf("after a settle that was sent again was refused the object store holds %v, and was asked %v", left, kept.did())
	}
}

// TestWhatARefusedFigureWroteIsRemoved: a figure's description is written
// under its task's token before the settle. A settle the store refuses
// leaves it named by nothing, and it is removed. A removal that fails is
// said and stops nothing. A settle that was sent before, and is refused in
// the worker's last exchange, keeps what it wrote.
func TestWhatARefusedFigureWroteIsRemoved(t *testing.T) {
	key := blob.FigureKey("prs_a", "1.2", 7)
	figure := func(t *testing.T) (*bench, *ledger, *bytes.Buffer) {
		b := describing(t, &stub.Describer{})
		kept, logged := &ledger{Store: b.objects}, &bytes.Buffer{}
		b.w.Objects, b.w.Log = kept, slog.New(slog.NewTextHandler(logged, nil))
		b.store.queue = append(b.store.queue, described("1.2", 7, illustratedPage, ""))
		b.store.script = refusing
		return b, kept, logged
	}
	t.Run("removed", func(t *testing.T) {
		b, kept, _ := figure(t)
		stop := b.run()
		eventually(t, "the description is removed", func() bool { return slices.Contains(kept.did(), "delete "+key) })
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		if got := kept.did(); !slices.Equal(got, []string{"put " + key, "delete " + key}) || len(b.left()) != 0 {
			t.Fatalf("the object store was asked %v and holds %v", got, b.left())
		}
	})
	t.Run("a removal that fails", func(t *testing.T) {
		b, kept, logged := figure(t)
		kept.failDelete = key
		stop := b.run()
		eventually(t, "the removal is tried", func() bool { return slices.Contains(kept.did(), "delete "+key) })
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logged.String(), "was not removed") || !strings.Contains(logged.String(), "task=figure-1.2") {
			t.Fatalf("a removal that failed was not said: %s", logged.String())
		}
		if left := b.left(); !slices.Equal(left, []string{key}) {
			t.Fatalf("the object store holds %v", left)
		}
	})
	t.Run("sent again in the last exchange", func(t *testing.T) {
		b, kept, _ := figure(t)
		b.store.script = func(n int, req tasks.Request, reply *tasks.Reply) error {
			if !req.Shutdown && len(req.Settles) > 0 {
				return errors.New("the database does not answer")
			}
			return refusing(n, req, reply)
		}
		stop := b.run()
		eventually(t, "the settle was sent and not answered", func() bool { return len(b.store.settles()) > 0 })
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		seen := b.store.seen()
		if final := seen[len(seen)-1]; !final.Shutdown || len(final.Settles) != 1 {
			t.Fatalf("the last exchange was %+v", final)
		}
		if got := kept.did(); !slices.Equal(got, []string{"put " + key}) {
			t.Fatalf("the object store was asked %v", got)
		}
	})
}

// held is a describer whose call waits until a case releases it.
type held struct {
	stub.Describer
	entered, release chan struct{}
}

func (h *held) DescribeFigure(ctx context.Context, req reader.FigureRequest) (reader.FigureResult, error) {
	h.entered <- struct{}{}
	<-h.release
	return h.Describer.DescribeFigure(ctx, req)
}

// TestASettleRefusedInTheLastExchangeRemovesWhatItWrote: a worker that is
// stopping settles what finishes in its grace period in its last exchange.
// A settle refused there, sent for the first time, has what it wrote
// removed before the worker ends.
func TestASettleRefusedInTheLastExchangeRemovesWhatItWrote(t *testing.T) {
	d := &held{entered: make(chan struct{}, 1), release: make(chan struct{})}
	b := describing(t, d)
	kept := &ledger{Store: b.objects}
	b.w.Objects = kept
	b.store.queue = append(b.store.queue, described("1.2", 7, illustratedPage, ""))
	b.store.script = refusing

	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() { ended <- b.w.Run(ctx) }()
	<-d.entered
	// The worker is told to stop while the call runs: it exchanges no more
	// until its last exchange, and the call returns within its grace period.
	cancel()
	close(d.release)
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	key := blob.FigureKey("prs_a", "1.2", 7)
	if got := kept.did(); !slices.Equal(got, []string{"put " + key, "delete " + key}) || len(b.left()) != 0 {
		t.Fatalf("the object store was asked %v and holds %v", got, b.left())
	}
	seen := b.store.seen()
	if final := seen[len(seen)-1]; !final.Shutdown || len(final.Settles) != 1 {
		t.Fatalf("the last exchange was %+v", final)
	}
}

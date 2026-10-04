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
	"time"

	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/store/postgres"
)

// retaining is a store that lists what a case says has expired, each time
// the sweep asks, and records what the sweep removes in the order it does.
type retaining struct {
	mu sync.Mutex

	// due is what Expired answers. A parse or a file leaves it when its
	// row is removed, as it leaves the real store's listing.
	due postgres.Expired
	// asked counts the sweeps, and failing is what the next calls fail
	// with, by name.
	asked   int
	failing map[string]error
	// log is every call the sweep made, in order.
	log []string
	// kept are the parses the store says are no longer to be removed when
	// the sweep comes to them.
	kept []string
}

func (r *retaining) CloseParse(_ context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failing["CloseParse"]; err != nil {
		return false, err
	}
	if slices.Contains(r.kept, id) {
		return false, nil
	}
	r.log = append(r.log, "work "+id)
	return true, nil
}

func (r *retaining) Expired(context.Context, int) (postgres.Expired, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked++
	if err := r.failing["Expired"]; err != nil {
		return postgres.Expired{}, err
	}
	return postgres.Expired{Due: true, Parses: slices.Clone(r.due.Parses), Files: slices.Clone(r.due.Files)}, nil
}

func (r *retaining) ExpireParse(_ context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failing["ExpireParse"]; err != nil {
		return false, err
	}
	r.log = append(r.log, "rows "+id)
	r.due.Parses = slices.DeleteFunc(r.due.Parses, func(p string) bool { return p == id })
	return true, nil
}

func (r *retaining) ForgetFile(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failing["ForgetFile"]; err != nil {
		return err
	}
	r.log = append(r.log, "row "+id)
	r.due.Files = slices.DeleteFunc(r.due.Files, func(f postgres.ExpiredFile) bool { return f.ID == id })
	return nil
}

func (r *retaining) note(what string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, what)
}

// noting is an object store that tells the store of the case what it
// removes, and fails what a case says.
type noting struct {
	blob.Store
	into       *retaining
	failDelete string // a key whose delete fails
	failList   bool
}

func (n *noting) Delete(ctx context.Context, key string) error {
	if key == n.failDelete {
		return errDown
	}
	n.into.note("object " + key)
	return n.Store.Delete(ctx, key)
}

func (n *noting) List(ctx context.Context, prefix string) ([]string, error) {
	if n.failList {
		return nil, errDown
	}
	return n.Store.List(ctx, prefix)
}

// sweeping is a worker with a retention store and objects of 2 parses and
// 2 files, of which one parse and one file have expired.
func sweeping(t *testing.T) (*Worker, *retaining, *noting, *bytes.Buffer) {
	t.Helper()
	b := newBench(t, nil)
	for _, key := range []string{
		blob.PageKey("prs_old", 1, 7), blob.ImageKey("prs_old", 1, 7, "image/png"), blob.IndexKey("prs_old", 9),
		blob.PageKey("prs_new", 1, 3),
		"sources/a/aa/fil_old", "sources/a/bb/fil_new",
	} {
		b.put(key, []byte("x"), "")
	}
	store := &retaining{
		due:     postgres.Expired{Parses: []string{"prs_old"}, Files: []postgres.ExpiredFile{{ID: "fil_old", Key: "sources/a/aa/fil_old"}}},
		failing: map[string]error{},
	}
	objects := &noting{Store: b.objects, into: store}
	var logged bytes.Buffer
	b.w.Retention, b.w.Objects, b.w.Log = store, objects, slog.New(slog.NewTextHandler(&logged, nil))
	return b.w, store, objects, &logged
}

// objectKeys lists what the object store holds.
func objectKeys(t *testing.T, s blob.Store) []string {
	t.Helper()
	got, err := s.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestTheSweepRemovesObjectsAndThenRows: what has expired leaves no object
// and no row, and what has not is not touched. The work on a parse is
// stopped before its objects are listed, every object of it is removed
// before its rows, and a file's object before its row. A parse the store
// says is no longer one to remove when the sweep comes to it is left
// whole.
func TestTheSweepRemovesObjectsAndThenRows(t *testing.T) {
	w, store, objects, logged := sweeping(t)
	w.sweep(context.Background())

	want := []string{
		"work prs_old",
		"object " + blob.IndexKey("prs_old", 9), "object " + blob.PageKey("prs_old", 1, 7), "object " + blob.ImageKey("prs_old", 1, 7, "image/png"),
		"rows prs_old", "object sources/a/aa/fil_old", "row fil_old",
	}
	if !slices.Equal(store.log, want) {
		t.Fatalf("the sweep did\n  %v\nwant\n  %v", store.log, want)
	}
	if left := objectKeys(t, objects.Store); !slices.Equal(left, []string{blob.PageKey("prs_new", 1, 3), "sources/a/bb/fil_new"}) {
		t.Fatalf("after the sweep the object store holds %v", left)
	}
	if !strings.Contains(logged.String(), "parses=1 files=1") {
		t.Errorf("the sweep did not say what it removed: %s", logged.String())
	}
	// With nothing expired the sweep removes nothing and says nothing.
	logged.Reset()
	w.sweep(context.Background())
	if len(store.log) != len(want) || logged.Len() != 0 {
		t.Fatalf("a sweep with nothing to remove did %v and logged %q", store.log[len(want):], logged.String())
	}

	// A parse that was listed and is no longer one to remove: nothing of
	// it is touched.
	store.due.Parses, store.kept = []string{"prs_new"}, []string{"prs_new"}
	w.sweep(context.Background())
	if len(store.log) != len(want) || !slices.Contains(objectKeys(t, objects.Store), blob.PageKey("prs_new", 1, 3)) {
		t.Fatalf("a parse that is not to be removed: the sweep did %v", store.log[len(want):])
	}
}

// TestAnInterruptedSweepCompletesOnTheNextRun: an object that could not be
// removed keeps its row, so nothing is forgotten that still has an object,
// and the sweep after it removes both. The same holds for a listing that
// failed and for a row the store did not remove.
func TestAnInterruptedSweepCompletesOnTheNextRun(t *testing.T) {
	for name, breakIt := range map[string]func(*retaining, *noting){
		"a parse's work stays":    func(r *retaining, _ *noting) { r.failing["CloseParse"] = errors.New("the database does not answer") },
		"a parse's object stays":  func(_ *retaining, o *noting) { o.failDelete = blob.PageKey("prs_old", 1, 7) },
		"a parse's listing fails": func(_ *retaining, o *noting) { o.failList = true },
		"a parse's rows stay":     func(r *retaining, _ *noting) { r.failing["ExpireParse"] = errors.New("the database does not answer") },
		"a file's object stays":   func(_ *retaining, o *noting) { o.failDelete = "sources/a/aa/fil_old" },
		"a file's row stays":      func(r *retaining, _ *noting) { r.failing["ForgetFile"] = errors.New("the database does not answer") },
		"the store does not say":  func(r *retaining, _ *noting) { r.failing["Expired"] = errors.New("the database does not answer") },
	} {
		t.Run(name, func(t *testing.T) {
			w, store, objects, logged := sweeping(t)
			breakIt(store, objects)
			w.sweep(context.Background())

			// Whatever stopped the sweep, no object went before the work on
			// its parse was stopped, and no row went before its objects.
			if strings.HasPrefix(name, "a parse's work") && len(objectKeys(t, objects.Store)) != 5 {
				t.Fatalf("with the work not stopped the object store holds %v", objectKeys(t, objects.Store))
			}
			for i, step := range store.log {
				if step == "rows prs_old" {
					for _, key := range objectKeys(t, objects.Store) {
						if strings.HasPrefix(key, blob.ParsePrefix("prs_old")) {
							t.Fatalf("the rows of the parse were removed at step %d while %s was there", i, key)
						}
					}
				}
			}
			parseKept := slices.Contains(store.due.Parses, "prs_old")
			fileKept := len(store.due.Files) == 1
			if !parseKept && !fileKept {
				t.Fatalf("nothing was left to sweep again: %v", store.log)
			}
			if fileKept && strings.HasPrefix(name, "a file's object") && !slices.Contains(objectKeys(t, objects.Store), "sources/a/aa/fil_old") {
				t.Fatal("the file's object is gone and its row is not")
			}
			if !strings.Contains(logged.String(), "level=WARN") {
				t.Errorf("a sweep that could not finish said nothing: %q", logged.String())
			}

			// The fault passes, and the next run finishes the job.
			objects.failDelete, objects.failList, store.failing = "", false, map[string]error{}
			w.sweep(context.Background())
			if len(store.due.Parses) != 0 || len(store.due.Files) != 0 {
				t.Fatalf("after the next sweep %+v is still listed", store.due)
			}
			if left := objectKeys(t, objects.Store); !slices.Equal(left, []string{blob.PageKey("prs_new", 1, 3), "sources/a/bb/fil_new"}) {
				t.Fatalf("after the next sweep the object store holds %v", left)
			}
		})
	}
}

// TestAWorkerSweepsWhileItRuns: a worker with a retention store asks it on
// its own interval for as long as it runs, and stops asking when it stops.
// One with none runs no sweep. A sweep whose context has ended removes
// nothing more.
func TestAWorkerSweepsWhileItRuns(t *testing.T) {
	b := newBench(t, nil)
	store := &retaining{failing: map[string]error{}}
	b.w.Retention, b.w.Sweep = store, 2*time.Millisecond
	stop := b.run()
	asked := func() int {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.asked
	}
	eventually(t, "the worker asks what has expired, and asks again", func() bool { return asked() >= 3 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	after := asked()
	time.Sleep(10 * time.Millisecond)
	if asked() != after {
		t.Fatalf("a stopped worker asked %d more times", asked()-after)
	}

	// The interval a worker takes when none is set.
	idle := newBench(t, nil)
	idle.w.Retention = store
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { idle.w.retain(ctx); close(done) }()
	cancel()
	<-done
	if asked() != after {
		t.Fatal("a sweep on the default interval ran within milliseconds")
	}

	// A sweep that is stopped leaves the rest for the next one.
	w, swept, objects, _ := sweeping(t)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	w.sweep(stopped)
	if len(swept.log) != 0 || len(objectKeys(t, objects.Store)) != 6 {
		t.Fatalf("a sweep whose context had ended did %v", swept.log)
	}
	swept.due.Parses = nil
	w.sweep(stopped)
	if len(swept.log) != 0 {
		t.Fatalf("a sweep whose context had ended removed a file: %v", swept.log)
	}
	// A store that fails because the worker is stopping is not a warning.
	swept.failing["Expired"] = context.Canceled
	var logged bytes.Buffer
	w.Log = slog.New(slog.NewTextHandler(&logged, nil))
	w.sweep(stopped)
	if logged.Len() != 0 {
		t.Fatalf("a sweep that was stopped warned: %s", logged.String())
	}
}

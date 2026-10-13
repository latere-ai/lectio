// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// storedPolicy is a routing policy as the API stores it, naming the reader
// "added" in its describe chain.
const storedPolicy = `{"read":{"chain":["stub"]},"describe":{"chain":["added"]},"extract":{"chain":["stub"]}}`

// TestStoredReadersAndThePolicyAreKeptAsSent: a stored reader is kept under
// its name as it was sent, and replaced under it; the stored policy is kept
// until it is removed. Every change adds 1 to the version a process
// watches, and nothing else does.
func TestStoredReadersAndThePolicyAreKeptAsSent(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		version := func() int64 {
			t.Helper()
			v, err := h.store.ReaderVersion(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		if st, err := h.store.ReaderState(ctx); err != nil || st.Version != 0 || st.Policy != nil || len(st.Readers) != 0 {
			t.Fatalf("a new store holds %+v, %v", st, err)
		}

		const spec = `{"adapter":"chat",  "model":"m"}`
		if created, err := h.store.PutReader(ctx, "added", spec); err != nil || !created {
			t.Fatalf("storing a reader: created %t, %v", created, err)
		}
		if created, err := h.store.PutReader(ctx, "added", `{"adapter":"chat","model":"n"}`); err != nil || created {
			t.Fatalf("replacing a reader: created %t, %v", created, err)
		}
		if _, err := h.store.PutReader(ctx, "another", spec); err != nil {
			t.Fatal(err)
		}
		if v := version(); v != 3 {
			t.Fatalf("after 3 changes the version is %d", v)
		}
		if err := h.store.PutPolicy(ctx, storedPolicy); err != nil {
			t.Fatal(err)
		}
		st, err := h.store.ReaderState(ctx)
		if err != nil || st.Version != 4 || st.Policy == nil || *st.Policy != storedPolicy || len(st.Readers) != 2 ||
			st.Readers[0] != (postgres.StoredReader{Name: "added", Spec: `{"adapter":"chat","model":"n"}`}) ||
			st.Readers[1] != (postgres.StoredReader{Name: "another", Spec: spec}) {
			t.Fatalf("the store holds %+v, %v", st, err)
		}
		if err := h.store.DeletePolicy(ctx); err != nil {
			t.Fatal(err)
		}
		if st, err := h.store.ReaderState(ctx); err != nil || st.Version != 5 || st.Policy != nil {
			t.Fatalf("after the policy was removed the store holds %+v, %v", st, err)
		}
		// Writing the settings is no change of what is stored.
		if err := h.store.Configure(ctx, defaults()); err != nil {
			t.Fatal(err)
		}
		if v := version(); v != 5 {
			t.Fatalf("writing the settings moved the version to %d", v)
		}
	})
}

// TestAStoredReaderThatIsNamedIsNotRemoved: a stored reader is removed only
// once nothing names it: not a chain in force, not the stored policy, not a
// set's definition, and no parse, task or extraction that has not ended,
// any of which would leave work waiting for a reader nobody runs. A name
// that is not stored is not found.
func TestAStoredReaderThatIsNamedIsNotRemoved(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		refusedAs := func(what string, code fault.Code) {
			t.Helper()
			refused(t, what, h.store.DeleteReader(ctx, "added"), code)
			if st, err := h.store.ReaderState(ctx); err != nil || len(st.Readers) != 1 {
				t.Fatalf("a refused removal changed what is stored: %+v, %v", st, err)
			}
		}
		refused(t, "a reader that is not stored", h.store.DeleteReader(ctx, "added"), fault.NotFound)
		if _, err := h.store.PutReader(ctx, "added", `{"adapter":"chat","model":"m"}`); err != nil {
			t.Fatal(err)
		}

		// A chain in force names it.
		inForce := defaults()
		inForce.Pools = append(inForce.Pools, tasks.Pool{Reader: "added", MaxInFlight: 10})
		inForce.ExtractChain = []string{stub, "added"}
		if err := h.store.Configure(ctx, inForce); err != nil {
			t.Fatal(err)
		}
		refusedAs("a reader a chain in force names", fault.Conflict)

		// The stored policy names it, and no process has written it yet.
		if err := h.store.Configure(ctx, defaults()); err != nil {
			t.Fatal(err)
		}
		if err := h.store.PutPolicy(ctx, storedPolicy); err != nil {
			t.Fatal(err)
		}
		refusedAs("a reader the stored policy names", fault.Conflict)
		if err := h.store.DeletePolicy(ctx); err != nil {
			t.Fatal(err)
		}

		// A set's definition names it.
		if _, err := h.store.CreateSet(ctx, postgres.SetRequest{ID: "set_1", Owner: "alice", Name: "harbor", Request: asked, Pin: "added"}); err != nil {
			t.Fatal(err)
		}
		refusedAs("a reader a set names", fault.Conflict)
		if err := h.store.DeleteSet(ctx, "set_1"); err != nil {
			t.Fatal(err)
		}

		// A parse that has not ended names it.
		if err := h.store.Configure(ctx, inForce); err != nil {
			t.Fatal(err)
		}
		inForce.ExtractChain = []string{stub}
		if err := h.store.Configure(ctx, inForce); err != nil {
			t.Fatal(err)
		}
		h.submit(postgres.Submission{Parse: "prs_a", Group: "acme", Owner: "alice", Pin: "added"})
		refusedAs("a reader a parse that has not ended names", fault.Conflict)
		if err := h.store.Cancel(ctx, "prs_a"); err != nil {
			t.Fatal(err)
		}

		// An extraction that is pending names it.
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_b", Group: "acme", Owner: "alice"}, 1)
		h.field("prs_b", "invoice", "added")
		refusedAs("a reader an extraction that is pending names", fault.Conflict)
		c := w.claim(1, 1)[0]
		if c.Reader != "added" {
			t.Fatalf("the pinned extraction was claimed for %s", c.Reader)
		}
		w.settle(filledBy(c))

		if err := h.store.DeleteReader(ctx, "added"); err != nil {
			t.Fatal(err)
		}
		if st, err := h.store.ReaderState(ctx); err != nil || len(st.Readers) != 0 {
			t.Fatalf("after the removal the store holds %+v, %v", st, err)
		}
		h.consistent()
	})
}

// TestAShorterChainTakesTheWorkPastItsEnd: a page that waits at the second
// place of a chain of 2, after the first reader declined it, waits at the
// last place of a chain of 1 once a process writes it, and is claimed for
// that reader. Without it the page would wait for a place nobody reads.
func TestAShorterChainTakesTheWorkPastItsEnd(t *testing.T) {
	everywhere(t, chain(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 1)
		w.settle(declined(w.claim(1, 1)[0], "page_unreadable"))
		c := w.claim(1, 1)[0]
		if c.Reader != "second" {
			t.Fatalf("the declined page was claimed for %s", c.Reader)
		}
		held := ended(c, tasks.Returned, "")
		held.Error = nil
		w.settle(held)
		if got := h.task("prs_a", "page-1"); got.ChainAt != 1 || got.State != tasks.Queued || got.Lane != "page@1" {
			t.Fatalf("the page waiting at the second place is %+v", got)
		}

		shorter := readers(tasks.Pool{Reader: "third", MaxInFlight: 100, Cost: 9})
		if err := h.store.Configure(ctx, shorter); err != nil {
			t.Fatal(err)
		}
		if got := h.task("prs_a", "page-1"); got.ChainAt != 0 || got.Lane != "page" || got.State != tasks.Queued {
			t.Fatalf("after the chain was shortened the page is %+v", got)
		}
		h.consistent()
		h.advance(time.Second)
		if c := w.claim(1, 1)[0]; c.Reader != "third" {
			t.Fatalf("the page was claimed for %s", c.Reader)
		}
		h.consistent()
	})
}

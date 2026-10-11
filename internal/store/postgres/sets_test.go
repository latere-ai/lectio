// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// other is a definition that is not the suite's: a schema with another
// member.
const other = `{"schema":{"type":"object","properties":{"number":{"type":"string"}}},"citations":true}`

// makeSet writes a set of an owner, defined as the suite's extractions are
// asked, and returns it.
func (h *harness) makeSet(id, owner, name string) postgres.Set {
	h.t.Helper()
	s, err := h.store.CreateSet(context.Background(), postgres.SetRequest{ID: id, Owner: owner, Name: name, Request: asked})
	if err != nil {
		h.t.Fatalf("making the set %s: %v", name, err)
	}
	return s
}

// join adds a parse to a set and returns whether an extraction was asked and
// the result an asking replaced.
func (h *harness) join(set, parse string) (bool, string) {
	h.t.Helper()
	asked, replaced, err := h.store.JoinSet(context.Background(), postgres.SetJoin{Set: set, Parse: parse, Deadline: time.Hour})
	if err != nil {
		h.t.Fatalf("adding %s to %s: %v", parse, set, err)
	}
	return asked, replaced
}

// fill claims the one extraction that can be claimed, which must be the
// parse's, and fills it.
func (h *harness) fill(w *worker, parse string) tasks.Claim {
	h.t.Helper()
	c := w.claim(1, 1)[0]
	if c.Parse != parse || c.Kind != tasks.Extract {
		h.t.Fatalf("claimed %s/%s where the extraction of %s was the only task", c.Parse, c.Task, parse)
	}
	w.settle(filledBy(c))
	return c
}

// TestASetIsMadeBeforeAnyParseAndFoundByName: a set is written with no parse
// in it, read by its id, and listed newest first under the owners a caller
// may see, by name too. An owner has one set of a name, and another owner may
// have one of the same name.
func TestASetIsMadeBeforeAnyParseAndFoundByName(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		made := h.makeSet("set_1", "alice", "harbor")
		if made.ID != "set_1" || made.Owner != "alice" || made.Name != "harbor" || made.Request != asked || made.Version != 1 ||
			made.Parses != (postgres.SetCounts{}) || !made.CreatedAt.Equal(h.now) || !made.UpdatedAt.Equal(h.now) {
			t.Fatalf("a set as made is %+v", made)
		}
		if got, err := h.store.Set(ctx, "set_1"); err != nil || got.Name != "harbor" || got.Request != asked {
			t.Fatalf("the set read back is %+v, %v", got, err)
		}
		_, err := h.store.Set(ctx, "set_gone")
		refused(t, "a set that is not there", err, fault.SetNotFound)

		_, err = h.store.CreateSet(ctx, postgres.SetRequest{ID: "set_2", Owner: "alice", Name: "harbor", Request: other})
		refused(t, "a second set of the name", err, fault.Conflict)
		h.makeSet("set_3", "bob", "harbor")
		h.makeSet("set_4", "alice", "invoices")

		list, more, err := h.store.Sets(ctx, []string{"alice"}, "", "", 10)
		if err != nil || more || len(list) != 2 || list[0].ID != "set_4" || list[1].ID != "set_1" {
			t.Fatalf("alice's sets are %+v, more %t, %v", list, more, err)
		}
		list, more, err = h.store.Sets(ctx, []string{"alice"}, "", "", 1)
		if err != nil || !more || len(list) != 1 || list[0].ID != "set_4" {
			t.Fatalf("the first page of alice's sets is %+v, more %t, %v", list, more, err)
		}
		if list, _, err = h.store.Sets(ctx, []string{"alice"}, "", "set_4", 1); err != nil || len(list) != 1 || list[0].ID != "set_1" {
			t.Fatalf("the page after set_4 is %+v, %v", list, err)
		}
		if list, _, err = h.store.Sets(ctx, nil, "harbor", "", 10); err != nil || len(list) != 2 {
			t.Fatalf("every owner's sets named harbor are %+v, %v", list, err)
		}
		if list, _, err = h.store.Sets(ctx, []string{}, "", "", 10); err != nil || len(list) != 0 {
			t.Fatalf("nobody's sets are %+v, %v", list, err)
		}
	})
}

// TestAParseJoinsASet: a parse joins a set of its owner, and its extraction
// under the set's name is asked with the set's request: queued at once with
// the first lease token of a first asking for a parse that has ended, and waiting with no task
// for one that is read. A join repeated asks nothing. A parse of another
// owner, a parse that is not there and a set that is not there are refused.
// The set counts its parses and lists them newest first with their
// extractions.
func TestAParseJoinsASet(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_a", Group: "acme", Owner: "alice"}, 1)
		h.readThrough(w, postgres.Submission{Parse: "prs_c", Group: "acme", Owner: "bob"}, 1)
		h.makeSet("set_1", "alice", "harbor")

		if asked, replaced := h.join("set_1", "prs_a"); !asked || replaced != "" {
			t.Fatalf("a parse with no extraction of the name joined: asked %t, replaced %q", asked, replaced)
		}
		f := h.read("prs_a", "harbor")
		if f.State != postgres.FieldPending || f.Request != asked || f.SetID != "set_1" || f.SetVersion != 1 || f.DeadlineAt == nil {
			t.Fatalf("the extraction a join asked is %+v", f)
		}
		if asked, _ := h.join("set_1", "prs_a"); asked {
			t.Fatal("a join repeated asked again")
		}
		if n := value[int](h, `SELECT count(*) FROM tasks WHERE kind = 'extract'`); n != 1 {
			t.Fatalf("%d extractions are queued after a join and its repeat, want 1", n)
		}
		c := h.fill(w, "prs_a")
		if c.Token != 1 || c.Context.Request != asked {
			t.Fatalf("the claim of the set's extraction is %+v", c)
		}

		h.submit(postgres.Submission{Parse: "prs_b", Group: "acme", Owner: "alice"})
		h.join("set_1", "prs_b")
		if f := h.read("prs_b", "harbor"); f.State != postgres.FieldPending || f.DeadlineAt != nil || f.SetID != "set_1" {
			t.Fatalf("the extraction of a parse that is read is %+v", f)
		}

		_, _, err := h.store.JoinSet(ctx, postgres.SetJoin{Set: "set_1", Parse: "prs_c", Deadline: time.Hour})
		refused(t, "a parse of another owner", err, fault.ParseNotFound)
		_, _, err = h.store.JoinSet(ctx, postgres.SetJoin{Set: "set_1", Parse: "prs_gone", Deadline: time.Hour})
		refused(t, "a parse that is not there", err, fault.ParseNotFound)
		_, _, err = h.store.JoinSet(ctx, postgres.SetJoin{Set: "set_gone", Parse: "prs_a", Deadline: time.Hour})
		refused(t, "a set that is not there", err, fault.SetNotFound)

		s, err := h.store.Set(ctx, "set_1")
		if err != nil || s.Parses != (postgres.SetCounts{Total: 2, Pending: 1, Succeeded: 1}) {
			t.Fatalf("the set counts %+v, %v", s.Parses, err)
		}
		members, more, err := h.store.SetParses(ctx, "set_1", "", 10)
		if err != nil || more || len(members) != 2 || members[0].Parse.ID != "prs_b" || members[1].Parse.ID != "prs_a" ||
			members[1].Field.State != postgres.FieldSucceeded || members[1].Field.Output != filledBy(c).Output || members[1].Parse.Owner != "alice" {
			t.Fatalf("the set's parses are %+v, more %t, %v", members, more, err)
		}
		if members, more, err = h.store.SetParses(ctx, "set_1", "prs_b", 1); err != nil || more || len(members) != 1 || members[0].Parse.ID != "prs_a" {
			t.Fatalf("the page after prs_b is %+v, more %t, %v", members, more, err)
		}
		h.consistent()
	})
}

// TestAJoinTakesInWhatTheSetsDefinitionFilled: a parse whose extraction of
// the set's name ended, asked with the set's request byte for byte, joins as
// it stands: no task, its object kept. One asked with another request is
// asked again with the set's, and its first lease token is above every token
// of the earlier asking. One that has not ended is refused.
func TestAJoinTakesInWhatTheSetsDefinitionFilled(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		for _, id := range []string{"prs_same", "prs_other", "prs_busy"} {
			h.readThrough(w, postgres.Submission{Parse: id, Group: "acme", Owner: "alice"}, 1)
		}
		h.field("prs_same", "harbor", "")
		kept := h.fill(w, "prs_same")
		if err := h.store.CreateField(ctx, postgres.FieldRequest{Parse: "prs_other", Name: "harbor", Request: other, Deadline: time.Hour}); err != nil {
			t.Fatal(err)
		}
		earlier := h.fill(w, "prs_other")
		if err := h.store.CreateField(ctx, postgres.FieldRequest{Parse: "prs_busy", Name: "harbor", Request: other, Deadline: time.Hour}); err != nil {
			t.Fatal(err)
		}
		h.makeSet("set_1", "alice", "harbor")
		// The join's asking is queued after the busy extraction's task.
		h.advance(time.Second)

		if asked, replaced := h.join("set_1", "prs_same"); asked || replaced != "" {
			t.Fatalf("an extraction the set's definition filled was asked again: replaced %q", replaced)
		}
		if f := h.read("prs_same", "harbor"); f.State != postgres.FieldSucceeded || f.Output != filledBy(kept).Output || f.SetID != "set_1" || f.SetVersion != 1 {
			t.Fatalf("the extraction taken in is %+v", f)
		}

		if asked, replaced := h.join("set_1", "prs_other"); !asked || replaced != filledBy(earlier).Output {
			t.Fatalf("an extraction of another request joined: asked %t, replaced %q", asked, replaced)
		}
		if f := h.read("prs_other", "harbor"); f.State != postgres.FieldPending || f.Request != asked || f.Output != "" || f.SetID != "set_1" {
			t.Fatalf("the extraction asked again by a join is %+v", f)
		}

		_, _, err := h.store.JoinSet(ctx, postgres.SetJoin{Set: "set_1", Parse: "prs_busy", Deadline: time.Hour})
		refused(t, "a join of an extraction that has not ended", err, fault.Conflict)
		if f := h.read("prs_busy", "harbor"); f.Request != other || f.SetID != "" {
			t.Fatalf("a refused join changed the extraction: %+v", f)
		}

		// The claims: the busy one's own asking, then the join's, under a
		// token of its second asking.
		h.fill(w, "prs_busy")
		second := h.fill(w, "prs_other")
		if second.Token <= 1<<32 || second.Context.Request != asked {
			t.Fatalf("the claim of the extraction a join asked again is %+v", second)
		}
		h.consistent()
	})
}

// TestASetHoldsAtMostItsBoundOfParses: a join past the most parses a set
// holds is refused, and one of a parse already in it is not.
func TestASetHoldsAtMostItsBoundOfParses(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_a", Group: "acme", Owner: "alice"}, 1)
		h.readThrough(w, postgres.Submission{Parse: "prs_b", Group: "acme", Owner: "alice"}, 1)
		h.makeSet("set_1", "alice", "harbor")
		join := func(parse string) string {
			doc, err := json.Marshal(map[string]any{"set": "set_1", "parse": parse, "deadline_ms": 3_600_000, "max_fields": postgres.MaxFields, "max_parses": 1})
			if err != nil {
				t.Fatal(err)
			}
			return value[string](h, `SELECT lectio_set_join($1, $2)::jsonb->>'answer'`, string(doc), h.now)
		}
		if got := join("prs_a"); got != "asked" {
			t.Fatalf("the first join answered %s", got)
		}
		if got := join("prs_b"); got != "full_set" {
			t.Fatalf("a join past the bound answered %s", got)
		}
		if got := join("prs_a"); got != "joined" {
			t.Fatalf("a join of a parse in the set answered %s", got)
		}
	})
}

// TestAChangeOfASetAsksEveryParseAgain: a change of a set's definition is
// refused while an extraction of the set has a task, and changes nothing.
// Once none has, it adds 1 to the version, asks every ended extraction again
// with the new request, queued at once under a token of its next asking, and
// replaces the request of one whose parse is read where it stands; that one
// is queued with the tokens of a first asking when its parse ends. The results the
// askings replaced are returned to be removed.
func TestAChangeOfASetAsksEveryParseAgain(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_done", Group: "acme", Owner: "alice"}, 1)
		h.makeSet("set_1", "alice", "harbor")
		h.join("set_1", "prs_done")

		change := postgres.SetRequest{ID: "set_1", Request: other}
		_, _, err := h.store.UpdateSet(ctx, change, time.Hour)
		refused(t, "a change while an extraction of the set is queued", err, fault.Conflict)
		if s, err := h.store.Set(ctx, "set_1"); err != nil || s.Version != 1 || s.Request != asked {
			t.Fatalf("a refused change left the set %+v, %v", s, err)
		}
		first := h.fill(w, "prs_done")

		// A parse that is read, whose prepare the worker holds, so the only
		// task that can be claimed is what the change queues.
		h.submit(postgres.Submission{Parse: "prs_reading", Group: "acme", Owner: "alice", Deadline: 30 * time.Minute})
		prep := w.claim(1, 1)[0]
		if prep.Parse != "prs_reading" || prep.Kind != tasks.Prepare {
			t.Fatalf("claimed %s/%s where the prepare of prs_reading was the only task", prep.Parse, prep.Task)
		}
		h.join("set_1", "prs_reading")
		h.advance(time.Minute)
		s, replaced, err := h.store.UpdateSet(ctx, change, time.Hour)
		if err != nil || s.Version != 2 || s.Request != other || !s.UpdatedAt.Equal(h.now) || len(replaced) != 1 || replaced[0] != filledBy(first).Output {
			t.Fatalf("the change answered %+v, replaced %v, %v", s, replaced, err)
		}
		if f := h.read("prs_done", "harbor"); f.State != postgres.FieldPending || f.Request != other || f.Output != "" || f.SetVersion != 2 || f.DeadlineAt == nil {
			t.Fatalf("an ended extraction after the change is %+v", f)
		}
		if f := h.read("prs_reading", "harbor"); f.State != postgres.FieldPending || f.Request != other || f.SetVersion != 2 || f.DeadlineAt != nil {
			t.Fatalf("the extraction of a parse that is read after the change is %+v", f)
		}

		again := h.fill(w, "prs_done")
		if again.Token <= 1<<32 || again.Context.Request != other {
			t.Fatalf("the claim of an extraction the change asked again is %+v", again)
		}
		w.settle(prepared(prep, 1))
		w.settle(done(w.claim(1, 1)[0]))
		w.settle(done(w.claim(1, 1)[0]))
		if p := h.parse("prs_reading"); p.State != "succeeded" {
			t.Fatalf("prs_reading ended %s", p.State)
		}
		late := h.fill(w, "prs_reading")
		if late.Token != 1 || late.Context.Request != other {
			t.Fatalf("the claim of an extraction whose request the change replaced is %+v", late)
		}
		_, _, err = h.store.UpdateSet(ctx, postgres.SetRequest{ID: "set_gone", Request: other}, time.Hour)
		refused(t, "a change of a set that is not there", err, fault.SetNotFound)
		h.consistent()
	})
}

// TestAnExtractionOfASetIsAskedAgainWithTheSetsRequestAlone: asked again by
// name with the set's request, an extraction of a set is asked again and
// stays in the set; with another request it is refused, and is left as it
// was.
func TestAnExtractionOfASetIsAskedAgainWithTheSetsRequestAlone(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_a", Group: "acme", Owner: "alice"}, 1)
		h.makeSet("set_1", "alice", "harbor")
		h.join("set_1", "prs_a")
		filled := h.fill(w, "prs_a")

		_, err := h.store.AskFieldAgain(ctx, postgres.FieldRequest{Parse: "prs_a", Name: "harbor", Request: other, Deadline: time.Hour})
		refused(t, "asking an extraction of a set again with another request", err, fault.Conflict)
		if f := h.read("prs_a", "harbor"); f.State != postgres.FieldSucceeded || f.Request != asked {
			t.Fatalf("the refused asking left %+v", f)
		}
		replaced, err := h.store.AskFieldAgain(ctx, postgres.FieldRequest{Parse: "prs_a", Name: "harbor", Request: asked, Deadline: time.Hour})
		if err != nil || replaced != filledBy(filled).Output {
			t.Fatalf("asking again with the set's request replaced %q, %v", replaced, err)
		}
		if f := h.read("prs_a", "harbor"); f.State != postgres.FieldPending || f.SetID != "set_1" {
			t.Fatalf("an extraction of a set asked again is %+v", f)
		}
	})
}

// TestADeletedSetLeavesItsParsesAndAParseThatGoesLeavesItsSets: a set that
// is deleted is gone, and its extractions stay on their parses, read by name
// and in no set. A parse that is deleted, or whose retention ends, is no
// longer counted or listed in its set.
func TestADeletedSetLeavesItsParsesAndAParseThatGoesLeavesItsSets(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		for _, id := range []string{"prs_a", "prs_b", "prs_c"} {
			h.readThrough(w, postgres.Submission{Parse: id, Group: "acme", Owner: "alice"}, 1)
		}
		h.makeSet("set_1", "alice", "harbor")
		for _, id := range []string{"prs_a", "prs_b", "prs_c"} {
			h.join("set_1", id)
			h.fill(w, id)
		}

		if err := h.store.DeleteParse(ctx, "alice", "prs_b"); err != nil {
			t.Fatal(err)
		}
		if removed, err := h.store.ExpireParse(ctx, "prs_b"); err != nil || !removed {
			t.Fatalf("removing prs_b: %t, %v", removed, err)
		}
		if s, err := h.store.Set(ctx, "set_1"); err != nil || s.Parses.Total != 2 {
			t.Fatalf("after a parse was deleted the set counts %+v, %v", s.Parses, err)
		}
		if members, _, err := h.store.SetParses(ctx, "set_1", "", 10); err != nil || len(members) != 2 || members[0].Parse.ID != "prs_c" {
			t.Fatalf("after a parse was deleted the set lists %+v, %v", members, err)
		}

		if err := h.store.DeleteSet(ctx, "set_1"); err != nil {
			t.Fatal(err)
		}
		_, err := h.store.Set(ctx, "set_1")
		refused(t, "a deleted set", err, fault.SetNotFound)
		refused(t, "deleting a set twice", h.store.DeleteSet(ctx, "set_1"), fault.SetNotFound)
		if f := h.read("prs_a", "harbor"); f.State != postgres.FieldSucceeded || f.SetID != "" || f.SetVersion != 0 {
			t.Fatalf("an extraction of a deleted set is %+v", f)
		}
		// A set made again under the name starts empty, and takes the
		// extraction in as it stands.
		h.makeSet("set_2", "alice", "harbor")
		if asked, _ := h.join("set_2", "prs_a"); asked {
			t.Fatal("a set made again asked an extraction its definition filled")
		}
		h.consistent()
	})
}

// reordered is the suite's request with its members in another order: equal
// to it as a JSON value, and other bytes.
const reordered = `{"citations":true,"schema":{"properties":{"a":{"const":null},"total":{"type":"number"}},"type":"object"}}`

// TestAChangeOfASetLeavesAParseBeingRemoved: a parse whose delete has begun,
// so its retention has ended, is left as it is by a change of its set:
// its extraction is not asked again and nothing of it is queued, so no
// object is written that the delete has not listed. An extraction the delete
// left pending, with no task, does not hold the change back.
func TestAChangeOfASetLeavesAParseBeingRemoved(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		for _, id := range []string{"prs_kept", "prs_done", "prs_queued"} {
			h.readThrough(w, postgres.Submission{Parse: id, Group: "acme", Owner: "alice"}, 1)
		}
		h.makeSet("set_1", "alice", "harbor")
		h.join("set_1", "prs_kept")
		kept := h.fill(w, "prs_kept")
		h.join("set_1", "prs_done")
		h.fill(w, "prs_done")
		h.join("set_1", "prs_queued")
		for _, id := range []string{"prs_done", "prs_queued"} {
			if err := h.store.DeleteParse(ctx, "alice", id); err != nil {
				t.Fatal(err)
			}
		}

		s, replaced, err := h.store.UpdateSet(ctx, postgres.SetRequest{ID: "set_1", Request: other}, time.Hour)
		if err != nil || s.Version != 2 || len(replaced) != 1 || replaced[0] != filledBy(kept).Output {
			t.Fatalf("the change answered %+v, replaced %v, %v", s, replaced, err)
		}
		if f := h.read("prs_done", "harbor"); f.State != postgres.FieldSucceeded || f.Request != asked || f.SetVersion != 1 {
			t.Fatalf("the extraction of a parse being removed after the change is %+v", f)
		}
		if f := h.read("prs_queued", "harbor"); f.State != postgres.FieldPending || f.Request != asked {
			t.Fatalf("the pending extraction of a parse being removed after the change is %+v", f)
		}
		if got := value[string](h, `SELECT string_agg(parse_id, ',' ORDER BY parse_id) FROM tasks WHERE kind = 'extract'`); got != "prs_kept" {
			t.Fatalf("after the change the extractions queued are of %s, want prs_kept alone", got)
		}
		h.consistent()
	})
}

// TestASetChangedToWhatItHoldsChangesNothing: a change to the definition the
// set holds, byte for byte, asks nothing and keeps the version. The same
// members in another order are a change: the order is the order a model is
// asked in.
func TestASetChangedToWhatItHoldsChangesNothing(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_a", Group: "acme", Owner: "alice"}, 1)
		h.makeSet("set_1", "alice", "harbor")
		h.join("set_1", "prs_a")
		h.fill(w, "prs_a")

		s, replaced, err := h.store.UpdateSet(ctx, postgres.SetRequest{ID: "set_1", Request: asked}, time.Hour)
		if err != nil || s.Version != 1 || len(replaced) != 0 {
			t.Fatalf("a change to what the set holds answered %+v, replaced %v, %v", s, replaced, err)
		}
		if f := h.read("prs_a", "harbor"); f.State != postgres.FieldSucceeded {
			t.Fatalf("a change to what the set holds asked again: %+v", f)
		}
		if s, _, err = h.store.UpdateSet(ctx, postgres.SetRequest{ID: "set_1", Request: reordered}, time.Hour); err != nil || s.Version != 2 || s.Request != reordered {
			t.Fatalf("a change of the members' order answered %+v, %v", s, err)
		}
	})
}

// TestARequestEqualAsAValueIsTheSets: an extraction asked with the set's
// request in another order of members is taken in by a join with nothing
// asked, and asked again by name, with the set's own bytes.
func TestARequestEqualAsAValueIsTheSets(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_a", Group: "acme", Owner: "alice"}, 1)
		if err := h.store.CreateField(ctx, postgres.FieldRequest{Parse: "prs_a", Name: "harbor", Request: reordered, Deadline: time.Hour}); err != nil {
			t.Fatal(err)
		}
		h.fill(w, "prs_a")
		h.makeSet("set_1", "alice", "harbor")
		if asked, _ := h.join("set_1", "prs_a"); asked {
			t.Fatal("a join asked again an extraction whose request is the set's as a value")
		}
		if _, err := h.store.AskFieldAgain(ctx, postgres.FieldRequest{Parse: "prs_a", Name: "harbor", Request: reordered, Deadline: time.Hour}); err != nil {
			t.Fatalf("asking again with the set's request in another order: %v", err)
		}
		if f := h.read("prs_a", "harbor"); f.State != postgres.FieldPending || f.Request != asked {
			t.Fatalf("asked again by name, the extraction is %+v, want the set's own bytes", f)
		}
	})
}

// TestASetRequestIsCompleteBeforeItIsSent: a set with no id, owner, name or
// request, a join with no set, parse or deadline, and a change with no set,
// request or deadline are refused before anything reaches the database.
func TestASetRequestIsCompleteBeforeItIsSent(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		_, err := h.store.CreateSet(ctx, postgres.SetRequest{ID: "set_1", Owner: "alice", Name: "harbor"})
		refused(t, "a set with no request", err, fault.InvalidRequest)
		_, _, err = h.store.JoinSet(ctx, postgres.SetJoin{Set: "set_1", Deadline: time.Hour})
		refused(t, "a join with no parse", err, fault.InvalidRequest)
		_, _, err = h.store.JoinSet(ctx, postgres.SetJoin{Set: "set_1", Parse: "prs_a"})
		refused(t, "a join with no deadline", err, fault.InvalidRequest)
		_, _, err = h.store.UpdateSet(ctx, postgres.SetRequest{ID: "set_1"}, time.Hour)
		refused(t, "a change with no request", err, fault.InvalidRequest)
		_, _, err = h.store.UpdateSet(ctx, postgres.SetRequest{ID: "set_1", Request: asked}, 0)
		refused(t, "a change with no deadline", err, fault.InvalidRequest)
		if n := value[int](h, `SELECT count(*) FROM sets`); n != 0 {
			t.Fatalf("%d sets after refused requests", n)
		}
	})
}

// TestAJoinPastTheExtractionsAParseHoldsIsRefused: a parse that holds as many
// extractions as it may joins no set that would add one.
func TestAJoinPastTheExtractionsAParseHoldsIsRefused(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		h.submit(postgres.Submission{Parse: "prs_a", Group: "acme", Owner: "alice"})
		h.exec(`UPDATE parses SET fields = $1 WHERE parse_id = 'prs_a'`, postgres.MaxFields)
		h.makeSet("set_1", "alice", "harbor")
		_, _, err := h.store.JoinSet(context.Background(), postgres.SetJoin{Set: "set_1", Parse: "prs_a", Deadline: time.Hour})
		refused(t, "a join of a parse that holds as many extractions as it may", err, fault.Conflict)
	})
}

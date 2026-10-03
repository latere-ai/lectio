// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"sync"
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/render"
)

func TestFiles(t *testing.T) {
	m := NewMemory()
	data := []byte("the same bytes")
	first, created := m.PutFile(File{ID: "fil_1", Owner: "alice", SHA256: Digest(data), Data: data})
	if !created || first.ID != "fil_1" {
		t.Fatalf("PutFile = %+v, %v", first, created)
	}
	again, created := m.PutFile(File{ID: "fil_2", Owner: "alice", SHA256: Digest(data), Data: data})
	if created || again.ID != "fil_1" {
		t.Fatalf("the same bytes are one file per owner: %+v, %v", again, created)
	}
	other, created := m.PutFile(File{ID: "fil_3", Owner: "bob", SHA256: Digest(data), Data: data})
	if !created || other.ID != "fil_3" {
		t.Fatalf("another owner's upload is another file: %+v, %v", other, created)
	}

	if _, err := m.File("alice", "fil_1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"fil_3", "fil_9"} {
		if _, err := m.File("alice", id); fault.CodeOf(err) != fault.FileNotFound {
			t.Errorf("File(alice, %s) = %v", id, err)
		}
		if err := m.DeleteFile("alice", id); fault.CodeOf(err) != fault.FileNotFound {
			t.Errorf("DeleteFile(alice, %s) = %v", id, err)
		}
	}

	if _, _, err := m.CreateParse(Parse{ID: "prs_1", Owner: "alice", File: "fil_1", State: StateRunning}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteFile("alice", "fil_1"); fault.CodeOf(err) != fault.NotTerminal {
		t.Fatalf("a file a running parse reads is kept: %v", err)
	}
	if _, err := m.UpdateParse("prs_1", func(p *Parse) { p.State = StateSucceeded }); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteFile("alice", "fil_1"); err != nil {
		t.Fatal(err)
	}
	if got := len(Digest(nil)); got != 64 {
		t.Fatalf("a digest is 64 hex characters, got %d", got)
	}
}

func TestIdempotentCreate(t *testing.T) {
	m := NewMemory()
	first, created, err := m.CreateParse(Parse{ID: "prs_1", Owner: "alice"}, "key", "body")
	if err != nil || !created || first.ID != "prs_1" {
		t.Fatalf("CreateParse = %+v, %v, %v", first, created, err)
	}
	again, created, err := m.CreateParse(Parse{ID: "prs_2", Owner: "alice"}, "key", "body")
	if err != nil || created || again.ID != "prs_1" {
		t.Fatalf("the same key and body return the first parse: %+v, %v, %v", again, created, err)
	}
	if _, _, err := m.CreateParse(Parse{ID: "prs_3", Owner: "alice"}, "key", "other body"); fault.CodeOf(err) != fault.IdempotencyConflict {
		t.Fatalf("the same key with another body: %v", err)
	}
	if p, created, err := m.CreateParse(Parse{ID: "prs_4", Owner: "bob"}, "key", "other body"); err != nil || !created || p.ID != "prs_4" {
		t.Fatalf("a key is one owner's: %+v, %v, %v", p, created, err)
	}

	// Many calls with one key make one parse.
	var wg sync.WaitGroup
	made := make(chan string, 50)
	for i := range 50 {
		wg.Go(func() {
			p, _, err := m.CreateParse(Parse{ID: fmt.Sprintf("prs_c%02d", i), Owner: "carol"}, "one", "body")
			if err == nil {
				made <- p.ID
			}
		})
	}
	wg.Wait()
	close(made)
	ids := map[string]bool{}
	for id := range made {
		ids[id] = true
	}
	if len(ids) != 1 {
		t.Fatalf("50 calls with one key made %d parses", len(ids))
	}
}

func TestParses(t *testing.T) {
	m := NewMemory()
	for i, p := range []Parse{
		{ID: "prs_1", Owner: "alice", State: StateSucceeded, File: "fil_a", Origin: &Origin{Path: "reports/q3.pdf"}, Labels: map[string]string{"batch": "oct", "kind": "invoice"}, ContentSHA: "sha", Fingerprint: "fp"},
		{ID: "prs_2", Owner: "alice", State: StateFailed, File: "fil_a", Labels: map[string]string{"batch": "oct"}},
		{ID: "prs_3", Owner: "alice", State: StateSucceeded, File: "fil_b", ContentSHA: "sha", Fingerprint: "fp"},
		{ID: "prs_4", Owner: "alice", State: StateRunning, File: "fil_b", ContentSHA: "sha", Fingerprint: "other"},
		{ID: "prs_5", Owner: "bob", State: StateSucceeded, File: "fil_c", ContentSHA: "sha", Fingerprint: "fp"},
	} {
		if _, _, err := m.CreateParse(p, "", ""); err != nil {
			t.Fatalf("parse %d: %v", i, err)
		}
	}

	if _, err := m.Parse("alice", "prs_5"); fault.CodeOf(err) != fault.ParseNotFound {
		t.Fatalf("another owner's parse is not found: %v", err)
	}
	if _, err := m.UpdateParse("prs_9", func(*Parse) {}); fault.CodeOf(err) != fault.ParseNotFound {
		t.Fatalf("UpdateParse of nothing: %v", err)
	}

	ids := func(ps []Parse) (out string) {
		for _, p := range ps {
			out += p.ID[4:]
		}
		return out
	}
	for name, tc := range map[string]struct {
		filter Filter
		after  string
		limit  int
		want   string
		more   bool
	}{
		"all, newest first":   {Filter{}, "", 10, "4321", false},
		"a page of two":       {Filter{}, "", 2, "43", true},
		"the next page":       {Filter{}, "prs_3", 2, "21", false},
		"after one not there": {Filter{}, "prs_25", 10, "21", false},
		"by state":            {Filter{State: StateSucceeded}, "", 10, "31", false},
		"by file":             {Filter{File: "fil_a"}, "", 10, "21", false},
		"by origin path":      {Filter{OriginPath: "reports/q3.pdf"}, "", 10, "1", false},
		"by one label":        {Filter{Labels: map[string]string{"batch": "oct"}}, "", 10, "21", false},
		"by two labels":       {Filter{Labels: map[string]string{"batch": "oct", "kind": "invoice"}}, "", 10, "1", false},
		"nothing matches":     {Filter{State: StateCanceled}, "", 10, "", false},
	} {
		got, more := m.ListParses("alice", tc.filter, tc.after, tc.limit)
		if ids(got) != tc.want || more != tc.more {
			t.Errorf("%s: %q more=%v, want %q more=%v", name, ids(got), more, tc.want, tc.more)
		}
	}

	if p, ok := m.Reusable("alice", "sha", "fp"); !ok || p.ID != "prs_3" {
		t.Fatalf("the newest succeeded parse of the same work is reused: %+v, %v", p, ok)
	}
	if _, ok := m.Reusable("alice", "sha", "other"); ok {
		t.Fatal("a running parse is not reused")
	}
	if _, ok := m.Reusable("carol", "sha", "fp"); ok {
		t.Fatal("another owner's parse is not reused")
	}
}

func TestResultsAndDelete(t *testing.T) {
	m := NewMemory()
	if _, _, err := m.CreateParse(Parse{ID: "prs_1", Owner: "alice", State: StateRunning}, "", ""); err != nil {
		t.Fatal(err)
	}
	m.PutPage("prs_1", document.Page{Number: 2, State: document.PageSucceeded}, &render.Image{MediaType: "image/png", Data: []byte("two")})
	m.PutPage("prs_1", document.Page{Number: 1, State: document.PageFailed}, nil)
	m.PutPage("prs_1", document.Page{Number: 1, State: document.PageSucceeded}, nil)
	m.PutPage("prs_other", document.Page{Number: 1}, &render.Image{})
	m.PutDocument(document.Document{Parse: "prs_1"})

	pages := m.Pages("prs_1")
	if len(pages) != 2 || pages[0].Number != 1 || pages[0].State != document.PageSucceeded || pages[1].Number != 2 {
		t.Fatalf("pages in order, the second write of page 1 kept: %+v", pages)
	}
	if _, ok := m.Page("prs_1", 3); ok {
		t.Fatal("a page not written is not there")
	}
	if img, ok := m.Image("prs_1", 2); !ok || string(img.Data) != "two" {
		t.Fatalf("image = %+v, %v", img, ok)
	}
	if _, ok := m.Image("prs_1", 1); ok {
		t.Fatal("a page stored without an image has none")
	}
	if _, ok := m.Document("prs_1"); !ok {
		t.Fatal("the document is there")
	}

	if err := m.DeleteParse("alice", "prs_1"); fault.CodeOf(err) != fault.NotTerminal {
		t.Fatalf("a running parse is not deleted: %v", err)
	}
	if err := m.DeleteParse("bob", "prs_1"); fault.CodeOf(err) != fault.ParseNotFound {
		t.Fatalf("another owner cannot delete it: %v", err)
	}
	if _, err := m.UpdateParse("prs_1", func(p *Parse) { p.State = StateCanceled }); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteParse("alice", "prs_1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Page("prs_1", 2); ok || len(m.Pages("prs_1")) != 0 {
		t.Fatal("a deleted parse leaves no page")
	}
	if _, ok := m.Image("prs_1", 2); ok {
		t.Fatal("a deleted parse leaves no image")
	}
	if _, ok := m.Document("prs_1"); ok {
		t.Fatal("a deleted parse leaves no document")
	}
	if _, ok := m.Page("prs_other", 1); !ok {
		t.Fatal("another parse's pages are untouched")
	}
}

func TestTerminal(t *testing.T) {
	for state, want := range map[string]bool{StateQueued: false, StateRunning: false, StateSucceeded: true, StateFailed: true, StateCanceled: true} {
		if got := (Parse{State: state}).Terminal(); got != want {
			t.Errorf("%s: Terminal() = %v", state, got)
		}
	}
}

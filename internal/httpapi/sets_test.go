// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
)

// TestASetIsMadeFirstAndAskedOfEachParseThatJoins: a set is made with no
// parse and found by its name. A parse joins it and is filled as the set
// defines it, and a join repeated asks nothing. The set's extraction is
// asked again by name with the set's request alone. A change of the set
// asks its parse again with the new definition, and is refused while that
// extraction runs. Another caller finds nothing of the set, and a parse of
// another owner does not join it. A deleted set leaves its parse's
// extraction. No page is read again.
func TestASetIsMadeFirstAndAskedOfEachParseThatJoins(t *testing.T) {
	hold, held := make(chan struct{}), atomic.Bool{}
	model := &textModel{answers: func(n int, _ reader.ExtractRequest) (reader.ExtractResult, bool) {
		// The 3rd call, the change's, waits until the test has tried to
		// change the set again while it runs.
		if n == 3 {
			held.Store(true)
			<-hold
		}
		return reader.ExtractResult{}, false
	}}
	e, rd := extracting(t, model, nil)
	const guidance = "The total is the amount due."

	made := e.do("POST", "/sets", `{"name":"invoices","schema":`+invoiceSchema+`,"instructions":"`+guidance+`"}`)
	view := made.json(t)
	var want any
	if err := json.Unmarshal([]byte(invoiceSchema), &want); err != nil {
		t.Fatal(err)
	}
	if made.status != http.StatusCreated || view["name"] != "invoices" || view["version"] != 1.0 || view["citations"] != true ||
		view["instructions"] != guidance || !reflect.DeepEqual(view["schema"], want) || at(view, "parses", "total") != 0.0 {
		t.Fatalf("making a set: %d %s", made.status, made.body)
	}
	set := view["id"].(string)
	if again := e.do("POST", "/sets", `{"name":"invoices","schema":`+totalSchema+`}`); again.status != http.StatusConflict || again.code(t) != "conflict" {
		t.Fatalf("a second set of the name: %d %s", again.status, again.body)
	}
	if bad := e.do("POST", "/sets", `{"name":"Invoices","schema":`+totalSchema+`}`); bad.status != http.StatusBadRequest {
		t.Fatalf("a name that is not one: %d %s", bad.status, bad.body)
	}
	if listed := e.do("GET", "/sets?name=invoices", nil).json(t)["sets"].([]any); len(listed) != 1 || at(listed[0], "id") != set {
		t.Fatalf("the set found by its name: %v", listed)
	}
	bob := e.as("bob-token")
	if got := bob.do("GET", "/sets/"+set, nil); got.status != http.StatusNotFound || got.code(t) != "set_not_found" {
		t.Fatalf("another caller read the set: %d %s", got.status, got.body)
	}
	if listed := bob.do("GET", "/sets", nil).json(t)["sets"].([]any); len(listed) != 0 {
		t.Fatalf("another caller lists %v", listed)
	}
	theirs := bob.parsed(bob.upload("invoice.tiff", testfixtures.Read(t, testfixtures.MultiTIFF)), "")["id"].(string)
	if got := e.do("PUT", "/sets/"+set+"/parses/"+theirs, nil); got.status != http.StatusNotFound || got.code(t) != "parse_not_found" {
		t.Fatalf("a parse of another owner joined: %d %s", got.status, got.body)
	}

	pid := e.invoice()
	read := rd.calls.Load()
	join := e.do("PUT", "/sets/"+set+"/parses/"+pid, nil)
	if join.status != http.StatusAccepted || join.json(t)["name"] != "invoices" || join.json(t)["state"] != "pending" {
		t.Fatalf("a parse joining: %d %s", join.status, join.body)
	}
	if f := e.filled(pid, "invoices", ""); f["state"] != "succeeded" || at(f, "data", "number") != "INV-0042" || f["instructions"] != guidance {
		t.Fatalf("the set's extraction of the parse: %v", f)
	}
	if again := e.do("PUT", "/sets/"+set+"/parses/"+pid, nil); again.status != http.StatusOK || again.json(t)["state"] != "succeeded" {
		t.Fatalf("a join repeated: %d %s", again.status, again.body)
	}
	members := e.do("GET", "/sets/"+set+"/parses", nil).json(t)["parses"].([]any)
	if len(members) != 1 || at(members[0], "parse", "id") != pid || at(members[0], "field", "data", "number") != "INV-0042" {
		t.Fatalf("the set's parses: %v", members)
	}
	if got := e.do("GET", "/sets/"+set, nil).json(t); at(got, "parses", "succeeded") != 1.0 || at(got, "parses", "total") != 1.0 {
		t.Fatalf("the set counts %v", got["parses"])
	}

	// By name, the set's extraction is asked again with the set's request
	// and with no other.
	ask := func(body string) reply { return e.do("PUT", "/parses/"+pid+"/fields/invoices", body) }
	if got := ask(`{"schema":` + totalSchema + `}`); got.status != http.StatusConflict || got.code(t) != "conflict" {
		t.Fatalf("asking the set's extraction with another request: %d %s", got.status, got.body)
	}
	if got := ask(`{"schema":` + invoiceSchema + `,"instructions":"` + guidance + `"}`); got.status != http.StatusAccepted {
		t.Fatalf("asking the set's extraction again with its request: %d %s", got.status, got.body)
	}
	e.filled(pid, "invoices", "")

	if got := e.do("PUT", "/sets/"+set, `{"name":"other","schema":`+totalSchema+`}`); got.status != http.StatusBadRequest {
		t.Fatalf("a change that names another set: %d %s", got.status, got.body)
	}
	change := e.do("PUT", "/sets/"+set, `{"schema":`+totalSchema+`}`)
	if view := change.json(t); change.status != http.StatusOK || view["version"] != 2.0 || view["instructions"] != nil {
		t.Fatalf("changing the set: %d %s", change.status, change.body)
	}
	for deadline := time.Now().Add(20 * time.Second); !held.Load(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the change's call never began")
		}
	}
	if got := e.do("PUT", "/sets/"+set, `{"schema":`+invoiceSchema+`}`); got.status != http.StatusConflict || got.code(t) != "conflict" {
		t.Fatalf("a change while the set's extraction runs: %d %s", got.status, got.body)
	}
	close(hold)
	var changed any
	if err := json.Unmarshal([]byte(totalSchema), &changed); err != nil {
		t.Fatal(err)
	}
	if f := e.filled(pid, "invoices", ""); f["state"] != "succeeded" || at(f, "data", "total") != 7.0 || !reflect.DeepEqual(f["schema"], changed) {
		t.Fatalf("after the change the extraction is %v", f)
	}

	if got := e.do("DELETE", "/sets/"+set, nil); got.status != http.StatusNoContent {
		t.Fatalf("deleting the set: %d %s", got.status, got.body)
	}
	if got := e.do("GET", "/sets/"+set, nil); got.status != http.StatusNotFound || got.code(t) != "set_not_found" {
		t.Fatalf("a deleted set: %d %s", got.status, got.body)
	}
	if f := e.filled(pid, "invoices", ""); f["state"] != "succeeded" || at(f, "data", "total") != 7.0 {
		t.Fatalf("the extraction of a deleted set: %v", f)
	}
	if rd.calls.Load() != read {
		t.Fatalf("the set read pages: %d calls of the reader, %d before", rd.calls.Load(), read)
	}
}

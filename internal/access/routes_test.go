// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"latere.ai/x/lectio/api"
	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/access"
)

// operation is one operation of the contract: its method and path, and
// whether it takes no token.
type operation struct {
	method, path string
	open         bool
}

// methods are the keys of a path item that name an operation.
var methods = []string{"get", "put", "post", "delete", "patch", "head", "options"}

// contractOperations reads every operation out of api/openapi.yaml, in
// the document's order.
func contractOperations(t *testing.T) []operation {
	t.Helper()
	var doc struct {
		Paths yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(api.OpenAPI, &doc); err != nil {
		t.Fatalf("the contract does not parse: %v", err)
	}
	var out []operation
	paths := doc.Paths.Content
	for i := 0; i+1 < len(paths); i += 2 {
		item := paths[i+1].Content
		for j := 0; j+1 < len(item); j += 2 {
			if !slices.Contains(methods, item[j].Value) {
				continue
			}
			var op struct {
				Security *[]any `yaml:"security"`
			}
			if err := item[j+1].Decode(&op); err != nil {
				t.Fatalf("%s %s: %v", item[j].Value, paths[i].Value, err)
			}
			out = append(out, operation{
				method: strings.ToUpper(item[j].Value), path: paths[i].Value,
				// An operation that sets an empty security list takes no
				// token, whatever the document's default is.
				open: op.Security != nil && len(*op.Security) == 0,
			})
		}
	}
	if len(out) == 0 {
		t.Fatal("the contract has no operation")
	}
	return out
}

// Every route of the contract has a row, in the contract's order, and no
// row is of a route the contract does not have. A route added to the
// contract fails here until somebody says which action it asks.
func TestEveryRouteOfTheContractHasARow(t *testing.T) {
	var want, got []string
	for _, op := range contractOperations(t) {
		want = append(want, op.method+" "+op.path)
	}
	for _, rt := range access.Routes() {
		got = append(got, rt.Method+" "+rt.Path)
	}
	for _, name := range want {
		if !slices.Contains(got, name) {
			t.Errorf("the contract's route %s has no row", name)
		}
	}
	for _, name := range got {
		if !slices.Contains(want, name) {
			t.Errorf("the row %s is of no route of the contract", name)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("the rows are not in the contract's order, or one is there twice:\n got %v\nwant %v", got, want)
	}
}

// Every row names an action of the vocabulary and fields the vocabulary
// publishes for it. Only a route the contract opens to everyone asks
// nothing.
func TestEveryRowAsksAnActionOfTheVocabulary(t *testing.T) {
	open := map[string]bool{}
	for _, op := range contractOperations(t) {
		open[op.method+" "+op.path] = op.open
	}
	for _, rt := range access.Routes() {
		name := rt.Method + " " + rt.Path
		if rt.Action == "" {
			if !open[name] {
				t.Errorf("%s asks nothing, and the contract requires a token for it", name)
			}
			if len(rt.Fields) != 0 {
				t.Errorf("%s asks nothing and sends %v", name, rt.Fields)
			}
			continue
		}
		if open[name] {
			t.Errorf("%s asks %s, and the contract opens it to everyone", name, rt.Action)
		}
		if !authorizer.Known(rt.Action) {
			t.Errorf("%s asks %q, which is outside the vocabulary", name, rt.Action)
			continue
		}
		published := authorizer.Fields(rt.Action)
		for _, field := range rt.Fields {
			if !slices.Contains(published, field) {
				t.Errorf("%s sends %q, which the vocabulary does not publish for %s", name, field, rt.Action)
			}
		}
		// A route about a stored parse or file sends its id, and no other
		// route does: the id is how the row says the object is read first.
		stored := strings.Contains(rt.Path, "{parse}") || strings.Contains(rt.Path, "{file}")
		if stored != slices.Contains(rt.Fields, authorizer.FieldID) {
			t.Errorf("%s names a stored object: %v, and sends its id: %v", name, stored, !stored)
		}
		if kind := authorizer.Kind(rt.Action); stored && strings.Contains(rt.Path, "{file}") != (kind == authorizer.KindFile) {
			t.Errorf("%s asks %s, which acts on a %s", name, rt.Action, kind)
		}
	}
}

var specRoute = regexp.MustCompile("^\\| `([A-Z]+) ([^`]+)` \\|.*\\| (`([a-z.]+)`|none) \\|$")

// The rows are the table of spec 003: the same routes, each asking the
// action the spec gives it.
func TestTheRowsAreTheSpecs(t *testing.T) {
	raw, err := os.ReadFile("../../specs/003-api.md")
	if err != nil {
		t.Fatalf("the spec the table is written in: %v", err)
	}
	_, after, found := strings.Cut(string(raw), "| Method and path | Purpose | Answers | Action asked |\n|---|---|---|---|\n")
	if !found {
		t.Fatal("spec 003 has no route table with the columns Method and path, Purpose, Answers and Action asked")
	}
	spec := map[string]string{}
	for line := range strings.SplitSeq(after, "\n") {
		if !strings.HasPrefix(line, "|") {
			break
		}
		m := specRoute.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("the row %q does not read as a route and its action", line)
		}
		spec[m[1]+" "+m[2]] = m[4]
	}
	rows := access.Routes()
	if len(spec) != len(rows) {
		t.Errorf("the spec's table has %d routes and the rows are %d", len(spec), len(rows))
	}
	for _, rt := range rows {
		name := rt.Method + " " + rt.Path
		action, ok := spec[name]
		if !ok {
			t.Errorf("%s is in no row of the spec's table", name)
		} else if action != rt.Action {
			t.Errorf("%s asks %q, and the spec says %q", name, rt.Action, action)
		}
	}
}

func TestRouteOf(t *testing.T) {
	rt, ok := access.RouteOf("POST", "/parses/{parse}/retry")
	if !ok || rt.Action != authorizer.ActionParseCreate || !slices.Contains(rt.Fields, authorizer.FieldID) {
		t.Errorf("a retry is %+v, %v", rt, ok)
	}
	if rt, ok := access.RouteOf("POST", "/parses"); !ok || rt.Action != authorizer.ActionParseCreate || slices.Contains(rt.Fields, authorizer.FieldID) {
		t.Errorf("a submit is %+v, %v", rt, ok)
	}
	if rt, ok := access.RouteOf("PUT", "/parses"); ok {
		t.Errorf("a route the contract does not have is %+v", rt)
	}
	// A caller's copy of the table is its own.
	rows := access.Routes()
	rows[0].Action, rows[0].Fields[0] = "changed", "changed"
	if again := access.Routes()[0]; again.Action != authorizer.ActionFileCreate || again.Fields[0] != authorizer.FieldOwner {
		t.Errorf("a caller changed the table: %+v", again)
	}
}

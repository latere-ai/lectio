// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"latere.ai/x/lectio/api"
	"latere.ai/x/lectio/internal/fault"
)

// contract is api/openapi.yaml, read once.
var contract = func() map[string]any {
	var doc map[string]any
	if err := yaml.Unmarshal(api.OpenAPI, &doc); err != nil {
		panic(err)
	}
	return doc
}()

// anywhere are the statuses any operation may answer, which the contract
// states once and does not repeat on each.
var anywhere = []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable}

func at(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// operation finds the contract's operation for a request path below the
// base path. template is the path as the contract spells it.
func operation(method, path string) (template string, op map[string]any) {
	paths, _ := contract["paths"].(map[string]any)
	want := strings.Split(path, "/")
	for candidate, item := range paths {
		have := strings.Split(candidate, "/")
		if len(have) != len(want) {
			continue
		}
		match := true
		for i := range have {
			if have[i] != want[i] && !strings.HasPrefix(have[i], "{") {
				match = false
			}
		}
		// A literal segment beats a parameter: /parses/{parse}/pages is
		// not /parses/{parse}/{anything}.
		if match && (template == "" || strings.Count(candidate, "{") < strings.Count(template, "{")) {
			if found, ok := at(item, strings.ToLower(method)).(map[string]any); ok {
				template, op = candidate, found
			}
		}
	}
	return template, op
}

// resolve follows a $ref inside the contract.
func resolve(schema any) map[string]any {
	m, _ := schema.(map[string]any)
	for {
		ref, ok := m["$ref"].(string)
		if !ok {
			return m
		}
		m, _ = at(contract, strings.Split(strings.TrimPrefix(ref, "#/"), "/")...).(map[string]any)
	}
}

// conform reports where a decoded JSON value departs from a schema of the
// contract: a required member that is missing, a member the schema does
// not describe, a value of another type, or one outside an enum. It checks
// what the contract's schemas use and no more.
func conform(schema any, value any, where string) []string {
	s := resolve(schema)
	if s == nil {
		return nil
	}
	var types []string
	switch t := s["type"].(type) {
	case string:
		types = []string{t}
	case []any:
		for _, one := range t {
			types = append(types, fmt.Sprint(one))
		}
	}
	if value == nil {
		if len(types) == 0 || slices.Contains(types, "null") {
			return nil
		}
		return []string{where + " is null"}
	}
	if enum, ok := s["enum"].([]any); ok && !slices.Contains(enum, value) {
		return []string{fmt.Sprintf("%s is %v, outside %v", where, value, enum)}
	}

	var out []string
	switch v := value.(type) {
	case map[string]any:
		if len(types) > 0 && !slices.Contains(types, "object") {
			return []string{where + " is an object"}
		}
		props, _ := s["properties"].(map[string]any)
		for _, name := range slices.Collect(anyStrings(s["required"])) {
			if _, ok := v[name]; !ok {
				out = append(out, where+" lacks "+name)
			}
		}
		for name, member := range v {
			switch {
			case props[name] != nil:
				out = append(out, conform(props[name], member, where+"."+name)...)
			case s["additionalProperties"] != nil && s["additionalProperties"] != false:
				out = append(out, conform(s["additionalProperties"], member, where+"."+name)...)
			case props != nil:
				out = append(out, where+" has "+name+", which the contract does not describe")
			}
		}
	case []any:
		if len(types) > 0 && !slices.Contains(types, "array") {
			return []string{where + " is an array"}
		}
		for i, item := range v {
			out = append(out, conform(s["items"], item, fmt.Sprintf("%s[%d]", where, i))...)
		}
	case string:
		if len(types) > 0 && !slices.Contains(types, "string") {
			out = append(out, where+" is a string")
		}
	case float64:
		if len(types) > 0 && !slices.Contains(types, "integer") && !slices.Contains(types, "number") {
			out = append(out, where+" is a number")
		}
	case bool:
		if len(types) > 0 && !slices.Contains(types, "boolean") {
			out = append(out, where+" is a boolean")
		}
	}
	return out
}

func anyStrings(v any) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		list, _ := v.([]any)
		for _, one := range list {
			if !yield(fmt.Sprint(one)) {
				return
			}
		}
	}
}

// held checks one response against the contract: the operation exists, the
// status is one it lists, and a JSON body has the shape the contract gives
// that status.
func held(t *testing.T, method, path string, status int, header http.Header, body []byte) {
	t.Helper()
	template, op := operation(method, path)
	if op == nil {
		if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d and is not in the contract", method, path, status)
		}
		return
	}
	response := at(op, "responses", fmt.Sprint(status))
	if response == nil {
		if !slices.Contains(anywhere, status) {
			t.Errorf("%s %s answered %d, which the contract does not list", method, template, status)
			return
		}
		response = map[string]any{"$ref": "#/components/responses/Error"}
	}
	mediaType, _, _ := strings.Cut(header.Get("Content-Type"), ";")
	schema := at(resolve(response), "content", mediaType, "schema")
	if schema == nil {
		if len(body) > 0 {
			t.Errorf("%s %s answered %d as %q, which the contract does not list", method, template, status, mediaType)
		}
		return
	}
	switch mediaType {
	case "application/json":
		var value any
		if err := json.Unmarshal(body, &value); err != nil {
			t.Errorf("%s %s answered %d with a body that is not JSON: %v", method, template, status, err)
			return
		}
		for _, problem := range conform(schema, value, "body") {
			t.Errorf("%s %s answered %d: %s", method, template, status, problem)
		}
	case "application/x-ndjson":
		for i, line := range bytes.Split(bytes.TrimSpace(body), []byte("\n")) {
			var value any
			if err := json.Unmarshal(line, &value); err != nil {
				t.Errorf("%s %s line %d is not JSON: %v", method, template, i+1, err)
				continue
			}
			for _, problem := range conform(schema, value, fmt.Sprintf("line %d", i+1)) {
				t.Errorf("%s %s answered %d: %s", method, template, status, problem)
			}
		}
	}
}

// TestTheServerRoutesTheContract holds the two lists equal: every
// operation of the contract is routed, every route is in the contract, and
// the two agree on which are planned.
func TestTheServerRoutesTheContract(t *testing.T) {
	routed := map[string]bool{}
	for _, rt := range (&Server{}).Routes() {
		routed[rt.Method+" "+rt.Path] = rt.Planned
	}
	described := map[string]bool{}
	for path, item := range contract["paths"].(map[string]any) {
		for method, op := range item.(map[string]any) {
			if method == "parameters" || path == "/openapi.yaml" {
				continue
			}
			described[strings.ToUpper(method)+" "+path] = at(op, "x-lectio-status") == "planned"
		}
	}
	if !maps.Equal(routed, described) {
		for _, key := range slices.Sorted(maps.Keys(described)) {
			if planned, ok := routed[key]; !ok {
				t.Errorf("the contract has %s and the server does not route it", key)
			} else if planned != described[key] {
				t.Errorf("%s: planned is %v in the contract and %v in the server", key, described[key], planned)
			}
		}
		for _, key := range slices.Sorted(maps.Keys(routed)) {
			if _, ok := described[key]; !ok {
				t.Errorf("the server routes %s and the contract does not have it", key)
			}
		}
	}

	// A planned operation lists the 501 it answers, and the description
	// says once which statuses any operation may answer.
	for path, item := range contract["paths"].(map[string]any) {
		for method, op := range item.(map[string]any) {
			if at(op, "x-lectio-status") == "planned" && at(op, "responses", "501") == nil {
				t.Errorf("%s %s is planned and lists no 501", method, path)
			}
		}
	}
}

// TestTheServerAnswersTheContractsCodes holds the envelope's codes and the
// server's table equal, so a code cannot be answered that a client was not
// told of, and none is promised that is never sent.
func TestTheServerAnswersTheContractsCodes(t *testing.T) {
	enum, _ := at(contract, "components", "schemas", "ErrorEnvelope", "properties", "error", "properties", "code", "enum").([]any)
	described := slices.Sorted(anyStrings(enum))
	var answered []string
	for code, p := range problems {
		answered = append(answered, string(code))
		if p.message == "" || !strings.HasSuffix(p.message, ".") || p.status < 400 {
			t.Errorf("%s: status %d, message %q", code, p.status, p.message)
		}
	}
	slices.Sort(answered)
	if !slices.Equal(described, answered) {
		t.Errorf("the contract's codes and the server's differ:\n  contract %v\n  server   %v", described, answered)
	}

	// What a parse or a page can fail with is a code the contract lists for
	// a problem.
	problem, _ := at(contract, "components", "schemas", "Problem", "properties", "code", "enum").([]any)
	for _, code := range []fault.Code{
		fault.FileNotFound, fault.FileTooLarge, fault.InvalidPages, fault.DocumentCorrupt, fault.UnsupportedMediaType,
		fault.TooManyPages, fault.SourceUnreachable, fault.PageUnreadable, fault.FigureUnreadable, fault.ReaderUnavailable,
		fault.BudgetExhausted, fault.DeadlineExceeded, fault.SchemaNotSatisfied, fault.Internal,
	} {
		if !slices.Contains(problem, any(string(code))) {
			t.Errorf("a parse can fail with %s and the contract's Problem does not list it", code)
		}
	}
}

func TestConformFindsWhatDeparts(t *testing.T) {
	schema := map[string]any{"$ref": "#/components/schemas/Parse"}
	good := map[string]any{
		"id": "prs_1", "state": "queued", "class": "batch", "priority": 0.0, "file": "fil_1", "created_at": "now",
		"progress": map[string]any{"stage": "queued", "pages_total": 0.0, "pages_done": 0.0, "pages_failed": 0.0},
		"usage":    map[string]any{}, "labels": map[string]any{"k": "v"}, "languages": []any{"de"},
	}
	if got := conform(schema, good, "body"); len(got) != 0 {
		t.Fatalf("a parse in the contract's shape: %v", got)
	}
	for name, change := range map[string]func(map[string]any){
		"lacks":        func(m map[string]any) { delete(m, "state") },
		"outside":      func(m map[string]any) { m["state"] = "paused" },
		"not describe": func(m map[string]any) { m["speed"] = 1.0 },
		"is a string":  func(m map[string]any) { m["priority"] = "high" },
		"is a number":  func(m map[string]any) { m["id"] = 4.0 },
		"is a boolean": func(m map[string]any) { m["file"] = true },
		"is an array":  func(m map[string]any) { m["usage"] = []any{} },
		"is an object": func(m map[string]any) { m["languages"] = map[string]any{} },
		"is null":      func(m map[string]any) { m["class"] = nil },
		"labels.k":     func(m map[string]any) { m["labels"] = map[string]any{"k": 1.0} },
		"languages[0]": func(m map[string]any) { m["languages"] = []any{2.0} },
	} {
		bad := maps.Clone(good)
		change(bad)
		got := conform(schema, bad, "body")
		if len(got) != 1 || !strings.Contains(got[0], name) {
			t.Errorf("%s: %v", name, got)
		}
	}
	// A box may be null, and a member with no type takes any value.
	block := map[string]any{"ref": "1.1", "kind": "text", "order": 1.0, "box": nil, "text": ""}
	if got := conform(map[string]any{"$ref": "#/components/schemas/Block"}, block, "body"); len(got) != 0 {
		t.Errorf("a block with no box: %v", got)
	}
	if got := conform(map[string]any{"description": "anything"}, nil, "body"); len(got) != 0 {
		t.Errorf("a schema with no type: %v", got)
	}
	if got := conform(nil, 1.0, "body"); len(got) != 0 {
		t.Errorf("no schema: %v", got)
	}
}

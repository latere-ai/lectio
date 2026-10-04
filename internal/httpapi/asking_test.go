// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/run"
)

// operations lists every operation of api/openapi.yaml as "METHOD /path",
// sorted, with the deletes last and a parse's before a file's: a walk that
// deletes what the other operations are about ends with nothing to ask.
func operations(t *testing.T) []string {
	t.Helper()
	var out, last []string
	for path, item := range contract["paths"].(map[string]any) {
		for method := range item.(map[string]any) {
			if method == "parameters" {
				continue
			}
			op := strings.ToUpper(method) + " " + path
			if method == "delete" {
				last = append(last, op)
				continue
			}
			out = append(out, op)
		}
	}
	slices.Sort(out)
	slices.Sort(last)
	slices.Reverse(last)
	return append(out, last...)
}

// asked is a server whose every question is recorded, with a file and a
// parse of alice's that carries every member a question about a parse
// may: a pinned reader, labels, an origin, and a counted selection.
type asked struct {
	*env
	rec         *recorder
	file, parse string
}

func asking(t *testing.T) *asked {
	t.Helper()
	rec := &recorder{}
	e := serve(t, func(s *Server, _ *run.Runner) { s.Authz = access.NewAuthorizer(rec, configured()) })
	file := e.upload("scan.png", sheet(t))
	parse := e.parsed(file, `,"reader":"stub","class":"batch","priority":2,"pages":"1","labels":{"batch":"oct"},"origin":{"store":"s3","path":"a/1.png","version":"4"}`)
	if parse["state"] != "succeeded" {
		t.Fatalf("the parse the walk is about ended %v", parse)
	}
	rec.take()
	return &asked{env: e, rec: rec, file: file, parse: parse["id"].(string)}
}

// request makes the request of one operation, with a body where the
// operation takes one.
func (a *asked) request(op string) reply {
	a.t.Helper()
	method, path, _ := strings.Cut(op, " ")
	path = strings.NewReplacer("{file}", a.file, "{parse}", a.parse, "{page}", "1", "{ref}", "1.1", "{name}", "invoice").Replace(path)
	switch op {
	case "POST /files":
		return a.do(method, path, sheet(a.t), "Content-Type", "image/png")
	case "POST /parses":
		return a.do(method, path, `{"source":{"file":"`+a.file+`"},"reader":"stub","class":"batch","priority":2,"pages":"1-2,2","labels":{"k":"v"},"origin":{"path":"a/2.png"},"reuse":false}`)
	}
	return a.do(method, path, nil)
}

// known are the members a request of the walk knows of its resource:
// every one of a stored object, every one of a create but an owner, which
// no request names, and none of a list or of a read that names nothing.
func known(row access.Route) []string {
	switch {
	case slices.Contains(row.Fields, authorizer.FieldID):
		return slices.Sorted(slices.Values(row.Fields))
	case row.Action == authorizer.ActionParseCreate || row.Action == authorizer.ActionFileCreate:
		return slices.Sorted(slices.Values(slices.DeleteFunc(row.Fields, func(f string) bool { return f == authorizer.FieldOwner })))
	}
	return nil
}

// TestEveryRouteAsksItsAction walks every operation of api/openapi.yaml
// with an authorizer that records what it is asked. An operation that asks
// nothing, asks another action than its row of access.Routes names, asks
// about another kind, or sends another set of fields fails here. The
// contract itself is the one operation that asks nothing.
func TestEveryRouteAsksItsAction(t *testing.T) {
	a := asking(t)
	ops := operations(t)
	if len(ops) != len(access.Routes()) {
		t.Fatalf("the contract has %d operations and the table of routes %d", len(ops), len(access.Routes()))
	}
	for _, op := range ops {
		method, path, _ := strings.Cut(op, " ")
		row, ok := access.RouteOf(method, path)
		if !ok {
			t.Errorf("%s has no row in the table of routes", op)
			continue
		}
		got := a.request(op)
		questions := a.rec.take()
		if row.Action == "" {
			if len(questions) != 0 || got.status != http.StatusOK {
				t.Errorf("%s takes no token and asks nothing: %d, asked %+v", op, got.status, questions)
			}
			continue
		}
		if len(questions) != 1 {
			t.Errorf("%s asked %d questions, want 1 (answered %d %s)", op, len(questions), got.status, got.body)
			continue
		}
		q := questions[0]
		if q.Action != row.Action || q.Resource.Kind != authorizer.Kind(row.Action) {
			t.Errorf("%s asked %s about a %s, want %s about a %s", op, q.Action, q.Resource.Kind, row.Action, authorizer.Kind(row.Action))
		}
		if q.Subject != "alice" {
			t.Errorf("%s asked as %q", op, q.Subject)
		}
		sent := slices.Collect(maps.Keys(q.Resource.Fields))
		if q.Resource.ID != "" {
			sent = append(sent, authorizer.FieldID)
		}
		slices.Sort(sent)
		if want := known(row); !slices.Equal(sent, want) {
			t.Errorf("%s sent %v, want %v", op, sent, want)
		}
		// A stored object is asked about as it is stored, under its owner.
		if slices.Contains(row.Fields, authorizer.FieldID) {
			id := a.parse
			if q.Resource.Kind == authorizer.KindFile {
				id = a.file
			}
			if q.Resource.ID != id || q.Resource.String(authorizer.FieldOwner) != "alice" {
				t.Errorf("%s asked about %q of %q", op, q.Resource.ID, q.Resource.String(authorizer.FieldOwner))
			}
		}
		if got.status == http.StatusUnauthorized || got.status == http.StatusForbidden || got.status >= http.StatusInternalServerError && got.status != http.StatusNotImplemented {
			t.Errorf("%s was allowed and answered %d %s", op, got.status, got.body)
		}
	}
}

// TestWhatAQuestionCarries: the members of a stored parse and of a submit,
// as the authorizer reads them.
func TestWhatAQuestionCarries(t *testing.T) {
	a := asking(t)
	a.request("GET /parses/{parse}")
	stored := a.rec.take()[0].Resource
	if stored.String("class") != "batch" || stored.Int("priority") != 2 || stored.String("reader") != "stub" || stored.Int("pages") != 1 {
		t.Errorf("a stored parse was asked about as %+v", stored)
	}
	if labels, _ := stored.Fields["labels"].(map[string]any); labels["batch"] != "oct" {
		t.Errorf("its labels: %v", stored.Fields["labels"])
	}
	if origin, _ := stored.Fields["origin"].(map[string]any); origin["store"] != "s3" || origin["path"] != "a/1.png" || origin["version"] != "4" {
		t.Errorf("its origin: %v", stored.Fields["origin"])
	}

	// A submit sends the size of a selection that names every page it
	// reads, counting a page named twice once, and none for a selection
	// that runs to the end of a file nobody has counted.
	a.request("POST /parses")
	if submit := a.rec.take()[0].Resource; submit.Int("pages") != 2 || submit.ID != "" || submit.String("owner") != "" {
		t.Errorf("a submit was asked about as %+v", submit)
	}
	a.do("POST", "/parses", `{"source":{"file":"`+a.file+`"},"pages":"1-"}`)
	if open := a.rec.take()[0].Resource; open.Fields["pages"] != nil || open.Fields["reader"] != nil || open.String("class") != "interactive" {
		t.Errorf("a submit with an open selection was asked about as %+v", open)
	}

	// An upload declares its size when its body is the file, and its type.
	a.request("POST /files")
	if upload := a.rec.take()[0].Resource; upload.Fields["size"] != int64(len(sheet(t))) || upload.String("media_type") != "image/png" {
		t.Errorf("an upload was asked about as %+v", upload)
	}
	// What is known of the request rides along: the user agent here.
	a.do("GET", "/readers", nil, "User-Agent", "walker/1", "X-Request-Id", "req-7")
	if req := a.rec.take()[0].Request; req.UserAgent != "walker/1" || req.ID != "req-7" || req.IP == "" {
		t.Errorf("the request was described as %+v", req)
	}
}

func TestASelectionIsMeasuredWithoutBeingListed(t *testing.T) {
	for expr, want := range map[string]int{
		"1": 1, "1-3,7": 4, "1-2,2": 2, "5-9,1-6": 9, "3,3,3": 1, " 2 - 4 , 9 ": 4, "1-2000000000": 2000000000,
		"": 0, "5-": 0, "1-3,7-": 0,
	} {
		n, ok := selected(expr)
		if n != want || ok != (want > 0) {
			t.Errorf("selected(%q) = %d, %t, want %d", expr, n, ok, want)
		}
	}
}

// TestADenyAndAnOutageAnswerBeforeAnythingIsDone: with an authorizer that
// denies, a stored object is answered as one that is not there and every
// other operation as forbidden, with the authorizer's reason in the
// developer detail and never in the sentence a person reads. With one that
// gives no decision every operation fails closed with
// authorizer_unavailable, and the answer names nothing of the authorizer.
// Either way nothing was done: the file and the parse are still there.
func TestADenyAndAnOutageAnswerBeforeAnythingIsDone(t *testing.T) {
	a := asking(t)
	for _, tc := range []struct {
		name   string
		answer func(authz.Request) (authz.Decision, error)
		// stored and other are what an operation on a stored object and
		// any other operation answer.
		stored, other int
		code          string
	}{
		{"a deny", func(authz.Request) (authz.Decision, error) { return authz.Decision{Reason: "plan_expired"}, nil },
			http.StatusNotFound, http.StatusForbidden, "forbidden"},
		{"no decision", func(authz.Request) (authz.Decision, error) {
			return authz.Decision{}, errors.New("post https://authz.internal.example/decide: connection refused")
		}, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "authorizer_unavailable"},
	} {
		a.rec.answer = tc.answer
		for _, op := range operations(t) {
			method, path, _ := strings.Cut(op, " ")
			row, _ := access.RouteOf(method, path)
			if row.Action == "" {
				continue
			}
			got := a.request(op)
			onStored := slices.Contains(row.Fields, authorizer.FieldID)
			want, code := tc.other, tc.code
			if onStored {
				want = tc.stored
				if want == http.StatusNotFound {
					code = "parse_not_found"
					if authorizer.Kind(row.Action) == authorizer.KindFile {
						code = "file_not_found"
					}
				}
			}
			if got.status != want || got.code(t) != code {
				t.Errorf("%s, %s: %d %s", tc.name, op, got.status, got.body)
				continue
			}
			message, _ := at(got.json(t), "error", "message").(string)
			reason, _ := at(got.json(t), "error", "details", "reason").(string)
			switch {
			case strings.Contains(string(got.body), "authz.internal.example") || strings.Contains(string(got.body), "connection refused"):
				t.Errorf("%s, %s: the answer names the authorizer: %s", tc.name, op, got.body)
			case strings.Contains(message, "plan_expired"):
				t.Errorf("%s, %s: the sentence a person reads holds the reason: %s", tc.name, op, got.body)
			case got.status == http.StatusForbidden && !strings.Contains(reason, "plan_expired"):
				t.Errorf("%s, %s: the developer detail lacks the reason: %s", tc.name, op, got.body)
			case got.status == http.StatusNotFound && strings.Contains(string(got.body), "plan_expired"):
				t.Errorf("%s, %s: a stored object's refusal differs from a missing object's: %s", tc.name, op, got.body)
			case got.status == http.StatusServiceUnavailable && at(got.json(t), "error", "details", "retryable") != true:
				t.Errorf("%s, %s: an outage may pass, and the answer does not say so: %s", tc.name, op, got.body)
			}
		}
		if n := len(a.rec.take()); n != len(operations(t))-1 {
			t.Errorf("%s: %d questions over %d operations that ask", tc.name, n, len(operations(t))-1)
		}
	}

	// Nothing was deleted, canceled or created while every answer was a
	// refusal, and a parse that is not there answers as the refused one did.
	a.rec.answer = nil
	if got := a.do("GET", "/parses/"+a.parse, nil); got.status != http.StatusOK || got.json(t)["state"] != "succeeded" {
		t.Fatalf("the parse after the refusals: %d %s", got.status, got.body)
	}
	if got := a.do("GET", "/files/"+a.file, nil); got.status != http.StatusOK {
		t.Fatalf("the file after the refusals: %d %s", got.status, got.body)
	}
	if listed := a.do("GET", "/parses", nil).json(t)["parses"].([]any); len(listed) != 1 {
		t.Fatalf("%d parses after the refusals, want the 1 of before", len(listed))
	}
	a.rec.take()
	missing := a.do("GET", "/parses/prs_00000000000000000000000000", nil)
	a.rec.answer = func(authz.Request) (authz.Decision, error) { return authz.Decision{Reason: "not_owner"}, nil }
	refused := a.do("GET", "/parses/"+a.parse, nil)
	if missing.status != refused.status || missing.code(t) != refused.code(t) || at(missing.json(t), "error", "message") != at(refused.json(t), "error", "message") {
		t.Errorf("a missing parse answers %s and a refused one %s", missing.body, refused.body)
	}
	shape := func(r reply, id string) string { return strings.ReplaceAll(string(r.body), id, "ID") }
	if shape(missing, "prs_00000000000000000000000000") != shape(refused, a.parse) {
		t.Errorf("the two answers differ beyond the id: %s and %s", missing.body, refused.body)
	}
}

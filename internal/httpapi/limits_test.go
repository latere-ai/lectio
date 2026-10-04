// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/fetch"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// bounded is a durable server whose authorizer allows everything with the
// limits a case sets, laid over the defaults the server is configured
// with: a file of at most 4096 bytes, a parse of at most 100 pages kept
// for an hour, a priority of at most 2, and a file kept for 30 minutes.
type bounded struct {
	*env
	rec   *recorder
	st    *postgres.Store
	calls *atomic.Int64
}

func holding(t *testing.T) *bounded {
	t.Helper()
	durably(t, false)
	rec, calls, release := &recorder{}, &atomic.Int64{}, make(chan struct{})
	defaults := authorizer.Limits{MaxFileBytes: 4096, MaxPages: 100, MaxPriority: 2, Retention: time.Hour}
	e := serve(t, func(s *Server, r *run.Runner) {
		s.Authz = access.NewAuthorizer(rec, defaults, access.FileRetention(30*time.Minute))
		s.Limits.MaxBytes, s.FileRetention = 4096, 30*time.Minute
		counted := &stub.Reader{Fail: func(reader.Page) error { calls.Add(1); return nil }}
		// The reader "other" holds every page it is given until the test
		// ends, so a parse that pins it stays open for as long as a case
		// needs.
		held := &stub.Reader{Fail: func(reader.Page) error { <-release; return nil }}
		r.Readers = map[string]reader.Reader{"stub": counted, "other": held}
	})
	// Registered after the server, so it runs before the server stops.
	t.Cleanup(func() { close(release) })
	return &bounded{env: e, rec: rec, st: storeOf(t, e), calls: calls}
}

// with sets the limits object every allow carries from here on. Empty is
// an allow that names none.
func (h *bounded) with(raw string) { h.rec.answer = allowing(raw, nil) }

// row reads a parse's row.
func (h *bounded) row(id string) postgres.Parse {
	h.t.Helper()
	p, err := h.st.Parse(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

// group reads a group's queue, with the weight and the bounds its last
// submit left it.
func (h *bounded) group(id string) postgres.GroupQueue {
	h.t.Helper()
	queue, err := h.st.Queue(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	for _, g := range queue {
		if g.Group == id {
			return g
		}
	}
	h.t.Fatalf("no group %q in %+v", id, queue)
	return postgres.GroupQueue{}
}

// submit submits a parse of a file with options and returns the answer.
func (h *bounded) submit(file, options string) reply {
	h.t.Helper()
	return h.do("POST", "/parses", `{"source":{"file":"`+file+`"},"reuse":false`+options+`}`, "Prefer", "wait=20")
}

// TestEachMemberOfAnAllowChangesTheOutcome: every member of the limits, one
// at a time, through the API over the durable backend. An allow that
// carries the member changes what the request does or how it is answered,
// and an allow that does not carry it leaves the server's configured
// default in force. A case per member of authorizer.Limits, and the test
// fails when a member has none.
func TestEachMemberOfAnAllowChangesTheOutcome(t *testing.T) {
	h := holding(t)
	h.with("")
	scan := h.upload("scan.png", sheet(t))
	tiff := h.upload("scan.tiff", testfixtures.Read(t, testfixtures.MultiTIFF))

	// ok submits and returns the parse that ended.
	ok := func(name, file, options string) map[string]any {
		t.Helper()
		r := h.submit(file, options)
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
		return r.json(t)
	}
	// refused submits and holds the answer to a status and a code.
	refused := func(name, file, options string, status int, code, field string) {
		t.Helper()
		r := h.submit(file, options)
		if r.status != status || r.code(t) != code || (field != "" && at(r.json(t), "error", "details", "field") != field) {
			t.Errorf("%s: %d %s, want %d %s", name, r.status, r.body, status, code)
		}
	}

	cases := map[string]func(t *testing.T){
		"Owner": func(t *testing.T) {
			// The allow says whose a new file and a new parse are.
			h.with(`{"owner": "org:acme"}`)
			up := h.do("POST", "/files?name=acme.png", append(sheet(t), 1), "Content-Type", "image/png")
			f, err := h.server.Backend.File(context.Background(), up.json(t)["id"].(string))
			if up.status != http.StatusCreated || err != nil || f.Owner != "org:acme" {
				t.Fatalf("an upload under an allow that names an owner: %d %s, %+v, %v", up.status, up.body, f, err)
			}
			if p := ok("named", f.ID, ""); p["owner"] != "org:acme" || h.row(p["id"].(string)).Group != "org:acme" {
				t.Errorf("the parse is %v's in the group %q", p["owner"], h.row(p["id"].(string)).Group)
			}
			// A file of the caller's own is not that owner's.
			refused("another owner's file", scan, "", http.StatusNotFound, "file_not_found", "")
			h.with("")
			if p := ok("absent", scan, ""); p["owner"] != "alice" {
				t.Errorf("with no owner named the parse is %v's", p["owner"])
			}
		},
		"Group": func(t *testing.T) {
			h.with(`{"group": "team-a"}`)
			if p := ok("named", scan, ""); h.row(p["id"].(string)).Group != "team-a" || h.row(p["id"].(string)).Owner != "alice" {
				t.Errorf("the parse joined %q", h.row(p["id"].(string)).Group)
			}
			h.with("")
			if p := ok("absent", scan, ""); h.row(p["id"].(string)).Group != "alice" {
				t.Errorf("with no group named the parse joined %q", h.row(p["id"].(string)).Group)
			}
		},
		"Weight": func(t *testing.T) {
			h.with(`{"group": "weighed", "weight": 8}`)
			ok("named", scan, "")
			if g := h.group("weighed"); g.Weight != 8 {
				t.Errorf("the group's weight is %d", g.Weight)
			}
			h.with(`{"group": "weighed"}`)
			ok("absent", scan, "")
			if g := h.group("weighed"); g.Weight != 1 {
				t.Errorf("with no weight named the group's is %d", g.Weight)
			}
		},
		"Project": func(t *testing.T) {
			h.with(`{"group": "divided", "project": "reports"}`)
			if p := ok("named", scan, ""); h.row(p["id"].(string)).Project != "reports" {
				t.Errorf("the parse joined the project %q", h.row(p["id"].(string)).Project)
			}
			h.with(`{"group": "divided"}`)
			if p := ok("absent", scan, ""); h.row(p["id"].(string)).Project != "" {
				t.Errorf("with no project named the parse joined %q", h.row(p["id"].(string)).Project)
			}
		},
		"ProjectWeight": func(t *testing.T) {
			weight := func() int {
				for _, p := range h.group("shared").Projects {
					if p.Project == "reports" {
						return p.Weight
					}
				}
				return 0
			}
			h.with(`{"group": "shared", "project": "reports", "project_weight": 3}`)
			ok("named", scan, "")
			if weight() != 3 {
				t.Errorf("the project's weight is %d", weight())
			}
			h.with(`{"group": "shared", "project": "reports"}`)
			ok("absent", scan, "")
			if weight() != 1 {
				t.Errorf("with no weight named the project's is %d", weight())
			}
		},
		"MaxRunning": func(t *testing.T) {
			// The bound the claim reads: internal/store/postgres proves a
			// group at it is skipped (TestAGroupAtMaxRunningIsSkipped).
			h.with(`{"group": "narrow", "max_running": 2}`)
			ok("named", scan, "")
			if g := h.group("narrow"); g.MaxRunning != 2 {
				t.Errorf("the group may run %d tasks at once", g.MaxRunning)
			}
			h.with(`{"group": "narrow"}`)
			ok("absent", scan, "")
			if g := h.group("narrow"); g.MaxRunning != 0 {
				t.Errorf("with no bound named the group's is %d", g.MaxRunning)
			}
		},
		"MaxQueued": func(t *testing.T) {
			// A parse that has not ended holds the group's one place: the
			// reader it pins does not answer until the test ends.
			h.with(`{"group": "full", "max_queued": 1}`)
			waiting := h.do("POST", "/parses", `{"source":{"file":"`+scan+`"},"reuse":false,"reader":"other"}`)
			if waiting.status != http.StatusAccepted {
				t.Fatalf("the first parse: %d %s", waiting.status, waiting.body)
			}
			second := h.do("POST", "/parses", `{"source":{"file":"`+scan+`"},"reuse":false}`)
			if second.status != http.StatusTooManyRequests || second.code(t) != "queue_full" {
				t.Errorf("a second parse of a group that may hold 1: %d %s", second.status, second.body)
			}
			// The same 2 submits with no bound named are both admitted.
			h.with(`{"group": "roomy"}`)
			for i := range 2 {
				options := []string{`,"reader":"other"`, ``}[i]
				if r := h.do("POST", "/parses", `{"source":{"file":"`+scan+`"},"reuse":false`+options+`}`); r.status != http.StatusAccepted && r.status != http.StatusOK {
					t.Errorf("with no bound named, parse %d: %d %s", i+1, r.status, r.body)
				}
			}
		},
		"MaxPriority": func(t *testing.T) {
			h.with(`{"max_priority": 5}`)
			if p := ok("raised", scan, `,"priority":-5`); p["priority"] != -5.0 {
				t.Errorf("the parse's priority is %v", p["priority"])
			}
			refused("past the allow's bound", scan, `,"priority":6`, http.StatusBadRequest, "invalid_request", "priority")
			h.with(`{"max_priority": 0}`)
			refused("a bound of zero", scan, `,"priority":1`, http.StatusBadRequest, "invalid_request", "priority")
			ok("priority 0 under a bound of zero", scan, "")
			h.with("")
			ok("at the server's bound", scan, `,"priority":2`)
			refused("past the server's bound", scan, `,"priority":3`, http.StatusBadRequest, "invalid_request", "priority")
		},
		"Classes": func(t *testing.T) {
			h.with(`{"classes": ["interactive"]}`)
			refused("a class the allow does not name", scan, `,"class":"batch"`, http.StatusForbidden, "forbidden", "class")
			ok("a class it names", scan, `,"class":"interactive"`)
			h.with("")
			if p := ok("absent", scan, `,"class":"batch"`); p["class"] != "batch" {
				t.Errorf("with no classes named the parse is %v", p["class"])
			}
		},
		"Readers": func(t *testing.T) {
			h.with(`{"readers": ["other"]}`)
			refused("a reader the allow does not name", scan, `,"reader":"stub"`, http.StatusForbidden, "reader_not_permitted", "reader")
			// What the routing policy picks is the server's choice, and no pin.
			ok("no reader pinned", scan, "")
			h.with("")
			if p := ok("absent", scan, `,"reader":"stub"`); p["reader"] != "stub" {
				t.Errorf("with no readers named the parse pinned %v", p["reader"])
			}
		},
		"MaxFileBytes": func(t *testing.T) {
			big := make([]byte, 2000)
			copy(big, sheet(t))
			h.with(`{"max_file_bytes": 1000}`)
			if r := h.do("POST", "/files?name=big.png", big, "Content-Type", "image/png"); r.status != http.StatusRequestEntityTooLarge || r.code(t) != "file_too_large" {
				t.Errorf("an upload past the allow's bound: %d %s", r.status, r.body)
			}
			h.with("")
			up := h.do("POST", "/files?name=big.png", big, "Content-Type", "image/png")
			if up.status != http.StatusCreated {
				t.Fatalf("with no bound named, the same upload: %d %s", up.status, up.body)
			}
			// A file that is there is held to the bound of the submit that
			// names it.
			h.with(`{"max_file_bytes": 1000}`)
			refused("a stored file past the bound", up.json(t)["id"].(string), "", http.StatusRequestEntityTooLarge, "file_too_large", "")
			ok("a stored file inside it", scan, "")
			// An allow lowers the server's bound and never raises it.
			h.with(`{"max_file_bytes": 1000000}`)
			if r := h.do("POST", "/files", make([]byte, 5000), "Content-Type", "image/png"); r.status != http.StatusRequestEntityTooLarge {
				t.Errorf("an upload past the server's bound under an allow that names a higher one: %d %s", r.status, r.body)
			}
		},
		"MaxPages": func(t *testing.T) {
			h.with(`{"max_pages": 2}`)
			before := h.calls.Load()
			if p := ok("past the bound", tiff, ""); p["state"] != "failed" || at(p, "error", "code") != "too_many_pages" || at(p, "progress", "pages_total") != 3.0 {
				t.Errorf("3 pages under a bound of 2: %v", p)
			}
			if h.calls.Load() != before {
				t.Errorf("%d pages of a parse past its bound were read", h.calls.Load()-before)
			}
			if p := ok("a selection inside it", tiff, `,"pages":"2-3"`); p["state"] != "succeeded" {
				t.Errorf("2 of 3 pages under a bound of 2: %v", p)
			}
			h.with("")
			if p := ok("absent", tiff, ""); p["state"] != "succeeded" || at(p, "progress", "pages_done") != 3.0 {
				t.Errorf("with no bound named: %v", p)
			}
		},
		"PagesPerDay": func(t *testing.T) {
			// 2 pages of the day are left for a file of 3: the parse fails
			// where its pages are counted, and no reader is called.
			h.with(`{"group": "budgeted", "pages_per_day": 2}`)
			before := h.calls.Load()
			if p := ok("past what is left", tiff, ""); p["state"] != "failed" || at(p, "error", "code") != "budget_exhausted" {
				t.Errorf("3 pages with 2 left of the day: %v", p)
			}
			if h.calls.Load() != before {
				t.Errorf("%d pages of a parse the day did not hold were read", h.calls.Load()-before)
			}
			if p := ok("what is left", tiff, `,"pages":"1-2"`); p["state"] != "succeeded" {
				t.Errorf("2 pages with 2 left of the day: %v", p)
			}
			// Nothing is left: the next submit is refused at once.
			refused("nothing left", scan, "", http.StatusPaymentRequired, "budget_exhausted", "")
			h.with(`{"group": "budgeted"}`)
			if p := ok("absent", tiff, ""); p["state"] != "succeeded" {
				t.Errorf("with no budget named: %v", p)
			}
		},
		"Retention": func(t *testing.T) {
			kept := func(p map[string]any) time.Duration {
				row := h.row(p["id"].(string))
				if row.ExpiresAt == nil || row.FinishedAt == nil {
					t.Fatalf("the parse ended at %v and expires at %v", row.FinishedAt, row.ExpiresAt)
				}
				return row.ExpiresAt.Sub(*row.FinishedAt)
			}
			h.with(`{"retention_seconds": 60}`)
			if got := kept(ok("named", scan, "")); got != time.Minute {
				t.Errorf("the parse is kept for %s", got)
			}
			up := h.do("POST", "/files?name=short.png", append(sheet(t), 2), "Content-Type", "image/png").json(t)
			if got := expiry(t, up).Sub(created(t, up)).Round(time.Second); got != time.Minute {
				t.Errorf("the file is kept for %s", got)
			}
			// An allow lowers the server's time and never raises it.
			h.with(`{"retention_seconds": 86400}`)
			if got := kept(ok("above the server's", scan, "")); got != time.Hour {
				t.Errorf("under an allow that names a longer time the parse is kept for %s", got)
			}
			h.with("")
			if got := kept(ok("absent", scan, "")); got != time.Hour {
				t.Errorf("with no time named the parse is kept for %s", got)
			}
			up = h.do("POST", "/files?name=long.png", append(sheet(t), 3), "Content-Type", "image/png").json(t)
			if got := expiry(t, up).Sub(created(t, up)).Round(time.Second); got != 30*time.Minute {
				t.Errorf("with no time named the file is kept for %s", got)
			}
		},
	}
	limits := reflect.TypeFor[authorizer.Limits]()
	for field := range limits.Fields() {
		name := field.Name
		run, ok := cases[name]
		if !ok {
			t.Errorf("no case changes an outcome by the member %s", name)
			continue
		}
		t.Run(name, run)
	}
	if len(cases) != limits.NumField() {
		t.Errorf("%d cases over %d members", len(cases), limits.NumField())
	}
}

func created(t *testing.T, file map[string]any) time.Time { return instant(t, file, "created_at") }
func expiry(t *testing.T, file map[string]any) time.Time  { return instant(t, file, "expires_at") }

func instant(t *testing.T, v map[string]any, member string) time.Time {
	t.Helper()
	raw, _ := v[member].(string)
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("%s of %v: %v", member, v, err)
	}
	return at
}

// TestAFetchedFileIsHeldToItsSubmitsAllow: a file a submit fetches from a
// URL is its parse's owner's, is held to the size the submit's allow names,
// and is kept as a file is, or for the allow's shorter time.
func TestAFetchedFileIsHeldToItsSubmitsAllow(t *testing.T) {
	scan := sheet(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(scan) }))
	t.Cleanup(origin.Close)
	rec := &recorder{}
	e := serve(t, func(s *Server, _ *run.Runner) {
		s.Authz = access.NewAuthorizer(rec, configured())
		s.Fetcher = &fetch.Fetcher{AllowHTTP: true, Permit: func(ap netip.AddrPort) bool { return strings.HasSuffix(origin.URL, ap.String()) }}
	})
	body := `{"source":{"url":"` + origin.URL + `/scan.png"}}`

	rec.answer = allowing(`{"owner": "org:acme"}`, nil)
	got := e.do("POST", "/parses", body, "Prefer", "wait=20")
	if got.status != http.StatusOK || got.json(t)["owner"] != "org:acme" {
		t.Fatalf("a url under an allow that names an owner: %d %s", got.status, got.body)
	}
	file, err := e.server.Backend.File(context.Background(), got.json(t)["file"].(string))
	if err != nil || file.Owner != "org:acme" {
		t.Fatalf("the fetched file is %+v, %v", file, err)
	}
	rec.answer = allowing(`{"max_file_bytes": 50}`, nil)
	if got := e.do("POST", "/parses", body); got.status != http.StatusRequestEntityTooLarge || got.code(t) != "file_too_large" {
		t.Fatalf("a fetched file past the allow's bound: %d %s", got.status, got.body)
	}

	// How long the file is kept: the server's time for a file, or the
	// shorter one the submit's allow names for what its parse writes.
	for name, tc := range map[string]struct {
		server, allow, want time.Duration
	}{
		"no time named":           {30 * time.Minute, 0, 30 * time.Minute},
		"a shorter time":          {30 * time.Minute, time.Minute, time.Minute},
		"a longer time":           {30 * time.Minute, 2 * time.Hour, 30 * time.Minute},
		"no time of the server's": {0, time.Minute, time.Minute},
		"no time at all":          {0, 0, 0},
	} {
		if got := (&Server{FileRetention: tc.server}).fileRetention(authorizer.Limits{Retention: tc.allow}); got != tc.want {
			t.Errorf("%s: a fetched file is kept for %s, want %s", name, got, tc.want)
		}
	}
}

// TestAListIsNarrowedByItsAllowsFilter: the filter of a list's allow is
// laid over what the caller asked for. Owners are the owners the list
// ranges over, labels are labels a parse must carry beside the caller's
// own, and a filter that can match nothing answers an empty list.
func TestAListIsNarrowedByItsAllowsFilter(t *testing.T) {
	rec := &recorder{}
	e := serve(t, func(s *Server, _ *run.Runner) { s.Authz = access.NewAuthorizer(rec, configured()) })
	file := e.upload("scan.png", sheet(t))
	ids := map[string]string{}
	for name, body := range map[string]string{
		"alice-a": `,"labels":{"team":"a"}`, "alice-b": `,"labels":{"team":"b"}`,
	} {
		ids[name] = e.parsed(file, `,"reuse":false`+body)["id"].(string)
	}
	bob := e.as("bob-token")
	ids["bob-a"] = bob.parsed(bob.upload("scan.png", sheet(t)), `,"reuse":false,"labels":{"team":"a"}`)["id"].(string)

	listed := func(query string) []string {
		t.Helper()
		r := e.do("GET", "/parses"+query, nil)
		if r.status != http.StatusOK {
			t.Fatalf("listing: %d %s", r.status, r.body)
		}
		var out []string
		for _, p := range r.json(t)["parses"].([]any) {
			for name, id := range ids {
				if at(p, "id") == id {
					out = append(out, name)
				}
			}
		}
		slices.Sort(out)
		return out
	}
	for _, tc := range []struct {
		name   string
		filter *authz.Filter
		query  string
		want   string
	}{
		{"no filter narrows nothing", nil, "", "alice-a alice-b bob-a"},
		{"one owner", &authz.Filter{Owners: []string{"alice"}}, "", "alice-a alice-b"},
		{"2 owners", &authz.Filter{Owners: []string{"alice", "bob"}}, "", "alice-a alice-b bob-a"},
		{"an owner with no parse", &authz.Filter{Owners: []string{"carol"}}, "", ""},
		{"owners that are named and are none", &authz.Filter{Owners: []string{}}, "", ""},
		{"a label", &authz.Filter{Labels: map[string]string{"team": "a"}}, "", "alice-a bob-a"},
		{"an owner and a label", &authz.Filter{Owners: []string{"bob"}, Labels: map[string]string{"team": "a"}}, "", "bob-a"},
		{"a label beside the caller's own", &authz.Filter{Labels: map[string]string{"team": "a"}}, "?state=succeeded", "alice-a bob-a"},
		{"the label the caller asked for", &authz.Filter{Labels: map[string]string{"team": "a"}}, "?label=team%3Da", "alice-a bob-a"},
		{"a label the caller asked for with another value", &authz.Filter{Labels: map[string]string{"team": "a"}}, "?label=team%3Db", ""},
		{"a filter that names nothing", &authz.Filter{}, "?label=team%3Db", "alice-b"},
	} {
		rec.answer = allowing("", tc.filter)
		if got := strings.Join(listed(tc.query), " "); got != tc.want {
			t.Errorf("%s: listed %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestTheOwnerPolicyThroughTheAPI: under the owner policy a subject cannot
// read, list, cancel or delete another subject's parse or file, and an
// admin subject reads them and lists every owner's, and changes nothing
// that is not its own.
func TestTheOwnerPolicyThroughTheAPI(t *testing.T) {
	gate := make(chan struct{})
	e := serve(t, func(_ *Server, r *run.Runner) {
		r.Readers = map[string]reader.Reader{"stub": &stub.Reader{Fail: func(reader.Page) error { <-gate; return nil }}}
	})
	defer close(gate)
	file := e.upload("scan.png", sheet(t))
	running := e.do("POST", "/parses", `{"source":{"file":"`+file+`"}}`).json(t)["id"].(string)
	bob, root := e.as("bob-token"), e.as("root-token")

	// Another subject: every read and every change is the answer of a
	// parse or a file that is not there, and its list is its own.
	for _, op := range []string{
		"GET /parses/" + running, "POST /parses/" + running + "/cancel", "DELETE /parses/" + running,
		"GET /parses/" + running + "/pages", "POST /parses/" + running + "/retry",
		"GET /files/" + file, "DELETE /files/" + file,
	} {
		method, path, _ := strings.Cut(op, " ")
		if got := bob.do(method, path, nil); got.status != http.StatusNotFound {
			t.Errorf("another subject, %s: %d %s", op, got.status, got.body)
		}
	}
	if listed := bob.do("GET", "/parses", nil).json(t)["parses"].([]any); len(listed) != 0 {
		t.Errorf("another subject lists %d parses", len(listed))
	}

	// An admin reads what another owns, and lists every owner's.
	for _, path := range []string{"/parses/" + running, "/parses/" + running + "/pages", "/files/" + file} {
		if got := root.do("GET", path, nil); got.status != http.StatusOK {
			t.Errorf("an admin, GET %s: %d %s", path, got.status, got.body)
		}
	}
	if listed := root.do("GET", "/parses", nil).json(t)["parses"].([]any); len(listed) != 1 || at(listed[0], "owner") != "alice" {
		t.Errorf("an admin lists %v", listed)
	}
	// An admin does not change what another owns: the parse still runs and
	// the file is still there.
	for _, op := range []string{
		"POST /parses/" + running + "/cancel", "DELETE /parses/" + running, "POST /parses/" + running + "/figures",
		"POST /parses/" + running + "/retry", "DELETE /files/" + file,
	} {
		method, path, _ := strings.Cut(op, " ")
		if got := root.do(method, path, nil); got.status != http.StatusNotFound {
			t.Errorf("an admin, %s: %d %s", op, got.status, got.body)
		}
	}
	if got := e.do("GET", "/parses/"+running, nil).json(t); got["state"] == "canceled" {
		t.Error("an admin canceled another owner's parse")
	}
	// What an admin creates is its own, and a parse of another owner's
	// file is a parse of a file that is not there.
	if got := root.do("POST", "/parses", `{"source":{"file":"`+file+`"}}`); got.status != http.StatusNotFound || got.code(t) != "file_not_found" {
		t.Errorf("an admin's parse of another owner's file: %d %s", got.status, got.body)
	}
	// The owner cancels its own.
	if got := e.do("POST", "/parses/"+running+"/cancel", nil); got.status != http.StatusOK {
		t.Fatalf("the owner's cancel: %d %s", got.status, got.body)
	}
	// Everybody lists the readers, which are nobody's.
	for _, who := range []*env{e, bob, root} {
		if got := who.do("GET", "/readers", nil); got.status != http.StatusOK {
			t.Errorf("the readers: %d %s", got.status, got.body)
		}
	}
}

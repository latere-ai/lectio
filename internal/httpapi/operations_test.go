// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/internal/testfixtures"
)

// totals sums a read of the meters per key, over its intervals, as
// "pages/calls/input/output": a case that reads across the turn of an hour
// finds its usage in 2 sums.
func totals(t *testing.T, r reply) map[string]string {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("the meters answered %d %s", r.status, r.body)
	}
	sums := map[string][4]float64{}
	for _, raw := range r.json(t)["sums"].([]any) {
		sum := raw.(map[string]any)
		have := sums[sum["key"].(string)]
		for i, member := range []string{"pages", "calls", "input_tokens", "output_tokens"} {
			have[i] += sum[member].(float64)
		}
		sums[sum["key"].(string)] = have
	}
	out := map[string]string{}
	for key, s := range sums {
		out[key] = fmt.Sprintf("%.0f/%.0f/%.0f/%.0f", s[0], s[1], s[2], s[3])
	}
	return out
}

// TestUsageIsReadFromTheMeters: the pages 2 callers read are metered as
// their tasks settle and read back summed by group, by owner and by
// reader, over hours and over days. What a caller is answered equals what
// its parses say they used. A caller sees its own usage, an admin
// everyone's, and one owner's or one group's is asked of the authorizer by
// name.
func TestUsageIsReadFromTheMeters(t *testing.T) {
	durably(t, false)
	e := serve(t, nil)
	bob, root := e.as("bob-token"), e.as("root-token")
	tiff := e.upload("scan.tiff", testfixtures.Read(t, testfixtures.MultiTIFF))
	// alice reads 3 pages of a scan, the same 3 again from the earlier
	// read, and a format that needs no reader. bob reads 1 page.
	var pages, input, output float64
	for _, p := range []map[string]any{
		e.parsed(tiff, `,"reuse":false`),
		e.parsed(tiff, ``),
		e.parsed(e.upload("sample.csv", testfixtures.Read(t, testfixtures.CSV)), ""),
	} {
		if p["state"] != "succeeded" {
			t.Fatalf("a parse ended %v", p)
		}
		usage := p["usage"].(map[string]any)
		pages += usage["pages"].(float64)
		in, _ := usage["input_tokens"].(float64)
		out, _ := usage["output_tokens"].(float64)
		input, output = input+in, output+out
	}
	if p := bob.parsed(bob.upload("scan.png", sheet(t)), `,"reuse":false`); p["state"] != "succeeded" {
		t.Fatalf("bob's parse ended %v", p)
	}

	// alice's usage is what her parses say: 7 pages, 3 of them read with a
	// call each.
	mine := fmt.Sprintf("%.0f/3/%.0f/%.0f", pages, input, output)
	if pages != 7 || input == 0 {
		t.Fatalf("alice's parses used %s", mine)
	}
	plain := e.do("GET", "/usage", nil)
	if got := totals(t, plain); len(got) != 1 || got["alice"] != mine {
		t.Fatalf("alice's usage by group is %v, want %s", got, mine)
	}
	if view := plain.json(t); view["by"] != "group" || view["interval"] != "hour" {
		t.Fatalf("a read that names nothing was taken as %v", view)
	}
	if got := totals(t, e.do("GET", "/usage?by=owner&interval=day", nil)); len(got) != 1 || got["alice"] != mine {
		t.Fatalf("alice's usage by owner over days is %v", got)
	}
	// By reader: the 6 pages claimed for the stub, 3 of them with a call,
	// and the page no reader read.
	byReader := totals(t, e.do("GET", "/usage?by=reader", nil))
	if len(byReader) != 2 || byReader["stub"] != fmt.Sprintf("6/3/%.0f/%.0f", input, output) || byReader[""] != "1/0/0/0" {
		t.Fatalf("alice's usage by reader is %v", byReader)
	}

	// bob sees his own, and an admin both.
	theirs := totals(t, bob.do("GET", "/usage", nil))
	if len(theirs) != 1 || theirs["alice"] != "" || theirs["bob"] == "" {
		t.Fatalf("bob's usage is %v", theirs)
	}
	if all := totals(t, root.do("GET", "/usage?by=owner", nil)); len(all) != 2 || all["alice"] != mine || all["bob"] != theirs["bob"] {
		t.Fatalf("an admin's read of the meters is %v", all)
	}
	// One owner or one group by name: one's own, and for an admin anyone's.
	if got := totals(t, e.do("GET", "/usage?owner=alice&group=alice", nil)); got["alice"] != mine {
		t.Fatalf("alice's usage, named, is %v", got)
	}
	if got := totals(t, root.do("GET", "/usage?by=owner&group=bob", nil)); len(got) != 1 || got["bob"] != theirs["bob"] {
		t.Fatalf("an admin's read of bob's group is %v", got)
	}
	for _, query := range []string{"?owner=bob", "?group=bob", "?owner=alice&group=bob"} {
		if got := e.do("GET", "/usage"+query, nil); got.status != http.StatusForbidden || got.code(t) != "forbidden" {
			t.Errorf("alice's read of %s: %d %s", query, got.status, got.body)
		}
	}

	// A span with nothing in it has no sum, and the answer says the span
	// it covers, in whole intervals.
	empty := e.do("GET", "/usage?from=2020-01-01T00:20:00Z&to=2020-01-01T02:40:00%2B01:00", nil)
	if view := empty.json(t); empty.status != http.StatusOK || len(view["sums"].([]any)) != 0 ||
		view["from"] != "2020-01-01T00:00:00Z" || view["to"] != "2020-01-01T02:00:00Z" {
		t.Fatalf("a read of a span with nothing in it: %d %s", empty.status, empty.body)
	}
}

// metering is a backend that records the reads of the meters it is asked
// for and answers each with what a case set.
type metering struct {
	*Memory
	asked []store.UsageQuery
	sums  []store.UsageSum
	err   error
}

func (m *metering) Usage(_ context.Context, q store.UsageQuery) ([]store.UsageSum, error) {
	m.asked = append(m.asked, q)
	return m.sums, m.err
}

// TestAReadOfTheMetersIsCheckedAndNarrowed: the span of a read is moved out
// to whole intervals, a read that names none ends now and goes back a day
// of hours or 30 days, and a parameter that is not one of the contract's is
// refused with its name. The read reaches the backend narrowed to the owner
// and the group it names and to the owners its allow's filter lists, and a
// filter that leaves nobody is answered with no sum and no read.
func TestAReadOfTheMetersIsCheckedAndNarrowed(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 34, 56, 0, time.UTC)
	at := func(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC) }
	rec := &recorder{}
	backend := &metering{Memory: &Memory{Store: store.NewMemory()}, sums: []store.UsageSum{{Key: "acme", Start: at(4, 12), Pages: 2, Calls: 3, InputTokens: 40, OutputTokens: 5}}}
	s := &Server{
		Backend: backend, Auth: callers, Authz: access.NewAuthorizer(rec, configured()),
		Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler),
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	e := &env{t: t, server: s, url: srv.URL, token: "alice-token"}

	for name, tc := range map[string]struct {
		query  string
		filter *authz.Filter
		// want is the read the backend is asked for, and nil when it is
		// asked for none.
		want *store.UsageQuery
	}{
		"nothing named": {"", nil, &store.UsageQuery{By: "group", Interval: "hour", From: at(3, 13), To: at(4, 13)}},
		"days": {"?interval=day&by=reader", nil,
			&store.UsageQuery{By: "reader", Interval: "day", From: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC), To: at(5, 0)}},
		"a span inside hours": {"?from=" + url.QueryEscape("2026-10-04T09:59:59+02:00") + "&to=2026-10-04T10:00:01Z", nil,
			&store.UsageQuery{By: "group", Interval: "hour", From: at(4, 7), To: at(4, 11)}},
		"a span of whole days": {"?interval=day&from=2026-10-01T00:00:00Z&to=2026-10-03T00:00:00Z", nil,
			&store.UsageQuery{By: "group", Interval: "day", From: at(1, 0), To: at(3, 0)}},
		"an owner and a group": {"?by=owner&owner=alice&group=acme", nil,
			&store.UsageQuery{By: "owner", Interval: "hour", From: at(3, 13), To: at(4, 13), Owners: []string{"alice"}, Groups: []string{"acme"}}},
		"a filter of owners": {"", &authz.Filter{Owners: []string{"alice", "bob"}},
			&store.UsageQuery{By: "group", Interval: "hour", From: at(3, 13), To: at(4, 13), Owners: []string{"alice", "bob"}}},
		"an owner the filter lists": {"?owner=bob", &authz.Filter{Owners: []string{"alice", "bob"}},
			&store.UsageQuery{By: "group", Interval: "hour", From: at(3, 13), To: at(4, 13), Owners: []string{"bob"}}},
		"a filter that names no owner": {"?group=acme", &authz.Filter{},
			&store.UsageQuery{By: "group", Interval: "hour", From: at(3, 13), To: at(4, 13), Groups: []string{"acme"}}},
		"an owner the filter does not list": {"?owner=carol", &authz.Filter{Owners: []string{"alice", "bob"}}, nil},
		"a filter that lists nobody":        {"", &authz.Filter{Owners: []string{}}, nil},
		"a filter by labels":                {"", &authz.Filter{Owners: []string{"alice"}, Labels: map[string]string{"team": "a"}}, nil},
	} {
		backend.asked = nil
		rec.answer = allowing("", tc.filter)
		got := e.do("GET", "/usage"+tc.query, nil)
		if got.status != http.StatusOK {
			t.Errorf("%s: %d %s", name, got.status, got.body)
			continue
		}
		sums := got.json(t)["sums"].([]any)
		switch {
		case tc.want == nil && (len(backend.asked) != 0 || len(sums) != 0):
			t.Errorf("%s: a read narrowed to nobody asked the backend %+v and answered %v", name, backend.asked, sums)
		case tc.want != nil && (len(backend.asked) != 1 || !reflect.DeepEqual(backend.asked[0], *tc.want) || len(sums) != 1):
			t.Errorf("%s: the backend was asked %+v, want %+v", name, backend.asked, *tc.want)
		}
	}
	// The question carries the owner and the group the request names.
	rec.take()
	e.do("GET", "/usage?owner=alice&group=acme", nil)
	if asked := rec.take(); len(asked) != 1 || asked[0].Action != "usage.read" || asked[0].Resource.String("owner") != "alice" || asked[0].Resource.String("group") != "acme" {
		t.Errorf("a read that names an owner and a group asked %+v", asked)
	}

	for name, tc := range map[string]struct{ query, field string }{
		"a key that is none":                {"?by=model", "by"},
		"an interval that is none":          {"?interval=week", "interval"},
		"a beginning that is no time":       {"?from=yesterday", "from"},
		"an end that is no time":            {"?to=2026-10-04", "to"},
		"a span that ends before it begins": {"?from=2026-10-04T10:00:00Z&to=2026-10-04T09:00:00Z", "from"},
		"a span of no interval":             {"?from=2026-10-04T10:00:00Z&to=2026-10-04T10:00:00Z", "from"},
		"a span of too many hours":          {"?from=2026-01-01T00:00:00Z&to=2026-10-04T00:00:00Z", "from"},
	} {
		got := e.do("GET", "/usage"+tc.query, nil)
		if got.status != http.StatusBadRequest || got.code(t) != "invalid_request" || fieldOf(t, got) != tc.field {
			t.Errorf("%s: %d %s", name, got.status, got.body)
		}
	}
	// As many intervals as a read may span, and no more.
	if got := e.do("GET", "/usage?interval=day&from=2024-01-08T00:00:00Z&to=2026-10-04T00:00:00Z", nil); got.status != http.StatusOK {
		t.Errorf("a read of %d days: %d %s", usageIntervals, got.status, got.body)
	}
	if got := e.do("GET", "/usage?interval=day&from=2024-01-07T00:00:00Z&to=2026-10-04T00:00:00Z", nil); got.status != http.StatusBadRequest {
		t.Errorf("a read of %d days: %d %s", usageIntervals+1, got.status, got.body)
	}

	// What the backend fails is answered as internal, and a deny as
	// forbidden with nothing read.
	backend.err, backend.asked = errors.New("the database is away"), nil
	rec.answer = nil
	if got := e.do("GET", "/usage", nil); got.status != http.StatusInternalServerError || got.code(t) != "internal" {
		t.Errorf("a read the backend fails: %d %s", got.status, got.body)
	}
	backend.asked = nil
	rec.answer = func(authz.Request) (authz.Decision, error) { return authz.Decision{Reason: "not_yours"}, nil }
	if got := e.do("GET", "/usage", nil); got.status != http.StatusForbidden || len(backend.asked) != 0 {
		t.Errorf("a read the authorizer denies: %d %s, with %d reads of the backend", got.status, got.body, len(backend.asked))
	}
}

// fieldOf is the field an error answer names.
func fieldOf(t *testing.T, r reply) string {
	t.Helper()
	field, _ := at(r.json(t), "error", "details", "field").(string)
	return field
}

// TestADevelopmentServerBuildsNothingOverTheTaskStore: the 7 operations
// that are built over the task store are routed and authenticated in a
// development server, ask their action, and answer 501, which the contract
// lists for each of them: a retry, the event stream, the meter, the view
// of the queue, and the 3 of an extraction. An extraction is checked as the
// contract says before it is answered, so a request that is malformed is
// told so by either server.
func TestADevelopmentServerBuildsNothingOverTheTaskStore(t *testing.T) {
	e := serve(t, nil)
	pid := e.parsed(e.upload("scan.png", sheet(t)), "")["id"].(string)
	for _, route := range []string{
		"POST /parses/" + pid + "/retry", "GET /parses/" + pid + "/events", "GET /usage", "GET /queue",
		"POST /parses/" + pid + "/fields", "GET /parses/" + pid + "/fields", "GET /parses/" + pid + "/fields/invoice",
	} {
		method, path, _ := strings.Cut(route, " ")
		var body any
		if strings.HasSuffix(route, "/fields") && method == "POST" {
			body = `{"name":"invoice","schema":{"type":"object"}}`
		}
		got := e.do(method, path, body)
		if got.status != http.StatusNotImplemented || got.code(t) != "not_implemented" {
			t.Errorf("%s in a development server: %d %s", route, got.status, got.body)
		}
		if got := e.as("").do(method, path, body); got.status != http.StatusUnauthorized {
			t.Errorf("%s with no token: %d %s", route, got.status, got.body)
		}
	}
	if got := e.as("bob-token").do("POST", "/parses/"+pid+"/retry", nil); got.status != http.StatusNotFound {
		t.Errorf("a retry of another caller's parse in a development server: %d %s", got.status, got.body)
	}
	if got := e.do("POST", "/parses/"+pid+"/fields", `{"name":"invoice","schema":{"type":"array"}}`); got.status != http.StatusBadRequest || got.code(t) != "invalid_schema" {
		t.Errorf("a schema that is refused, in a development server: %d %s", got.status, got.body)
	}
}

// TestTheQueueIsViewedPerGroupAndProject: a known set of parses of 2 groups
// is queued through the API, each under its own allow, and no worker runs.
// The view holds each group with its weight, its bounds, its parses that
// have not ended and its queued and running tasks per class, the same for
// each project, and the reader's pool with the calls in flight. A caller
// is answered the groups its allow's filter lists, with the calls in
// flight that are theirs, and one group by name when the authorizer
// allows.
func TestTheQueueIsViewedPerGroupAndProject(t *testing.T) {
	d := dispatched(t, nil)
	groups := map[string]string{"alice": "acme", "bob": "beta"}
	d.rec.answer = func(req authz.Request) (authz.Decision, error) {
		allow := authz.Decision{Allow: true}
		switch {
		case req.Action == "queue.read" && req.Subject == "root":
			// The operator sees every group.
		case req.Action == "queue.read" && req.Resource.String("group") == "":
			allow.Filter = &authz.Filter{Owners: []string{groups[req.Subject]}}
		case req.Action == "queue.read" && req.Resource.String("group") != groups[req.Subject]:
			return authz.Decision{Reason: "not_your_group"}, nil
		case req.Subject == "alice":
			allow.Limits = []byte(`{"group": "acme", "weight": 4, "max_queued": 10, "max_running": 6}`)
			if req.Resource.String("class") == "batch" {
				allow.Limits = []byte(`{"group": "acme", "weight": 4, "max_queued": 10, "max_running": 6, "project": "search", "project_weight": 2}`)
			}
		case req.Subject == "bob":
			allow.Limits = []byte(`{"group": "beta"}`)
		}
		return allow, nil
	}
	alice, bob, root := d.env, d.as("bob-token"), d.as("root-token")
	queued := func(e *env, options string) {
		t.Helper()
		file := e.upload("scan.png", sheet(t))
		if r := e.do("POST", "/parses", `{"source":{"file":"`+file+`"},"reuse":false`+options+`}`); r.status != http.StatusAccepted {
			t.Fatalf("submit: %d %s", r.status, r.body)
		}
	}
	// acme: one parse in its own project and a batch parse in the project
	// search. beta: 2 parses.
	queued(alice, ``)
	queued(alice, `,"class":"batch"`)
	queued(bob, ``)
	queued(bob, ``)

	// group finds a group of a view, and classes lists its tasks per class
	// as "class:queued/running".
	group := func(view map[string]any, id string) map[string]any {
		t.Helper()
		for _, g := range view["groups"].([]any) {
			if at(g, "group") == id {
				return g.(map[string]any)
			}
		}
		t.Fatalf("no group %s in %v", id, view["groups"])
		return nil
	}
	classes := func(of any) string {
		var out []string
		for _, c := range at(of, "classes").([]any) {
			out = append(out, fmt.Sprintf("%v:%v/%v", at(c, "class"), at(c, "queued"), at(c, "running")))
		}
		return strings.Join(out, " ")
	}

	// Nothing was claimed: each parse waits with its prepare task.
	view := root.do("GET", "/queue", nil)
	if view.status != http.StatusOK || len(view.json(t)["groups"].([]any)) != 2 {
		t.Fatalf("the queue: %d %s", view.status, view.body)
	}
	acme, beta := group(view.json(t), "acme"), group(view.json(t), "beta")
	if acme["weight"] != 4.0 || acme["max_queued"] != 10.0 || acme["max_running"] != 6.0 || acme["parses"] != 2.0 || classes(acme) != "interactive:1/0 batch:1/0" {
		t.Fatalf("acme is viewed as %v", acme)
	}
	projects := acme["projects"].([]any)
	if len(projects) != 2 || at(projects[0], "project") != "" || classes(projects[0]) != "interactive:1/0" || at(projects[0], "parses") != 1.0 ||
		at(projects[1], "project") != "search" || at(projects[1], "weight") != 2.0 || classes(projects[1]) != "batch:1/0" {
		t.Fatalf("the projects of acme are viewed as %v", projects)
	}
	if beta["weight"] != 1.0 || beta["max_queued"] != 0.0 || beta["parses"] != 2.0 || classes(beta) != "interactive:2/0" {
		t.Fatalf("beta is viewed as %v", beta)
	}
	pool := view.json(t)["pools"].([]any)
	if len(pool) != 1 || at(pool[0], "reader") != "stub" || at(pool[0], "in_flight") != 0.0 || at(pool[0], "breaker") != "closed" || len(at(pool[0], "scopes").([]any)) != 0 {
		t.Fatalf("the pools are viewed as %v", pool)
	}

	// The 4 prepares are run, each counting 3 pages, and 5 of the 12 pages
	// are claimed and held.
	ctx := context.Background()
	prepares, err := d.st.Exchange(ctx, d.worker, tasks.Request{Free: 10, Idle: true})
	if err != nil || len(prepares.Claims) != 4 {
		t.Fatalf("claiming the prepares: %d claims, %v", len(prepares.Claims), err)
	}
	var settles []tasks.Settle
	for _, c := range prepares.Claims {
		settles = append(settles, tasks.Settle{
			Parse: c.Parse, Task: c.Task, Token: c.Token, Outcome: tasks.Done, Units: 1,
			Prepare: &tasks.Prepared{Manifest: []byte(`{"media_type":"application/pdf","source":"reader","pages_total":3,"selected":[1,2,3]}`), Pages: []int{1, 2, 3}},
		})
	}
	reply, err := d.st.Exchange(ctx, d.worker, tasks.Request{Settles: settles, Free: 5, Idle: true})
	if err != nil || len(reply.Claims) != 5 || len(reply.Refused) != 0 {
		t.Fatalf("claiming 5 pages: %+v, %v", reply, err)
	}
	held := map[string]float64{}
	for _, c := range reply.Claims {
		held[c.Group]++
	}
	view = root.do("GET", "/queue", nil)
	for id, pages := range map[string]float64{"acme": 6, "beta": 6} {
		var waiting, running float64
		for _, c := range group(view.json(t), id)["classes"].([]any) {
			waiting, running = waiting+at(c, "queued").(float64), running+at(c, "running").(float64)
		}
		if running != held[id] || waiting+running != pages {
			t.Errorf("%s is viewed with %v tasks waiting and %v running, and %v of its %v pages were claimed", id, waiting, running, held[id], pages)
		}
	}
	if pool := view.json(t)["pools"].([]any); at(pool[0], "in_flight") != 5.0 {
		t.Fatalf("with 5 pages claimed the pool is viewed as %v", pool[0])
	}

	// A caller is answered its own group, and the calls that are its own.
	for sub, e := range map[string]*env{"alice": alice, "bob": bob} {
		own := e.do("GET", "/queue", nil)
		listed := own.json(t)["groups"].([]any)
		if own.status != http.StatusOK || len(listed) != 1 || at(listed[0], "group") != groups[sub] || at(own.json(t)["pools"].([]any)[0], "in_flight") != held[groups[sub]] {
			t.Errorf("%s's view of the queue: %d %s", sub, own.status, own.body)
		}
		named := e.do("GET", "/queue?group="+groups[sub], nil)
		if named.status != http.StatusOK || len(named.json(t)["groups"].([]any)) != 1 {
			t.Errorf("%s's view of its group by name: %d %s", sub, named.status, named.body)
		}
	}
	if got := alice.do("GET", "/queue?group=beta", nil); got.status != http.StatusForbidden || got.code(t) != "forbidden" {
		t.Errorf("alice's view of beta: %d %s", got.status, got.body)
	}
	if got := root.do("GET", "/queue?group=beta", nil); got.status != http.StatusOK || len(got.json(t)["groups"].([]any)) != 1 {
		t.Errorf("the operator's view of beta: %d %s", got.status, got.body)
	}
	// The question carries the group the request names.
	d.rec.take()
	alice.do("GET", "/queue?group=acme", nil)
	if asked := d.rec.take(); len(asked) != 1 || asked[0].Action != "queue.read" || asked[0].Resource.String("group") != "acme" {
		t.Errorf("a read that names a group asked %+v", asked)
	}
	// A filter that leaves nobody is answered with nothing, and not with
	// everything.
	d.rec.answer = allowing("", &authz.Filter{Owners: []string{}})
	if got := alice.do("GET", "/queue", nil); got.status != http.StatusOK || len(got.json(t)["groups"].([]any)) != 0 || len(got.json(t)["pools"].([]any)) != 0 {
		t.Errorf("a view narrowed to nobody: %d %s", got.status, got.body)
	}
}

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

// TestADevelopmentServerBuildsNothingOverTheTaskStore: the 4 operations
// that are built over the task store are routed and authenticated in a
// development server, ask their action, and answer 501, which the contract
// lists for each of them.
func TestADevelopmentServerBuildsNothingOverTheTaskStore(t *testing.T) {
	e := serve(t, nil)
	pid := e.parsed(e.upload("scan.png", sheet(t)), "")["id"].(string)
	for _, route := range []string{"POST /parses/" + pid + "/retry", "GET /parses/" + pid + "/events", "GET /usage", "GET /queue"} {
		method, path, _ := strings.Cut(route, " ")
		got := e.do(method, path, nil)
		if got.status != http.StatusNotImplemented || got.code(t) != "not_implemented" {
			t.Errorf("%s in a development server: %d %s", route, got.status, got.body)
		}
		if got := e.as("").do(method, path, nil); got.status != http.StatusUnauthorized {
			t.Errorf("%s with no token: %d %s", route, got.status, got.body)
		}
	}
	if got := e.as("bob-token").do("POST", "/parses/"+pid+"/retry", nil); got.status != http.StatusNotFound {
		t.Errorf("a retry of another caller's parse in a development server: %d %s", got.status, got.body)
	}
}

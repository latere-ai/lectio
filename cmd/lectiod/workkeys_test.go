// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Whose key an extraction and a figure are read with, in a running durable
// server (specs/013-limits-and-usage.md): as a page's, the key of the
// parse's group, asked of the operator's endpoint. The gateway stub answers
// a page with a paragraph and a figure, a figure with what it shows, and an
// extraction with an object, and records the key of every call.

// summarySchema is the schema the extractions of these cases ask for, and
// summarized the request that asks for it under a name.
const summarySchema = `{"type":"object","required":["summary"],"properties":{"summary":{"type":"string"}}}`

func summarized(name string) []byte {
	return []byte(`{"name":"` + name + `","schema":` + summarySchema + `}`)
}

// byGroup counts bearers by the group each key was issued for.
func (k *keyPlane) byGroup(bearers []string) map[string]int {
	out := map[string]int{}
	for _, bearer := range bearers {
		out[k.groupOf(bearer)]++
	}
	return out
}

// TestExtractionsAndFiguresOfTwoGroupsAreReadWithTwoKeys: with
// LECTIO_KEYS=endpoint, the figures and the extractions of 2 groups are
// read with the 2 keys their pages were read with. The gateway's record
// attributes every call to its group, each group's key was asked once, the
// meter of each group equals the gateway's count of the calls made with
// its key, and neither a key nor the endpoint's bearer is in a log, a row,
// an object or an answer.
func TestExtractionsAndFiguresOfTwoGroupsAreReadWithTwoKeys(t *testing.T) {
	p := newPlane(t)
	endpoint, gw := newKeyPlane(t), newGateway(t)
	settings := keyed(t, gw)
	base, apiLogs, _ := started(t, env(p.env("api", settings...)...))
	_, workerLogs, _ := started(t, env(p.env("worker", append(settings, endpoint.reads()...)...)...))

	answers := ""
	pages := map[string]int{"alice": 2, "bob": 1}
	for sub, n := range pages {
		tok := tokenOf(sub)
		id := submittedAs(t, base, tok, "scan.tiff", frames(n))
		if done := endedAs(t, base, tok, id); done["state"] != "succeeded" {
			t.Fatalf("the parse of %s ended %v\n%s", sub, done, workerLogs.String())
		}
		on := base + "/v1/parses/" + id

		// Every page holds a figure, and each is described by one call.
		status, listing, raw := call(t, "POST", on+"/figures", tok, nil, "Prefer", "wait=60")
		run, _ := listing["run"].(map[string]any)
		if status != http.StatusOK || run["state"] != "succeeded" || run["total"] != float64(n) || run["done"] != float64(n) || run["failed"] != 0.0 {
			t.Fatalf("the figures of %s: %d %s\n%s", sub, status, raw, workerLogs.String())
		}
		for _, f := range listing["figures"].([]any) {
			fig := f.(map[string]any)
			if fig["description"] != "A figure described through the gateway." || fig["text"] != "a label" || fig["figure"].(map[string]any)["type"] != "diagram" {
				t.Fatalf("a figure of %s: %v", sub, fig)
			}
		}
		if status, _, raw := call(t, "POST", on+"/fields", tok, summarized("summary")); status != http.StatusAccepted {
			t.Fatalf("asking the extraction of %s: %d %s", sub, status, raw)
		}
		field := fieldOf(t, on, tok, "summary")
		if field["state"] != "succeeded" || field["data"].(map[string]any)["summary"] != "A page read through the gateway." ||
			field["citations"].(map[string]any)["/summary"].([]any)[0] != "1.1" || field["model"] != "example-model" {
			t.Fatalf("the extraction of %s: %v", sub, field)
		}
		_, _, raw = call(t, "GET", on+"/figures", tok, nil)
		answers += string(raw)
		_, _, raw = call(t, "GET", on+"/fields", tok, nil)
		answers += string(raw)
	}

	// The gateway attributes each call to the group its key was issued for.
	conn := p.connect()
	for kind, want := range map[string]map[string]int{
		askedPage:    {groupOf("alice"): 2, groupOf("bob"): 1},
		askedFigure:  {groupOf("alice"): 2, groupOf("bob"): 1},
		askedExtract: {groupOf("alice"): 1, groupOf("bob"): 1},
	} {
		got := endpoint.byGroup(gw.served(kind))
		if len(got) != 2 || got[groupOf("alice")] != want[groupOf("alice")] || got[groupOf("bob")] != want[groupOf("bob")] {
			t.Errorf("the gateway attributes its calls for a %s as %v, want %v", kind, got, want)
		}
	}
	read, refused := gw.calls()
	for group, calls := range endpoint.byGroup(read) {
		if asked := endpoint.asked(group); len(asked) != 1 {
			t.Errorf("the key of %s was asked %d times, want once for its pages, its figures and its extraction", group, len(asked))
		}
		// The meter of a group equals the gateway's count of its calls.
		metered := value[string](t, conn, `SELECT sum(calls) || ' calls, ' || sum(input_tokens) || ' in, ' || sum(output_tokens) || ' out'
		                                     FROM usage WHERE group_id = $1`, group)
		if want := strconv.Itoa(calls) + " calls, " + strconv.Itoa(100*calls) + " in, " + strconv.Itoa(20*calls) + " out"; metered != want {
			t.Errorf("the meter of %s reads %s, and the gateway served it %s", group, metered, want)
		}
	}
	if kinds := value[string](t, conn, `SELECT string_agg(kind || '=' || calls, ' ' ORDER BY kind) FROM (SELECT kind, sum(calls) AS calls FROM usage GROUP BY kind) u`); refused != 0 || kinds != "extract=2 figure=3 page=3" {
		t.Fatalf("the meter by kind reads %q after %d refusals", kinds, refused)
	}

	// No key and no bearer is anywhere but in memory.
	sinks := map[string]string{"the API's log": apiLogs.String(), "the worker's log": workerLogs.String(), "what the API answers": answers}
	for _, key := range p.objects.Keys() {
		data, _ := p.objects.Get(key)
		sinks["the object "+key] = string(data)
	}
	for _, table := range []string{"fields", "figures", "figure_runs", "descriptions", "tasks", "parses", "usage", "pool_scopes"} {
		sinks["the table "+table] = value[string](t, conn, `SELECT coalesce(string_agg(t::text, E'\n'), '') FROM "`+table+`" t`)
	}
	if len(sinks["the table fields"]) == 0 || len(sinks["the table figures"]) == 0 || len(sinks["the table descriptions"]) == 0 {
		t.Fatal("the rows of the extractions and the figures were not read")
	}
	for where, text := range sinks {
		if strings.Contains(text, "sk-issued") || strings.Contains(text, keysBearer) {
			t.Errorf("%s holds a key or the endpoint's bearer", where)
		}
	}
	if n := value[int](t, conn, drift); n != 0 {
		t.Fatalf("%d counters differ from a recount of the rows", n)
	}
}

// TestARateLimitPausesAnExtractionAndAFigureOfItsGroupAlone: a rate-limit
// reply to an extraction's call, and to a figure's, pauses the key scope of
// its group as a page's does. The task waits behind the pause with no
// attempt spent, the group beside it is served meanwhile, and when the
// limit lifts the work completes. The calls the gateway refused are in the
// meter as the calls they were.
func TestARateLimitPausesAnExtractionAndAFigureOfItsGroupAlone(t *testing.T) {
	p := newPlane(t)
	endpoint, gw := newKeyPlane(t), newGateway(t)
	base, logs, _ := started(t, env(p.env("all", keyed(t, gw, endpoint.reads()...)...)...))
	alice, bob := tokenOf("alice"), tokenOf("bob")
	limited := base + "/v1/parses/" + submittedAs(t, base, alice, "scan.tiff", frames(2))
	free := base + "/v1/parses/" + submittedAs(t, base, bob, "scan.tiff", frames(1))
	for at, tok := range map[string]string{limited: alice, free: bob} {
		if done := endedAs(t, base, tok, at[strings.LastIndex(at, "/")+1:]); done["state"] != "succeeded" {
			t.Fatalf("a parse ended %v", done)
		}
	}

	// The limit begins once the pages are read.
	gw.limit(func(bearer string) bool { return endpoint.groupOf(bearer) == groupOf("alice") })
	if status, _, raw := call(t, "POST", limited+"/fields", alice, summarized("summary")); status != http.StatusAccepted {
		t.Fatalf("asking an extraction: %d %s", status, raw)
	}
	if status, _, raw := call(t, "POST", limited+"/figures", alice, nil); status != http.StatusAccepted {
		t.Fatalf("starting a run: %d %s", status, raw)
	}
	conn := p.connect()
	scopes := `SELECT coalesce(string_agg(reader || ' ' || scope, ',' ORDER BY scope), '') FROM pool_scopes`
	until(t, "the limited group's scope is paused", 2*time.Minute, func() bool {
		_, refused := gw.calls()
		return refused >= 1 && value[string](t, conn, scopes) == "gateway "+groupOf("alice")
	})

	// The group beside it is served while the pause holds.
	if status, _, raw := call(t, "POST", free+"/fields", bob, summarized("summary")); status != http.StatusAccepted {
		t.Fatalf("asking an extraction of the other group: %d %s", status, raw)
	}
	if f := fieldOf(t, free, bob, "summary"); f["state"] != "succeeded" {
		t.Fatalf("the extraction of the group beside the limited one: %v\n%s", f, logs.String())
	}
	if status, f, _ := call(t, "GET", limited+"/fields/summary", alice, nil); status != http.StatusOK || f["state"] != "pending" {
		t.Fatalf("the limited extraction is %v", f)
	}
	if spent := value[int](t, conn, `SELECT count(*) FROM tasks WHERE kind IN ('extract', 'figure') AND (attempt <> 0 OR error IS NOT NULL OR state NOT IN ('queued', 'leased'))`); spent != 0 {
		t.Fatalf("%d limited tasks spent an attempt or failed", spent)
	}

	gw.limit(nil)
	if f := fieldOf(t, limited, alice, "summary"); f["state"] != "succeeded" || f["attempts"] != 1.0 {
		t.Fatalf("once the limit lifted the extraction is %v\n%s", f, logs.String())
	}
	var listing map[string]any
	until(t, "the run ends once the limit lifted", 2*time.Minute, func() bool {
		_, listing, _ = call(t, "GET", limited+"/figures", alice, nil)
		return listing["run"].(map[string]any)["state"] != "running"
	})
	if run := listing["run"].(map[string]any); run["state"] != "succeeded" || run["done"] != 2.0 || run["failed"] != 0.0 {
		t.Fatalf("once the limit lifted the run is %v", run)
	}

	// The meter of the limited group holds every call made with its key,
	// the refused ones as the calls they were, and the tokens of the ones
	// that were served: 2 pages, 2 figures and 1 extraction.
	_, refused := gw.calls()
	metered := value[string](t, conn, `SELECT sum(calls) || '/' || sum(input_tokens) FROM usage WHERE group_id = $1`, groupOf("alice"))
	if want := strconv.Itoa(5+refused) + "/500"; refused < 1 || metered != want {
		t.Fatalf("the meter of the limited group reads %s after %d refusals, want %s", metered, refused, want)
	}
	if n := value[int](t, conn, drift); n != 0 {
		t.Fatalf("%d counters differ from a recount of the rows", n)
	}
}

// TestAGroupTheKeyEndpointRefusesFailsItsExtractionsAndItsFigures: a 402
// from the key endpoint fails an extraction and the figures of its group
// with budget_exhausted, and a 403 with reader_not_permitted, with no call
// to a model, no attempt spent, no scope paused and nothing counted
// against a reader. A key endpoint that is down fails nothing: the
// extraction waits, pending, behind its group's paused scope, and is
// filled on its first attempt once the endpoint answers. A group beside
// them is served throughout.
func TestAGroupTheKeyEndpointRefusesFailsItsExtractionsAndItsFigures(t *testing.T) {
	p := newPlane(t)
	endpoint, gw := newKeyPlane(t), newGateway(t)
	settings := keyed(t, gw)
	base, _, _ := started(t, env(p.env("api", settings...)...))
	_, _, stop := started(t, env(p.env("worker", append(settings, endpoint.reads()...)...)...))

	// The pages of 4 groups are read while every group is issued a key.
	on := map[string]string{}
	for _, sub := range []string{"alice", "bob", "carol", "dave"} {
		tok := tokenOf(sub)
		id := submittedAs(t, base, tok, "scan.png", striped(t))
		if done := endedAs(t, base, tok, id); done["state"] != "succeeded" {
			t.Fatalf("the parse of %s ended %v", sub, done)
		}
		on[sub] = base + "/v1/parses/" + id
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	// A worker that holds no key is started, and the endpoint now refuses 2
	// groups and does not answer for a third.
	var up atomic.Bool
	endpoint.answers(func(w http.ResponseWriter, _ int, q keyAsked) bool {
		switch {
		case q.Group == groupOf("bob"):
			w.WriteHeader(http.StatusPaymentRequired)
		case q.Group == groupOf("carol"):
			w.WriteHeader(http.StatusForbidden)
		case q.Group == groupOf("dave") && !up.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			return false
		}
		return true
	})
	_, logs, _ := started(t, env(p.env("worker", append(settings, endpoint.reads()...)...)...))
	served := len(gw.served(askedPage))

	for sub, want := range map[string]string{"bob": "budget_exhausted", "carol": "reader_not_permitted"} {
		tok := tokenOf(sub)
		if status, _, raw := call(t, "POST", on[sub]+"/fields", tok, summarized("summary")); status != http.StatusAccepted {
			t.Fatalf("asking the extraction of %s: %d %s", sub, status, raw)
		}
		if f := fieldOf(t, on[sub], tok, "summary"); f["state"] != "failed" || code(f) != want || f["data"] != nil {
			t.Fatalf("the extraction of %s, refused a key: %v\n%s", sub, f, logs.String())
		}
		status, listing, raw := call(t, "POST", on[sub]+"/figures", tok, nil, "Prefer", "wait=60")
		if status != http.StatusOK || listing["run"].(map[string]any)["state"] != "failed" || listing["run"].(map[string]any)["failed"] != 1.0 ||
			code(listing["figures"].([]any)[0].(map[string]any)) != want {
			t.Fatalf("the figures of %s, refused a key: %d %s", sub, status, raw)
		}
	}

	// The group whose key the endpoint does not answer for.
	dave := tokenOf("dave")
	if status, _, raw := call(t, "POST", on["dave"]+"/fields", dave, summarized("summary")); status != http.StatusAccepted {
		t.Fatalf("asking the extraction of dave: %d %s", status, raw)
	}
	conn := p.connect()
	paused := `SELECT count(*) FROM pool_scopes WHERE reader = 'gateway' AND scope = $1 AND paused_until > now()`
	waiting := `SELECT count(*) FROM tasks WHERE kind = 'extract' AND group_id = $1 AND state = 'queued' AND attempt = 0 AND error IS NULL`
	until(t, "the extraction waits behind its group's paused scope", 2*time.Minute, func() bool {
		return len(endpoint.asked(groupOf("dave"))) >= 3 && value[int](t, conn, paused, groupOf("dave")) == 1 && value[int](t, conn, waiting, groupOf("dave")) == 1
	})
	if status, f, _ := call(t, "GET", on["dave"]+"/fields/summary", dave, nil); status != http.StatusOK || f["state"] != "pending" {
		t.Fatalf("with the key endpoint down the extraction is %v", f)
	}

	// The group beside them is served.
	alice := tokenOf("alice")
	if status, _, raw := call(t, "POST", on["alice"]+"/fields", alice, summarized("summary")); status != http.StatusAccepted {
		t.Fatalf("asking the extraction of alice: %d %s", status, raw)
	}
	if f := fieldOf(t, on["alice"], alice, "summary"); f["state"] != "succeeded" {
		t.Fatalf("the extraction of a group beside the refused ones: %v\n%s", f, logs.String())
	}
	up.Store(true)
	if f := fieldOf(t, on["dave"], dave, "summary"); f["state"] != "succeeded" || f["attempts"] != 1.0 {
		t.Fatalf("once the key endpoint answered the extraction is %v\n%s", f, logs.String())
	}

	// No model was called for a group that was refused, nothing is counted
	// against the reader, and no refused task spent an attempt or a call.
	for _, kind := range []string{askedExtract, askedFigure} {
		for group := range endpoint.byGroup(gw.served(kind)) {
			if group != groupOf("alice") && group != groupOf("dave") {
				t.Errorf("a model was called for a %s of %s, which was refused a key", kind, group)
			}
		}
	}
	if extra := len(gw.served(askedPage)) - served; extra != 0 || len(gw.served(askedExtract)) != 2 {
		t.Fatalf("the gateway served %d pages more and %d extractions, want none and 2", extra, len(gw.served(askedExtract)))
	}
	if breaker := value[string](t, conn, `SELECT failures || ' ' || (opened_at IS NULL) FROM pools WHERE reader = 'gateway'`); breaker != "0 true" {
		t.Fatalf("refusals of a key counted against the reader: %q", breaker)
	}
	if scopes := value[string](t, conn, `SELECT coalesce(string_agg(DISTINCT scope, ','), '') FROM pool_scopes`); scopes != groupOf("dave") {
		t.Fatalf("the scopes that were paused are %q, want the one whose key was not answered for", scopes)
	}
	refusedRows := `SELECT count(*) FROM fields WHERE state = 'failed' AND calls = 0`
	if n := value[int](t, conn, refusedRows); n != 2 || value[int](t, conn, `SELECT coalesce(sum(calls), 0) FROM figure_runs WHERE state = 'failed'`) != 0 {
		t.Fatalf("%d refused extractions made no call, want 2, and the refused runs made some", n)
	}
	if !strings.Contains(logs.String(), "the key endpoint refused the group a key") {
		t.Errorf("the log does not say a group was refused:\n%s", logs.String())
	}
	if n := value[int](t, conn, drift); n != 0 {
		t.Fatalf("%d counters differ from a recount of the rows", n)
	}
}

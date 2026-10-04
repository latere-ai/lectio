// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Whose key reads a page, in a running durable server
// (specs/013-limits-and-usage.md): the server with LECTIO_KEYS=endpoint, a
// stub of an operator's key endpoint, and a stub of a model gateway that
// records the key of every call it is sent. A group is the subject of its
// caller, as the owner policy has it, so 2 callers are 2 groups.

// keysBearer is what the stub key endpoint requires. A case that finds it,
// or a key the stub issued, in a log, a row, an object or an answer of the
// API has found a leak.
const keysBearer = "kt-bearer-do-not-print"

// keyAsked is one request the stub key endpoint received.
type keyAsked struct {
	Authorization string `json:"-"`
	Group         string `json:"group"`
	Owner         string `json:"owner"`
	Parse         string `json:"parse"`
}

// keyPlane is a stub of an operator's key endpoint. It issues one key per
// request, good for an hour, records every request, and answers a status or
// a body the case scripted for a request instead when it scripted one.
type keyPlane struct {
	*httptest.Server

	mu       sync.Mutex
	requests []keyAsked
	// groups is the group each issued key was issued for.
	groups map[string]string
	// script answers the nth request, counted from 1, in place of a key. It
	// reports false to let the key be issued.
	script func(w http.ResponseWriter, n int, q keyAsked) bool
}

func newKeyPlane(t *testing.T) *keyPlane {
	t.Helper()
	k := &keyPlane{groups: map[string]string{}}
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := keyAsked{Authorization: r.Header.Get("Authorization")}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil || r.Method != http.MethodPost {
			t.Errorf("the key endpoint was sent a %s that does not decode: %v", r.Method, err)
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		k.requests = append(k.requests, q)
		if k.script != nil && k.script(w, len(k.requests), q) {
			return
		}
		key := "sk-issued-do-not-print-" + strconv.Itoa(len(k.groups)+1)
		k.groups[key] = q.Group
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"key": key, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}); err != nil {
			t.Errorf("writing a key: %v", err)
		}
	}))
	t.Cleanup(k.Close)
	return k
}

// answers scripts the stub.
func (k *keyPlane) answers(script func(w http.ResponseWriter, n int, q keyAsked) bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.script = script
}

// asked is a copy of the requests the stub received for a group.
func (k *keyPlane) asked(group string) []keyAsked {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []keyAsked
	for _, q := range k.requests {
		if q.Group == group {
			out = append(out, q)
		}
	}
	return out
}

// groupOf is the group a key was issued for, empty for a key the stub did
// not issue.
func (k *keyPlane) groupOf(key string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.groups[key]
}

// gateway is a stub of a model gateway that speaks chat completions. It
// records the bearer of every call and answers each with a page of one
// block, or with a rate limit while a case says the bearer is limited.
type gateway struct {
	*httptest.Server

	mu sync.Mutex
	// read are the bearers of the calls answered with a page, and refused
	// the bearers of the calls answered with a rate limit.
	read, refused []string
	// limited reports whether a call with the bearer is answered 429.
	limited func(bearer string) bool
}

func newGateway(t *testing.T) *gateway {
	t.Helper()
	g := &gateway{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("reading a call: %v", err)
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.limited != nil && g.limited(bearer) {
			g.refused = append(g.refused, bearer)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		g.read = append(g.read, bearer)
		content := `{"blocks":[{"kind":"paragraph","text":"A page read through the gateway.","description":null,"box":[100,100,900,200],"level":null}]}`
		if err := json.NewEncoder(w).Encode(map[string]any{
			"model":   "example-model",
			"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": content}}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 20},
		}); err != nil {
			t.Errorf("writing a completion: %v", err)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

// limit sets which bearers the gateway answers with a rate limit.
func (g *gateway) limit(limited func(bearer string) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limited = limited
}

// calls is a copy of the bearers of the calls answered with a page, and how
// many calls were answered with a rate limit.
func (g *gateway) calls() (read []string, refused int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.read...), len(g.refused)
}

// keyed is the environment of a durable server that reads its pages
// through the gateway, in pairs: one Reader of the chat adapter, named
// gateway, and pauses short enough that a case waits for none.
func keyed(t *testing.T, g *gateway, more ...string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reader.yaml")
	doc := "apiVersion: lectio.latere.ai/v1\nkind: Reader\nmetadata: { name: gateway }\n" +
		"spec: { adapter: chat, endpoint: " + g.URL + "/v1, model: example-model }\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return append([]string{"LECTIO_CONFIG", path, "LECTIO_KEYS", "endpoint", "LECTIO_POOL_RESUME", "100ms", "LECTIO_POOL_RECOVERY", "100ms"}, more...)
}

// reads is where the endpoint is and the bearer it requires, in pairs: the 2
// variables only a process that runs tasks is given.
func (k *keyPlane) reads() []string {
	return []string{"LECTIO_KEYS_URL", k.URL, "LECTIO_KEYS_TOKEN", keysBearer}
}

// groupOf is the group of a caller under the owner policy: its subject.
func groupOf(sub string) string { return provider().URL() + "|" + sub }

// pagesOf reads every page of a parse as its caller and returns the raw
// answers, in page order.
func pagesOf(t *testing.T, base, tok, id string, n int) []string {
	t.Helper()
	out := make([]string, n)
	for i := range out {
		status, _, raw := call(t, "GET", base+"/v1/parses/"+id+"/pages/"+strconv.Itoa(i+1), tok, nil)
		if status != http.StatusOK {
			t.Fatalf("page %d of %s: %d %s", i+1, id, status, raw)
		}
		out[i] = string(raw)
	}
	return out
}

// TestPagesOfTwoGroupsAreReadWithTwoKeys: with LECTIO_KEYS=endpoint the
// pages of 2 groups are read with 2 keys, each asked of the operator's
// endpoint once, with the bearer and the group, the owner and the parse,
// and the gateway's record of calls attributes every page to its group. The
// API and the worker run as 2 servers, as a deployment runs them: the API
// is given the source and neither the endpoint nor its bearer, and starts.
// Neither a key nor the bearer is in a log, a row, an object or an answer.
func TestPagesOfTwoGroupsAreReadWithTwoKeys(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	endpoint, gw := newKeyPlane(t), newGateway(t)
	settings := keyed(t, gw)
	base, apiLogs, _ := started(t, env(p.env("api", settings...)...))
	probes, workerLogs, _ := started(t, env(p.env("worker", append(settings, endpoint.reads()...)...)...))

	alice, bob := tokenOf("alice"), tokenOf("bob")
	// 3 pages for one group and 2 for the other, so a call counted for the
	// wrong group shows.
	first := submittedAs(t, base, alice, "scan.tiff", frames(3))
	second := submittedAs(t, base, bob, "scan.tiff", frames(2))
	for id, tok := range map[string]string{first: alice, second: bob} {
		if done := endedAs(t, base, tok, id); done["state"] != "succeeded" {
			t.Fatalf("a parse read with its group's key ended %v\n%s", done, workerLogs.String())
		}
	}

	answers := strings.Join(append(pagesOf(t, base, alice, first, 3), pagesOf(t, base, bob, second, 2)...), "\n")
	if strings.Count(answers, `"reader":"gateway"`) != 5 || strings.Count(answers, `"attempts":1`) != 5 {
		t.Fatalf("the pages were not each read once through the gateway:\n%s", answers)
	}
	for group, parse := range map[string]string{groupOf("alice"): first, groupOf("bob"): second} {
		want := keyAsked{Authorization: "Bearer " + keysBearer, Group: group, Owner: group, Parse: parse}
		if asked := endpoint.asked(group); len(asked) != 1 || asked[0] != want {
			t.Fatalf("the key endpoint was asked %+v for %s, want once: %+v", asked, group, want)
		}
	}
	read, refused := gw.calls()
	byGroup := map[string]int{}
	for _, bearer := range read {
		byGroup[endpoint.groupOf(bearer)]++
	}
	if len(read) != 5 || refused != 0 || byGroup[groupOf("alice")] != 3 || byGroup[groupOf("bob")] != 2 {
		t.Fatalf("the gateway attributes its calls as %v, want 3 pages to alice's group and 2 to bob's", byGroup)
	}

	// The store is opened with a key scope per group by both roles, the
	// worker says which source is in force, and the API builds none.
	conn := p.connect()
	if !value[bool](t, conn, `SELECT scope_by_group FROM settings`) {
		t.Fatal("the task store holds one key scope for every group")
	}
	if !strings.Contains(workerLogs.String(), `"msg":"keys","source":"endpoint"`) || strings.Contains(apiLogs.String(), `"msg":"keys"`) {
		t.Fatalf("the worker's log does not name the key source, or the API's does:\n%s", workerLogs.String())
	}
	if status, _ := get(t, probes+"/readyz"); status != http.StatusOK {
		t.Fatalf("the worker is not ready: %d", status)
	}

	// No key and no bearer is anywhere but in memory.
	sinks := map[string]string{"the API's log": apiLogs.String(), "the worker's log": workerLogs.String(), "the pages the API answers": answers}
	for _, key := range p.objects.Keys() {
		data, _ := p.objects.Get(key)
		sinks["the object "+key] = string(data)
	}
	rows, err := conn.Query(context.Background(), `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil || len(tables) < 10 {
		t.Fatalf("the schema lists %d tables, %v", len(tables), err)
	}
	for _, table := range tables {
		sinks["the table "+table] = value[string](t, conn, `SELECT coalesce(string_agg(t::text, E'\n'), '') FROM "`+table+`" t`)
	}
	for where, text := range sinks {
		if strings.Contains(text, "sk-issued") || strings.Contains(text, keysBearer) {
			t.Errorf("%s holds a key or the endpoint's bearer", where)
		}
	}
}

// TestAKeyEndpointThatIsDownLeavesPagesUnclaimedAndNotFailed: while the
// endpoint fails, and while it refuses the bearer, a group's pages are
// neither failed nor charged an attempt. They wait in the queue behind the
// group's paused key scope, no reader is called, and the worker stays
// ready. Once the endpoint answers the pages are read, each on its first
// attempt.
func TestAKeyEndpointThatIsDownLeavesPagesUnclaimedAndNotFailed(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	endpoint, gw := newKeyPlane(t), newGateway(t)
	// The endpoint is down, then refuses the bearer, then is down again.
	endpoint.answers(func(w http.ResponseWriter, n int, _ keyAsked) bool {
		status := http.StatusServiceUnavailable
		if n == 2 {
			status = http.StatusUnauthorized
		}
		w.WriteHeader(status)
		return true
	})
	base, logs, _ := started(t, env(p.env("all", keyed(t, gw, endpoint.reads()...)...)...))
	internal := internalOf(t, logs)
	until(t, "the server is ready", time.Minute, func() bool {
		status, _ := get(t, internal+"/readyz")
		return status == http.StatusOK
	})

	alice, group := tokenOf("alice"), groupOf("alice")
	id := submittedAs(t, base, alice, "scan.tiff", frames(3))
	conn := p.connect()
	waiting := `SELECT count(*) FROM tasks WHERE parse_id = $1 AND kind = 'page' AND state = 'queued' AND attempt = 0 AND expiries = 0 AND error IS NULL`
	paused := `SELECT count(*) FROM pool_scopes WHERE reader = 'gateway' AND scope = $1 AND paused_until > now()`
	// The endpoint was asked 3 times, so the pages went back to the queue
	// and were claimed again at least twice, and they are in the queue now,
	// behind the group's pause, with nothing counted against them.
	until(t, "the pages wait unclaimed behind the group's paused scope", 2*time.Minute, func() bool {
		return len(endpoint.asked(group)) >= 3 && value[int](t, conn, waiting, id) == 3 && value[int](t, conn, paused, group) == 1
	})
	if state := value[string](t, conn, `SELECT state || ' ' || pages_failed || ' ' || calls FROM parses WHERE parse_id = $1`, id); state != "running 0 0" {
		t.Fatalf("with the key endpoint down the parse is %q", state)
	}
	if read, _ := gw.calls(); len(read) != 0 {
		t.Fatalf("a reader was called %d times with no key", len(read))
	}
	if scopes := value[string](t, conn, `SELECT string_agg(scope, ',') FROM pool_scopes`); scopes != group {
		t.Fatalf("the paused scopes are %q, want the group's alone", scopes)
	}
	if failures := value[string](t, conn, `SELECT failures || ' ' || (opened_at IS NULL) FROM pools WHERE reader = 'gateway'`); failures != "0 true" {
		t.Fatalf("a key endpoint that is down counted against the reader: %q", failures)
	}
	// A worker whose key endpoint is down is ready: the pages wait, and the
	// worker runs every other task.
	if status, body := get(t, internal+"/readyz"); status != http.StatusOK {
		t.Fatalf("with the key endpoint down the worker is not ready: %d %s", status, body)
	}

	endpoint.answers(nil)
	if done := endedAs(t, base, alice, id); done["state"] != "succeeded" || done["progress"].(map[string]any)["pages_done"] != 3.0 {
		t.Fatalf("once the key endpoint answered the parse ended %v\n%s", done, logs.String())
	}
	if answers := strings.Join(pagesOf(t, base, alice, id, 3), "\n"); strings.Count(answers, `"attempts":1`) != 3 {
		t.Fatalf("a page that waited for its key was charged an attempt:\n%s", answers)
	}
	read, _ := gw.calls()
	for _, bearer := range read {
		if endpoint.groupOf(bearer) != group {
			t.Fatalf("a page was read with a key that is not its group's")
		}
	}
	if len(read) != 3 || value[int](t, conn, `SELECT calls FROM parses WHERE parse_id = $1`, id) != 3 {
		t.Fatalf("the 3 pages cost %d calls at the gateway", len(read))
	}
	for _, want := range []string{"the key endpoint issued no key", "the key endpoint answered 503", "the key endpoint answered 401"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not say %q", want)
		}
	}
	if strings.Contains(logs.String(), keysBearer) || strings.Contains(logs.String(), "sk-issued") {
		t.Fatal("the log holds a key or the endpoint's bearer")
	}
	if n := value[int](t, conn, drift); n != 0 {
		t.Fatalf("%d counters differ from a recount of the rows", n)
	}
}

// TestAGroupTheKeyEndpointRefusesFailsItsPagesAtOnce: a 402 fails the pages
// of its group with budget_exhausted and a 403 with reader_unavailable, each
// on one request for the whole parse and with no call to a reader. A
// refusal is its group's alone: it pauses no scope, counts against no
// reader, and another group reads beside it.
func TestAGroupTheKeyEndpointRefusesFailsItsPagesAtOnce(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	endpoint, gw := newKeyPlane(t), newGateway(t)
	refusals := map[string]int{groupOf("alice"): http.StatusPaymentRequired, groupOf("bob"): http.StatusForbidden}
	endpoint.answers(func(w http.ResponseWriter, _ int, q keyAsked) bool {
		status, refused := refusals[q.Group]
		if refused {
			w.WriteHeader(status)
		}
		return refused
	})
	base, logs, _ := started(t, env(p.env("all", keyed(t, gw, endpoint.reads()...)...)...))

	for sub, want := range map[string]string{"alice": "budget_exhausted", "bob": "reader_unavailable"} {
		tok := tokenOf(sub)
		id := submittedAs(t, base, tok, "scan.tiff", frames(3))
		done := endedAs(t, base, tok, id)
		if progress := done["progress"].(map[string]any); done["state"] != "failed" || progress["pages_failed"] != 3.0 || progress["pages_done"] != 0.0 {
			t.Fatalf("the parse of a group the endpoint refuses ended %v", done)
		}
		answers := strings.Join(pagesOf(t, base, tok, id, 3), "\n")
		if strings.Count(answers, `"code":"`+want+`"`) != 3 || strings.Count(answers, `"state":"failed"`) != 3 {
			t.Fatalf("the pages of %s did not fail with %s:\n%s", sub, want, answers)
		}
		if asked := endpoint.asked(groupOf(sub)); len(asked) != 1 {
			t.Fatalf("3 pages of a refused group cost %d requests, want 1", len(asked))
		}
	}
	carol := tokenOf("carol")
	if done := endedAs(t, base, carol, submittedAs(t, base, carol, "scan.tiff", frames(2))); done["state"] != "succeeded" {
		t.Fatalf("a group beside the refused ones ended %v\n%s", done, logs.String())
	}

	read, _ := gw.calls()
	for _, bearer := range read {
		if endpoint.groupOf(bearer) != groupOf("carol") {
			t.Fatal("a reader was called for a group that was refused a key")
		}
	}
	conn := p.connect()
	state := value[string](t, conn, `SELECT (SELECT count(*) FROM pool_scopes) || ' ' || failures || ' ' || (opened_at IS NULL) FROM pools WHERE reader = 'gateway'`)
	if len(read) != 2 || state != "0 0 true" {
		t.Fatalf("after 2 refusals the gateway saw %d calls and the pool is %q", len(read), state)
	}
	if n := value[int](t, conn, `SELECT count(*) FROM tasks WHERE kind = 'page' AND (attempt <> 0 OR calls <> 0)`); n != 0 {
		t.Fatalf("%d refused pages were charged an attempt or a call", n)
	}
	if !strings.Contains(logs.String(), "the key endpoint refused the group a key") {
		t.Errorf("the log does not say the group was refused:\n%s", logs.String())
	}
}

// TestARateLimitOnOneGroupsKeyPausesThatGroupAlone: with a key per group, a
// rate-limit reply to a call made with one group's key pauses that group's
// calls to the reader and no other group's. The group beside it is read
// while the pause holds, the limited pages spend no attempt, and they are
// read when the limit lifts.
func TestARateLimitOnOneGroupsKeyPausesThatGroupAlone(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	endpoint, gw := newKeyPlane(t), newGateway(t)
	alice, bob := tokenOf("alice"), tokenOf("bob")
	gw.limit(func(bearer string) bool { return endpoint.groupOf(bearer) == groupOf("alice") })
	base, logs, _ := started(t, env(p.env("all", keyed(t, gw, endpoint.reads()...)...)...))

	limited := submittedAs(t, base, alice, "scan.tiff", frames(3))
	conn := p.connect()
	scopes := `SELECT coalesce(string_agg(reader || ' ' || scope, ',' ORDER BY scope), '') FROM pool_scopes`
	until(t, "the limited group's scope is paused", 2*time.Minute, func() bool {
		return value[string](t, conn, scopes) == "gateway "+groupOf("alice")
	})

	// The other group is read while the first is limited.
	if done := endedAs(t, base, bob, submittedAs(t, base, bob, "scan.tiff", frames(3))); done["state"] != "succeeded" {
		t.Fatalf("the group beside the limited one ended %v\n%s", done, logs.String())
	}
	if got := value[string](t, conn, scopes); got != "gateway "+groupOf("alice") {
		t.Fatalf("the scopes a rate limit paused are %q, want the limited group's alone", got)
	}
	state := value[string](t, conn, `SELECT p.state || ' ' || p.pages_done || ' ' || (SELECT coalesce(sum(attempt), 0) FROM tasks t WHERE t.parse_id = p.parse_id) FROM parses p WHERE p.parse_id = $1`, limited)
	if state != "running 0 0" {
		t.Fatalf("while its key is limited the group's parse is %q", state)
	}
	read, refused := gw.calls()
	for _, bearer := range read {
		if endpoint.groupOf(bearer) != groupOf("bob") {
			t.Fatal("a call of the limited group was answered with a page")
		}
	}
	if len(read) != 3 || refused == 0 {
		t.Fatalf("the gateway read %d pages and limited %d calls", len(read), refused)
	}

	gw.limit(nil)
	if done := endedAs(t, base, alice, limited); done["state"] != "succeeded" {
		t.Fatalf("once the limit lifted the parse ended %v\n%s", done, logs.String())
	}
	if answers := strings.Join(pagesOf(t, base, alice, limited, 3), "\n"); strings.Count(answers, `"attempts":1`) != 3 {
		t.Fatalf("a page that waited out a rate limit was charged an attempt:\n%s", answers)
	}
}

// TestAKeySourceThatCannotRunStopsAProcessThatRunsTasks: the endpoint with
// no address, with no bearer, or beside a model key stops a worker and a
// process in both roles at start, with an error that names the variables
// and no value. Nothing is opened before it.
func TestAKeySourceThatCannotRunStopsAProcessThatRunsTasks(t *testing.T) {
	address, bearer := []string{"LECTIO_KEYS_URL", "https://sk-secret.example/keys"}, []string{"LECTIO_KEYS_TOKEN", "sk-secret-bearer"}
	for _, role := range []string{"worker", "all"} {
		common := []string{"LECTIO_ROLE", role, "LECTIO_DATABASE_URL", "postgres://db.example/lectio", "LECTIO_KEYS", "endpoint"}
		for name, tc := range map[string]struct {
			pairs []string
			want  []string
		}{
			"no address":           {bearer, []string{"LECTIO_KEYS_URL is not set", "LECTIO_KEYS is endpoint"}},
			"no bearer":            {address, []string{"LECTIO_KEYS_TOKEN is not set", "LECTIO_KEYS is endpoint"}},
			"a model key beside":   {append(append([]string{"LECTIO_MODEL_KEY", "sk-secret-model"}, address...), bearer...), []string{"LECTIO_MODEL_KEY is set", "LECTIO_KEYS is endpoint"}},
			"an address of no use": {append([]string{"LECTIO_KEYS_URL", "sk-secret-url"}, bearer...), []string{"LECTIO_KEYS_URL is not an absolute http or https URL"}},
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := serve(ctx, nil, env(append(common, tc.pairs...)...), io.Discard, io.Discard, nil)
			cancel()
			if err == nil || strings.Contains(err.Error(), "sk-secret") {
				t.Errorf("%s, role %s: %v", name, role, err)
				continue
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s, role %s: the error does not say %q: %v", name, role, want, err)
				}
			}
		}
	}
	// The source with nothing configured for it is the API's to start with:
	// what stops it here is the next thing it lacks, and not the endpoint.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := serve(ctx, nil, env("LECTIO_ROLE", "api", "LECTIO_DATABASE_URL", "postgres://db.example/lectio", "LECTIO_KEYS", "endpoint"), io.Discard, io.Discard, nil)
	if err == nil || strings.Contains(err.Error(), "LECTIO_KEYS") || !strings.Contains(err.Error(), "LECTIO_OIDC_ISSUERS is not set") {
		t.Errorf("the API with the endpoint as its source and neither of its 2 variables: %v", err)
	}
}

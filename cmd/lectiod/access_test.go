// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/testfixtures"
)

// Who is calling and who decides, in a running server
// (specs/012-identity-and-authorization.md, specs/016-distribution.md): the
// durable server with a stub issuer and a stub authorizer, and each mode
// the settings select.

// reason is the developer detail of an error answer.
func reason(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	s, _ := d["reason"].(string)
	return s
}

// code is the code of an error answer.
func code(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

// TestTheDurableServerAsksItsAuthorizer: a durable server with an issuer
// and an authorizer verifies its callers' tokens and asks the endpoint
// about every request. A create is recorded under the owner its allow
// names, the parse joins the group and takes the bounds the allow carries,
// a list is narrowed by the allow's filter, a deny is forbidden with the
// endpoint's reason, an outage fails closed, and what the allow's retention
// has ended for is removed by the sweep: no row and no object is left of
// the parse or of the file.
func TestTheDurableServerAsksItsAuthorizer(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	az := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()), stub.WithAllow())
	alice := provider().URL() + "|alice"
	const acme = "org:acme"
	az.Allow(stub.Rule{Subject: alice, Action: authorizer.ActionFileCreate, Limits: map[string]any{
		"owner": acme, "group": "acme", "max_file_bytes": 1 << 20, "retention_seconds": 2,
	}})
	az.Allow(stub.Rule{Subject: alice, Action: authorizer.ActionParseCreate, Limits: map[string]any{
		"owner": acme, "group": "acme", "weight": 2, "max_running": 4, "max_queued": 10, "max_priority": 1,
		"classes": []string{"interactive"}, "max_file_bytes": 1 << 20, "max_pages": 50, "pages_per_day": 1000,
		"retention_seconds": 2,
	}})
	for _, action := range []string{authorizer.ActionParseRead, authorizer.ActionFileRead, authorizer.ActionReaderList} {
		az.Allow(stub.Rule{Subject: alice, Action: action})
	}
	az.Allow(stub.Rule{Subject: alice, Action: authorizer.ActionParseList, Filter: &authz.Filter{Owners: []string{acme}}})

	base, logs, _ := started(t, env(p.env("all", "LECTIO_AUTHORIZER_URL", az.URL(), "LECTIO_AUTHORIZER_TOKEN", az.Token())...))
	// At start the server sent the probe, and the endpoint denied it.
	if seen := az.Requests(); len(seen) != 1 || seen[0].Resource.ID != authz.ProbeID || seen[0].Subject != "" {
		t.Fatalf("at start the endpoint saw %+v", seen)
	}
	for _, want := range []string{`"callers":"oidc"`, `"decisions":"authorizer"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not say %s", want)
		}
	}
	az.ClearRequests()

	// A caller the endpoint denies is forbidden, with the endpoint's reason
	// as the developer detail. Another caller's parse is not found.
	bob := tokenOf("bob")
	status, body, raw := call(t, "POST", base+"/v1/files?name=scan.png", bob, testfixtures.Read(t, testfixtures.PNG), "Content-Type", "image/png")
	if status != http.StatusForbidden || code(body) != "forbidden" || !strings.Contains(reason(body), "no rule allows") {
		t.Fatalf("an upload the endpoint denies: %d %s", status, raw)
	}
	if status, body, raw := call(t, "GET", base+"/v1/parses", bob, nil); status != http.StatusForbidden || code(body) != "forbidden" {
		t.Fatalf("a list the endpoint denies: %d %s", status, raw)
	}

	// Alice's upload and submit are recorded under the owner the allows
	// name, and no question about a create named an owner.
	mine := tokenOf("alice")
	status, file, raw := call(t, "POST", base+"/v1/files?name=report.pdf", mine, testfixtures.Read(t, testfixtures.MultipagePDF), "Content-Type", "application/pdf")
	if status != http.StatusCreated || file["expires_at"] == nil {
		t.Fatalf("upload: %d %s", status, raw)
	}
	status, parse, raw := finished(t, base, mine)(call(t, "POST", base+"/v1/parses", mine, []byte(`{"source":{"file":"`+file["id"].(string)+`"},"priority":1}`), "Prefer", "wait=30"))
	if status != http.StatusOK || parse["state"] != "succeeded" || parse["owner"] != acme {
		t.Fatalf("submit: %d %s", status, raw)
	}
	id := parse["id"].(string)
	conn := p.connect()
	if got := value[string](t, conn, `SELECT owner || ' ' || group_id || ' ' || max_pages FROM parses WHERE parse_id = $1`, id); got != acme+" acme 50" {
		t.Errorf("the parse's row is %q", got)
	}
	if got := value[string](t, conn, `SELECT weight || ' ' || max_running || ' ' || max_queued || ' ' || max_priority || ' ' || pages_per_day FROM groups WHERE group_id = 'acme'`); got != "2 4 10 1 1000" {
		t.Errorf("the group's row is %q", got)
	}
	if got := value[int](t, conn, `SELECT reserved FROM group_days WHERE group_id = 'acme'`); got != 3 {
		t.Errorf("the group's day holds %d pages, want the 3 the parse read", got)
	}
	if got := value[string](t, conn, `SELECT owner FROM files WHERE file_id = $1`, file["id"]); got != acme {
		t.Errorf("the file's owner is %q", got)
	}
	var creates int
	for _, q := range az.Requests() {
		if q.Subject != alice {
			continue
		}
		if q.Action == authorizer.ActionFileCreate || q.Action == authorizer.ActionParseCreate {
			creates++
			if q.Resource.ID != "" || q.Resource.String(authorizer.FieldOwner) != "" || q.Claims["sub"] != "alice" {
				t.Errorf("a create was asked as %+v", q)
			}
		}
	}
	if creates != 2 {
		t.Errorf("the endpoint saw %d creates of alice's, want 2", creates)
	}

	// The limits of the allow hold: a class it does not name is forbidden,
	// and a priority past its bound is not valid.
	submit := func(options string) (int, map[string]any, []byte) {
		return call(t, "POST", base+"/v1/parses", mine, []byte(`{"source":{"file":"`+file["id"].(string)+`"}`+options+`}`))
	}
	if status, body, raw := submit(`,"class":"batch"`); status != http.StatusForbidden || code(body) != "forbidden" {
		t.Errorf("a class the allow does not name: %d %s", status, raw)
	}
	if status, body, raw := submit(`,"priority":2`); status != http.StatusBadRequest || code(body) != "invalid_request" {
		t.Errorf("a priority past the allow's bound: %d %s", status, raw)
	}

	// Alice reads the parse, which is the organization's, and her list is
	// narrowed to the owners the allow's filter names.
	if status, got, raw := call(t, "GET", base+"/v1/parses/"+id, mine, nil); status != http.StatusOK || got["owner"] != acme {
		t.Fatalf("reading the parse: %d %s", status, raw)
	}
	if status, _, raw := call(t, "GET", base+"/v1/parses/"+id, bob, nil); status != http.StatusNotFound {
		t.Fatalf("another caller reading the parse: %d %s", status, raw)
	}
	_, listed, raw := call(t, "GET", base+"/v1/parses", mine, nil)
	if all, _ := listed["parses"].([]any); len(all) != 1 {
		t.Fatalf("the list: %s", raw)
	}
	// A delete is an action the endpoint did not allow: the parse is not
	// found, and it is still there.
	if status, _, raw := call(t, "DELETE", base+"/v1/parses/"+id, mine, nil); status != http.StatusNotFound {
		t.Fatalf("a delete the endpoint denies: %d %s", status, raw)
	}

	// The endpoint goes away. A create is asked every time, so it fails
	// closed, and the answer names neither the endpoint nor its bearer.
	az.Fail(http.StatusServiceUnavailable)
	status, body, raw = submit("")
	if status != http.StatusServiceUnavailable || code(body) != "authorizer_unavailable" || strings.Contains(string(raw), az.URL()) || strings.Contains(string(raw), az.Token()) {
		t.Fatalf("a submit with the endpoint down: %d %s", status, raw)
	}
	az.Resume()
	if strings.Contains(logs.String(), az.Token()) {
		t.Fatal("the log holds the authorizer's bearer")
	}

	// 2 seconds after the parse ended its retention is over, and the
	// file's 2 seconds after that: the sweep removes each, objects first.
	until(t, "the parse and the file expire and leave nothing", 30*time.Second, func() bool {
		if n := value[int](t, conn, `SELECT (SELECT count(*) FROM parses WHERE parse_id = $1) + (SELECT count(*) FROM files WHERE file_id = $2)`, id, file["id"]); n != 0 {
			return false
		}
		for _, key := range p.objects.Keys() {
			if strings.HasPrefix(key, "parses/"+id+"/") || strings.HasSuffix(key, "/"+file["id"].(string)) {
				return false
			}
		}
		return true
	})
	if status, _, raw := call(t, "GET", base+"/v1/parses/"+id, mine, nil); status != http.StatusNotFound {
		t.Fatalf("a parse past its retention: %d %s", status, raw)
	}
	if status, _, raw := call(t, "GET", base+"/v1/files/"+file["id"].(string), mine, nil); status != http.StatusNotFound {
		t.Fatalf("a file past its retention: %d %s", status, raw)
	}
	if !strings.Contains(logs.String(), "the retention sweep removed what expired") {
		t.Error("the sweep did not say what it removed")
	}
}

// TestTheDurableServerUnderTheOwnerPolicy: with issuers and no authorizer
// the durable server decides itself. A subject reads and changes its own,
// another subject finds nothing, and a subject of LECTIO_ADMIN_SUBJECTS
// reads what another owns and changes none of it. A worker verifies no
// token and starts with no issuer.
func TestTheDurableServerUnderTheOwnerPolicy(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	admin := provider().URL() + "|root"
	base, logs, _ := started(t, env(p.env("api", "LECTIO_ADMIN_SUBJECTS", admin, "LECTIO_GROUP_DEFAULTS", "max_priority=3")...))
	if _, workerLogs, stop := started(t, env(p.env("worker", "LECTIO_OIDC_ISSUERS", "")...)); stop == nil || strings.Contains(workerLogs.String(), `"msg":"identity"`) {
		t.Fatal("a worker built an identity")
	}
	if !strings.Contains(logs.String(), `"decisions":"owner policy"`) {
		t.Errorf("the log does not say who decides:\n%s", logs.String())
	}
	alice, bob, root := tokenOf("alice"), tokenOf("bob"), tokenOf("root")
	status, file, raw := call(t, "POST", base+"/v1/files?name=sample.csv", alice, testfixtures.Read(t, testfixtures.CSV), "Content-Type", "text/csv")
	if status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, raw)
	}
	status, parse, raw := finished(t, base, alice)(call(t, "POST", base+"/v1/parses", alice, []byte(`{"source":{"file":"`+file["id"].(string)+`"},"priority":3}`), "Prefer", "wait=30"))
	if status != http.StatusOK || parse["owner"] != provider().URL()+"|alice" || parse["state"] != "succeeded" {
		t.Fatalf("submit: %d %s", status, raw)
	}
	at := base + "/v1/parses/" + parse["id"].(string)
	// The server's own bound on a priority holds where no allow names one.
	if status, body, raw := call(t, "POST", base+"/v1/parses", alice, []byte(`{"source":{"file":"`+file["id"].(string)+`"},"priority":4}`)); status != http.StatusBadRequest || code(body) != "invalid_request" {
		t.Errorf("a priority past LECTIO_GROUP_DEFAULTS: %d %s", status, raw)
	}

	for name, tc := range map[string]struct {
		method, url, token string
		want               int
	}{
		"the owner reads":             {"GET", at, alice, http.StatusOK},
		"another subject reads":       {"GET", at, bob, http.StatusNotFound},
		"another subject deletes":     {"DELETE", at, bob, http.StatusNotFound},
		"another subject's file":      {"GET", base + "/v1/files/" + file["id"].(string), bob, http.StatusNotFound},
		"an admin reads":              {"GET", at, root, http.StatusOK},
		"an admin reads the document": {"GET", at + "/document", root, http.StatusOK},
		"an admin reads the file":     {"GET", base + "/v1/files/" + file["id"].(string), root, http.StatusOK},
		"an admin deletes":            {"DELETE", at, root, http.StatusNotFound},
		"an admin deletes the file":   {"DELETE", base + "/v1/files/" + file["id"].(string), root, http.StatusNotFound},
	} {
		if status, _, raw := call(t, tc.method, tc.url, tc.token, nil); status != tc.want {
			t.Errorf("%s: %d %s, want %d", name, status, raw, tc.want)
		}
	}
	for who, want := range map[string]int{alice: 1, bob: 0, root: 1} {
		_, listed, raw := call(t, "GET", base+"/v1/parses", who, nil)
		if all, _ := listed["parses"].([]any); len(all) != want {
			t.Errorf("a list of %d parses, want %d: %s", len(all), want, raw)
		}
	}
	// A token for another audience, and no token, are refused.
	other := provider().Mint(issuerClaims("alice", "another-service"))
	if status, body, raw := call(t, "GET", at, other, nil); status != http.StatusUnauthorized || code(body) != "invalid_token" || reason(body) == "" || strings.Contains(string(raw), other) {
		t.Errorf("a token for another audience: %d %s", status, raw)
	}
	if status, body, _ := call(t, "GET", at, "", nil); status != http.StatusUnauthorized || code(body) != "missing_token" {
		t.Errorf("no token: %d %v", status, body)
	}
	// The owner deletes its own.
	if status, _, raw := call(t, "DELETE", at, alice, nil); status != http.StatusNoContent {
		t.Fatalf("the owner's delete: %d %s", status, raw)
	}
}

// answering is an endpoint that answers every question with one body.
func answering(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTheModesOfADevelopmentServer: LECTIO_DEV keeps its one static token
// and the owner policy. With issuers it verifies tokens and takes no static
// token; with an authorizer's URL it asks the endpoint. An endpoint that
// allows the probe does not read what it is asked, and the server is
// refused; one that does not answer is named, the server starts, and every
// request fails closed. An issuer that does not answer at start is named
// and the server starts. A limit a development server cannot hold is
// refused and not passed.
func TestTheModesOfADevelopmentServer(t *testing.T) {
	scan := testfixtures.Read(t, testfixtures.PNG)
	upload := func(base, token string) (int, map[string]any, []byte) {
		return call(t, "POST", base+"/v1/files?name=scan.png", token, scan, "Content-Type", "image/png")
	}

	t.Run("the static token and the owner policy", func(t *testing.T) {
		base, logs, _ := started(t, env("LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0", "LECTIO_DEV_TOKEN", "t0"))
		for _, want := range []string{`"callers":"development token"`, `"decisions":"owner policy"`, "LECTIO_DEV_TOKEN"} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("the log does not say %s", want)
			}
		}
		if strings.Contains(logs.String(), `"t0"`) {
			t.Error("the log holds the development token")
		}
		status, file, raw := upload(base, "t0")
		if status != http.StatusCreated {
			t.Fatalf("upload: %d %s", status, raw)
		}
		status, parse, raw := call(t, "POST", base+"/v1/parses", "t0", []byte(`{"source":{"file":"`+file["id"].(string)+`"}}`), "Prefer", "wait=20")
		if status != http.StatusOK || parse["owner"] != "dev" {
			t.Fatalf("submit: %d %s", status, raw)
		}
		if status, body, _ := upload(base, "dev"); status != http.StatusUnauthorized || code(body) != "invalid_token" {
			t.Errorf("a token that is not the server's: %d %v", status, body)
		}
		// No default of a group names a bound on priority, so 0 is the one
		// a submit may name.
		if status, body, raw := call(t, "POST", base+"/v1/parses", "t0", []byte(`{"source":{"file":"`+file["id"].(string)+`"},"priority":1}`)); status != http.StatusBadRequest || code(body) != "invalid_request" {
			t.Errorf("a priority with no bound configured: %d %s", status, raw)
		}
	})

	t.Run("issuers take the place of the static token", func(t *testing.T) {
		base, logs, _ := started(t, env("LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0", "LECTIO_OIDC_ISSUERS", provider().URL()))
		if !strings.Contains(logs.String(), `"callers":"oidc"`) || strings.Contains(logs.String(), "LECTIO_DEV_TOKEN") {
			t.Errorf("the log does not say the callers are verified:\n%s", logs.String())
		}
		if status, body, _ := upload(base, "dev"); status != http.StatusUnauthorized || code(body) != "invalid_token" {
			t.Errorf("the static token on a server with issuers: %d %v", status, body)
		}
		if status, _, raw := upload(base, tokenOf("alice")); status != http.StatusCreated {
			t.Errorf("a token of the issuer: %d %s", status, raw)
		}
	})

	t.Run("an issuer that does not answer at start is named", func(t *testing.T) {
		// Nothing listens on port 1: the connection is refused at once.
		base, logs, _ := started(t, env("LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0", "LECTIO_OIDC_ISSUERS", "http://127.0.0.1:1"))
		if !strings.Contains(logs.String(), "an issuer's keys could not be read at start") {
			t.Errorf("the log does not name the issuer:\n%s", logs.String())
		}
		if status, body, _ := upload(base, tokenOf("alice")); status != http.StatusUnauthorized || code(body) != "invalid_token" {
			t.Errorf("a token nobody can verify: %d %v", status, body)
		}
	})

	t.Run("an authorizer decides, and a limit the server cannot hold is refused", func(t *testing.T) {
		az := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		base, logs, _ := started(t, env("LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0",
			"LECTIO_AUTHORIZER_URL", az.URL(), "LECTIO_AUTHORIZER_TOKEN", az.Token()))
		if !strings.Contains(logs.String(), `"decisions":"authorizer"`) {
			t.Errorf("the log does not say who decides:\n%s", logs.String())
		}
		status, file, raw := upload(base, "dev")
		if status != http.StatusCreated {
			t.Fatalf("upload: %d %s", status, raw)
		}
		submit := []byte(`{"source":{"file":"` + file["id"].(string) + `"}}`)
		az.Allow(stub.Rule{Action: authorizer.ActionParseCreate, Limits: map[string]any{"max_pages": 10, "max_priority": 2}})
		if status, _, raw := call(t, "POST", base+"/v1/parses", "dev", submit, "Prefer", "wait=20"); status != http.StatusOK {
			t.Fatalf("a submit under limits the server holds: %d %s", status, raw)
		}
		az.Allow(stub.Rule{Action: authorizer.ActionParseCreate, Limits: map[string]any{"group": "acme", "max_queued": 5, "pages_per_day": 100}})
		status, body, raw := call(t, "POST", base+"/v1/parses", "dev", submit)
		if status != http.StatusUnprocessableEntity || code(body) != "capability_unsupported" || !strings.Contains(reason(body), "max_queued, pages_per_day") {
			t.Fatalf("a submit under limits the server cannot hold: %d %s", status, raw)
		}
		az.Deny(stub.Rule{Action: authorizer.ActionReaderList}, "plan_expired")
		status, body, raw = call(t, "GET", base+"/v1/readers", "dev", nil)
		if status != http.StatusForbidden || !strings.Contains(reason(body), "plan_expired") || strings.Contains(body["error"].(map[string]any)["message"].(string), "plan_expired") {
			t.Fatalf("a deny: %d %s", status, raw)
		}
	})

	t.Run("an endpoint that allows the probe is refused", func(t *testing.T) {
		loose := answering(t, http.StatusOK, `{"allow": true}`)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := serve(ctx, nil, env("LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0",
			"LECTIO_AUTHORIZER_URL", loose.URL, "LECTIO_AUTHORIZER_TOKEN", "sk-authorizer"), io.Discard, io.Discard, nil)
		if err == nil || !strings.Contains(err.Error(), "LECTIO_AUTHORIZER_URL") || !strings.Contains(err.Error(), "probe") || strings.Contains(err.Error(), "sk-authorizer") {
			t.Fatalf("a server whose authorizer allows the probe: %v", err)
		}
	})

	t.Run("an endpoint that does not answer is named and nothing is allowed", func(t *testing.T) {
		down := answering(t, http.StatusInternalServerError, `{}`)
		base, logs, _ := started(t, env("LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0",
			"LECTIO_AUTHORIZER_URL", down.URL, "LECTIO_AUTHORIZER_TOKEN", "sk-authorizer"))
		if !strings.Contains(logs.String(), "the authorizer did not answer the probe at start") || strings.Contains(logs.String(), "sk-authorizer") {
			t.Errorf("the log does not name the authorizer, or holds its bearer:\n%s", logs.String())
		}
		status, body, raw := upload(base, "dev")
		if status != http.StatusServiceUnavailable || code(body) != "authorizer_unavailable" || strings.Contains(string(raw), down.URL) {
			t.Fatalf("an upload with the authorizer down: %d %s", status, raw)
		}
		if !strings.Contains(logs.String(), "the authorizer gave no decision") {
			t.Errorf("the log does not say why the request failed:\n%s", logs.String())
		}
	})
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/testfixtures"
)

// TestAReaderStoredThroughTheAPIIsInForceInEveryProcess: an admin stores a
// reader with the API of one process, and a worker in another reads with it
// without a restart, by the pin of a parse and by the routing policy the
// admin sets. A reader the routing policy names is not removed; once the
// configured policy is back, it is, and the set of every process follows.
// A caller who is no admin changes nothing, and a configured reader is
// nobody's to change.
func TestAReaderStoredThroughTheAPIIsInForceInEveryProcess(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	admin := provider().URL() + "|root"
	poll := "LECTIO_READERS_POLL"
	base, _, _ := started(t, env(p.env("api", "LECTIO_ADMIN_SUBJECTS", admin, poll, "100ms")...))
	_, workerLogs, _ := started(t, env(p.env("worker", "LECTIO_OIDC_ISSUERS", "", poll, "100ms")...))
	root, bob := tokenOf("root"), tokenOf("bob")

	if status, _, raw := call(t, "PUT", base+"/v1/readers/own-text", bob, []byte(`{"adapter":"text"}`)); status != http.StatusForbidden {
		t.Fatalf("a caller who is no admin stored a reader: %d %s", status, raw)
	}
	if status, body, raw := call(t, "PUT", base+"/v1/readers/stub", root, []byte(`{"adapter":"text"}`)); status != http.StatusConflict || code(body) != "conflict" {
		t.Fatalf("the configured reader was changed: %d %s", status, raw)
	}
	if status, body, raw := call(t, "PUT", base+"/v1/readers/own-text", root, []byte(`{"adapter":"stub"}`)); status != http.StatusBadRequest || code(body) != "invalid_request" {
		t.Fatalf("a stub reader was stored: %d %s", status, raw)
	}
	status, view, raw := call(t, "PUT", base+"/v1/readers/own-text", root, []byte(`{"adapter":"text"}`))
	if status != http.StatusCreated || view["source"] != "added" || view["adapter"] != "text" || view["text_layer"] != true {
		t.Fatalf("storing a reader: %d %s", status, raw)
	}
	if status, view, raw := call(t, "GET", base+"/v1/readers/own-text", bob, nil); status != http.StatusOK || view["spec"].(map[string]any)["adapter"] != "text" {
		t.Fatalf("reading the stored reader: %d %s", status, raw)
	}

	// The worker reads a parse pinned to it once it has read the change.
	until(t, "the worker holds the stored reader", 30*time.Second, func() bool {
		return strings.Contains(workerLogs.String(), `"msg":"readers in force"`)
	})
	pinned := parsedFile(t, base, "letter.pdf", testfixtures.Read(t, testfixtures.TextPDF), `,"reader":"own-text","allow_failed_pages":1`)
	if pinned["state"] != "succeeded" || pinned["reader"] != "own-text" {
		t.Fatalf("a parse pinned to the stored reader ended %v", pinned)
	}

	policy := []byte(`{"read":{"chain":["own-text","stub"]},"describe":{"chain":["stub"]},"extract":{"chain":["stub"]}}`)
	if status, body, raw := call(t, "PUT", base+"/v1/policy", root, policy); status != http.StatusOK || body["source"] != "set" ||
		body["read"].(map[string]any)["chain"].([]any)[0] != "own-text" {
		t.Fatalf("setting the policy: %d %s", status, raw)
	}
	_, list, _ := call(t, "GET", base+"/v1/readers", bob, nil)
	if first := list["readers"].([]any)[0].(map[string]any); first["name"] != "own-text" || first["default"] != true {
		t.Fatalf("with the policy set the readers list %v", list["readers"])
	}
	if status, body, raw := call(t, "DELETE", base+"/v1/readers/own-text", root, nil); status != http.StatusConflict || code(body) != "conflict" {
		t.Fatalf("a reader the policy names was removed: %d %s", status, raw)
	}
	if status, body, raw := call(t, "DELETE", base+"/v1/policy", root, nil); status != http.StatusOK || body["source"] != "configured" {
		t.Fatalf("putting the configured policy back: %d %s", status, raw)
	}
	if status, _, raw := call(t, "DELETE", base+"/v1/readers/own-text", root, nil); status != http.StatusNoContent {
		t.Fatalf("removing the reader: %d %s", status, raw)
	}
	if status, body, raw := call(t, "GET", base+"/v1/readers/own-text", bob, nil); status != http.StatusNotFound || code(body) != "not_found" {
		t.Fatalf("the removed reader: %d %s", status, raw)
	}
	if status, body, raw := call(t, "DELETE", base+"/v1/readers/stub", root, nil); status != http.StatusConflict || code(body) != "conflict" {
		t.Fatalf("the configured reader was removed: %d %s", status, raw)
	}
}

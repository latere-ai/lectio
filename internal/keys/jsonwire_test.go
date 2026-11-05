// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"net/http"
	"testing"
)

// The request to the endpoint names its group, owner and parse, and leaves
// out the models when it asks for none.
func TestTheRequestToTheEndpointIsExact(t *testing.T) {
	got, err := jsonRequest(question{Group: "acme", Owner: "alice", Parse: "prs_1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"group":"acme","owner":"alice","parse":"prs_1"}`; string(got) != want {
		t.Errorf("%s, want %s", got, want)
	}
}

// An answer the endpoint writes with a repeated name or in another letter
// case is read: a repeated name takes its last value.
func TestAnAnswerOfAnotherWriterIsRead(t *testing.T) {
	for name, body := range map[string]string{
		"a repeated name": `{"key":"sk-first","key":"sk-issued","expires_at":"2030-01-01T00:00:00Z"}`,
		"capitals":        `{"KEY":"sk-issued","Expires_At":"2030-01-01T00:00:00Z"}`,
		"bad UTF-8":       "{\"key\":\"sk-issued\",\"expires_at\":\"2030-01-01T00:00:00Z\",\"note\":\"\xff\"}",
	} {
		p := newPlane(t)
		p.answers(func(w http.ResponseWriter, _ *http.Request, _ string) bool {
			_, _ = w.Write([]byte(body))
			return true
		})
		e, _ := p.source()
		key, err := e.Key(ctx, "acme", "alice", "prs_1")
		if err != nil || key.Reveal() != "sk-issued" {
			t.Errorf("%s: the key is %q, %v", name, key.Reveal(), err)
		}
	}
}

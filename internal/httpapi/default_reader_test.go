// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/config"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// TestAnAllowsDefaultReaderReadsWhatNamesNone: an allow that gives its
// caller a default reader reads a submit that names no reader with it, as
// if the submit named it, and an extraction that names no extractor; a
// submit that names its own keeps it. A default the server does not have
// is reader_not_found on the member the request would have named, and a
// default that does not do what is asked leaves the routing policy's.
func TestAnAllowsDefaultReaderReadsWhatNamesNone(t *testing.T) {
	limits := `{"reader":"chosen"}`
	e := serve(t, func(s *Server, r *run.Runner) {
		r.Readers = map[string]reader.Reader{"stub": &stub.Reader{}, "chosen": &stub.Reader{}}
		r.Chain = []string{"stub"}
		s.Authz = access.NewAuthorizer(&recorder{answer: func(authz.Request) (authz.Decision, error) {
			return authz.Decision{Allow: true, Limits: json.RawMessage(limits)}, nil
		}}, configured())
	})
	file := e.upload("scan.png", sheet(t))
	if got := e.parsed(file, ""); got["reader"] != "chosen" || got["state"] != "succeeded" {
		t.Fatalf("a submit that names no reader: %v", got)
	}
	if got := e.parsed(file, `,"reader":"stub"`); got["reader"] != "stub" {
		t.Fatalf("a submit that names its reader: %v", got)
	}

	// The memory server has no extractor, so the default leaves the
	// extraction to the policy, which names none.
	asked, err := e.server.checkField(fieldRequest{Name: "title", Schema: json.RawMessage(`{"type":"object"}`)}, access.Decision{})
	if err != nil || asked.Extractor != "" {
		t.Fatalf("an extraction with no default: %+v, %v", asked, err)
	}

	limits = `{"reader":"gone"}`
	if got := e.do("POST", "/parses", `{"source":{"file":"`+file+`"}}`); got.status != http.StatusBadRequest || got.code(t) != "reader_not_found" {
		t.Fatalf("a submit under a default the server does not have: %d %s", got.status, got.body)
	}
}

// TestADefaultReaderIsHeldToTheRoleItDoes: a default that extracts is the
// extractor of an extraction that names none, and one that describes the
// describer of a figure run; a default that reads pages alone leaves both
// to the routing policy.
func TestADefaultReaderIsHeldToTheRoleItDoes(t *testing.T) {
	set, err := config.StubFile().Build([]config.Added{{Name: "own", Spec: []byte(`{"adapter":"text"}`)}},
		[]byte(`{"read":{"chain":["own","stub"]},"describe":{"chain":["stub"]},"extract":{"chain":["stub"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Live: config.NewLive(set)}
	for _, tc := range []struct{ def, role, want string }{
		{"stub", roleFields, "stub"}, {"stub", roleFigures, "stub"}, {"stub", rolePages, "stub"},
		{"own", roleFields, ""}, {"own", roleFigures, ""}, {"own", rolePages, "own"}, {"", roleFields, ""},
	} {
		if got, err := s.fallback(tc.def, tc.role, "member"); err != nil || got != tc.want {
			t.Errorf("the default %q for %s: %q, %v", tc.def, tc.role, got, err)
		}
	}
	asked, err := s.checkField(fieldRequest{Name: "title", Schema: json.RawMessage(`{"type":"object"}`)},
		access.Decision{Limits: authorizer.Limits{Reader: "stub", Readers: []string{"stub"}}})
	if err != nil || asked.Extractor != "stub" {
		t.Fatalf("an extraction under a default extractor: %+v, %v", asked, err)
	}
	if _, err := s.checkField(fieldRequest{Name: "title", Schema: json.RawMessage(`{"type":"object"}`)},
		access.Decision{Limits: authorizer.Limits{Reader: "gone"}}); err == nil {
		t.Fatal("an extraction under a default the server does not have was asked")
	}
}

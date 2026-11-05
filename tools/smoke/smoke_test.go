// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package smoke

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stub serves what lectiod serves under the prefix: the API description, and
// a 401 for a route asked with no bearer, each naming the release in the
// Lectio-Version header. An empty version sends no header.
func stub(t *testing.T, version string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/parsing/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte("openapi: 3.1.0\n"))
	})
	mux.HandleFunc("GET /v1/parsing/readers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"missing_token"}}`))
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if version != "" {
			w.Header().Set("Lectio-Version", version)
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func script(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Join(filepath.Dir(file), "release.sh")
}

// run executes the script and returns its combined output and whether it
// exited zero. It skips when the tools the script needs are not on PATH: the
// gate's hermetic run puts only the Go toolchain there.
func run(t *testing.T, env ...string) (string, bool) {
	t.Helper()
	for _, cmd := range []string{"bash", "curl", "grep", "mktemp"} {
		if _, err := exec.LookPath(cmd); err != nil {
			t.Skipf("%s is not on PATH, so release.sh cannot run here", cmd)
		}
	}
	cmd := exec.CommandContext(context.Background(), "bash", script(t))
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func TestSmokePassesWhenTheServedVersionMatchesTheTag(t *testing.T) {
	srv := stub(t, "v1.2.3")
	evidence := filepath.Join(t.TempDir(), "smoke.md")
	out, ok := run(t, "BASE_URL="+srv.URL+"/v1/parsing", "TAG=v1.2.3", "COMMIT=abc1234", "OUTPUT_MD="+evidence)
	if !ok {
		t.Fatalf("the smoke failed against a matching tag:\n%s", out)
	}
	if !strings.Contains(out, "served version matches the tag") {
		t.Fatalf("the version check did not run:\n%s", out)
	}
	md, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Release evidence", "- Tag: `v1.2.3`", "- Commit: `abc1234`", "Served version: `v1.2.3`"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("the evidence lacks %q:\n%s", want, md)
		}
	}
}

func TestSmokeFailsOnAVersionThatIsNotTheTag(t *testing.T) {
	srv := stub(t, "v1.2.2")
	out, ok := run(t, "BASE_URL="+srv.URL+"/v1/parsing", "TAG=v1.2.3")
	if ok {
		t.Fatalf("the smoke passed against a served version of v1.2.2:\n%s", out)
	}
	if !strings.Contains(out, "version mismatch") {
		t.Errorf("the failure does not name the mismatch:\n%s", out)
	}
}

func TestSmokeFailsWhenTheServiceIsDown(t *testing.T) {
	srv := stub(t, "v1.2.3")
	base := srv.URL + "/v1/parsing"
	srv.Close()
	out, ok := run(t, "BASE_URL="+base, "TAG=v1.2.3")
	if ok {
		t.Fatalf("the smoke passed against a service that is down:\n%s", out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("the failure is not reported:\n%s", out)
	}
}

func TestSmokeFailsWhenNoVersionIsServed(t *testing.T) {
	srv := stub(t, "")
	out, ok := run(t, "BASE_URL="+srv.URL+"/v1/parsing", "TAG=v1.2.3")
	if ok {
		t.Fatalf("the smoke passed with no Lectio-Version header:\n%s", out)
	}
	if !strings.Contains(out, "no Lectio-Version header") {
		t.Errorf("the failure does not name the missing header:\n%s", out)
	}
}

func TestSmokeRecordsTheVersionWithNoTag(t *testing.T) {
	srv := stub(t, "v1.2.3")
	out, ok := run(t, "BASE_URL="+srv.URL+"/v1/parsing")
	if !ok {
		t.Fatalf("the smoke failed with no tag set:\n%s", out)
	}
	if !strings.Contains(out, "served version recorded (v1.2.3)") {
		t.Errorf("the served version was not recorded:\n%s", out)
	}
}

func TestSmokeRefusesWithNoTarget(t *testing.T) {
	out, ok := run(t, "BASE_URL=")
	if ok {
		t.Fatalf("the smoke passed with no BASE_URL:\n%s", out)
	}
	if !strings.Contains(out, "BASE_URL is required") {
		t.Errorf("the failure does not say what is missing:\n%s", out)
	}
}

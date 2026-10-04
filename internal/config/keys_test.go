// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strings"
	"testing"
)

// TestTheKeySourceIsStaticUnlessNamed: with nothing set every group's pages
// are read with the key of LECTIO_MODEL_KEY, and the task store is opened
// with one key scope for all groups.
func TestTheKeySourceIsStaticUnlessNamed(t *testing.T) {
	for _, pairs := range [][]string{nil, {"LECTIO_KEYS", " static "}} {
		s, err := FromEnv(env(append(pairs, "LECTIO_MODEL_KEY", "sk-operator")...))
		if err != nil {
			t.Fatal(err)
		}
		if s.Keys != KeysStatic || s.KeysURL != "" || !s.KeysToken.IsZero() || s.ModelKey.Reveal() != "sk-operator" {
			t.Fatalf("the key source of %v: %+v", pairs, s)
		}
		if s.Queue(Stub()).KeysPerGroup {
			t.Fatalf("with one key for every group the store is opened with a scope per group: %v", pairs)
		}
	}
}

// TestTheEndpointIsReadByAProcessThatRunsTasks: with LECTIO_KEYS=endpoint a
// worker, and a process in both roles, read the endpoint's address and its
// bearer, and open the task store with a key scope per group.
func TestTheEndpointIsReadByAProcessThatRunsTasks(t *testing.T) {
	for _, role := range []string{RoleWorker, RoleAll, ""} {
		s, err := FromEnv(env("LECTIO_ROLE", role, "LECTIO_KEYS", "endpoint",
			"LECTIO_KEYS_URL", " https://plane.example/keys/lectio ", "LECTIO_KEYS_TOKEN", " kt-secret-bearer "))
		if err != nil {
			t.Fatalf("role %q: %v", role, err)
		}
		if s.Keys != KeysEndpoint || s.KeysURL != "https://plane.example/keys/lectio" || s.KeysToken.Reveal() != "kt-secret-bearer" {
			t.Fatalf("role %q: the key source is %+v", role, s)
		}
		if !s.Queue(Stub()).KeysPerGroup {
			t.Fatalf("role %q: with a key per group the store is opened with one scope for all", role)
		}
		for _, shown := range []string{fmt.Sprint(s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s)} {
			if strings.Contains(shown, "kt-secret-bearer") {
				t.Fatalf("the settings print the bearer: %s", shown)
			}
		}
	}
}

// TestTheAPIReadsNeitherTheEndpointNorItsBearer: both roles read one
// ConfigMap, so a process in the role api is given LECTIO_KEYS=endpoint with
// no address and no bearer, and starts. Given both, it reads neither: the
// API faces callers and holds no credential that obtains a tenant's key. It
// opens the task store with a scope per group, as the workers do, because
// each process that opens the store writes its settings. A model key beside
// the endpoint is not the API's to refuse: it reads no page.
func TestTheAPIReadsNeitherTheEndpointNorItsBearer(t *testing.T) {
	for name, pairs := range map[string][]string{
		"neither set":            nil,
		"both set":               {"LECTIO_KEYS_URL", "https://plane.example/keys", "LECTIO_KEYS_TOKEN", "kt-secret-bearer"},
		"an address that is not": {"LECTIO_KEYS_URL", "sk-secret-url"},
		"a model key beside it":  {"LECTIO_MODEL_KEY", "sk-operator"},
	} {
		s, err := FromEnv(env(append(pairs, "LECTIO_ROLE", RoleAPI, "LECTIO_KEYS", "endpoint")...))
		if err != nil {
			t.Fatalf("%s: the API was refused: %v", name, err)
		}
		if s.Keys != KeysEndpoint || s.KeysURL != "" || !s.KeysToken.IsZero() {
			t.Fatalf("%s: the API read the endpoint: %q, bearer set: %t", name, s.KeysURL, !s.KeysToken.IsZero())
		}
		if !s.Queue(Stub()).KeysPerGroup {
			t.Fatalf("%s: the API opens the store with one scope for all while the workers open it with one per group", name)
		}
	}
	// With the static source the API ignores the 2 variables the same way.
	if s, err := FromEnv(env("LECTIO_ROLE", RoleAPI, "LECTIO_KEYS_URL", "https://plane.example/keys", "LECTIO_KEYS_TOKEN", "kt")); err != nil || s.KeysURL != "" || !s.KeysToken.IsZero() {
		t.Fatalf("the API under the static source: %+v, %v", s, err)
	}
}

// TestAKeySourceThatCannotRunIsRefused: each setting of the key source a
// process that runs tasks cannot start with is an error that names the
// variable, both variables where 2 disagree, and never a value.
func TestAKeySourceThatCannotRunIsRefused(t *testing.T) {
	url, token := []string{"LECTIO_KEYS_URL", "https://sk-secret.example/keys"}, []string{"LECTIO_KEYS_TOKEN", "sk-secret-bearer"}
	endpoint := []string{"LECTIO_KEYS", "endpoint"}
	join := func(parts ...[]string) []string {
		var out []string
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	for _, role := range []string{RoleWorker, RoleAll} {
		for name, tc := range map[string]struct {
			pairs []string
			want  []string
		}{
			"a source that is not one":              {[]string{"LECTIO_KEYS", "sk-secret-source"}, []string{"LECTIO_KEYS is not static or endpoint"}},
			"the endpoint with no address":          {join(endpoint, token), []string{"LECTIO_KEYS_URL is not set", "LECTIO_KEYS is endpoint"}},
			"the endpoint with no bearer":           {join(endpoint, url), []string{"LECTIO_KEYS_TOKEN is not set", "LECTIO_KEYS is endpoint"}},
			"the endpoint with neither":             {endpoint, []string{"LECTIO_KEYS_URL is not set", "LECTIO_KEYS_TOKEN is not set"}},
			"an address that is no URL":             {join(endpoint, token, []string{"LECTIO_KEYS_URL", "sk-secret-url"}), []string{"LECTIO_KEYS_URL is not an absolute http or https URL"}},
			"an address of another scheme":          {join(endpoint, token, []string{"LECTIO_KEYS_URL", "ftp://sk-secret.example"}), []string{"LECTIO_KEYS_URL is not an absolute"}},
			"the endpoint and a model key":          {join(endpoint, url, token, []string{"LECTIO_MODEL_KEY", "sk-secret-model"}), []string{"LECTIO_MODEL_KEY is set", "LECTIO_KEYS is endpoint"}},
			"an address with the static source":     {url, []string{"LECTIO_KEYS_URL", "LECTIO_KEYS is static"}},
			"a bearer with the static source":       {join([]string{"LECTIO_KEYS", "static"}, token), []string{"LECTIO_KEYS_TOKEN", "LECTIO_KEYS is static"}},
			"the endpoint and a development server": {join(endpoint, url, token, []string{"LECTIO_DEV", "true"}), []string{"LECTIO_KEYS is endpoint", "LECTIO_DEV is true"}},
		} {
			_, err := FromEnv(env(append(tc.pairs, "LECTIO_ROLE", role)...))
			if err == nil {
				t.Errorf("%s, role %s: accepted", name, role)
				continue
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s, role %s: the error does not say %q: %v", name, role, want, err)
				}
			}
			if strings.Contains(err.Error(), "sk-secret") {
				t.Errorf("%s, role %s: the error holds a value: %v", name, role, err)
			}
		}
	}
	// A development server runs tasks whatever its role says.
	if _, err := FromEnv(env("LECTIO_DEV", "true", "LECTIO_ROLE", RoleAPI, "LECTIO_KEYS", "endpoint")); err == nil || !strings.Contains(err.Error(), "LECTIO_DEV") {
		t.Errorf("a development server in the role api was given the endpoint: %v", err)
	}
	if _, err := FromEnv(env("LECTIO_DEV", "true", "LECTIO_ROLE", RoleAPI, "LECTIO_KEYS_TOKEN", "sk-secret-bearer")); err == nil || !strings.Contains(err.Error(), "LECTIO_KEYS is static") {
		t.Errorf("a development server in the role api was given a bearer nothing sends: %v", err)
	}
}

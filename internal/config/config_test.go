// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(pairs ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(name string) string { return m[name] }
}

func TestSettingsHaveDefaults(t *testing.T) {
	s, err := FromEnv(env())
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr != ":8080" || s.BasePath != "/v1" || s.Dev || s.DevToken != "dev" || s.MaxFileBytes != 256<<20 || s.MaxPages != 3000 ||
		s.Workers != 8 || s.Attempts != 5 || s.MaxDeadline != time.Hour || s.Grace != 25*time.Second || !s.ModelKey.IsZero() || s.FetchAllow != nil {
		t.Fatalf("defaults: %+v", s)
	}
}

func TestSettingsAreReadFromTheEnvironment(t *testing.T) {
	s, err := FromEnv(env(
		"LECTIO_ADDR", "127.0.0.1:9000", "LECTIO_BASE_PATH", "/api/parsing", "LECTIO_DEV", "true", "LECTIO_DEV_TOKEN", "t0",
		"LECTIO_DATABASE_URL", "postgres://db/lectio", "LECTIO_CONFIG", "/etc/lectio", "LECTIO_MODEL_KEY", " sk-live ",
		"LECTIO_MAX_FILE_BYTES", "1024", "LECTIO_MAX_PAGES", "10", "LECTIO_WORKERS", "2", "LECTIO_TASK_ATTEMPTS", "4",
		"LECTIO_MAX_DEADLINE", "10m", "LECTIO_SHUTDOWN_GRACE", "5s", "LECTIO_FETCH_ALLOW", " Store.Internal , ,minio:9000",
	))
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr != "127.0.0.1:9000" || s.BasePath != "/api/parsing" || !s.Dev || s.DevToken != "t0" || s.DatabaseURL == "" || s.ConfigPath != "/etc/lectio" ||
		s.ModelKey.Reveal() != "sk-live" || s.MaxFileBytes != 1024 || s.MaxPages != 10 || s.Workers != 2 || s.Attempts != 4 ||
		s.MaxDeadline != 10*time.Minute || s.Grace != 5*time.Second || strings.Join(s.FetchAllow, "|") != "store.internal|minio:9000" {
		t.Fatalf("settings: %+v", s)
	}
}

func TestASettingThatDoesNotParseNamesItsVariableAndNotItsValue(t *testing.T) {
	_, err := FromEnv(env(
		"LECTIO_DEV", "sk-secret-yes", "LECTIO_BASE_PATH", "sk-secret-path", "LECTIO_MAX_FILE_BYTES", "sk-secret-1",
		"LECTIO_MAX_PAGES", "0", "LECTIO_WORKERS", "-1", "LECTIO_MAX_DEADLINE", "sk-secret-2", "LECTIO_SHUTDOWN_GRACE", "0s",
	))
	if err == nil {
		t.Fatal("no error")
	}
	for _, name := range []string{"LECTIO_DEV", "LECTIO_BASE_PATH", "LECTIO_MAX_FILE_BYTES", "LECTIO_MAX_PAGES", "LECTIO_WORKERS", "LECTIO_MAX_DEADLINE", "LECTIO_SHUTDOWN_GRACE"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("the error repeats a value: %v", err)
	}
}

// write puts files into a new directory and returns it.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const twoReaders = `
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: default }
spec:
  adapter: chat
  endpoint: https://gateway.example/v1
  model: some-model
  maxInFlight: 16
  cost: 1
  timeout: 120s
  image: { dpi: 160, longEdge: 2048, format: png }
  constrained: true
  boxes: { order: yxyx, space: grid }
  temperature: 0
  outputLimitParam: max_tokens
---
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: engine }
spec:
  adapter: layout
  endpoint: http://engine.internal:8000/read
  image: { dpi: 200, format: jpeg }
`

const policy = `
apiVersion: lectio.latere.ai/v1
kind: Policy
metadata: { name: default }
spec:
  read:    { chain: [default, engine] }
  extract: { chain: [text] }
  escalate: { onInvalid: 2, max: 1 }
`

func TestDocumentsDeclareReadersAndThePolicy(t *testing.T) {
	// A directory: files are read in name order, and other files are left.
	dir := write(t, map[string]string{
		"10-readers.yaml": twoReaders, "20-policy.yml": policy, "notes.txt": "not a document",
		"30-stub.json": `{"apiVersion":"lectio.latere.ai/v1","kind":"Reader","metadata":{"name":"offline"},"spec":{"adapter":"stub"}}`,
	})
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Readers) != 3 || strings.Join(got.Chain, ",") != "default,engine" {
		t.Fatalf("readers %v, chain %v", got.Readers, got.Chain)
	}
	if d := got.Readers["default"].Describe(); d.Name != "default" || d.Image.DPI != 160 || d.Image.LongEdge != 2048 || d.Image.Format != "png" {
		t.Fatalf("the chat reader: %+v", d)
	}
	if d := got.Readers["engine"].Describe(); d.Name != "engine" || d.Image.DPI != 200 || d.Image.Format != "jpeg" {
		t.Fatalf("the layout reader: %+v", d)
	}
	// What is set and not acted on is named.
	if want := `Reader "default" maxInFlight and cost|Policy extract.chain|Policy escalate`; strings.Join(got.Unapplied, "|") != want {
		t.Fatalf("unapplied: %q", got.Unapplied)
	}

	// One file with one reader and no policy: the reader is the chain.
	one := write(t, map[string]string{"reader.yaml": `{apiVersion: lectio.latere.ai/v1, kind: Reader, metadata: {name: only}, spec: {adapter: stub}}`})
	got, err = Load(filepath.Join(one, "reader.yaml"))
	if err != nil || strings.Join(got.Chain, ",") != "only" || len(got.Unapplied) != 0 {
		t.Fatalf("one reader: %+v, %v", got, err)
	}

	if s := Stub(); len(s.Readers) != 1 || s.Chain[0] != "stub" || s.Readers["stub"] == nil {
		t.Fatalf("the stub configuration: %+v", s)
	}
}

func TestAConfigurationThatDoesNotHoldIsRefusedWhole(t *testing.T) {
	head := "apiVersion: lectio.latere.ai/v1\n"
	stub := func(name string) string {
		return head + "kind: Reader\nmetadata: { name: " + name + " }\nspec: { adapter: stub }\n"
	}
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"no document":                 {map[string]string{"a.yaml": ""}, "declares no Reader"},
		"not YAML":                    {map[string]string{"a.yaml": "kind: [unclosed"}, "document 1"},
		"another version":             {map[string]string{"a.yaml": "apiVersion: v2\nkind: Reader\nmetadata: {name: x}\nspec: {adapter: stub}\n"}, `apiVersion is "v2"`},
		"another kind":                {map[string]string{"a.yaml": head + "kind: Pool\nmetadata: {name: x}\n"}, "not Reader or Policy"},
		"no name":                     {map[string]string{"a.yaml": head + "kind: Reader\nspec: {adapter: stub}\n"}, "metadata.name is empty"},
		"a name used twice":           {map[string]string{"a.yaml": stub("x") + "---\n" + stub("x")}, "used twice"},
		"an adapter that is none":     {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: vision}\n"}, `adapter is "vision"`},
		"a member no spec has":        {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: stub, topP: 1}\n"}, "topP"},
		"a timeout of nothing":        {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: stub, timeout: soon}\n"}, "timeout is not a duration"},
		"a box order that is none":    {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: chat, endpoint: 'https://gateway.example/v1', model: m, boxes: {order: zigzag}}\n"}, "zigzag"},
		"a member the design dropped": {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: stub, requestsPerMinute: 60}\n"}, "requestsPerMinute"},
		"a chat reader, no model":     {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: chat, endpoint: 'https://gateway.example/v1'}\n"}, "names no model"},
		"a layout reader, no url":     {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: layout}\n"}, "x"},
		"two readers, no policy":      {map[string]string{"a.yaml": stub("x") + "---\n" + stub("y")}, "no Policy to order them"},
		"two policies":                {map[string]string{"a.yaml": stub("x"), "b.yaml": policy + "---\n" + policy}, "this is the second"},
		"a policy member no spec":     {map[string]string{"a.yaml": stub("x") + "---\n" + head + "kind: Policy\nmetadata: {name: p}\nspec: {write: {chain: [x]}}\n"}, "write"},
		"a chain naming no one":       {map[string]string{"a.yaml": stub("x") + "---\n" + policy}, `names "default", which is no Reader`},
	} {
		got, err := Load(write(t, tc.files))
		if err == nil || !strings.Contains(err.Error(), tc.want) || got.Readers != nil {
			t.Errorf("%s: %v (%v)", name, err, got.Readers)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("a path that is not there")
	}
	// A file that cannot be read.
	dir := write(t, map[string]string{"a.yaml": stub("x")})
	if err := os.Chmod(filepath.Join(dir, "a.yaml"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil && os.Getuid() != 0 {
		t.Error("a file that cannot be read")
	}
}

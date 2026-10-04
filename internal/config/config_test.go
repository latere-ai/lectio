// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/tasks"
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
		s.Workers != 8 || s.Attempts != 5 || s.MaxDeadline != time.Hour || s.Grace != 25*time.Second || !s.ModelKey.IsZero() || s.FetchAllow != nil || s.ConverterURL != "" {
		t.Fatalf("defaults: %+v", s)
	}
	if s.FileRetention != 24*time.Hour || s.ParseRetention != 30*24*time.Hour {
		t.Fatalf("a file is kept for %s and a parse for %s, want 24 hours and 30 days", s.FileRetention, s.ParseRetention)
	}
}

// TestTheGroupDefaultsAreASetting: what a group takes when its allow names
// none is a list of name=value over the members an allow may name. A
// member the list leaves out is zero, and what does not read names the
// variable and the member and never the value.
func TestTheGroupDefaultsAreASetting(t *testing.T) {
	s, err := FromEnv(env("LECTIO_GROUP_DEFAULTS", " weight=4, max_running = 16 ,max_queued=200,max_priority=10,pages_per_day=5000,"))
	if want := (GroupDefaults{Weight: 4, MaxRunning: 16, MaxQueued: 200, MaxPriority: 10, PagesPerDay: 5000}); err != nil || s.GroupDefaults != want {
		t.Fatalf("the group defaults: %+v, %v", s.GroupDefaults, err)
	}
	if s, err := FromEnv(env("LECTIO_GROUP_DEFAULTS", "max_priority=3")); err != nil || s.GroupDefaults != (GroupDefaults{MaxPriority: 3}) {
		t.Fatalf("one member: %+v, %v", s.GroupDefaults, err)
	}
	if s, err := FromEnv(env()); err != nil || s.GroupDefaults != (GroupDefaults{}) {
		t.Fatalf("none set: %+v, %v", s.GroupDefaults, err)
	}
	for value, want := range map[string]string{
		"max_pages=77":                  "not one of",
		"max_priority":                  "not one of",
		"max_queued=77,max_queued=78":   "max_queued twice",
		"max_running=sk-secret":         "max_running is not a whole number",
		"pages_per_day=-77":             "pages_per_day is not a whole number",
		"max_priority=99999999999999":   "max_priority is not a whole number",
		"weight=1001":                   "weight is above 1000",
		"weight=77,retention_seconds=5": "not one of",
	} {
		_, err := FromEnv(env("LECTIO_GROUP_DEFAULTS", value))
		if err == nil || !strings.Contains(err.Error(), "LECTIO_GROUP_DEFAULTS") || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", value, err)
			continue
		}
		for _, secret := range []string{"sk-secret", "77", "78", "99999999999999", "1001"} {
			if strings.Contains(value, secret) && strings.Contains(err.Error(), secret) {
				t.Errorf("%q: the error repeats the value: %v", value, err)
			}
		}
	}
}

// TestTheRetentionsAreSettings: how long a file and a parse are kept are
// durations above zero, each read from its own variable.
func TestTheRetentionsAreSettings(t *testing.T) {
	s, err := FromEnv(env("LECTIO_FILE_RETENTION", "90m", "LECTIO_PARSE_RETENTION", "168h"))
	if err != nil || s.FileRetention != 90*time.Minute || s.ParseRetention != 7*24*time.Hour {
		t.Fatalf("the retentions: %s and %s, %v", s.FileRetention, s.ParseRetention, err)
	}
	for _, name := range []string{"LECTIO_FILE_RETENTION", "LECTIO_PARSE_RETENTION"} {
		for _, value := range []string{"30 days", "0h", "-1h"} {
			if _, err := FromEnv(env(name, value)); err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), value) {
				t.Errorf("%s=%q: %v", name, value, err)
			}
		}
	}
}

func TestSettingsAreReadFromTheEnvironment(t *testing.T) {
	s, err := FromEnv(env(
		"LECTIO_ADDR", "127.0.0.1:9000", "LECTIO_BASE_PATH", "/api/parsing", "LECTIO_DEV", "true", "LECTIO_DEV_TOKEN", "t0",
		"LECTIO_DATABASE_URL", " postgres://db.example/lectio ", "LECTIO_DATABASE_POOL_URL", "postgres://pooler.example/lectio",
		"LECTIO_CONFIG", "/etc/lectio", "LECTIO_MODEL_KEY", " sk-live ",
		"LECTIO_MAX_FILE_BYTES", "1024", "LECTIO_MAX_PAGES", "10", "LECTIO_WORKERS", "2", "LECTIO_TASK_ATTEMPTS", "4",
		"LECTIO_MAX_DEADLINE", "10m", "LECTIO_SHUTDOWN_GRACE", "5s", "LECTIO_FETCH_ALLOW", " Store.Internal , ,minio:9000",
		"LECTIO_CONVERTER_URL", " unix:///run/lectio/convert.sock ",
	))
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr != "127.0.0.1:9000" || s.BasePath != "/api/parsing" || !s.Dev || s.DevToken != "t0" ||
		s.DatabaseURL != "postgres://db.example/lectio" || s.DatabasePoolURL != "postgres://pooler.example/lectio" || s.ConfigPath != "/etc/lectio" ||
		s.ModelKey.Reveal() != "sk-live" || s.MaxFileBytes != 1024 || s.MaxPages != 10 || s.Workers != 2 || s.Attempts != 4 ||
		s.MaxDeadline != 10*time.Minute || s.Grace != 5*time.Second || strings.Join(s.FetchAllow, "|") != "store.internal|minio:9000" ||
		s.ConverterURL != "unix:///run/lectio/convert.sock" {
		t.Fatalf("settings: %+v", s)
	}
}

// TestTheServingPathOpensThePooledURL: the store's pool opens the pooled
// endpoint where one is named and the direct one otherwise, and migrations
// keep the direct one either way.
func TestTheServingPathOpensThePooledURL(t *testing.T) {
	direct, err := FromEnv(env("LECTIO_DATABASE_URL", "postgres://db.example/lectio"))
	if err != nil {
		t.Fatal(err)
	}
	if direct.ServingURL() != "postgres://db.example/lectio" || direct.DatabasePoolURL != "" {
		t.Fatalf("with no pooled URL the serving path opens %q", direct.ServingURL())
	}
	pooled, err := FromEnv(env("LECTIO_DATABASE_URL", "postgres://db.example/lectio", "LECTIO_DATABASE_POOL_URL", "postgres://pooler.example/lectio"))
	if err != nil {
		t.Fatal(err)
	}
	if pooled.ServingURL() != "postgres://pooler.example/lectio" || pooled.DatabaseURL != "postgres://db.example/lectio" {
		t.Fatalf("with a pooled URL the serving path opens %q and migrations %q", pooled.ServingURL(), pooled.DatabaseURL)
	}
	if none, _ := FromEnv(env()); none.ServingURL() != "" {
		t.Fatalf("with no database the serving path opens %q", none.ServingURL())
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
  describe: { chain: [default] }
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
	// A reader that reaches a model with an instruction can describe a
	// figure too, under the same name; one behind a layout engine cannot.
	if len(got.Describers) != 2 || got.Describers["default"] == nil || got.Describers["offline"] == nil || got.Describers["engine"] != nil ||
		strings.Join(got.DescribeChain, ",") != "default" || got.Describers["default"].Describe().Name != "default" {
		t.Fatalf("describers %v, chain %v", got.Describers, got.DescribeChain)
	}
	// What is set and not acted on is named.
	if want := `Policy extract.chain|Policy escalate`; strings.Join(got.Unapplied, "|") != want {
		t.Fatalf("unapplied: %q", got.Unapplied)
	}
	if want := `Reader "default" maxInFlight and cost`; strings.Join(got.RunnerUnapplied, "|") != want {
		t.Fatalf("what the task store applies and the runner does not: %q", got.RunnerUnapplied)
	}

	// One file with one reader and no policy: the reader is the chain.
	one := write(t, map[string]string{"reader.yaml": `{apiVersion: lectio.latere.ai/v1, kind: Reader, metadata: {name: only}, spec: {adapter: stub}}`})
	got, err = Load(filepath.Join(one, "reader.yaml"))
	if err != nil || strings.Join(got.Chain, ",") != "only" || len(got.Unapplied) != 0 {
		t.Fatalf("one reader: %+v, %v", got, err)
	}

	if got.DescribeChain != nil || got.Describers["only"] == nil {
		t.Fatalf("with no policy nothing is in the describe chain: %+v", got)
	}

	if s := Stub(); len(s.Readers) != 1 || s.Chain[0] != "stub" || s.Readers["stub"] == nil || s.Describers["stub"] == nil || s.DescribeChain[0] != "stub" {
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
		"no document":                       {map[string]string{"a.yaml": ""}, "declares no Reader"},
		"not YAML":                          {map[string]string{"a.yaml": "kind: [unclosed"}, "document 1"},
		"another version":                   {map[string]string{"a.yaml": "apiVersion: v2\nkind: Reader\nmetadata: {name: x}\nspec: {adapter: stub}\n"}, `apiVersion is "v2"`},
		"another kind":                      {map[string]string{"a.yaml": head + "kind: Pool\nmetadata: {name: x}\n"}, "not Reader or Policy"},
		"no name":                           {map[string]string{"a.yaml": head + "kind: Reader\nspec: {adapter: stub}\n"}, "metadata.name is empty"},
		"a name used twice":                 {map[string]string{"a.yaml": stub("x") + "---\n" + stub("x")}, "used twice"},
		"an adapter that is none":           {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: vision}\n"}, `adapter is "vision"`},
		"a member no spec has":              {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: stub, topP: 1}\n"}, "topP"},
		"a timeout of nothing":              {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: stub, timeout: soon}\n"}, "timeout is not a duration"},
		"a box order that is none":          {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: chat, endpoint: 'https://gateway.example/v1', model: m, boxes: {order: zigzag}}\n"}, "zigzag"},
		"a member the design dropped":       {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: stub, requestsPerMinute: 60}\n"}, "requestsPerMinute"},
		"a chat reader, no model":           {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: chat, endpoint: 'https://gateway.example/v1'}\n"}, "names no model"},
		"a layout reader, no url":           {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: x}\nspec: {adapter: layout}\n"}, "x"},
		"two readers, no policy":            {map[string]string{"a.yaml": stub("x") + "---\n" + stub("y")}, "no Policy to order them"},
		"two policies":                      {map[string]string{"a.yaml": stub("x"), "b.yaml": policy + "---\n" + policy}, "this is the second"},
		"a policy member no spec":           {map[string]string{"a.yaml": stub("x") + "---\n" + head + "kind: Policy\nmetadata: {name: p}\nspec: {write: {chain: [x]}}\n"}, "write"},
		"a describe chain naming no one":    {map[string]string{"a.yaml": stub("x") + "---\n" + head + "kind: Policy\nmetadata: {name: p}\nspec: {read: {chain: [x]}, describe: {chain: [y]}}\n"}, `describe chain names "y", which is no Reader`},
		"a layout engine asked to describe": {map[string]string{"a.yaml": head + "kind: Reader\nmetadata: {name: e}\nspec: {adapter: layout, endpoint: 'http://engine.internal/read'}\n---\n" + head + "kind: Policy\nmetadata: {name: p}\nspec: {read: {chain: [e]}, describe: {chain: [e]}}\n"}, "cannot describe a figure"},
		"a chain naming no one":             {map[string]string{"a.yaml": stub("x") + "---\n" + policy}, `names "default", which is no Reader`},
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

// TestAReaderOfAPagesOwnTextNeedsNoEndpoint: a Reader document of the text
// adapter names no endpoint, no model and no key, a policy puts it before
// a reader that calls a model, and its pool is bounded by what a fleet of
// workers runs and not by what one endpoint takes.
func TestAReaderOfAPagesOwnTextNeedsNoEndpoint(t *testing.T) {
	head := "apiVersion: lectio.latere.ai/v1\n"
	dir := write(t, map[string]string{"readers.yaml": head + `kind: Reader
metadata: { name: own }
spec: { adapter: text }
---
` + head + `kind: Reader
metadata: { name: vision }
spec: { adapter: chat, endpoint: 'https://gateway.example/v1', model: some-model }
---
` + head + `kind: Policy
metadata: { name: default }
spec:
  read: { chain: [own, vision] }
  describe: { chain: [vision] }
`})
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := got.Readers["own"].Describe()
	if strings.Join(got.Chain, ",") != "own,vision" || d.Name != "own" || !d.Text || d.Version == "" || d.Image.DPI != 160 || d.Image.Format != "png" {
		t.Fatalf("chain %v, the text reader %+v", got.Chain, d)
	}
	// It describes no figure, and a model reader does not ask for a
	// page's text.
	if got.Describers["own"] != nil || got.Describers["vision"] == nil || got.Readers["vision"].Describe().Text {
		t.Fatalf("describers %v", got.Describers)
	}
	pools := map[string]tasks.Pool{}
	for _, p := range got.Pools {
		pools[p.Reader] = p
	}
	if pools["own"] != (tasks.Pool{Reader: "own", MaxInFlight: DefaultTextInFlight, Cost: 1}) || pools["vision"].MaxInFlight != DefaultMaxInFlight {
		t.Fatalf("the pools: %+v", got.Pools)
	}

	// Its image and its pool are its document's to set, and a reader with
	// another image is another version.
	set := write(t, map[string]string{"reader.yaml": head + "kind: Reader\nmetadata: { name: own }\nspec: { adapter: text, image: { dpi: 96, format: jpeg }, maxInFlight: 200, cost: 1 }\n"})
	one, err := Load(set)
	if err != nil {
		t.Fatal(err)
	}
	if o := one.Readers["own"].Describe(); o.Image.DPI != 96 || o.Image.Format != "jpeg" || o.Version == d.Version || one.Pools[0].MaxInFlight != 200 || strings.Join(one.Chain, ",") != "own" {
		t.Fatalf("a text reader with an image of its own: %+v, pools %+v", o, one.Pools)
	}

	for name, tc := range map[string]struct{ spec, want string }{
		"an endpoint":             {"{ adapter: text, endpoint: 'https://gateway.example/v1' }", "calls no endpoint"},
		"a model":                 {"{ adapter: text, model: some-model }", "calls no endpoint"},
		"an image format unknown": {"{ adapter: text, image: { format: webp } }", "webp"},
		"asked to describe":       {"{ adapter: text }\n---\n" + head + "kind: Policy\nmetadata: { name: p }\nspec: { read: { chain: [own] }, describe: { chain: [own] } }", "cannot describe a figure"},
	} {
		bad := write(t, map[string]string{"reader.yaml": head + "kind: Reader\nmetadata: { name: own }\nspec: " + tc.spec + "\n"})
		if got, err := Load(bad); err == nil || !strings.Contains(err.Error(), tc.want) || got.Readers != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestTheDurableServersSettings: what the durable server is run with has the
// specs' defaults, and each is read from its variable.
func TestTheDurableServersSettings(t *testing.T) {
	s, err := FromEnv(env())
	if err != nil {
		t.Fatal(err)
	}
	if s.Role != RoleAll || s.InternalAddr != ":8081" || s.Bucket != "" || s.S3PathStyle || !s.S3SecretKey.IsZero() ||
		s.Lease != 60*time.Second || s.Expiries != 3 || s.SweepInterval != 30*time.Second || s.Flush != 200*time.Millisecond ||
		s.Poll != time.Second || s.PoolRecovery != 30*time.Second || s.PoolResume != 10*time.Second || s.CacheBytes != 2<<30 {
		t.Fatalf("defaults: %+v", s)
	}
	s, err = FromEnv(env(
		"LECTIO_ROLE", "worker", "LECTIO_INTERNAL_ADDR", "127.0.0.1:9001", "LECTIO_KEYS", "static",
		"LECTIO_BUCKET", "lectio", "LECTIO_BUCKET_PREFIX", "staging", "LECTIO_S3_ENDPOINT", "https://objects.example",
		"LECTIO_S3_REGION", "us-east-1", "LECTIO_S3_ACCESS_KEY", "access", "LECTIO_S3_SECRET_KEY", " s3-secret ", "LECTIO_S3_PATH_STYLE", "true",
		"LECTIO_TASK_LEASE", "2s", "LECTIO_TASK_EXPIRIES", "2", "LECTIO_SWEEP_INTERVAL", "500ms", "LECTIO_WORKER_FLUSH", "50ms",
		"LECTIO_WORKER_POLL", "100ms", "LECTIO_POOL_RECOVERY", "3s", "LECTIO_POOL_RESUME", "1s", "LECTIO_CACHE_BYTES", "4096",
	))
	if err != nil {
		t.Fatal(err)
	}
	if s.Role != RoleWorker || s.InternalAddr != "127.0.0.1:9001" || s.Bucket != "lectio" || s.BucketPrefix != "staging" ||
		s.S3Endpoint != "https://objects.example" || s.S3Region != "us-east-1" || s.S3AccessKey != "access" || s.S3SecretKey.Reveal() != "s3-secret" || !s.S3PathStyle ||
		s.Lease != 2*time.Second || s.Expiries != 2 || s.SweepInterval != 500*time.Millisecond || s.Flush != 50*time.Millisecond ||
		s.Poll != 100*time.Millisecond || s.PoolRecovery != 3*time.Second || s.PoolResume != time.Second || s.CacheBytes != 4096 {
		t.Fatalf("settings: %+v", s)
	}
	if strings.Contains(fmt.Sprintf("%+v", s), "s3-secret") {
		t.Fatal("the settings print the bucket's secret key")
	}

	_, err = FromEnv(env("LECTIO_ROLE", "leader", "LECTIO_KEYS", "sk-secret-endpoint", "LECTIO_S3_PATH_STYLE", "sk-secret-style", "LECTIO_TASK_LEASE", "0s"))
	if err == nil {
		t.Fatal("settings that do not parse were accepted")
	}
	for _, name := range []string{"LECTIO_ROLE", "LECTIO_KEYS", "LECTIO_S3_PATH_STYLE", "LECTIO_TASK_LEASE"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("the error holds a value: %v", err)
	}
}

// TestAReaderIsAPoolOfTheTaskStore: each Reader document is a pool, with
// the bound and the cost it declares, and the task store is opened with
// them and the policy's chain. A cost is a whole number of units: the fair
// queue counts in them, so a fraction is refused and not rounded.
func TestAReaderIsAPoolOfTheTaskStore(t *testing.T) {
	dir := write(t, map[string]string{"readers.yaml": `
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: small }
spec: { adapter: stub }
---
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: large }
spec: { adapter: stub, maxInFlight: 4, cost: 5 }
---
apiVersion: lectio.latere.ai/v1
kind: Policy
metadata: { name: policy }
spec: { read: { chain: [small, large] } }
`})
	readers, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	pools := map[string]tasks.Pool{}
	for _, p := range readers.Pools {
		pools[p.Reader] = p
	}
	if len(pools) != 2 || pools["small"] != (tasks.Pool{Reader: "small", MaxInFlight: DefaultMaxInFlight, Cost: 1}) ||
		pools["large"] != (tasks.Pool{Reader: "large", MaxInFlight: 4, Cost: 5}) {
		t.Fatalf("the pools are %+v", readers.Pools)
	}
	if stub := Stub().Pools; len(stub) != 1 || stub[0].Reader != "stub" || stub[0].MaxInFlight != DefaultMaxInFlight {
		t.Fatalf("the stub's pool is %+v", stub)
	}

	s, err := FromEnv(env("LECTIO_TASK_LEASE", "5s", "LECTIO_TASK_ATTEMPTS", "4"))
	if err != nil {
		t.Fatal(err)
	}
	queue := s.Queue(readers).WithDefaults()
	if err := queue.Validate(); err != nil {
		t.Fatalf("the settings the task store is opened with: %v", err)
	}
	if queue.Lease != 5*time.Second || queue.Attempts != 4 || strings.Join(queue.ReadChain, ",") != "small,large" || len(queue.Pools) != 2 {
		t.Fatalf("the task store's settings are %+v", queue)
	}

	for name, spec := range map[string]string{
		"a fraction of a unit": `{ adapter: stub, cost: 1.5 }`,
		"a cost below zero":    `{ adapter: stub, cost: -1 }`,
		"a bound below zero":   `{ adapter: stub, maxInFlight: -4 }`,
	} {
		bad := write(t, map[string]string{"reader.yaml": "apiVersion: lectio.latere.ai/v1\nkind: Reader\nmetadata: { name: only }\nspec: " + spec + "\n"})
		if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), `Reader "only"`) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

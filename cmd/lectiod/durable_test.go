// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/s3/s3test"

	"latere.ai/x/lectio/internal/config"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/internal/testservers"
)

// TestMain runs this binary as a lectiod process when a test started it as
// one, and otherwise runs the tests and removes the containers they started.
func TestMain(m *testing.M) {
	if os.Getenv(childEnv) != "" {
		child()
		return
	}
	testservers.Main(m)
}

// bucket is the bucket of the object store a durable server of the tests
// writes to.
const bucket = "lectio"

// provider is the stub issuer every durable server of the tests verifies
// its callers against. It is one for the test binary, started when a test
// first needs it, and it listens on loopback, so a server a test runs as a
// process of its own reads its keys as one in the test's process does.
var provider = sync.OnceValue(func() *issuertest.Server {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	s := issuertest.NewHandler(
		issuertest.WithIssuer("http://"+ln.Addr().String()),
		issuertest.WithDefaultAudience(config.DefaultOIDCAudience),
	)
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return s
})

// issuerClaims are the claims of a token for a subject and an audience,
// good for longer than any test runs.
func issuerClaims(sub, audience string) issuertest.Claims {
	return issuertest.Claims{Sub: sub, Aud: issuertest.StringList{audience}, Exp: time.Now().Add(12 * time.Hour).Unix()}
}

// tokenOf mints a token of the provider for a subject, addressed to this
// server.
func tokenOf(sub string) string {
	return provider().Mint(issuerClaims(sub, config.DefaultOIDCAudience))
}

// token is the bearer of the tests' one caller.
var token = sync.OnceValue(func() string { return tokenOf("dev") })

// plane is what a durable server runs against in a test: a database of its
// own on the suite's Postgres, and an S3 server in the test's process, which
// every server of the test shares and which outlives each of them.
type plane struct {
	t       *testing.T
	dsn     string
	objects *s3test.Server
}

// newPlane makes a database and an object store for a test. The test skips
// where no container runtime answers.
func newPlane(t *testing.T) *plane {
	t.Helper()
	srv, err := testservers.StartPostgres()
	if err != nil {
		t.Skipf("no container runtime answered, so the durable server did not run: %v", err)
	}
	return &plane{t: t, dsn: testservers.Database(t, srv), objects: s3test.New(t, bucket)}
}

// env is the environment of a durable server of the test, in pairs: short
// leases and intervals, so what takes a minute in a deployment takes a
// second here, and listeners on ports the system picks. more overrides.
func (p *plane) env(role string, more ...string) []string {
	return append([]string{
		"LECTIO_ROLE", role, "LECTIO_DATABASE_URL", p.dsn, "LECTIO_OIDC_ISSUERS", provider().URL(),
		"LECTIO_ADDR", "127.0.0.1:0", "LECTIO_INTERNAL_ADDR", "127.0.0.1:0",
		"LECTIO_BUCKET", bucket, "LECTIO_S3_ENDPOINT", p.objects.URL(), "LECTIO_S3_REGION", s3test.Region,
		"LECTIO_S3_ACCESS_KEY", s3test.Key, "LECTIO_S3_SECRET_KEY", s3test.Secret, "LECTIO_S3_PATH_STYLE", "true",
		"LECTIO_TASK_LEASE", "2s", "LECTIO_SWEEP_INTERVAL", "300ms", "LECTIO_WORKER_FLUSH", "20ms", "LECTIO_WORKER_POLL", "50ms",
		"LECTIO_SHUTDOWN_GRACE", "5s", "LECTIO_WORKERS", "4",
	}, more...)
}

// internalOf reads the address of a server's probes from its log.
func internalOf(t *testing.T, logs *buffer) string {
	t.Helper()
	for line := range strings.SplitSeq(logs.String(), "\n") {
		var entry struct {
			Msg      string `json:"msg"`
			Internal string `json:"internal"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "listening" && entry.Internal != "" {
			return "http://" + entry.Internal
		}
	}
	t.Fatalf("the log names no internal listener:\n%s", logs.String())
	return ""
}

// get reads an address with no token.
func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

// parsedFile uploads a file to a server, parses it, and returns the parse once
// it has ended.
func parsedFile(t *testing.T, base, name string, data []byte, options string) map[string]any {
	t.Helper()
	status, file, raw := call(t, "POST", base+"/v1/files?name="+name, token(), data, "Content-Type", "application/octet-stream")
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("upload: %d %s", status, raw)
	}
	status, parse, raw := call(t, "POST", base+"/v1/parses", token(), []byte(`{"source":{"file":"`+file["id"].(string)+`"}`+options+`}`))
	if status != http.StatusAccepted && status != http.StatusOK {
		t.Fatalf("submit: %d %s", status, raw)
	}
	return ended(t, base, parse["id"].(string))
}

// ended waits for a parse of the tests' one caller to end and returns it.
func ended(t *testing.T, base, id string) map[string]any {
	t.Helper()
	return endedAs(t, base, token(), id)
}

// endedAs waits for a parse to end, reading it with its caller's bearer, and
// returns it.
func endedAs(t *testing.T, base, bearer, id string) map[string]any {
	t.Helper()
	// A runner that shares few processors between every package's tests
	// takes minutes for what a workstation does in seconds.
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		status, parse, raw := call(t, "GET", base+"/v1/parses/"+id, bearer, nil)
		if status != http.StatusOK {
			t.Fatalf("reading the parse: %d %s", status, raw)
		}
		if state := parse["state"]; state != "queued" && state != "running" {
			return parse
		}
	}
	t.Fatalf("parse %s did not end", id)
	return nil
}

// TestTheDurableServerRunsBothRolesInOneProcess: with a database and a
// bucket and no LECTIO_DEV, the server applies its schema, serves the
// contract from durable state, runs the tasks, and answers its probes on
// the internal listener. A PDF of several pages and an image are each
// parsed end to end, and what a parse wrote is in the bucket under its
// prefix.
func TestTheDurableServerRunsBothRolesInOneProcess(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	base, logs, stop := started(t, env(p.env("all")...))
	internal := internalOf(t, logs)

	// The worker registers after the listeners are up, so the server is
	// ready a moment after it answers.
	until(t, "the server is ready", time.Minute, func() bool {
		status, _ := get(t, internal+"/readyz")
		return status == http.StatusOK
	})
	for path, want := range map[string]string{"/livez": "ok", "/readyz": "ok", "/version": `"version"`} {
		if status, body := get(t, internal+path); status != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("%s: %d %q", path, status, body)
		}
	}
	// The probes are on the internal listener and the contract on the
	// public one, and neither serves the other's.
	if status, _ := get(t, base+"/readyz"); status != http.StatusNotFound {
		t.Fatalf("the public listener answered /readyz with %d", status)
	}
	if status, _ := get(t, internal+"/v1/openapi.yaml"); status != http.StatusNotFound {
		t.Fatalf("the internal listener answered the contract with %d", status)
	}

	pdf := parsedFile(t, base, "report.pdf", testfixtures.Read(t, testfixtures.MultipagePDF), "")
	if pdf["state"] != "succeeded" || pdf["progress"].(map[string]any)["pages_done"] != 3.0 || pdf["deadline_at"] == nil {
		t.Fatalf("the parse of a PDF ended %v", pdf)
	}
	at := base + "/v1/parses/" + pdf["id"].(string)
	status, _, md := call(t, "GET", at+"/document?format=markdown", token(), nil)
	if status != http.StatusOK || strings.Count(string(md), "# Page ") != 3 {
		t.Fatalf("its document: %d %q", status, md)
	}
	if status, _, img := call(t, "GET", at+"/pages/2/image", token(), nil); status != http.StatusOK || !strings.HasPrefix(string(img), "\x89PNG") {
		t.Fatalf("the image of its second page: %d, %d bytes", status, len(img))
	}
	var stored int
	for _, key := range p.objects.Keys() {
		if strings.HasPrefix(key, "parses/"+pdf["id"].(string)+"/") {
			stored++
		}
	}
	// 3 page results, 3 images and the index, and the pages assemble wrote
	// again.
	if stored < 7 {
		t.Fatalf("the parse left %d objects under its prefix: %v", stored, p.objects.Keys())
	}

	scan := parsedFile(t, base, "scan.png", testfixtures.Read(t, testfixtures.PNG), `,"labels":{"batch":"oct"}`)
	if scan["state"] != "succeeded" || scan["labels"].(map[string]any)["batch"] != "oct" {
		t.Fatalf("the parse of an image ended %v", scan)
	}
	// Describing figures and extraction are not built over the task store
	// and answer 501.
	for _, route := range []string{"POST /parses/" + scan["id"].(string) + "/figures", "GET /parses/" + scan["id"].(string) + "/fields"} {
		method, path, _ := strings.Cut(route, " ")
		if status, body, _ := call(t, method, base+"/v1"+path, token(), nil); status != http.StatusNotImplemented || body["error"].(map[string]any)["code"] != "not_implemented" {
			t.Fatalf("%s: %d %v", route, status, body)
		}
	}
	// The meters hold the 4 pages the 2 parses read, each with one call of
	// the stub, the queue holds no group once both ended, a parse that
	// ended tells its pages and its end as events, and one with no failed
	// page has nothing to read again.
	if got := meter(t, base, token(), "stub"); !strings.HasPrefix(got, "4/4/") {
		t.Fatalf("the meters of the 2 parses are %s", got)
	}
	if status, view, raw := call(t, "GET", base+"/v1/queue", token(), nil); status != http.StatusOK || len(view["groups"].([]any)) != 1 ||
		view["groups"].([]any)[0].(map[string]any)["parses"] != 0.0 || len(view["pools"].([]any)) != 1 {
		t.Fatalf("the queue with every parse ended: %d %s", status, raw)
	}
	if events := follow(t, base, pdf["id"].(string), "").rest(); len(events) != 5 || len(pagesIn(t, events)) != 3 || events[4].data["state"] != "succeeded" {
		t.Fatalf("the events of a parse that ended: %v", events)
	}
	if status, body, raw := call(t, "POST", at+"/retry", token(), nil); status != http.StatusConflict || code(body) != "conflict" {
		t.Fatalf("a retry of a parse with every page read: %d %s", status, raw)
	}

	if err := stop(); err != nil {
		t.Fatalf("a stop that was asked for is not a failure: %v", err)
	}
	log := logs.String()
	for _, want := range []string{`"msg":"listening"`, `"role":"all"`, "the worker is registered", "the worker stopped", `"msg":"stopped"`, `"callers":"oidc"`, `"decisions":"owner policy"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not say %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "lost when the process stops") || strings.Contains(log, p.dsn) || strings.Contains(log, s3test.Secret+`"`) {
		t.Fatalf("the log of a durable server holds what it must not:\n%s", log)
	}
	// The worker gave its registration back when it stopped.
	conn := p.connect()
	var workers int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM workers`).Scan(&workers); err != nil || workers != 0 {
		t.Fatalf("%d workers are registered after the stop, %v", workers, err)
	}
}

// TestTheRolesRunAsTwoServers: the API role serves the contract and runs no
// task, the worker role runs the tasks and serves no public route, and a
// parse submitted to the first completes on the second. A worker is ready
// once it holds a registration.
func TestTheRolesRunAsTwoServers(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	base, apiLogs, _ := started(t, env(p.env("api")...))
	status, _, raw := call(t, "POST", base+"/v1/files?name=sample.csv", token(), testfixtures.Read(t, testfixtures.CSV), "Content-Type", "text/csv")
	if status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, raw)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	status, parse, raw := call(t, "POST", base+"/v1/parses", token(), []byte(`{"source":{"file":"`+file["id"].(string)+`"}}`), "Prefer", "wait=1")
	if status != http.StatusAccepted || parse["state"] != "queued" {
		t.Fatalf("with no worker a submit answers %d %s", status, raw)
	}
	if strings.Contains(apiLogs.String(), "the worker is registered") {
		t.Fatal("the api role registered a worker")
	}

	probes, workerLogs, stop := started(t, env(p.env("worker")...))
	if probes != internalOf(t, workerLogs) {
		t.Fatalf("a worker's one listener is its probes: %q and %q", probes, internalOf(t, workerLogs))
	}
	if status, _ := get(t, probes+"/v1/readers"); status != http.StatusNotFound {
		t.Fatalf("a worker answered a public route with %d", status)
	}
	until(t, "a worker that registered is ready", time.Minute, func() bool {
		status, _ := get(t, probes+"/readyz")
		return status == http.StatusOK
	})
	if done := ended(t, base, parse["id"].(string)); done["state"] != "succeeded" {
		t.Fatalf("the parse the worker ran ended %v", done)
	}
	if status, _, md := call(t, "GET", base+"/v1/parses/"+parse["id"].(string)+"/document?format=markdown", token(), nil); status != http.StatusOK || !strings.Contains(string(md), "| Invoice A | 100.50 | EUR |") {
		t.Fatalf("its document: %d %q", status, md)
	}
	if err := stop(); err != nil {
		t.Fatalf("stopping the worker: %v", err)
	}
}

// TestTheDurableServerDoesNotStartOnWhatItCannotRun: each thing the durable
// server needs and does not have is an error that names it, and nothing of
// the error is a secret.
func TestTheDurableServerDoesNotStartOnWhatItCannotRun(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	// A schema a newer binary wrote is one this one does not open.
	newer := &plane{t: t, dsn: testservers.Database(t, mustPostgres(t)), objects: p.objects}
	if err := postgres.Migrate(context.Background(), newer.dsn); err != nil {
		t.Fatal(err)
	}
	conn := newer.connect()
	if _, err := conn.Exec(context.Background(), `UPDATE schema_migrations SET version = 9999`); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		env  []string
		want string
	}{
		"no bucket":                    {p.env("all", "LECTIO_BUCKET", ""), "LECTIO_BUCKET is not set"},
		"no secret key":                {p.env("all", "LECTIO_S3_SECRET_KEY", ""), "LECTIO_S3_SECRET_KEY is not set"},
		"a role that is not one":       {p.env("leader"), "LECTIO_ROLE"},
		"a database that is not there": {p.env("api", "LECTIO_DATABASE_URL", "postgres://lectio:lectio@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"), "store:"},
		"a schema of a newer binary":   {newer.env("all"), "version 9999"},
		"a worker with no schema yet":  {newer.env("worker"), "context deadline exceeded"},
		"an internal address taken":    {p.env("all", "LECTIO_INTERNAL_ADDR", taken.Addr().String()), "LECTIO_INTERNAL_ADDR"},
		"a public address taken":       {p.env("api", "LECTIO_ADDR", taken.Addr().String()), "address already in use"},
	} {
		// Each case fails as soon as the server reaches what it lacks,
		// which on a loaded machine is after it opened its store, so each
		// is given a minute. The worker that waits for a schema fails by
		// running out of time, and is given a second.
		within := time.Minute
		if name == "a worker with no schema yet" {
			within = time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), within)
		err := serve(ctx, nil, env(tc.env...), io.Discard, io.Discard, nil)
		cancel()
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "lectio:lectio") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

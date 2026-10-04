// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/testfixtures"
)

func env(pairs ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(name string) string { return m[name] }
}

// buffer is a log the test reads while the server writes it.
type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// started runs the server and returns its base URL, its log, and a stop
// that ends it and returns what it returned.
func started(t *testing.T, getenv func(string) string) (base string, logs *buffer, stop func() error) {
	t.Helper()
	logs = &buffer{}
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan string, 1), make(chan error, 1)
	go func() { done <- serve(ctx, nil, getenv, io.Discard, logs, ready) }()
	select {
	case addr := <-ready:
		base = "http://" + addr
	case err := <-done:
		cancel()
		t.Fatalf("the server did not start: %v", err)
	}
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() { cancel(); err = <-done })
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return base, logs, stop
}

// call makes a request with the dev token and decodes a JSON answer.
func call(t *testing.T, method, url, token string, body []byte, headers ...string) (int, map[string]any, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded, raw
}

// TestTheDevServerParsesAFile is the path a new user takes: start the
// server with nothing configured, upload a file, parse it, read it.
func TestTheDevServerParsesAFile(t *testing.T) {
	base, logs, stop := started(t, env("LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0", "LECTIO_MODEL_KEY", "sk-never-logged"))

	status, file, raw := call(t, "POST", base+"/v1/files?name=sample.csv", "dev", testfixtures.Read(t, testfixtures.CSV), "Content-Type", "text/csv")
	if status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, raw)
	}
	status, parse, raw := call(t, "POST", base+"/v1/parses", "dev", []byte(`{"source":{"file":"`+file["id"].(string)+`"}}`), "Prefer", "wait=20")
	if status != http.StatusOK || parse["state"] != "succeeded" {
		t.Fatalf("parse: %d %s", status, raw)
	}
	status, _, md := call(t, "GET", base+"/v1/parses/"+parse["id"].(string)+"/document?format=markdown", "dev", nil)
	if status != http.StatusOK || !strings.Contains(string(md), "| Invoice A | 100.50 | EUR |") {
		t.Fatalf("document: %d %q", status, md)
	}
	// A scan goes to the configured reader, here the stub.
	scan := image.NewGray(image.Rect(0, 0, 40, 40))
	for i := range scan.Pix {
		scan.Pix[i] = byte(255 * (i / 40 % 2))
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, scan); err != nil {
		t.Fatal(err)
	}
	_, file, _ = call(t, "POST", base+"/v1/files?name=scan.png", "dev", encoded.Bytes(), "Content-Type", "image/png")
	status, parse, raw = call(t, "POST", base+"/v1/parses", "dev", []byte(`{"source":{"file":"`+file["id"].(string)+`"}}`), "Prefer", "wait=20")
	if status != http.StatusOK || parse["state"] != "succeeded" {
		t.Fatalf("parse of a scan: %d %s", status, raw)
	}
	status, page, raw := call(t, "GET", base+"/v1/parses/"+parse["id"].(string)+"/pages/1", "dev", nil)
	if status != http.StatusOK || page["reader"] != "stub" || len(page["blocks"].([]any)) != 3 {
		t.Fatalf("its page: %d %s", status, raw)
	}

	// With nothing configured the stub describes figures too: the request
	// is taken, and this page holds no figure to describe.
	status, figures, raw := call(t, "POST", base+"/v1/parses/"+parse["id"].(string)+"/figures", "dev", nil, "Prefer", "wait=20")
	if run, _ := figures["run"].(map[string]any); status != http.StatusOK || run["state"] != "succeeded" || run["total"] != 0.0 {
		t.Fatalf("figures: %d %s", status, raw)
	}

	status, readers, raw := call(t, "GET", base+"/v1/readers", "dev", nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"name":"stub"`) || len(readers["readers"].([]any)) != 1 {
		t.Fatalf("readers: %d %s", status, raw)
	}
	if status, _, _ := call(t, "GET", base+"/v1/readers", "other", nil); status != http.StatusUnauthorized {
		t.Fatalf("another token: %d", status)
	}

	if err := stop(); err != nil {
		t.Fatalf("a stop that was asked for is not a failure: %v", err)
	}
	log := logs.String()
	for _, want := range []string{"lost when the process stops", "stub reader", `"msg":"listening"`, `"msg":"stopped"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not say %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "sk-never-logged") {
		t.Fatalf("the log holds the model key:\n%s", log)
	}
}

// parsed uploads a fixture, parses it, and returns the parse as it ended.
func parsed(t *testing.T, base, fixture, name string) map[string]any {
	t.Helper()
	status, file, raw := call(t, "POST", base+"/v1/files?name="+name, "dev", testfixtures.Read(t, fixture))
	if status != http.StatusCreated {
		t.Fatalf("upload of %s: %d %s", name, status, raw)
	}
	status, parse, raw := call(t, "POST", base+"/v1/parses", "dev", []byte(`{"source":{"file":"`+file["id"].(string)+`"}}`), "Prefer", "wait=30")
	// A parse that outlasts the wait is answered 202 and read until it
	// ends: a loaded machine under the race detector takes longer than
	// one wait to convert and render a file.
	for deadline := time.Now().Add(5 * time.Minute); status == http.StatusAccepted && time.Now().Before(deadline); {
		time.Sleep(200 * time.Millisecond)
		if status, parse, raw = call(t, "GET", base+"/v1/parses/"+parse["id"].(string), "dev", nil); status != http.StatusOK {
			break
		}
		if state := parse["state"]; state == "queued" || state == "running" {
			status = http.StatusAccepted
		}
	}
	if status != http.StatusOK {
		t.Fatalf("parse of %s: %d %s", name, status, raw)
	}
	return parse
}

// TestTheDevServerReadsOfficeDocuments takes a Word document and a workbook
// through the API: each is read from the file itself, with no model.
func TestTheDevServerReadsOfficeDocuments(t *testing.T) {
	base, _, _ := started(t, env("LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0"))

	report := parsed(t, base, testfixtures.ReportDOCX, "report.docx")
	if report["state"] != "succeeded" {
		t.Fatalf("parse of a Word document: %v", report)
	}
	status, _, md := call(t, "GET", base+"/v1/parses/"+report["id"].(string)+"/document?format=markdown", "dev", nil)
	for _, want := range []string{"# Field station report", "## Readings", "- Reset the float.", `<th colspan="2">Rain (mm), measured and corrected</th>`, `<td rowspan="2">Team A</td>`, "Gauges were read at 08:00 local time."} {
		if status != http.StatusOK || !strings.Contains(string(md), want) {
			t.Errorf("the document does not hold %q: %d\n%s", want, status, md)
		}
	}
	status, page, raw := call(t, "GET", base+"/v1/parses/"+report["id"].(string)+"/pages/1", "dev", nil)
	if status != http.StatusOK || page["source"] != "native" || page["reader"] != nil || len(page["blocks"].([]any)) != 15 {
		t.Fatalf("its page: %d %s", status, raw)
	}

	ledger := parsed(t, base, testfixtures.LedgerXLSX, "ledger.xlsx")
	if progress := ledger["progress"].(map[string]any); ledger["state"] != "succeeded" || progress["pages_total"] != 3.0 {
		t.Fatalf("parse of a workbook: %v", ledger)
	}
	status, _, md = call(t, "GET", base+"/v1/parses/"+ledger["id"].(string)+"/document?format=markdown&pages=1", "dev", nil)
	for _, want := range []string{"# Rainfall", "2026-03-02", "28.6%", "Total of March"} {
		if status != http.StatusOK || !strings.Contains(string(md), want) {
			t.Errorf("the first sheet does not hold %q: %d\n%s", want, status, md)
		}
	}

	// A legacy spreadsheet is refused where it is uploaded.
	status, body, raw := call(t, "POST", base+"/v1/files?name=ledger.xls", "dev", []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1 a compound file"))
	if status != http.StatusUnsupportedMediaType || body["error"].(map[string]any)["code"] != "unsupported_media_type" || !strings.Contains(string(raw), ".xlsx") {
		t.Fatalf("upload of a legacy spreadsheet: %d %s", status, raw)
	}
}

// TestTheDevServerConvertsThroughAConverter stands a converter up beside
// the server: a presentation is sent to it, and its answer, a PDF, is what
// the pages are read from. A server with no converter refuses the same
// file when the parse is prepared.
func TestTheDevServerConvertsThroughAConverter(t *testing.T) {
	var asked []string
	var mu sync.Mutex
	converter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type")+" to "+r.Header.Get("Accept"))
		mu.Unlock()
		w.Header().Set("Content-Type", r.Header.Get("Accept"))
		_, _ = w.Write(testfixtures.Read(t, testfixtures.MultipagePDF))
	}))
	t.Cleanup(converter.Close)

	base, _, _ := started(t, env("LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0", "LECTIO_CONVERTER_URL", converter.URL))
	deck := parsed(t, base, testfixtures.PPTX, "deck.pptx")
	if progress := deck["progress"].(map[string]any); deck["state"] != "succeeded" || progress["pages_total"] != 3.0 {
		t.Fatalf("parse of a presentation: %v", deck)
	}
	status, page, raw := call(t, "GET", base+"/v1/parses/"+deck["id"].(string)+"/pages/3", "dev", nil)
	if status != http.StatusOK || page["source"] != "reader" || page["reader"] != "stub" {
		t.Fatalf("a page of the conversion: %d %s", status, raw)
	}
	mu.Lock()
	want := []string{"POST /v1/convert application/vnd.openxmlformats-officedocument.presentationml.presentation to application/pdf"}
	if !slices.Equal(asked, want) {
		t.Fatalf("the converter was asked %q, want %q", asked, want)
	}
	mu.Unlock()

	bare, logs, _ := started(t, env("LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0"))
	refused := parsed(t, bare, testfixtures.PPTX, "deck.pptx")
	if failure, _ := refused["error"].(map[string]any); refused["state"] != "failed" || failure["code"] != "unsupported_media_type" || refused["progress"].(map[string]any)["pages_total"] != 0.0 {
		t.Fatalf("a presentation with no converter: %v", refused)
	}
	if !strings.Contains(logs.String(), "LECTIO_CONVERTER_URL is not set") {
		t.Fatalf("the log does not say that nothing converts:\n%s", logs.String())
	}
}

func TestTheServerIsConfiguredByItsEnvironment(t *testing.T) {
	dir := t.TempDir()
	config := `
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: offline }
spec: { adapter: stub, maxInFlight: 4 }
`
	if err := os.WriteFile(filepath.Join(dir, "readers.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	base, logs, _ := started(t, env(
		"LECTIO_DEV", "1", "LECTIO_ADDR", "127.0.0.1:0", "LECTIO_BASE_PATH", "/api/parsing",
		"LECTIO_DEV_TOKEN", "t0", "LECTIO_CONFIG", dir, "LECTIO_MAX_FILE_BYTES", "64",
	))
	status, _, raw := call(t, "GET", base+"/api/parsing/readers", "t0", nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"name":"offline"`) {
		t.Fatalf("readers: %d %s", status, raw)
	}
	if status, _, _ := call(t, "GET", base+"/v1/readers", "t0", nil); status != http.StatusNotFound {
		t.Fatalf("the default base path, with another set: %d", status)
	}
	if status, body, _ := call(t, "POST", base+"/api/parsing/files", "t0", make([]byte, 100), "Content-Type", "text/plain"); status != http.StatusRequestEntityTooLarge || body["error"].(map[string]any)["code"] != "file_too_large" {
		t.Fatalf("a file over the limit: %d %v", status, body)
	}
	if log := logs.String(); !strings.Contains(log, "does not apply") || strings.Contains(log, "stub reader") {
		t.Fatalf("the log names what is set and not applied, and no stub reader:\n%s", log)
	}
}

func TestTheServerDoesNotStartOnWhatItCannotRun(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })

	for name, tc := range map[string]struct {
		args   []string
		getenv func(string) string
		want   string
	}{
		"no dev mode and no database":       {nil, env(), "LECTIO_DATABASE_URL is not set"},
		"no dev mode, with a database":      {nil, env("LECTIO_DATABASE_URL", "postgres://db/lectio"), "not built yet"},
		"a setting that does not parse":     {nil, env("LECTIO_DEV", "true", "LECTIO_WORKERS", "many"), "LECTIO_WORKERS"},
		"a configuration that is not there": {nil, env("LECTIO_DEV", "true", "LECTIO_CONFIG", "/nonexistent/lectio"), "config:"},
		"an address that is taken":          {nil, env("LECTIO_DEV", "true", "LECTIO_ADDR", taken.Addr().String()), "address already in use"},
		"an argument":                       {[]string{"serve"}, env("LECTIO_DEV", "true"), `unknown argument "serve"`},
		"a converter it cannot reach":       {nil, env("LECTIO_DEV", "true", "LECTIO_CONVERTER_URL", "converter:8090"), "LECTIO_CONVERTER_URL"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := serve(ctx, tc.args, tc.getenv, io.Discard, io.Discard, nil)
		cancel()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}

	var out bytes.Buffer
	if err := serve(context.Background(), []string{"--version"}, env(), &out, io.Discard, nil); err != nil || !strings.HasPrefix(out.String(), "lectiod ") {
		t.Fatalf("version: %q, %v", out.String(), err)
	}
}

// TestAStopLetsARequestFinish stops the server while an upload is still
// sending its body and expects the upload to be answered.
func TestAStopLetsARequestFinish(t *testing.T) {
	base, _, stop := started(t, env("LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0", "LECTIO_SHUTDOWN_GRACE", "10s"))
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	head := "POST /v1/files?name=note.txt HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer dev\r\n" +
		"Content-Type: text/plain\r\nContent-Length: 10\r\nConnection: close\r\n\r\nhello"
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatal(err)
	}
	// The handler is now reading the body. A request whose header had not
	// arrived yet would be dropped by the stop, so the header gets a moment.
	time.Sleep(200 * time.Millisecond)
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-stopped:
		t.Fatalf("the server stopped with a request open: %v", err)
	default:
	}
	if _, err := conn.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	answer, err := io.ReadAll(conn)
	if !strings.HasPrefix(string(answer), "HTTP/1.1 201") {
		t.Fatalf("the open request was answered with %q (%v)", answer, err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestMainRunsTheCommand(t *testing.T) {
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	os.Args = []string{"lectiod", "version"}
	main()
}

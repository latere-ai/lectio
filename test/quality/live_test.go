// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package quality_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/quality"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/internal/testservers"
)

// TestMain removes the containers a live run started. The gate's tests
// start none.
func TestMain(m *testing.M) { testservers.Main(m) }

// TestLiveQuality reads the quality corpus with a configured reader and
// scores what comes back. It runs the server as it ships: the test builds
// lectiod, starts it as a process, and uploads, parses and reads every file
// over the API, so a number in its report is a number a caller would get.
// It calls a model, so it runs only when asked:
//
//	LECTIO_LIVE_CONFIG=reader.yaml LECTIO_LIVE_OUT=out/quality make live-quality
//
// LECTIO_LIVE_CONFIG is a file or a directory of Reader and Policy
// documents, as LECTIO_CONFIG takes them, and LECTIO_MODEL_KEY the key the
// reader's endpoint takes. LECTIO_LIVE_OUT is the directory the report, the
// pages and the server's log are written to. LECTIO_LIVE_CONVERTER is the
// address of a running conversion sidecar; without it the files that need
// conversion are left out. LECTIO_LIVE_FILES names files of the corpus,
// separated by commas, to run only those.
//
// The server is the durable one, in the role all, against a Postgres and
// an object store this test starts in containers. LECTIO_LIVE_SERVER=dev
// runs the development server instead, which needs no container runtime.
//
// The test fails when a file misses a bar of its class or its parse does
// not succeed. Nothing it writes holds the key.
func TestLiveQuality(t *testing.T) {
	config, out := os.Getenv("LECTIO_LIVE_CONFIG"), os.Getenv("LECTIO_LIVE_OUT")
	if config == "" {
		t.Skip("set LECTIO_LIVE_CONFIG to read the quality corpus with a real reader")
	}
	if out == "" {
		t.Fatal("LECTIO_LIVE_OUT names no directory for the report")
	}
	for _, path := range []*string{&config, &out} {
		abs, err := filepath.Abs(*path)
		if err != nil {
			t.Fatal(err)
		}
		*path = abs
	}
	if _, err := os.Stat(config); err != nil {
		t.Fatalf("LECTIO_LIVE_CONFIG: %v", err)
	}
	if err := os.MkdirAll(out, 0o750); err != nil {
		t.Fatal(err)
	}
	key, converterURL := os.Getenv("LECTIO_MODEL_KEY"), os.Getenv("LECTIO_LIVE_CONVERTER")
	only := strings.FieldsFunc(os.Getenv("LECTIO_LIVE_FILES"), func(r rune) bool { return r == ',' || r == ' ' })
	for _, name := range only {
		if !slices.ContainsFunc(corpus, func(f file) bool { return f.name == name }) {
			t.Fatalf("LECTIO_LIVE_FILES names %s, which is no file of the corpus", name)
		}
	}

	srv := startServer(t, out, config, key, converterURL)
	report := quality.NewReport(time.Now().UTC().Format(time.RFC3339), srv.readers(t), srv.kind)
	for _, f := range corpus {
		if len(only) > 0 && !slices.Contains(only, f.name) {
			report.Skipped = append(report.Skipped, "`"+f.name+"`: LECTIO_LIVE_FILES does not name it")
			continue
		}
		if f.detected != f.working && converterURL == "" {
			report.Skipped = append(report.Skipped, "`"+f.name+"`: it needs conversion, and LECTIO_LIVE_CONVERTER names no sidecar")
			continue
		}
		res := srv.run(t, f, filepath.Join(out, f.name))
		report.Add(res)
		last := report.Files[len(report.Files)-1]
		t.Logf("%s: %d pages, %d of %d blocks matched, CER %.2f%%, kinds %.1f%%, in %.1fs: %s",
			f.name, last.Scores.Pages, last.Scores.Matched, last.Scores.Blocks, 100*last.Scores.CER, 100*last.Scores.Kinds, last.Seconds, verdict(last))
	}

	raw, err := report.JSON()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(out, "report.json"), raw)
	writeFile(t, filepath.Join(out, "report.md"), []byte(report.Markdown()))
	t.Logf("the report is in %s\n%s", out, report.Markdown())

	// The server is stopped before its log is read, so the log is whole.
	srv.stop(t)
	if key != "" {
		for _, name := range []string{"report.json", "report.md", "server.log"} {
			written, err := os.ReadFile(filepath.Join(out, name))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(written, []byte(key)) {
				t.Errorf("%s holds the model key", name)
			}
		}
	}
	for _, why := range report.Failed() {
		t.Errorf("under its bars: %s", why)
	}
}

func verdict(r quality.Result) string {
	switch {
	case r.Error != "":
		return "failed: " + r.Error
	case len(r.Misses) > 0:
		return fmt.Sprintf("missed %v", r.Misses)
	}
	return "met"
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// server is a lectiod process of the run.
type server struct {
	// kind says which server it is, for the report.
	kind  string
	base  string
	token string
	cmd   *exec.Cmd
	done  chan error
}

// startServer builds lectiod and starts it with the reader configuration
// of the run. Its log goes to server.log in the output directory.
func startServer(t *testing.T, out, config, key, converterURL string) *server {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "lectiod")
	build := exec.CommandContext(context.Background(), "go", "build", "-o", bin, "../../cmd/lectiod")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building lectiod: %v\n%s", err, output)
	}

	srv := &server{token: rand.Text(), done: make(chan error, 1)}
	env := map[string]string{
		"LECTIO_ADDR": "127.0.0.1:0", "LECTIO_CONFIG": config, "LECTIO_MODEL_KEY": key,
		"LECTIO_CONVERTER_URL": converterURL, "LECTIO_DEV_TOKEN": srv.token,
		// 2 pages are read at once: enough to show that pages are read side
		// by side, and few enough for an engine that reads one at a time.
		"LECTIO_WORKERS": "2",
	}
	switch mode := os.Getenv("LECTIO_LIVE_SERVER"); mode {
	case "", "durable":
		srv.kind = "durable, role all, Postgres and an S3 object store in containers"
		db, err := testservers.StartPostgres()
		if err != nil {
			t.Fatalf("no container runtime answered for Postgres; LECTIO_LIVE_SERVER=dev runs without one: %v", err)
		}
		objects, err := testservers.StartObjects()
		if err != nil {
			t.Fatalf("no container runtime answered for the object store; LECTIO_LIVE_SERVER=dev runs without one: %v", err)
		}
		maps.Copy(env, map[string]string{
			"LECTIO_ROLE": "all", "LECTIO_DATABASE_URL": testservers.Database(t, db), "LECTIO_INTERNAL_ADDR": "127.0.0.1:0",
			"LECTIO_BUCKET": objects.Bucket, "LECTIO_S3_ENDPOINT": objects.Endpoint, "LECTIO_S3_REGION": objects.Region,
			"LECTIO_S3_ACCESS_KEY": objects.AccessKey, "LECTIO_S3_SECRET_KEY": objects.SecretKey, "LECTIO_S3_PATH_STYLE": "true",
		})
	case "dev":
		srv.kind = "development, in memory"
		env["LECTIO_DEV"] = "true"
	default:
		t.Fatalf("LECTIO_LIVE_SERVER is %q, and it is durable or dev", mode)
	}

	logPath := filepath.Join(out, "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	srv.cmd = exec.CommandContext(context.Background(), bin)
	// The server is configured by what this run sets and by nothing a
	// shell happened to export.
	for _, pair := range os.Environ() {
		if !strings.HasPrefix(pair, "LECTIO_") {
			srv.cmd.Env = append(srv.cmd.Env, pair)
		}
	}
	for name, value := range env {
		if value != "" {
			srv.cmd.Env = append(srv.cmd.Env, name+"="+value)
		}
	}
	srv.cmd.Stdout, srv.cmd.Stderr = logFile, logFile
	if err := srv.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { srv.done <- errors.Join(srv.cmd.Wait(), logFile.Close()) }()
	t.Cleanup(func() { srv.stop(t) })

	// The server logs the address it listens on once it does.
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(100 * time.Millisecond) {
		logged, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.SplitSeq(string(logged), "\n") {
			var entry struct{ Msg, Addr string }
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "listening" && entry.Addr != "" {
				srv.base = "http://" + entry.Addr + "/v1"
				return srv
			}
		}
		select {
		case err := <-srv.done:
			srv.done <- err
			t.Fatalf("lectiod stopped before it listened: %v\n%s", err, logged)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("lectiod did not listen in 2 minutes:\n%s", logged)
		}
	}
}

// stop ends the server as a deployment does, with a signal it handles, and
// waits for it. A second call does nothing.
func (s *server) stop(t *testing.T) {
	t.Helper()
	if s.cmd == nil {
		return
	}
	cmd := s.cmd
	s.cmd = nil
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("stopping lectiod: %v", err)
	}
	select {
	case err := <-s.done:
		if err != nil {
			t.Errorf("lectiod ended with %v", err)
		}
	case <-time.After(time.Minute):
		t.Errorf("lectiod did not stop in a minute: %v", cmd.Process.Kill())
	}
}

// call makes a request of the API and returns the status and the body.
func (s *server) call(t *testing.T, method, path string, body []byte, headers ...string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, s.base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err := errors.Join(err, resp.Body.Close()); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

// readers names the readers the server reads pages with, in the order of
// its chain.
func (s *server) readers(t *testing.T) string {
	t.Helper()
	status, raw := s.call(t, http.MethodGet, "/readers", nil)
	var listed struct {
		Readers []struct {
			Name string `json:"name"`
		} `json:"readers"`
	}
	if err := json.Unmarshal(raw, &listed); status != http.StatusOK || err != nil || len(listed.Readers) == 0 {
		t.Fatalf("the server's readers: %d %s", status, raw)
	}
	names := make([]string, 0, len(listed.Readers))
	for _, r := range listed.Readers {
		names = append(names, r.Name)
	}
	return strings.Join(names, ", ")
}

// parsed is a parse as the API returns it, as far as the run reads it.
type parsed struct {
	ID    string          `json:"id"`
	State string          `json:"state"`
	Error *document.Error `json:"error"`
}

// run takes one file through the API: it uploads it, parses it with no
// page taken from an earlier read, waits for the parse, reads every page
// back, and scores the pages against the file's truth. The pages, the
// images the reader was given and the document as Markdown are written to
// dir, for whoever looks at a miss.
func (s *server) run(t *testing.T, f file, dir string) quality.Result {
	t.Helper()
	res := quality.Result{File: f.name, Format: f.format, Class: f.class}
	truth := truthOf(t, f)
	res.Scores.Pages = len(truth.Pages)

	status, raw := s.call(t, http.MethodPost, "/files?name="+url.QueryEscape(f.name), testfixtures.Read(t, f.fixture), "Content-Type", "application/octet-stream")
	var uploaded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &uploaded); (status != http.StatusCreated && status != http.StatusOK) || err != nil {
		res.Error = fmt.Sprintf("the upload was answered %d: %s", status, raw)
		return res
	}
	submit, err := json.Marshal(map[string]any{"source": map[string]string{"file": uploaded.ID}, "reuse": false})
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	status, raw = s.call(t, http.MethodPost, "/parses", submit, "Content-Type", "application/json")
	var parse parsed
	if err := json.Unmarshal(raw, &parse); (status != http.StatusAccepted && status != http.StatusOK) || err != nil {
		res.Error = fmt.Sprintf("the submit was answered %d: %s", status, raw)
		return res
	}
	at := "/parses/" + parse.ID
	for parse.State == "queued" || parse.State == "running" {
		if time.Since(began) > 30*time.Minute {
			res.Error = "the parse did not end in 30 minutes"
			return res
		}
		time.Sleep(250 * time.Millisecond)
		if status, raw = s.call(t, http.MethodGet, at, nil); status != http.StatusOK || json.Unmarshal(raw, &parse) != nil {
			res.Error = fmt.Sprintf("the parse was answered %d: %s", status, raw)
			return res
		}
	}
	res.Seconds = time.Since(began).Seconds()
	if parse.State != "succeeded" {
		res.Error = "the parse ended " + parse.State
		if parse.Error != nil {
			res.Error += ": " + parse.Error.Code + ", " + parse.Error.Detail
		}
		return res
	}

	pages := make([]document.Page, 0, len(truth.Pages))
	for n := 1; n <= len(truth.Pages); n++ {
		status, raw := s.call(t, http.MethodGet, at+"/pages/"+strconv.Itoa(n), nil)
		var page document.Page
		if err := json.Unmarshal(raw, &page); status != http.StatusOK || err != nil {
			res.Error = fmt.Sprintf("page %d was answered %d: %s", n, status, raw)
			return res
		}
		writeFile(t, filepath.Join(dir, "page-"+strconv.Itoa(n)+".json"), raw)
		if page.Usage != nil {
			res.InputTokens += page.Usage.InputTokens
			res.OutputTokens += page.Usage.OutputTokens
		}
		if res.Model == "" {
			res.Model = page.Model
		}
		pages = append(pages, page)
		// A page a reader read has the image the reader was given.
		if status, img := s.call(t, http.MethodGet, at+"/pages/"+strconv.Itoa(n)+"/image", nil); status == http.StatusOK {
			ext := ".png"
			if bytes.HasPrefix(img, []byte("\xff\xd8")) {
				ext = ".jpg"
			}
			writeFile(t, filepath.Join(dir, "page-"+strconv.Itoa(n)+ext), img)
		}
	}
	if status, md := s.call(t, http.MethodGet, at+"/document?format=markdown", nil); status == http.StatusOK {
		writeFile(t, filepath.Join(dir, "document.md"), md)
	}

	res.Scores = quality.Score(truth, pages)
	// What assembly made of the pages is not a measure with a bar. It is
	// said beside the scores, since a table that is not joined or a
	// heading at another level is a difference a caller sees.
	status, raw = s.call(t, http.MethodGet, at+"/document", nil)
	var doc document.Document
	if err := json.Unmarshal(raw, &doc); status != http.StatusOK || err != nil {
		res.Error = fmt.Sprintf("the document was answered %d: %s", status, raw)
		return res
	}
	res.Scores.Notes = append(res.Scores.Notes, assembled(truth, doc)...)
	return res
}

// assembled says where a document's spans and outline differ from the
// truth's.
func assembled(truth quality.Truth, doc document.Document) []string {
	var notes []string
	for _, want := range truth.Spans {
		if !slices.ContainsFunc(doc.Spans, func(s document.Span) bool { return slices.Equal(s.Parts, want.Parts) }) {
			notes = append(notes, fmt.Sprintf("assembly: the table of %s is not joined into one span; the document's spans are %v", strings.Join(want.Parts, " and "), doc.Spans))
		}
	}
	levels := map[string]int{}
	for _, h := range doc.Outline {
		levels[strings.Join(strings.Fields(h.Text), " ")] = h.Level
	}
	for _, want := range truth.Outline {
		switch level, ok := levels[want.Text]; {
		case !ok:
			notes = append(notes, fmt.Sprintf("assembly: the outline lacks %q", want.Text))
		case level != want.Level:
			notes = append(notes, fmt.Sprintf("assembly: %q is at level %d of the outline, and the truth has it at %d", want.Text, level, want.Level))
		}
	}
	return notes
}

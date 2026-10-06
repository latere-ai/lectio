// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/api"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/fetch"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// drawn renders every page of an image file as an image that is not
// blank, so a file of several pages is read without a real renderer. Any
// other file goes to the real one.
type drawn struct{}

func (drawn) Render(ctx context.Context, data []byte, mediaType string, n int, want reader.Description) (render.Image, error) {
	if mediaType != "image/tiff" {
		return render.Images{}.Render(ctx, data, mediaType, n, want)
	}
	return render.Image{Data: []byte{byte(n)}, MediaType: "image/png", Width: 100, Height: 100}, nil
}

// sheet is a white PNG with a dark bar across it.
func sheet(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 60, 40))
	for y := range 40 {
		for x := range 60 {
			c := color.RGBA{255, 255, 255, 255}
			if y > 13 && y < 26 {
				c = color.RGBA{0, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// over is the durable backend the cases run over, when a case of
// durable_test.go set one. Nil runs them over the memory store and the
// in-process runner.
var over *durableBench

// env is a server over a backend that runs parses.
type env struct {
	t      *testing.T
	server *Server
	url    string
	token  string
}

// serve starts a server. change edits it and its runner before they start.
func serve(t *testing.T, change func(*Server, *run.Runner)) *env {
	t.Helper()
	rd := &stub.Reader{}
	st := store.NewMemory()
	runner := &run.Runner{
		Store: st, Pipeline: &parse.Pipeline{Limits: pages.DefaultLimits(), Renderer: drawn{}},
		Readers: map[string]reader.Reader{"stub": rd}, Chain: []string{"stub"},
		Backoff: func(int) time.Duration { return 0 },
	}
	s := &Server{
		Backend: &Memory{Store: st, Runner: runner}, Auth: callers, Authz: ownerPolicy(),
		Readers: runner.Readers, Chain: runner.Chain, Limits: pages.DefaultLimits(),
		Log: slog.New(slog.DiscardHandler),
	}
	if change != nil {
		change(s, runner)
		s.Readers, s.Chain = runner.Readers, runner.Chain
	}
	ctx, cancel := context.WithCancel(context.Background())
	if over != nil {
		// The same server over the durable backend: what the case set on the
		// runner is what the worker is run with.
		stop := over.start(ctx, t, s, runner)
		srv := httptest.NewServer(s.Handler())
		t.Cleanup(func() { srv.Close(); cancel(); stop() })
		return &env{t: t, server: s, url: srv.URL, token: "alice-token"}
	}
	runner.Start(ctx)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.Close(); cancel(); runner.Wait() })
	return &env{t: t, server: s, url: srv.URL, token: "alice-token"}
}

// as returns the same server seen by another caller.
func (e *env) as(token string) *env {
	other := *e
	other.token = token
	return &other
}

// reply is a response, read.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// json decodes the body.
func (r reply) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("%d %s: %v", r.status, r.body, err)
	}
	return out
}

// code is the error code of an error response.
func (r reply) code(t *testing.T) string {
	t.Helper()
	code, _ := at(r.json(t), "error", "code").(string)
	return code
}

// do makes a request below the base path and holds the answer to the
// contract. body is JSON for a string, the bytes themselves for a []byte,
// and nothing for nil. headers are name, value pairs.
func (e *env) do(method, path string, body any, headers ...string) reply {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.url+"/v1"+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	if _, isJSON := body.(string); isJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	route, _, _ := strings.Cut(path, "?")
	held(e.t, method, route, resp.StatusCode, resp.Header, raw)
	return reply{resp.StatusCode, resp.Header, raw}
}

// upload stores a file and returns its id.
func (e *env) upload(name string, data []byte) string {
	e.t.Helper()
	r := e.do("POST", "/files?name="+name, data, "Content-Type", "application/octet-stream")
	if r.status != http.StatusCreated && r.status != http.StatusOK {
		e.t.Fatalf("upload %s: %d %s", name, r.status, r.body)
	}
	return r.json(e.t)["id"].(string)
}

// parsed submits a parse of a file, waits for it, and returns it.
func (e *env) parsed(file string, options string) map[string]any {
	e.t.Helper()
	r := e.do("POST", "/parses", `{"source":{"file":"`+file+`"}`+options+`}`, "Prefer", "wait=20")
	if r.status != http.StatusOK {
		e.t.Fatalf("submit: %d %s", r.status, r.body)
	}
	return r.json(e.t)
}

// ended waits for a parse to end and returns it.
func (e *env) ended(id string) map[string]any {
	e.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		p := e.do("GET", "/parses/"+id, nil).json(e.t)
		if state := p["state"]; state != "queued" && state != "running" {
			return p
		}
	}
	e.t.Fatalf("parse %s did not end", id)
	return nil
}

func TestAFileIsParsedAndRead(t *testing.T) {
	e := serve(t, nil)
	scan := sheet(t)

	// The same bytes are one file.
	up := e.do("POST", "/files?name=../scans/scan.png", scan, "Content-Type", "application/octet-stream")
	if up.status != http.StatusCreated {
		t.Fatalf("upload: %d %s", up.status, up.body)
	}
	file := up.json(t)
	if file["name"] != "scan.png" || file["media_type"] != "image/png" || file["size"] != float64(len(scan)) || len(file["sha256"].(string)) != 64 {
		t.Fatalf("file: %v", file)
	}
	id := file["id"].(string)
	if again := e.do("POST", "/files", scan, "Content-Type", "image/png"); again.status != http.StatusOK || again.json(t)["id"] != id {
		t.Fatalf("the same bytes again: %d %s", again.status, again.body)
	}
	if got := e.do("GET", "/files/"+id, nil); got.status != http.StatusOK || got.json(t)["sha256"] != file["sha256"] {
		t.Fatalf("read the file: %d %s", got.status, got.body)
	}

	p := e.parsed(id, `,"labels":{"batch":"oct"},"origin":{"store":"s3","path":"scans/scan.png"},"languages":["de"]`)
	pid := p["id"].(string)
	if p["state"] != "succeeded" || p["class"] != "interactive" || p["file"] != id || p["owner"] != "alice" {
		t.Fatalf("parse: %v", p)
	}
	if prog := p["progress"].(map[string]any); prog["stage"] != "done" || prog["pages_total"] != 1.0 || prog["pages_done"] != 1.0 || prog["pages_failed"] != 0.0 {
		t.Fatalf("progress: %v", prog)
	}
	if at(p, "usage", "pages") != 1.0 || p["started_at"] == nil || p["finished_at"] == nil || at(p, "origin", "path") != "scans/scan.png" {
		t.Fatalf("parse: %v", p)
	}

	// The result, whole and one object at a time.
	if got := e.do("GET", "/parses/"+pid, nil).json(t); got["state"] != "succeeded" {
		t.Fatalf("read the parse: %v", got)
	}
	list := e.do("GET", "/parses/"+pid+"/pages", nil).json(t)["pages"].([]any)
	if len(list) != 1 || at(list[0], "state") != "succeeded" || at(list[0], "blocks") != 3.0 {
		t.Fatalf("pages: %v", list)
	}
	page := e.do("GET", "/parses/"+pid+"/pages/1", nil).json(t)
	if blocks := page["blocks"].([]any); len(blocks) != 3 || at(blocks[1], "ref") != "1.2" || page["reader"] != "stub" || page["source"] != "reader" {
		t.Fatalf("page: %v", page)
	}
	block := e.do("GET", "/parses/"+pid+"/blocks/1.2", nil).json(t)
	if block["kind"] != "text" || !strings.Contains(block["text"].(string), "sha256") || len(block["box"].([]any)) != 4 {
		t.Fatalf("block: %v", block)
	}
	// The image is the one the reader saw: here, the upload itself.
	if img := e.do("GET", "/parses/"+pid+"/pages/1/image", nil); img.status != http.StatusOK || !bytes.Equal(img.body, scan) || img.header.Get("Content-Type") != "image/png" {
		t.Fatalf("image: %d, %d bytes of %s", img.status, len(img.body), img.header.Get("Content-Type"))
	}
	doc := e.do("GET", "/parses/"+pid+"/document", nil).json(t)
	if doc["parse"] != pid || len(doc["pages"].([]any)) != 1 || at(doc, "usage", "pages") != 1.0 {
		t.Fatalf("document: %v", doc)
	}
	if md := e.do("GET", "/parses/"+pid+"/document?format=markdown", nil); md.status != http.StatusOK || !strings.HasPrefix(md.header.Get("Content-Type"), "text/markdown") || !strings.Contains(string(md.body), "# Page 1") {
		t.Fatalf("markdown: %d %q", md.status, md.body)
	}
	if text := e.do("GET", "/parses/"+pid+"/document?format=text", nil); text.status != http.StatusOK || !strings.HasPrefix(text.header.Get("Content-Type"), "text/plain") || !strings.Contains(string(text.body), "Page 1") {
		t.Fatalf("text: %d %q", text.status, text.body)
	}
	chunks := e.do("GET", "/parses/"+pid+"/chunks", nil)
	if chunks.status != http.StatusOK || chunks.header.Get("Content-Type") != "application/x-ndjson" || !strings.Contains(string(chunks.body), `"blocks":["1.1"`) {
		t.Fatalf("chunks: %d %q", chunks.status, chunks.body)
	}

	// The same file again is a parse of its own, with its own labels. What
	// is not repeated is the reading: the page is taken from the earlier
	// read and no model is called, unless the caller says not.
	again := e.parsed(id, `,"languages":["de"],"class":"batch","labels":{"batch":"nov"}`)
	if again["id"] == pid || at(again, "labels", "batch") != "nov" || at(again, "progress", "pages_reused") != 1.0 || at(again, "usage", "input_tokens") != nil {
		t.Fatalf("a second parse of the same file: %v", again)
	}
	if reused := e.do("GET", "/parses/"+again["id"].(string)+"/pages/1", nil).json(t); reused["reused"] != true || len(reused["blocks"].([]any)) != 3 {
		t.Fatalf("its page: %v", reused)
	}
	if fresh := e.parsed(id, `,"languages":["de"],"reuse":false`); at(fresh, "progress", "pages_reused") != nil || at(fresh, "usage", "input_tokens") == nil {
		t.Fatalf("no reuse: %v", fresh)
	}
	if other := e.parsed(id, `,"languages":["en"]`); at(other, "progress", "pages_reused") != nil {
		t.Fatalf("other hints are another read: %v", other)
	}
	if pinned := e.parsed(id, `,"reader":"stub"`); pinned["id"] == pid || pinned["reader"] != "stub" {
		t.Fatalf("a named reader: %v", pinned)
	}

	// Every block of a parse in one read, one per line.
	bulk := e.do("GET", "/parses/"+pid+"/blocks", nil)
	if lines := strings.Split(strings.TrimSpace(string(bulk.body)), "\n"); bulk.status != http.StatusOK || bulk.header.Get("Content-Type") != "application/x-ndjson" || len(lines) != 3 || !strings.Contains(lines[1], `"ref":"1.2"`) {
		t.Fatalf("blocks: %d %q", bulk.status, bulk.body)
	}
	if none := e.do("GET", "/parses/"+pid+"/blocks?pages=1", nil); len(strings.Split(strings.TrimSpace(string(none.body)), "\n")) != 3 {
		t.Fatalf("blocks of page 1: %q", none.body)
	}
	if bad := e.do("GET", "/parses/"+pid+"/blocks?pages=9", nil); bad.status != http.StatusBadRequest || bad.code(t) != "invalid_pages" {
		t.Fatalf("blocks of a page that is not there: %d %s", bad.status, bad.body)
	}

	// Deleting a parse deletes what it wrote; the file goes when no parse
	// that has not ended reads it.
	if del := e.do("DELETE", "/parses/"+pid, nil); del.status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", del.status, del.body)
	}
	for _, path := range []string{"", "/pages", "/pages/1", "/pages/1/image", "/blocks", "/blocks/1.1", "/document", "/chunks"} {
		if got := e.do("GET", "/parses/"+pid+path, nil); got.status != http.StatusNotFound || got.code(t) != "parse_not_found" {
			t.Errorf("%s after the delete: %d %s", path, got.status, got.body)
		}
	}
	if del := e.do("DELETE", "/parses/"+pid, nil); del.status != http.StatusNotFound {
		t.Fatalf("delete again: %d %s", del.status, del.body)
	}
	if del := e.do("DELETE", "/files/"+id, nil); del.status != http.StatusNoContent {
		t.Fatalf("delete the file: %d %s", del.status, del.body)
	}
	if got := e.do("GET", "/files/"+id, nil); got.status != http.StatusNotFound || got.code(t) != "file_not_found" {
		t.Fatalf("the file after the delete: %d %s", got.status, got.body)
	}
}

func TestANativeFileIsParsedWithNoReader(t *testing.T) {
	e := serve(t, func(_ *Server, r *run.Runner) { r.Readers, r.Chain = nil, nil })

	// Uploaded as a multipart form, named by its part.
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	if err := mw.WriteField("note", "ignored"); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file", "sample.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(testfixtures.Read(t, testfixtures.CSV)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	up := e.do("POST", "/files", form.Bytes(), "Content-Type", mw.FormDataContentType())
	if up.status != http.StatusCreated || up.json(t)["name"] != "sample.csv" || up.json(t)["media_type"] != "text/csv" {
		t.Fatalf("upload: %d %s", up.status, up.body)
	}

	p := e.parsed(up.json(t)["id"].(string), "")
	pid := p["id"].(string)
	if p["state"] != "succeeded" {
		t.Fatalf("parse: %v", p)
	}
	page := e.do("GET", "/parses/"+pid+"/pages/1", nil).json(t)
	if blocks := page["blocks"].([]any); page["source"] != "native" || len(blocks) != 1 || at(blocks[0], "kind") != "table" || at(blocks[0], "box") != nil {
		t.Fatalf("page: %v", page)
	}
	if md := e.do("GET", "/parses/"+pid+"/document?format=markdown&tables=html", nil); !strings.Contains(string(md.body), "<table>") {
		t.Fatalf("a table as html: %q", md.body)
	}
	// A page that was not read from an image has none.
	if img := e.do("GET", "/parses/"+pid+"/pages/1/image", nil); img.status != http.StatusNotFound || img.code(t) != "page_not_found" {
		t.Fatalf("image of a native page: %d %s", img.status, img.body)
	}
	if got := e.do("GET", "/readers", nil).json(t)["readers"].([]any); len(got) != 0 {
		t.Fatalf("readers: %v", got)
	}
}

// gated is a server whose reader stops on one page until it is released.
func gated(t *testing.T, page int) (e *env, reached, release chan struct{}) {
	reached, release = make(chan struct{}), make(chan struct{})
	e = serve(t, func(_ *Server, r *run.Runner) {
		r.Workers = 1
		r.Readers = map[string]reader.Reader{"stub": &stub.Reader{Fail: func(p reader.Page) error {
			if p.Number == page {
				close(reached)
				<-release
			}
			return nil
		}}}
	})
	return e, reached, release
}

func TestAParseIsReadWhileItRunsAndCanBeCanceled(t *testing.T) {
	e, reached, release := gated(t, 3)
	file := e.upload("scan.tiff", testfixtures.Read(t, testfixtures.MultiTIFF))
	sub := e.do("POST", "/parses", `{"source":{"file":"`+file+`"}}`)
	if sub.status != http.StatusAccepted {
		t.Fatalf("submit: %d %s", sub.status, sub.body)
	}
	pid := sub.json(t)["id"].(string)
	<-reached

	p := e.do("GET", "/parses/"+pid, nil).json(t)
	if p["state"] != "running" || at(p, "progress", "stage") != "reading" || at(p, "progress", "pages_done") != 2.0 {
		t.Fatalf("mid-run: %v", p)
	}
	list := e.do("GET", "/parses/"+pid+"/pages", nil).json(t)["pages"].([]any)
	if len(list) != 3 || at(list[1], "state") != "succeeded" || at(list[2], "state") != "pending" {
		t.Fatalf("pages mid-run: %v", list)
	}
	if got := e.do("GET", "/parses/"+pid+"/pages/2", nil); got.status != http.StatusOK {
		t.Fatalf("a page that was read: %d %s", got.status, got.body)
	}
	for path, want := range map[string]string{
		"/pages/3": "page_not_ready", "/pages/3/image": "page_not_ready", "/blocks/3.1": "page_not_ready", "/blocks/3.1/image": "page_not_ready",
		"/document": "document_not_ready", "/document?format=markdown": "document_not_ready", "/chunks": "document_not_ready",
	} {
		got := e.do("GET", "/parses/"+pid+path, nil)
		if got.status != http.StatusConflict || got.code(t) != want || at(got.json(t), "error", "details", "retryable") != true {
			t.Errorf("%s mid-run: %d %s", path, got.status, got.body)
		}
	}
	if got := e.do("GET", "/parses/"+pid+"/pages/9", nil); got.status != http.StatusNotFound || got.code(t) != "page_not_found" {
		t.Fatalf("a page the parse does not read: %d %s", got.status, got.body)
	}
	// The bulk read answers while the parse runs, with what was read.
	if mid := e.do("GET", "/parses/"+pid+"/blocks", nil); mid.status != http.StatusOK || strings.Count(string(mid.body), "\n") != 6 {
		t.Fatalf("blocks mid-run: %d %q", mid.status, mid.body)
	}
	if del := e.do("DELETE", "/parses/"+pid, nil); del.status != http.StatusConflict || del.code(t) != "not_terminal" {
		t.Fatalf("delete mid-run: %d %s", del.status, del.body)
	}
	if del := e.do("DELETE", "/files/"+file, nil); del.status != http.StatusConflict || del.code(t) != "not_terminal" {
		t.Fatalf("delete the file mid-run: %d %s", del.status, del.body)
	}

	if c := e.do("POST", "/parses/"+pid+"/cancel", nil); c.status != http.StatusOK {
		t.Fatalf("cancel: %d %s", c.status, c.body)
	}
	close(release)
	if p := e.ended(pid); p["state"] != "canceled" || p["error"] != nil {
		t.Fatalf("after the cancel: %v", p)
	}
	if c := e.do("POST", "/parses/"+pid+"/cancel", nil); c.status != http.StatusConflict || c.code(t) != "already_terminal" {
		t.Fatalf("cancel again: %d %s", c.status, c.body)
	}
	// What was read stays, as pages and as a document. What was not read
	// is recorded as skipped: nothing went wrong with it.
	if got := e.do("GET", "/parses/"+pid+"/pages/2", nil); got.status != http.StatusOK || got.json(t)["state"] != "succeeded" {
		t.Fatalf("a page read before the cancel: %d %s", got.status, got.body)
	}
	if got := e.do("GET", "/parses/"+pid+"/pages/3", nil); got.status != http.StatusOK || got.json(t)["state"] != "skipped" || got.json(t)["error"] != nil {
		t.Fatalf("a page not read before the cancel: %d %s", got.status, got.body)
	}
	list = e.do("GET", "/parses/"+pid+"/pages", nil).json(t)["pages"].([]any)
	if len(list) != 3 || at(list[1], "state") != "succeeded" || at(list[2], "state") != "skipped" {
		t.Fatalf("pages after the cancel: %v", list)
	}
	if md := e.do("GET", "/parses/"+pid+"/document?format=markdown", nil); md.status != http.StatusOK || !strings.Contains(string(md.body), "# Page 2") || strings.Contains(string(md.body), "# Page 3") {
		t.Fatalf("the document of a canceled parse: %d %q", md.status, md.body)
	}
	if blocks := e.do("GET", "/parses/"+pid+"/blocks", nil); blocks.status != http.StatusOK || !strings.Contains(string(blocks.body), `"ref":"2.1"`) {
		t.Fatalf("its blocks: %d %q", blocks.status, blocks.body)
	}
	if c := e.do("POST", "/parses/prs_none/cancel", nil); c.status != http.StatusNotFound {
		t.Fatalf("cancel of nothing: %d %s", c.status, c.body)
	}
}

func TestASubmitCanBeHeldUntilTheParseEnds(t *testing.T) {
	e, reached, release := gated(t, 1)
	file := e.upload("scan.png", sheet(t))
	// The wait runs out with the parse still running.
	sub := e.do("POST", "/parses", `{"source":{"file":"`+file+`"}}`, "Prefer", "respond-async, wait=1")
	<-reached
	if sub.status != http.StatusAccepted || sub.header.Get("Preference-Applied") != "wait=1" || sub.json(t)["state"] != "running" {
		t.Fatalf("held: %d %s", sub.status, sub.body)
	}
	close(release)
	e.ended(sub.json(t)["id"].(string))

	for header, want := range map[string]time.Duration{
		"wait=5": 5 * time.Second, "wait=600": maxWait * time.Second, " respond-async , wait=2 ": 2 * time.Second,
		"wait=0": 0, "wait=-3": 0, "wait=soon": 0, "handling=lenient": 0, "": 0,
	} {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("Prefer", header)
		if got := wait(r); got != want {
			t.Errorf("Prefer: %q waits %v, want %v", header, got, want)
		}
	}
}

func TestAFailedPageFailsTheParseUnlessAllowed(t *testing.T) {
	e := serve(t, func(_ *Server, r *run.Runner) {
		r.Readers = map[string]reader.Reader{"stub": &stub.Reader{Fail: func(p reader.Page) error {
			if p.Number == 2 {
				return reader.Errorf(reader.Permanent, "no")
			}
			return nil
		}}}
	})
	file := e.upload("scan.tiff", testfixtures.Read(t, testfixtures.MultiTIFF))

	strict := e.parsed(file, "")
	if strict["state"] != "failed" || at(strict, "error", "code") != "page_unreadable" || at(strict, "progress", "pages_failed") != 1.0 {
		t.Fatalf("no failed page allowed: %v", strict)
	}
	lenient := e.parsed(file, `,"allow_failed_pages":1`)
	if lenient["state"] != "succeeded" || lenient["allow_failed_pages"] != 1.0 || at(lenient, "progress", "pages_done") != 2.0 {
		t.Fatalf("one failed page allowed: %v", lenient)
	}

	// Either way the pages that were read are there, and the failed one
	// says why.
	for _, p := range []map[string]any{strict, lenient} {
		pid := p["id"].(string)
		failed := e.do("GET", "/parses/"+pid+"/pages/2", nil).json(t)
		if failed["state"] != "failed" || at(failed, "error", "code") != "page_unreadable" || len(failed["blocks"].([]any)) != 0 {
			t.Fatalf("the failed page: %v", failed)
		}
		doc := e.do("GET", "/parses/"+pid+"/document", nil).json(t)
		if at(doc["pages"].([]any)[1], "error", "code") != "page_unreadable" {
			t.Fatalf("the document: %v", doc)
		}
		if md := e.do("GET", "/parses/"+pid+"/document?format=markdown&page_breaks=true&repeated=keep", nil); !strings.Contains(string(md.body), "<!-- page 3 -->\n\n## Page 3") || strings.Contains(string(md.body), "Page 2") {
			t.Fatalf("markdown: %q", md.body)
		}
	}

	// A file no reader can be shown fails each page with the reason.
	pdf := e.parsed(e.upload("report.pdf", testfixtures.Read(t, testfixtures.MultipagePDF)), "")
	if pdf["state"] != "failed" || at(pdf, "progress", "pages_failed") != 3.0 {
		t.Fatalf("a pdf with no renderer: %v", pdf)
	}
	if page := e.do("GET", "/parses/"+pdf["id"].(string)+"/pages/1", nil).json(t); at(page, "error", "code") != "unsupported_media_type" {
		t.Fatalf("its page: %v", page)
	}
}

func TestADocumentIsRenderedAsItIsRead(t *testing.T) {
	e := serve(t, nil)
	pid := e.parsed(e.upload("scan.tiff", testfixtures.Read(t, testfixtures.MultiTIFF)), "")["id"].(string)
	read := func(query string) string {
		t.Helper()
		got := e.do("GET", "/parses/"+pid+"/document?format=markdown"+query, nil)
		if got.status != http.StatusOK {
			t.Fatalf("%s: %d %s", query, got.status, got.body)
		}
		return string(got.body)
	}

	// A page's number is furniture: it is printed only when everything is.
	numbers := func(md string) (n int) {
		for line := range strings.SplitSeq(md, "\n") {
			if line == "1" || line == "2" || line == "3" {
				n++
			}
		}
		return n
	}
	if once := read(""); strings.Count(once, "# Page ") != 3 || numbers(once) != 0 {
		t.Fatalf("the default rendering: %q", once)
	}
	if keep := read("&repeated=keep"); numbers(keep) != 3 {
		t.Fatalf("everything kept: %q", keep)
	}
	if drop := read("&repeated=drop"); strings.Count(drop, "# Page ") != 3 || numbers(drop) != 0 {
		t.Fatalf("furniture dropped: %q", drop)
	}
	if two := read("&pages=2"); !strings.Contains(two, "# Page 2") || strings.Contains(two, "# Page 1") {
		t.Fatalf("one page: %q", two)
	}

	lines := func(query string) int {
		t.Helper()
		got := e.do("GET", "/parses/"+pid+"/chunks"+query, nil)
		if got.status != http.StatusOK {
			t.Fatalf("%s: %d %s", query, got.status, got.body)
		}
		return len(strings.Split(strings.TrimSpace(string(got.body)), "\n"))
	}
	if n := lines("?by=page&max_chars=200"); n != 3 {
		t.Fatalf("by page: %d chunks", n)
	}
	if n := lines("?by=section"); n != 3 {
		t.Fatalf("by section, one title a page: %d chunks", n)
	}

	for query, field := range map[string]string{
		"/document?format=xml":            "format",
		"/document?tables=csv":            "tables",
		"/document?repeated=twice":        "repeated",
		"/document?page_breaks=maybe":     "page_breaks",
		"/chunks?by=sentence":             "by",
		"/chunks?max_chars=10":            "max_chars",
		"/chunks?max_chars=many":          "max_chars",
		"/pages/0":                        "page",
		"/pages/one":                      "page",
		"/pages/x/image":                  "page",
		"/blocks/first":                   "ref",
		"/document?format=text&pages=3-1": "",
		"/document?format=text&pages=9":   "",
	} {
		got := e.do("GET", "/parses/"+pid+query, nil)
		wantCode := "invalid_request"
		if field == "" {
			wantCode = "invalid_pages"
		}
		if got.status != http.StatusBadRequest || got.code(t) != wantCode || (field != "" && at(got.json(t), "error", "details", "field") != field) {
			t.Errorf("%s: %d %s", query, got.status, got.body)
		}
	}
	if got := e.do("GET", "/parses/"+pid+"/blocks/1.99", nil); got.status != http.StatusNotFound || got.code(t) != "block_not_found" {
		t.Fatalf("a block that is not there: %d %s", got.status, got.body)
	}
}

func TestASubmitIsCheckedAgainstTheContract(t *testing.T) {
	e := serve(t, func(s *Server, _ *run.Runner) { s.MaxDeadline = 30 * time.Minute })
	file := e.upload("scan.png", sheet(t))
	src := `"source":{"file":"` + file + `"}`
	long := strings.Repeat("x", 300)
	var labels []string
	for i := range 17 {
		labels = append(labels, `"k`+string(rune('a'+i))+`":"v"`)
	}

	for name, tc := range map[string]struct {
		body, code, field string
		status            int
	}{
		"a member the contract does not name":  {`{` + src + `,"mode":"fast"}`, "unknown_field", "mode", 400},
		"one inside the source":                {`{"source":{"file":"` + file + `","path":"x"}}`, "unknown_field", "path", 400},
		"one inside the origin":                {`{` + src + `,"origin":{"bucket":"b"}}`, "unknown_field", "bucket", 400},
		"no source":                            {`{}`, "invalid_request", "source", 400},
		"a file and a url":                     {`{"source":{"file":"` + file + `","url":"https://example.com/a.pdf"}}`, "invalid_request", "source", 400},
		"a url with no fetcher":                {`{"source":{"url":"https://example.com/a.pdf"}}`, "invalid_request", "source.url", 400},
		"a file that is not there":             {`{"source":{"file":"fil_none"}}`, "file_not_found", "", 404},
		"a malformed selection":                {`{` + src + `,"pages":"3-1"}`, "invalid_pages", "", 400},
		"a reader that is not configured":      {`{` + src + `,"reader":"other"}`, "reader_not_found", "", 400},
		"a class that is not one":              {`{` + src + `,"class":"fast"}`, "invalid_request", "class", 400},
		"a negative allowance":                 {`{` + src + `,"allow_failed_pages":-1}`, "invalid_request", "allow_failed_pages", 400},
		"a deadline that is not a duration":    {`{` + src + `,"deadline":"soon"}`, "invalid_request", "deadline", 400},
		"a deadline of nothing":                {`{` + src + `,"deadline":"0s"}`, "invalid_request", "deadline", 400},
		"a deadline past the longest allowed":  {`{` + src + `,"deadline":"2h"}`, "invalid_request", "deadline", 400},
		"too many languages":                   {`{` + src + `,"languages":["a","b","c","d","e","f","g","h","i"]}`, "invalid_request", "languages", 400},
		"an empty language":                    {`{` + src + `,"languages":[""]}`, "invalid_request", "languages", 400},
		"too many labels":                      {`{` + src + `,"labels":{` + strings.Join(labels, ",") + `}}`, "invalid_request", "labels", 400},
		"a label that is too long":             {`{` + src + `,"labels":{"k":"` + long + `"}}`, "invalid_request", "labels", 400},
		"a label with no key":                  {`{` + src + `,"labels":{"":"v"}}`, "invalid_request", "labels", 400},
		"an origin that is too long":           {`{` + src + `,"origin":{"store":"` + long + `"}}`, "invalid_request", "origin", 400},
		"a member of the wrong type":           {`{` + src + `,"priority":"high"}`, "invalid_request", "priority", 400},
		"a body that is not JSON":              {`source=file`, "invalid_request", "", 400},
		"a body that is an array":              {`[]`, "invalid_request", "", 400},
		"two values":                           {`{` + src + `} {}`, "invalid_request", "", 400},
		"a body over the limit":                {`{` + src + `,"pages":"` + strings.Repeat("1,", maxBody/2) + `1"}`, "invalid_request", "", 400},
		"the same selection, written two ways": {`{` + src + `,"pages":" "}`, "", "", 202},
	} {
		got := e.do("POST", "/parses", tc.body)
		if tc.code == "" {
			if got.status != http.StatusAccepted && got.status != http.StatusOK {
				t.Errorf("%s: %d %s", name, got.status, got.body)
			}
			continue
		}
		detail, _ := at(got.json(t), "error", "details", "field").(string)
		if got.status != tc.status || got.code(t) != tc.code || detail != tc.field {
			t.Errorf("%s: %d %s", name, got.status, got.body)
		}
		if at(got.json(t), "error", "message") == "" || at(got.json(t), "error", "details", "reason") == nil {
			t.Errorf("%s: an error has a message and a reason: %s", name, got.body)
		}
	}
	if got := e.do("POST", "/parses", `{`+src+`}`, "Idempotency-Key", long); got.status != http.StatusBadRequest || at(got.json(t), "error", "details", "field") != "Idempotency-Key" {
		t.Errorf("a key that is too long: %d %s", got.status, got.body)
	}

	// A deadline within the limit is kept, as an instant.
	ok := e.do("POST", "/parses", `{`+src+`,"deadline":"10m","priority":3,"reuse":false}`, "Prefer", "wait=20")
	if ok.status != http.StatusOK || ok.json(t)["deadline_at"] == nil || ok.json(t)["priority"] != 3.0 {
		t.Fatalf("a deadline: %d %s", ok.status, ok.body)
	}
}

func TestASubmitIsSafeToRepeatWithAKey(t *testing.T) {
	e := serve(t, nil)
	file := e.upload("scan.png", sheet(t))
	body := `{"source":{"file":"` + file + `"},"reuse":false}`

	first := e.do("POST", "/parses", body, "Idempotency-Key", "k1", "Prefer", "wait=20")
	again := e.do("POST", "/parses", body, "Idempotency-Key", "k1")
	if first.status != http.StatusOK || again.status != http.StatusOK || first.json(t)["id"] != again.json(t)["id"] {
		t.Fatalf("the same key and body: %d %s then %d %s", first.status, first.body, again.status, again.body)
	}
	other := e.do("POST", "/parses", `{"source":{"file":"`+file+`"},"reuse":false,"priority":1}`, "Idempotency-Key", "k1")
	if other.status != http.StatusConflict || other.code(t) != "idempotency_conflict" {
		t.Fatalf("the same key with another body: %d %s", other.status, other.body)
	}
	if fresh := e.do("POST", "/parses", body, "Idempotency-Key", "k2", "Prefer", "wait=20"); fresh.json(t)["id"] == first.json(t)["id"] {
		t.Fatal("another key is another parse")
	}
}

func TestParsesAreListedNewestFirst(t *testing.T) {
	e := serve(t, nil)
	file := e.upload("scan.png", sheet(t))
	var ids []string
	for _, options := range []string{
		`,"reuse":false,"labels":{"batch":"oct","kind":"invoice"},"origin":{"path":"a/1.png"}`,
		`,"reuse":false,"labels":{"batch":"oct"}`,
		`,"reuse":false`,
	} {
		ids = append(ids, e.parsed(file, options)["id"].(string))
	}
	listed := func(query string) (got []string, next string) {
		t.Helper()
		r := e.do("GET", "/parses"+query, nil)
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", query, r.status, r.body)
		}
		for _, p := range r.json(t)["parses"].([]any) {
			got = append(got, at(p, "id").(string))
		}
		next, _ = r.json(t)["next_cursor"].(string)
		return got, next
	}
	eq := func(what string, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: %v, want %v", what, got, want)
		}
	}

	all, next := listed("")
	eq("all", all, ids[2], ids[1], ids[0])
	if next != "" {
		t.Errorf("a whole list has no cursor: %q", next)
	}
	page, next := listed("?limit=2")
	eq("the first page", page, ids[2], ids[1])
	rest, next := listed("?limit=2&cursor=" + next)
	eq("the next page", rest, ids[0])
	if next != "" {
		t.Errorf("the last page has no cursor: %q", next)
	}
	byLabel, _ := listed("?label=batch%3Doct")
	eq("by a label", byLabel, ids[1], ids[0])
	byLabels, _ := listed("?label=batch%3Doct&label=kind%3Dinvoice")
	eq("by two labels", byLabels, ids[0])
	byPath, _ := listed("?origin.path=a/1.png")
	eq("by origin path", byPath, ids[0])
	byFile, _ := listed("?file=" + file + "&state=succeeded")
	eq("by file and state", byFile, ids[2], ids[1], ids[0])
	none, _ := listed("?state=failed")
	eq("none", none)
	if mine, _ := e.as("bob-token").do("GET", "/parses", nil).json(t)["parses"].([]any); len(mine) != 0 {
		t.Errorf("another caller sees %d of them", len(mine))
	}

	for query, field := range map[string]string{"?state=paused": "state", "?label=batch": "label", "?label==v": "label", "?limit=0": "limit", "?limit=201": "limit", "?limit=ten": "limit"} {
		if got := e.do("GET", "/parses"+query, nil); got.status != http.StatusBadRequest || at(got.json(t), "error", "details", "field") != field {
			t.Errorf("%s: %d %s", query, got.status, got.body)
		}
	}
}

func TestAFileIsFetchedFromAURL(t *testing.T) {
	scan := sheet(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/scans/scan.png":
			_, _ = w.Write(scan)
		case "/blob.bin":
			_, _ = w.Write([]byte{0, 1, 2, 3, 4, 5, 6, 7})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)
	e := serve(t, func(s *Server, _ *run.Runner) {
		s.Fetcher = &fetch.Fetcher{AllowHTTP: true, Permit: func(ap netip.AddrPort) bool { return strings.HasSuffix(origin.URL, ap.String()) }}
	})

	got := e.do("POST", "/parses", `{"source":{"url":"`+origin.URL+`/scans/scan.png?sig=abc"}}`, "Prefer", "wait=20")
	if got.status != http.StatusOK || got.json(t)["state"] != "succeeded" {
		t.Fatalf("a url: %d %s", got.status, got.body)
	}
	file := e.do("GET", "/files/"+got.json(t)["file"].(string), nil).json(t)
	if file["name"] != "scan.png" || file["media_type"] != "image/png" {
		t.Fatalf("the fetched file: %v", file)
	}

	for target, want := range map[string]struct {
		status int
		code   string
	}{
		origin.URL + "/gone":      {http.StatusUnprocessableEntity, "source_unreachable"},
		origin.URL + "/blob.bin":  {http.StatusUnsupportedMediaType, "unsupported_media_type"},
		"ftp://example.com/a.pdf": {http.StatusBadRequest, "invalid_request"},
	} {
		if got := e.do("POST", "/parses", `{"source":{"url":"`+target+`"}}`); got.status != want.status || got.code(t) != want.code {
			t.Errorf("%s: %d %s", target, got.status, got.body)
		}
	}

	// Without the test's permission, the server on loopback is refused.
	guarded := serve(t, func(s *Server, _ *run.Runner) { s.Fetcher = &fetch.Fetcher{AllowHTTP: true} })
	refused := guarded.do("POST", "/parses", `{"source":{"url":"`+origin.URL+`/scans/scan.png"}}`)
	if refused.status != http.StatusUnprocessableEntity || refused.code(t) != "source_unreachable" || strings.Contains(string(refused.body), "127.0.0.1") {
		t.Fatalf("a private address: %d %s", refused.status, refused.body)
	}
}

func TestUploadsAreChecked(t *testing.T) {
	e := serve(t, func(s *Server, _ *run.Runner) { s.Limits.MaxBytes = 4096 })
	for name, tc := range map[string]struct {
		path        string
		body        []byte
		contentType string
		status      int
		code        string
	}{
		"an empty body":              {"/files", []byte{}, "image/png", 400, "invalid_request"},
		"a file over the limit":      {"/files", make([]byte, 5000), "image/png", 413, "file_too_large"},
		"a type that is not read":    {"/files?name=blob.bin", []byte{0, 1, 2, 3, 4, 5, 6, 7}, "application/octet-stream", 415, "unsupported_media_type"},
		"a name that is too long":    {"/files?name=" + strings.Repeat("n", 300), sheet(t), "image/png", 400, "invalid_request"},
		"a form with no file part":   {"/files", []byte("--b\r\nContent-Disposition: form-data; name=\"note\"\r\n\r\nx\r\n--b--\r\n"), "multipart/form-data; boundary=b", 400, "invalid_request"},
		"a form with no boundary":    {"/files", []byte("x"), "multipart/form-data", 400, "invalid_request"},
		"a name that is only a path": {"/files?name=/", sheet(t), "image/png", 201, ""},
	} {
		got := e.do("POST", tc.path, tc.body, "Content-Type", tc.contentType)
		if got.status != tc.status || (tc.code != "" && got.code(t) != tc.code) {
			t.Errorf("%s: %d %s", name, got.status, got.body)
		}
		if tc.code == "" && got.json(t)["name"] != nil {
			t.Errorf("%s: %s", name, got.body)
		}
	}
}

func TestACallerIsKnownAndSeesOnlyItsOwn(t *testing.T) {
	e := serve(t, nil)
	file := e.upload("scan.png", sheet(t))
	pid := e.parsed(file, "")["id"].(string)

	for _, rt := range e.server.Routes() {
		path := strings.NewReplacer("{file}", file, "{parse}", pid, "{page}", "1", "{ref}", "1.1", "{name}", "invoice").Replace(rt.Path)
		// No operation answers a caller it does not know.
		if got := e.as("").do(rt.Method, path, nil); got.status != http.StatusUnauthorized || got.code(t) != "missing_token" {
			t.Errorf("%s %s with no token: %d %s", rt.Method, rt.Path, got.status, got.body)
		}
		if got := e.as("stolen").do(rt.Method, path, nil); got.status != http.StatusUnauthorized || got.code(t) != "invalid_token" {
			t.Errorf("%s %s with an unknown token: %d %s", rt.Method, rt.Path, got.status, got.body)
		}
		// A planned operation is there, and says it is not built.
		if rt.Planned {
			if got := e.do(rt.Method, path, nil); got.status != http.StatusNotImplemented || got.code(t) != "not_implemented" {
				t.Errorf("%s %s: %d %s", rt.Method, rt.Path, got.status, got.body)
			}
		}
		// Another caller's file or parse is not found, whatever is asked of
		// it, by an operation that is built and by one that is planned.
		if strings.Contains(rt.Path, "{") {
			if got := e.as("bob-token").do(rt.Method, path, nil); got.status != http.StatusNotFound {
				t.Errorf("%s %s as another caller: %d %s", rt.Method, rt.Path, got.status, got.body)
			}
		}
	}
	if got := e.as("bob-token").do("POST", "/parses", `{"source":{"file":"`+file+`"}}`); got.status != http.StatusNotFound || got.code(t) != "file_not_found" {
		t.Fatalf("a parse of another caller's file: %d %s", got.status, got.body)
	}

	// A scheme that is not Bearer is no token.
	req, _ := http.NewRequest("GET", e.url+"/v1/parses", nil)
	req.Header.Set("Authorization", "Basic YWxpY2U6eA==")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("basic auth: %d", resp.StatusCode)
	}
}

func TestReadersAreListedWithTheDefaultFirst(t *testing.T) {
	e := serve(t, func(_ *Server, r *run.Runner) {
		r.Readers = map[string]reader.Reader{"strong": &stub.Reader{}, "default": &stub.Reader{}, "spare": &stub.Reader{}}
		r.Chain = []string{"default", "strong", "gone"}
	})
	got := e.do("GET", "/readers", nil).json(t)["readers"].([]any)
	if len(got) != 3 || at(got[0], "name") != "default" || at(got[0], "default") != true || at(got[1], "name") != "strong" || at(got[2], "name") != "spare" || at(got[1], "default") != nil {
		t.Fatalf("readers: %v", got)
	}
	if at(got[0], "boxes") != true || at(got[0], "image", "format") != "png" || len(at(got[0], "accepts").([]any)) != 2 || at(got[0], "version") != "stub" {
		t.Fatalf("a reader: %v", got[0])
	}
}

// textReader is a stub that describes itself as a reader of a file's own
// text, which calls no model.
type textReader struct{ stub.Reader }

func (textReader) Describe() reader.Description {
	d := (&stub.Reader{}).Describe()
	d.Text, d.Local = true, true
	return d
}

// TestAReaderOfTheFilesOwnTextSaysSo: the listing marks a reader that reads
// the text its file carries and calls no model with text_layer, and leaves
// the member out of a reader of a model.
func TestAReaderOfTheFilesOwnTextSaysSo(t *testing.T) {
	e := serve(t, func(_ *Server, r *run.Runner) {
		r.Readers = map[string]reader.Reader{"text": &textReader{}, "model": &stub.Reader{}}
		r.Chain = []string{"text", "model"}
	})
	got := e.do("GET", "/readers", nil).json(t)["readers"].([]any)
	if len(got) != 2 || at(got[0], "name") != "text" || at(got[0], "text_layer") != true || at(got[1], "name") != "model" || at(got[1], "text_layer") != nil {
		t.Fatalf("readers: %v", got)
	}
}

func TestWhatIsNotRoutedAnswersInTheSameShape(t *testing.T) {
	e := serve(t, nil)
	if got := e.do("GET", "/nothing", nil); got.status != http.StatusNotFound || got.code(t) != "not_found" {
		t.Fatalf("an address that is not routed: %d %s", got.status, got.body)
	}
	if got := e.do("PUT", "/parses", nil); got.status != http.StatusMethodNotAllowed || got.code(t) != "method_not_allowed" || !strings.Contains(got.header.Get("Allow"), "POST") {
		t.Fatalf("a method that is not routed: %d %s (Allow: %s)", got.status, got.body, got.header.Get("Allow"))
	}
	if got := e.as("").do("GET", "/openapi.yaml", nil); got.status != http.StatusOK || !bytes.Equal(got.body, api.OpenAPI) {
		t.Fatalf("the contract: %d, %d bytes", got.status, len(got.body))
	}
	resp, err := http.Get(e.url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}

	// Mounted elsewhere, the same routes are there and not under /v1.
	s := &Server{Backend: &Memory{Store: store.NewMemory()}, Auth: tokens{"t": "alice"}, Authz: ownerPolicy(), BasePath: "/api/parsing/"}
	h := s.Handler()
	for path, want := range map[string]int{"/api/parsing/readers": http.StatusOK, "/v1/readers": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer t")
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
}

func TestAnErrorNobodyClassifiedIsInternalAndSaysNothing(t *testing.T) {
	var logged bytes.Buffer
	s := &Server{Log: slog.New(slog.NewTextHandler(&logged, nil))}
	for name, err := range map[string]error{
		"an error with no code":            errors.New("dial tcp 10.0.0.7:5432: connection refused"),
		"a code the API does not answer":   fault.New(fault.DocumentCorrupt, "the cross-reference table at 10.0.0.7 is broken"),
		"a classified error, for contrast": fault.New(fault.Conflict, "already there"),
	} {
		rec := httptest.NewRecorder()
		s.fail(rec, httptest.NewRequest("DELETE", "/v1/parses/prs_1", nil), err)
		internal := name != "a classified error, for contrast"
		if internal != (rec.Code == http.StatusInternalServerError) || internal == strings.Contains(rec.Body.String(), "reason") || strings.Contains(rec.Body.String(), "10.0.0.7") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
		held(t, "DELETE", "/parses/prs_1", rec.Code, rec.Header(), rec.Body.Bytes())
	}
	if !strings.Contains(logged.String(), "connection refused") || strings.Contains(logged.String(), "already there") {
		t.Fatalf("the log holds what the caller was not told, and only that: %s", logged.String())
	}

	// The defaults: the process clock and logger.
	if (&Server{}).log() != slog.Default() || time.Since((&Server{}).now()) > time.Minute {
		t.Fatal("a server with no clock or logger takes the process's")
	}
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	if got := (&Server{Now: func() time.Time { return fixed }}).now(); !got.Equal(fixed) || got.Location() != time.UTC {
		t.Fatalf("the clock, in UTC: %v", got)
	}
}

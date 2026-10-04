// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package quality_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/s3/s3test"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/assemble"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/quality"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/internal/testservers"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
	"latere.ai/x/lectio/reader/text"
)

// own is the reader that reads a page from the text its file carries, as
// a Reader document of the text adapter configures it.
func own(t testing.TB, image reader.ImageSpec) *text.Reader {
	t.Helper()
	r, err := text.New(text.Config{Name: "own", Image: image})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestTheTextReaderAloneReadsTheTypesetCorpus holds the reader that calls
// no model to the bars a model is held to, on the files of the corpus
// that carry their text: every page of the typeset survey is read from
// its text and scores within the bars of its class, and every page of the
// scanned survey, which holds no text, is declined for the next reader.
func TestTheTextReaderAloneReadsTheTypesetCorpus(t *testing.T) {
	bars, _ := quality.BarsOf(quality.Typeset)
	for _, f := range corpus {
		if f.working != detect.MIMEPDF || f.detected != f.working {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			truth := truthOf(t, f)
			p, got := prepared(t, f, "")
			r := own(t, reader.ImageSpec{DPI: 72})
			var read []document.Page
			declined := 0
			for _, n := range got.Manifest.Selected {
				page, err := p.ReadPage(context.Background(), got.Manifest, got.Working, n, r, parse.PageOptions{})
				switch {
				case reader.ClassOf(err) == reader.Refused && err != nil:
					declined++
					continue
				case err != nil:
					t.Fatalf("page %d: %v", n, err)
				}
				if blank := slices.Contains(f.blank, n); blank != page.Image.Blank || blank != (page.Page.Reader == "") {
					t.Errorf("page %d: blank %v, reader %q", n, page.Image.Blank, page.Page.Reader)
				} else if !blank && (page.Page.Source != document.SourceTextLayer || page.Page.Model != "" || *page.Page.Usage != (document.Usage{Pages: 1})) {
					t.Errorf("page %d was read from its text: source %s, model %q, usage %+v", n, page.Page.Source, page.Page.Model, page.Page.Usage)
				}
				read = append(read, page.Page)
			}
			if f.class == quality.Scan {
				// A scan holds a picture of each page and no word.
				if want := len(truth.Pages) - len(f.blank); declined != want || len(read) != len(f.blank) {
					t.Fatalf("%d pages were declined and %d read, want %d declined", declined, len(read), want)
				}
				return
			}
			if declined != 0 {
				t.Fatalf("%d pages of a file that carries its text were declined", declined)
			}
			doc := assemble.Document("gate", read)
			scores := quality.Score(truth, read)
			if misses := bars.Misses(scores); len(misses) > 0 {
				t.Errorf("under the bars of a typeset file: %v\n%s", misses, strings.Join(scores.Notes, "\n"))
			}
			// What the reader does not read: a formula is the characters
			// the file holds, and so is text and no formula.
			if scores.Matched != scores.Blocks-1 || scores.CellsRight != scores.CellsTotal || scores.PairsRight != scores.Pairs {
				t.Errorf("%d of %d blocks matched, %d of %d cells, %d of %d pairs in order\n%s", scores.Matched, scores.Blocks,
					scores.CellsRight, scores.CellsTotal, scores.PairsRight, scores.Pairs, strings.Join(scores.Notes, "\n"))
			}
			if len(doc.Spans) != len(truth.Spans) || len(doc.Outline) != len(truth.Outline) {
				t.Errorf("assembly finds %d spans and %d headings, the truth holds %d and %d", len(doc.Spans), len(doc.Outline), len(truth.Spans), len(truth.Outline))
			}
			t.Logf("CER %.2f%%, kinds %.1f%%, cells %.1f%%, order %.1f%%, boxes %.1f%%; %d of %d blocks matched",
				100*scores.CER, 100*scores.Kinds, 100**scores.Cells, 100*scores.Order, 100**scores.Boxes, scores.Matched, scores.Blocks)
		})
	}
}

// TestAChainReadsAPageWithTheFirstReaderThatCan runs a chain of the text
// reader and a counting stub in one process, over a logbook of 12 pages of
// which one is a picture. The stub is called for the picture and for no
// other page. A parse that names the text reader gets that reader alone,
// so its picture fails. A second parse takes what the first read, and one
// with a text reader of another version does not.
func TestAChainReadsAPageWithTheFirstReaderThatCan(t *testing.T) {
	const length, picture = 12, 5
	log := testfixtures.Logbook(length, picture)
	counted := &stub.Reader{}
	st := store.NewMemory()
	r := &run.Runner{
		Store: st, Pipeline: &parse.Pipeline{Limits: pages.DefaultLimits(), Renderer: engine},
		Readers: map[string]reader.Reader{"own": own(t, reader.ImageSpec{DPI: 36}), "stub": counted},
		Chain:   []string{"own", "stub"}, Workers: 4, Backoff: func(int) time.Duration { return 0 },
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	defer func() { cancel(); r.Wait() }()

	file, _ := st.PutFile(store.File{ID: "fil_log", Owner: "port", Name: "log.pdf", SHA256: "log", Data: log})
	parsed := func(p store.Parse) store.Parse {
		t.Helper()
		p.Owner, p.File, p.ContentSHA, p.State, p.Stage, p.Class = "port", file.ID, "log", store.StateQueued, store.StageQueued, store.ClassInteractive
		if _, _, err := st.CreateParse(p, "", ""); err != nil {
			t.Fatal(err)
		}
		r.Submit(p)
		select {
		case <-r.Done(p.ID):
		case <-time.After(5 * time.Minute):
			t.Fatalf("parse %s did not end", p.ID)
		}
		ended, err := st.Parse("port", p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return ended
	}
	calls := func() (total int) {
		for n := 1; n <= length; n++ {
			total += counted.Calls(n)
		}
		return total
	}

	first := parsed(store.Parse{ID: "prs_first", Reuse: true})
	if first.State != store.StateSucceeded || first.PagesDone != length || first.PagesFailed != 0 {
		t.Fatalf("the parse ended %s with %d pages read and %d failed: %+v", first.State, first.PagesDone, first.PagesFailed, first.Error)
	}
	if calls() != 1 || counted.Calls(picture) != 1 {
		t.Fatalf("the stub was called %d times, %d of them for the picture; want 1 call, for the picture", calls(), counted.Calls(picture))
	}
	for _, page := range st.Pages(first.ID) {
		heading, paragraphs := testfixtures.LogbookEntry(page.Number)
		switch {
		case page.Number == picture:
			if page.Source != document.SourceReader || page.Reader != "stub" || page.Model != stub.Name {
				t.Errorf("the picture was read by %q from %s", page.Reader, page.Source)
			}
		case page.Source != document.SourceTextLayer || page.Reader != "own" || page.Model != "" || *page.Usage != (document.Usage{Pages: 1}):
			t.Errorf("page %d was read by %q from %s, model %q, usage %+v", page.Number, page.Reader, page.Source, page.Model, page.Usage)
		case len(page.Blocks) != 4 || page.Blocks[0].Text != heading || page.Blocks[1].Text != paragraphs[0] || page.Blocks[2].Text != paragraphs[1]:
			t.Errorf("page %d reads %+v", page.Number, page.Blocks)
		}
	}

	// A parse that names its reader has a chain of one.
	pinned := parsed(store.Parse{ID: "prs_pinned", Reader: "own", AllowFailedPages: 1})
	failed, _ := st.Page(pinned.ID, picture)
	if pinned.State != store.StateSucceeded || pinned.PagesFailed != 1 || failed.State != document.PageFailed || failed.Error == nil || failed.Error.Code != "page_unreadable" || calls() != 1 {
		t.Fatalf("a parse that names the text reader: %s, %d failed, the picture %+v, %d calls", pinned.State, pinned.PagesFailed, failed.Error, calls())
	}

	// What was read is kept under a key that names the text reader's
	// version as it names any reader's.
	again := parsed(store.Parse{ID: "prs_again", Reuse: true})
	if again.State != store.StateSucceeded || again.PagesReused != length || calls() != 1 {
		t.Fatalf("a second parse took %d of %d pages from the first, with %d calls", again.PagesReused, length, calls())
	}
	r.Readers["own"] = own(t, reader.ImageSpec{DPI: 48})
	other := parsed(store.Parse{ID: "prs_other", Reuse: true})
	if other.State != store.StateSucceeded || other.PagesReused != 0 || calls() != 2 {
		t.Fatalf("a parse with a text reader of another version took %d pages from an earlier read, with %d calls", other.PagesReused, calls())
	}
}

// counting is a model endpoint that speaks chat completions, answers every
// call with one block, and counts its calls.
type counting struct {
	*httptest.Server
	calls atomic.Int64
}

func newCounting(t *testing.T) *counting {
	t.Helper()
	c := &counting{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("reading a call: %v", err)
		}
		c.calls.Add(1)
		content := `{"blocks":[{"kind":"figure","text":"","description":"A picture of a page, read by the model.","box":[100,100,900,900],"level":null}]}`
		if err := json.NewEncoder(w).Encode(map[string]any{
			"model":   "counted-model",
			"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": content}}},
			"usage":   map[string]any{"prompt_tokens": 1100, "completion_tokens": 26},
		}); err != nil {
			t.Errorf("writing a completion: %v", err)
		}
	}))
	t.Cleanup(c.Close)
	return c
}

// ended waits for a parse to end and returns it.
func (s *server) ended(t *testing.T, id string) parsed {
	t.Helper()
	var p parsed
	// A runner that shares few processors with other suites reads a page
	// in many times what a laptop takes.
	for deadline := time.Now().Add(15 * time.Minute); ; time.Sleep(100 * time.Millisecond) {
		status, raw := s.call(t, http.MethodGet, "/parses/"+id, nil)
		if err := json.Unmarshal(raw, &p); status != http.StatusOK || err != nil {
			t.Fatalf("the parse was answered %d: %s", status, raw)
		}
		if p.State != "queued" && p.State != "running" {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("parse %s did not end in 15 minutes: %s", id, raw)
		}
	}
}

// submit uploads a file once and starts a parse of it.
func (s *server) submit(t *testing.T, file string, options map[string]any) string {
	t.Helper()
	body := map[string]any{"source": map[string]string{"file": file}}
	maps.Copy(body, options)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	status, answer := s.call(t, http.MethodPost, "/parses", raw, "Content-Type", "application/json")
	var p parsed
	if err := json.Unmarshal(answer, &p); (status != http.StatusAccepted && status != http.StatusOK) || err != nil || p.ID == "" {
		t.Fatalf("the submit was answered %d: %s", status, answer)
	}
	return p.ID
}

func (s *server) upload(t *testing.T, name string, data []byte) string {
	t.Helper()
	status, raw := s.call(t, http.MethodPost, "/files?name="+url.QueryEscape(name), data, "Content-Type", "application/octet-stream")
	var uploaded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &uploaded); (status != http.StatusCreated && status != http.StatusOK) || err != nil || uploaded.ID == "" {
		t.Fatalf("the upload was answered %d: %s", status, raw)
	}
	return uploaded.ID
}

// pages reads the pages of a parse as the API returns them.
func (s *server) pages(t *testing.T, id string, count int) []document.Page {
	t.Helper()
	out := make([]document.Page, 0, count)
	for n := 1; n <= count; n++ {
		status, raw := s.call(t, http.MethodGet, "/parses/"+id+"/pages/"+strconv.Itoa(n), nil)
		var page document.Page
		if err := json.Unmarshal(raw, &page); status != http.StatusOK || err != nil {
			t.Fatalf("page %d was answered %d: %s", n, status, raw)
		}
		out = append(out, page)
	}
	return out
}

// TestAFileOf300PagesThatCarriesItsTextIsParsedWithNoModelCall is the
// first acceptance row of specs/017-agent-driven-parsing.md, through the
// server as it ships: lectiod in the role all over Postgres and an object
// store, with a chain of the text reader and a reader that calls a model
// endpoint which counts its calls. A logbook of 300 pages, one of them a
// picture, is parsed with one call: the one for the picture.
//
// The same server then reads the typeset survey of the corpus, which is
// scored as the API returns it against the bars of its class, a parse
// that names the text reader, and a parse that takes what the first read.
func TestAFileOf300PagesThatCarriesItsTextIsParsedWithNoModelCall(t *testing.T) {
	db, err := testservers.StartPostgres()
	if err != nil {
		t.Skipf("no container runtime answered, so the durable server did not run: %v", err)
	}
	const length, picture, bucket = 300, 150, "lectio"
	objects := s3test.New(t, bucket)
	model := newCounting(t)

	dir := t.TempDir()
	config := filepath.Join(dir, "readers.yaml")
	writeFile(t, config, fmt.Appendf(nil, `apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: own }
spec: { adapter: text, image: { dpi: 36 } }
---
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: vision }
spec: { adapter: chat, endpoint: %s/v1, model: counted-model, image: { dpi: 36, format: png } }
---
apiVersion: lectio.latere.ai/v1
kind: Policy
metadata: { name: default }
spec: { read: { chain: [own, vision] } }
`, model.URL))
	issuer, token := caller(t)
	srv := launch(t, lectiod(t), filepath.Join(dir, "server.log"), token, map[string]string{
		"LECTIO_ROLE": "all", "LECTIO_ADDR": "127.0.0.1:0", "LECTIO_INTERNAL_ADDR": "127.0.0.1:0",
		"LECTIO_CONFIG": config, "LECTIO_MODEL_KEY": "a-key-for-the-counting-endpoint", "LECTIO_OIDC_ISSUERS": issuer,
		"LECTIO_DATABASE_URL": testservers.Database(t, db),
		"LECTIO_BUCKET":       bucket, "LECTIO_S3_ENDPOINT": objects.URL(), "LECTIO_S3_REGION": s3test.Region,
		"LECTIO_S3_ACCESS_KEY": s3test.Key, "LECTIO_S3_SECRET_KEY": s3test.Secret, "LECTIO_S3_PATH_STYLE": "true",
		"LECTIO_WORKERS": "8", "LECTIO_WORKER_FLUSH": "20ms", "LECTIO_WORKER_POLL": "50ms",
	})
	defer func() {
		if t.Failed() {
			if logged, err := os.ReadFile(filepath.Join(dir, "server.log")); err == nil {
				t.Logf("the server's log:\n%s", logged[max(0, len(logged)-8000):])
			}
		}
	}()

	began := time.Now()
	log := srv.upload(t, "log.pdf", testfixtures.Logbook(length, picture))
	first := srv.ended(t, srv.submit(t, log, nil))
	took := time.Since(began)
	if first.State != "succeeded" {
		t.Fatalf("the parse ended %s: %+v", first.State, first.Error)
	}
	if got := model.calls.Load(); got != 1 {
		t.Fatalf("the model endpoint was called %d times for %d pages of which 1 holds no text; want 1 call", got, length)
	}
	for _, page := range srv.pages(t, first.ID, length) {
		heading, paragraphs := testfixtures.LogbookEntry(page.Number)
		switch {
		case page.State != document.PageSucceeded || page.Reused:
			t.Errorf("page %d is %s, reused %v", page.Number, page.State, page.Reused)
		case page.Number == picture:
			if page.Source != document.SourceReader || page.Reader != "vision" || page.Model != "counted-model" || len(page.Blocks) != 1 ||
				page.Blocks[0].Kind != document.KindFigure || page.Usage == nil || page.Usage.InputTokens != 1100 || page.Usage.OutputTokens != 26 {
				t.Errorf("the picture: read by %q from %s with %q, %d blocks, usage %+v", page.Reader, page.Source, page.Model, len(page.Blocks), page.Usage)
			}
		case page.Source != document.SourceTextLayer || page.Reader != "own" || page.Model != "" || page.Usage == nil || *page.Usage != (document.Usage{Pages: 1}):
			t.Errorf("page %d: read by %q from %s, model %q, usage %+v", page.Number, page.Reader, page.Source, page.Model, page.Usage)
		case len(page.Blocks) != 4 || page.Blocks[0].Text != heading || page.Blocks[0].Kind != document.KindHeading ||
			page.Blocks[1].Text != paragraphs[0] || page.Blocks[2].Text != paragraphs[1] || page.Blocks[3].Kind != document.KindPageNumber:
			t.Errorf("page %d reads %+v", page.Number, page.Blocks)
		}
	}
	// The document counts every page and the tokens of the one call.
	status, raw := srv.call(t, http.MethodGet, "/parses/"+first.ID+"/document", nil)
	var doc document.Document
	if err := json.Unmarshal(raw, &doc); status != http.StatusOK || err != nil {
		t.Fatalf("the document was answered %d: %s", status, raw)
	}
	fromText, byModel := 0, 0
	for _, page := range doc.Pages {
		switch page.Source {
		case document.SourceTextLayer:
			fromText++
		case document.SourceReader:
			byModel++
		}
	}
	if fromText != length-1 || byModel != 1 || doc.Usage != (document.Usage{Pages: length, InputTokens: 1100, OutputTokens: 26}) {
		t.Errorf("the document lists %d pages read from their text and %d by a model, usage %+v", fromText, byModel, doc.Usage)
	}
	t.Logf("%d pages were parsed in %s with %d model call", length, took.Round(time.Millisecond), model.calls.Load())

	// A parse that names the text reader gets that reader alone: the
	// picture fails, and no model is called for it.
	pinned := srv.ended(t, srv.submit(t, log, map[string]any{"reader": "own", "pages": "149-151", "allow_failed_pages": 1, "reuse": false}))
	status, raw = srv.call(t, http.MethodGet, "/parses/"+pinned.ID+"/pages/"+strconv.Itoa(picture), nil)
	var failed document.Page
	if err := json.Unmarshal(raw, &failed); status != http.StatusOK || err != nil {
		t.Fatalf("the picture's page was answered %d: %s", status, raw)
	}
	if pinned.State != "succeeded" || failed.State != document.PageFailed || failed.Error == nil || failed.Error.Code != "page_unreadable" || model.calls.Load() != 1 {
		t.Errorf("a parse that names the text reader ended %s; its picture is %s, %+v; the model was called %d times", pinned.State, failed.State, failed.Error, model.calls.Load())
	}

	// A second parse of the chain takes what the first one read.
	again := srv.ended(t, srv.submit(t, log, map[string]any{"pages": "1-3,150"}))
	for _, n := range []int{1, 2, 3, picture} {
		status, raw := srv.call(t, http.MethodGet, "/parses/"+again.ID+"/pages/"+strconv.Itoa(n), nil)
		var page document.Page
		if err := json.Unmarshal(raw, &page); status != http.StatusOK || err != nil || !page.Reused || page.State != document.PageSucceeded {
			t.Errorf("page %d of a second parse: %d, reused %v, %s", n, status, page.Reused, page.State)
		}
	}
	if again.State != "succeeded" || model.calls.Load() != 1 {
		t.Errorf("a second parse ended %s with %d model calls in all", again.State, model.calls.Load())
	}

	// The typeset survey of the corpus, through the same server: every
	// page with something on it is read from its text, no model is
	// called, and what the API returns is within the bars of its class.
	survey := corpus[slices.IndexFunc(corpus, func(f file) bool { return f.name == "survey.pdf" })]
	truth := truthOf(t, survey)
	scored := srv.ended(t, srv.submit(t, srv.upload(t, survey.name, testfixtures.Read(t, survey.fixture)), map[string]any{"reuse": false}))
	if scored.State != "succeeded" || model.calls.Load() != 1 {
		t.Fatalf("the survey ended %s with %d model calls in all: %+v", scored.State, model.calls.Load(), scored.Error)
	}
	bars, _ := quality.BarsOf(survey.class)
	scores := quality.Score(truth, srv.pages(t, scored.ID, len(truth.Pages)))
	if misses := bars.Misses(scores); len(misses) > 0 || scores.Matched < scores.Blocks-1 {
		t.Errorf("the survey read from its text is under the bars of a typeset file: %v, %d of %d blocks matched\n%s", misses, scores.Matched, scores.Blocks, strings.Join(scores.Notes, "\n"))
	}
	t.Logf("survey.pdf through the durable server: CER %.2f%%, kinds %.1f%%, cells %.1f%%, order %.1f%%, boxes %.1f%%",
		100*scores.CER, 100*scores.Kinds, 100**scores.Cells, 100*scores.Order, 100**scores.Boxes)
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package layout

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/reader"
)

const key = "ocr-key-do-not-print"

// engine is a layout engine for one test.
type engine struct {
	*httptest.Server
	status    int
	header    map[string]string
	body      string
	auth      string
	mediaType string
	image     string
	languages string
}

func serve(t *testing.T) *engine {
	t.Helper()
	e := &engine{status: 200}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.auth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("the request is not a form: %v", err)
		}
		if f, h, err := r.FormFile("file"); err == nil {
			raw, _ := io.ReadAll(f)
			e.image, e.mediaType = string(raw), h.Header.Get("Content-Type")
		}
		e.languages = r.FormValue("languages")
		for k, v := range e.header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(e.status)
		_, _ = io.WriteString(w, e.body)
	}))
	t.Cleanup(e.Close)
	return e
}

func (e *engine) reader(t *testing.T, mutate ...func(*Config)) *Reader {
	t.Helper()
	cfg := Config{Name: "ocr", Endpoint: e.URL + "/v1/ocr"}
	for _, m := range mutate {
		m(&cfg)
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func jpeg() reader.Page {
	return reader.Page{Number: 2, Data: []byte("jpeg-bytes"), MediaType: "image/jpeg", Width: 2000, Height: 1000, Credential: reader.NewCredential(key)}
}

func TestConfigIsChecked(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no name":     {Endpoint: "http://ocr.example/v1/ocr"},
		"no endpoint": {Name: "ocr"},
		"not a url":   {Name: "ocr", Endpoint: "ocr.example"},
		"bad format":  {Name: "ocr", Endpoint: "http://ocr.example/v1/ocr", Image: reader.ImageSpec{Format: "tiff"}},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New must refuse it", name)
		}
	}
}

func TestDescribe(t *testing.T) {
	r, err := New(Config{Name: "ocr", Endpoint: "http://ocr.example/v1/ocr"})
	if err != nil {
		t.Fatal(err)
	}
	if d := r.Describe(); d.Name != "ocr" || !d.Boxes || d.Image != (reader.ImageSpec{DPI: 200, Format: "png"}) || d.Accepts[0] != "image/png" {
		t.Fatalf("Describe() = %+v", d)
	}
	j, err := New(Config{Name: "ocr", Endpoint: "http://ocr.example/v1/ocr", Image: reader.ImageSpec{DPI: 150, Format: "jpeg"}})
	if err != nil {
		t.Fatal(err)
	}
	if d := j.Describe(); d.Accepts[0] != "image/jpeg" || d.Image.DPI != 150 {
		t.Fatalf("a reader asking for jpeg prefers it: %+v", d)
	}
}

func TestReadPage(t *testing.T) {
	e := serve(t)
	e.body = `{"elements":[
	  {"category":"Text","bbox":[200,500,1800,900],"text":"Body text.","reading_order":2},
	  {"category":"Section-header","bbox":[200,100,1800,300],"text":"Results","reading_order":1},
	  {"category":"Table","bbox":[200,910,1800,990],"text":"<table><tr><td>a</td><td>b</td></tr></table>","reading_order":3}
	],"model":"layout-model","usage":{"input_tokens":900,"output_tokens":120}}`
	page := jpeg()
	page.Languages = []string{"en", "fr"}

	got, err := e.reader(t).ReadPage(context.Background(), page)
	if err != nil {
		t.Fatal(err)
	}
	if e.auth != "Bearer "+key || e.mediaType != "image/jpeg" || e.image != "jpeg-bytes" || e.languages != "en,fr" {
		t.Fatalf("request: auth %q, type %q, image %q, languages %q", e.auth, e.mediaType, e.image, e.languages)
	}
	if got.Model != "layout-model" || got.Usage != (document.Usage{Pages: 1, InputTokens: 900, OutputTokens: 120}) || len(got.Blocks) != 3 {
		t.Fatalf("result = %+v", got)
	}
	head, body, table := got.Blocks[0], got.Blocks[1], got.Blocks[2]
	if head.Kind != document.KindHeading || head.Text != "Results" || *head.Box != (document.Box{0.1, 0.1, 0.9, 0.3}) {
		t.Fatalf("the engine's reading order decides, and pixels become fractions: %+v", head)
	}
	if body.Kind != document.KindText || table.Table == nil || table.Text != "a | b" {
		t.Fatalf("body %+v, table %+v", body, table)
	}
}

func TestReadPageWithoutAKeyOrASize(t *testing.T) {
	e := serve(t)
	e.body = `{"elements":[{"category":"Text","bbox":[1,2,3,4],"text":"x"}]}`
	page := reader.Page{Data: []byte("png"), MediaType: "image/png"}
	got, err := e.reader(t).ReadPage(context.Background(), page)
	if err != nil || e.auth != "" || e.languages != "" {
		t.Fatalf("ReadPage = %+v, %v; auth %q", got, err, e.auth)
	}
	if got.Blocks[0].Box != nil {
		t.Fatal("a box cannot be placed on an image whose size is not known")
	}
}

func TestReadPageFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(*engine)
		page  func(*reader.Page)
		class reader.Class
		wait  time.Duration
	}{
		{name: "not an image", page: func(p *reader.Page) { p.MediaType = "application/pdf" }, class: reader.Permanent},
		{name: "not JSON", set: func(e *engine) { e.body = "loading" }, class: reader.Invalid},
		{name: "no elements", set: func(e *engine) { e.body = `{"model":"m"}` }, class: reader.Invalid},
		{name: "still loading", set: func(e *engine) { e.status, e.header = 503, map[string]string{"Retry-After": "30"} }, class: reader.RateLimited, wait: 30 * time.Second},
		{name: "loading, no hint", set: func(e *engine) { e.status = 503 }, class: reader.Retryable},
		{name: "refused", set: func(e *engine) { e.status = 400 }, class: reader.Permanent},
	} {
		e := serve(t)
		e.body = `{"elements":[]}`
		if tc.set != nil {
			tc.set(e)
		}
		page := jpeg()
		if tc.page != nil {
			tc.page(&page)
		}
		_, err := e.reader(t).ReadPage(context.Background(), page)
		if err == nil || reader.ClassOf(err) != tc.class || reader.RetryAfterOf(err) != tc.wait {
			t.Errorf("%s: err = %v, want class %v after %v", tc.name, err, tc.class, tc.wait)
		}
		if err != nil && strings.Contains(err.Error(), key) {
			t.Errorf("%s: the key is in the error: %v", tc.name, err)
		}
	}

	// An empty list of elements is an answer, not a failure.
	e := serve(t)
	e.body = `{"elements":[]}`
	if got, err := e.reader(t).ReadPage(context.Background(), jpeg()); err != nil || len(got.Blocks) != 0 {
		t.Fatalf("an empty page: %+v, %v", got, err)
	}
}

func TestTransportFailuresAreRetryable(t *testing.T) {
	e := serve(t)
	r := e.reader(t)
	e.Close()
	if _, err := r.ReadPage(context.Background(), jpeg()); reader.ClassOf(err) != reader.Retryable {
		t.Fatalf("a closed engine: %v", err)
	}

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	timed, err := New(Config{Name: "slow", Endpoint: slow.URL, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := timed.ReadPage(context.Background(), jpeg()); reader.ClassOf(err) != reader.Retryable {
		t.Fatalf("a call past its timeout: %v", err)
	}
}

var _ reader.Reader = (*Reader)(nil)

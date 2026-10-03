// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/reader"
)

const key = "sk-test-key-do-not-print"

// endpoint is a chat completions endpoint for one test: it records the
// request it received and answers with what the test set.
type endpoint struct {
	*httptest.Server
	got     request
	auth    string
	path    string
	status  int
	header  map[string]string
	content string // the model's text, wrapped into a completion
	finish  string
	body    string // a whole response body, used instead of content when set
}

func serve(t *testing.T) *endpoint {
	t.Helper()
	e := &endpoint{status: 200, finish: "stop"}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.auth, e.path = r.Header.Get("Authorization"), r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		e.got = request{}
		if err := json.Unmarshal(raw, &e.got); err != nil {
			t.Errorf("the request is not JSON: %v", err)
		}
		for k, v := range e.header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(e.status)
		if e.body != "" {
			_, _ = io.WriteString(w, e.body)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "model-that-answered",
			"choices": []any{map[string]any{"finish_reason": e.finish, "message": map[string]any{"content": e.content}}},
			"usage":   map[string]any{"prompt_tokens": 1200, "completion_tokens": 300},
		})
	}))
	t.Cleanup(e.Close)
	return e
}

func (e *endpoint) reader(t *testing.T, mutate ...func(*Config)) *Reader {
	t.Helper()
	cfg := Config{Name: "default", Endpoint: e.URL + "/v1/", Model: "some-model"}
	for _, m := range mutate {
		m(&cfg)
	}
	r, err := NewReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *endpoint) extractor(t *testing.T, mutate ...func(*Config)) *Extractor {
	t.Helper()
	cfg := Config{Name: "text", Endpoint: e.URL + "/v1", Model: "some-text-model"}
	for _, m := range mutate {
		m(&cfg)
	}
	x, err := NewExtractor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func png() reader.Page {
	return reader.Page{Number: 3, Data: []byte("\x89PNG-bytes"), MediaType: "image/png", Width: 1000, Height: 1400, Credential: reader.NewCredential(key)}
}

const goodPage = `{"blocks":[
  {"kind":"title","text":"Annual report","box":[100,50,900,120],"level":1},
  {"kind":"table","text":"<table><tr><td>Q1</td><td>10</td></tr></table>","box":[100,300,900,600],"level":null},
  {"kind":"watermark","text":"draft","box":[900,700,100,650],"level":null}
]}`

func TestConfigIsChecked(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no name":     {Endpoint: "https://gateway.example/v1", Model: "m"},
		"no model":    {Name: "r", Endpoint: "https://gateway.example/v1"},
		"no endpoint": {Name: "r", Model: "m"},
		"not a url":   {Name: "r", Model: "m", Endpoint: "gateway.example/v1"},
		"bad scheme":  {Name: "r", Model: "m", Endpoint: "ftp://gateway.example/v1"},
		"bad format":  {Name: "r", Model: "m", Endpoint: "https://gateway.example/v1", Image: reader.ImageSpec{Format: "webp"}},
	} {
		if _, err := NewReader(cfg); err == nil {
			t.Errorf("%s: NewReader must refuse it", name)
		}
		if _, err := NewExtractor(cfg); err == nil {
			t.Errorf("%s: NewExtractor must refuse it", name)
		}
	}
}

func TestDescribe(t *testing.T) {
	r, err := NewReader(Config{Name: "default", Endpoint: "https://gateway.example/v1", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	d := r.Describe()
	if d.Name != "default" || !d.Boxes || d.Image != (reader.ImageSpec{DPI: 160, LongEdge: 2048, Format: "png"}) || d.Accepts[0] != "image/png" {
		t.Fatalf("Describe() = %+v", d)
	}

	j, err := NewReader(Config{Name: "j", Endpoint: "https://gateway.example/v1", Model: "m", Image: reader.ImageSpec{DPI: 200, LongEdge: 1024, Format: "jpeg"}})
	if err != nil {
		t.Fatal(err)
	}
	if d := j.Describe(); d.Accepts[0] != "image/jpeg" || d.Image.DPI != 200 {
		t.Fatalf("a reader asking for jpeg prefers it: %+v", d)
	}

	x, err := NewExtractor(Config{Name: "text", Endpoint: "https://gateway.example/v1", Model: "m", Constrain: true, MaxInput: 9000})
	if err != nil {
		t.Fatal(err)
	}
	if d := x.Describe(); d.Name != "text" || !d.Constrained || d.MaxInput != 9000 {
		t.Fatalf("Describe() = %+v", d)
	}
}

func TestReadPage(t *testing.T) {
	e := serve(t)
	e.content = goodPage
	page := png()
	page.Languages = []string{"de", "en"}

	got, err := e.reader(t).ReadPage(context.Background(), page)
	if err != nil {
		t.Fatal(err)
	}

	// What went out.
	if e.path != "/v1/chat/completions" || e.auth != "Bearer "+key {
		t.Fatalf("path %q, authorization %q", e.path, e.auth)
	}
	if e.got.Model != "some-model" || e.got.Temperature != 0 || e.got.MaxTokens != 8192 || e.got.ResponseFormat != nil {
		t.Fatalf("request = %+v", e.got)
	}
	parts := e.got.Messages[0].Content
	if len(parts) != 2 || !strings.Contains(parts[0].Text, "page_footer") || !strings.HasSuffix(parts[0].Text, "written in: de, en.") {
		t.Fatalf("instruction = %q", parts[0].Text)
	}
	if want := "data:image/png;base64,iVBORy1ieXRlcw=="; parts[1].ImageURL.URL != want {
		t.Fatalf("image = %q, want %q", parts[1].ImageURL.URL, want)
	}

	// What came back.
	if got.Model != "model-that-answered" || got.Truncated || got.Usage != (document.Usage{Pages: 1, InputTokens: 1200, OutputTokens: 300}) {
		t.Fatalf("result = %+v", got)
	}
	if len(got.Blocks) != 3 {
		t.Fatalf("blocks = %+v", got.Blocks)
	}
	title, table, odd := got.Blocks[0], got.Blocks[1], got.Blocks[2]
	if title.Kind != document.KindTitle || title.Level != 1 || *title.Box != (document.Box{0.1, 0.05, 0.9, 0.12}) {
		t.Fatalf("title = %+v", title)
	}
	if table.Table == nil || table.Table.Cols != 2 || table.Text != "Q1 | 10" {
		t.Fatalf("table = %+v", table)
	}
	if odd.Kind != document.KindText || len(odd.Flags) != 2 || *odd.Box != (document.Box{0.1, 0.65, 0.9, 0.7}) {
		t.Fatalf("an unknown kind with an inverted box is text, flagged twice, with the box repaired: %+v", odd)
	}
}

func TestReadPageConstrained(t *testing.T) {
	e := serve(t)
	e.content = `{"blocks":[]}`
	got, err := e.reader(t, func(c *Config) { c.Constrain = true; c.MaxOutputTokens = 500 }).ReadPage(context.Background(), png())
	if err != nil || len(got.Blocks) != 0 {
		t.Fatalf("ReadPage = %+v, %v", got, err)
	}
	f := e.got.ResponseFormat
	if f == nil || f.Type != "json_schema" || !f.JSONSchema.Strict || f.JSONSchema.Name != "page" || e.got.MaxTokens != 500 {
		t.Fatalf("response format = %+v", f)
	}
	var schema map[string]any
	if err := json.Unmarshal(f.JSONSchema.Schema, &schema); err != nil || schema["additionalProperties"] != false {
		t.Fatalf("schema = %s (%v)", f.JSONSchema.Schema, err)
	}
	if !strings.Contains(string(f.JSONSchema.Schema), `"key_value"`) {
		t.Fatal("the schema must list the closed set of kinds")
	}
}

func TestReadPageReadsWhatModelsActuallyReturn(t *testing.T) {
	for name, reply := range map[string]string{
		"a bare list":          `[{"kind":"text","text":"hello","box":[0,0,500,500],"level":null}]`,
		"a fenced object":      "```json\n{\"blocks\":[{\"kind\":\"text\",\"text\":\"hello\",\"box\":[0,0,500,500],\"level\":null}]}\n```",
		"a raw newline inside": "{\"blocks\":[{\"kind\":\"text\",\"text\":\"hel\nlo\",\"box\":[0,0,500,500],\"level\":null}]}",
	} {
		e := serve(t)
		e.content = reply
		got, err := e.reader(t).ReadPage(context.Background(), png())
		if err != nil || len(got.Blocks) != 1 || got.Blocks[0].Kind != document.KindText {
			t.Errorf("%s: ReadPage = %+v, %v", name, got, err)
		}
	}

	// Content sent as a list of parts.
	e := serve(t)
	e.body = `{"model":"m","choices":[{"finish_reason":"stop","message":{"content":[{"type":"text","text":"{\"blocks\":"},{"type":"text","text":"[]}"}]}}]}`
	if got, err := e.reader(t).ReadPage(context.Background(), png()); err != nil || len(got.Blocks) != 0 {
		t.Fatalf("parts: ReadPage = %+v, %v", got, err)
	}
}

func TestReadPageTruncated(t *testing.T) {
	e := serve(t)
	e.content, e.finish = `{"blocks":[{"kind":"text","text":"cut sho","box":[0,0,500,500],"level":null}]}`, "length"
	got, err := e.reader(t).ReadPage(context.Background(), png())
	if err != nil || !got.Truncated || len(got.Blocks[0].Flags) != 1 || got.Blocks[0].Flags[0] != document.FlagTruncated {
		t.Fatalf("a reply that ended at the output limit is flagged: %+v, %v", got, err)
	}
}

func TestReadPageFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(*endpoint)
		page  func(*reader.Page)
		class reader.Class
		wait  time.Duration
	}{
		{name: "not an image", page: func(p *reader.Page) { p.MediaType = "application/pdf" }, class: reader.Permanent},
		{name: "prose instead of JSON", set: func(e *endpoint) { e.content = "I cannot read this page." }, class: reader.Invalid},
		{name: "an object without blocks", set: func(e *endpoint) { e.content = `{"note":"nothing"}` }, class: reader.Invalid},
		{name: "a body that is not a completion", set: func(e *endpoint) { e.body = "<html>bad gateway</html>" }, class: reader.Invalid},
		{name: "no choices", set: func(e *endpoint) { e.body = `{"choices":[]}` }, class: reader.Invalid},
		{name: "a refusal", set: func(e *endpoint) {
			e.body = `{"choices":[{"finish_reason":"stop","message":{"content":null,"refusal":"no"}}]}`
		}, class: reader.Permanent},
		{name: "a content filter", set: func(e *endpoint) {
			e.body = `{"choices":[{"finish_reason":"content_filter","message":{"content":""}}]}`
		}, class: reader.Permanent},
		{name: "content that is neither text nor parts", set: func(e *endpoint) {
			e.body = `{"choices":[{"finish_reason":"stop","message":{"content":42}}]}`
		}, class: reader.Invalid},
		{name: "rate limited", set: func(e *endpoint) { e.status, e.header = 429, map[string]string{"Retry-After": "7"} }, class: reader.RateLimited, wait: 7 * time.Second},
		{name: "server error", set: func(e *endpoint) { e.status = 500 }, class: reader.Retryable},
		{name: "budget", set: func(e *endpoint) { e.status, e.body = 402, `{"error":{"code":"budget_exhausted"}}` }, class: reader.Budget},
		{name: "unauthorized", set: func(e *endpoint) { e.status = 401 }, class: reader.Permanent},
	} {
		e := serve(t)
		e.content = goodPage
		if tc.set != nil {
			tc.set(e)
		}
		page := png()
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
}

func TestTransportFailuresAreRetryable(t *testing.T) {
	e := serve(t)
	r := e.reader(t)
	e.Close()
	_, err := r.ReadPage(context.Background(), png())
	if reader.ClassOf(err) != reader.Retryable {
		t.Fatalf("a closed endpoint: %v", err)
	}

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	timed, err := NewReader(Config{Name: "slow", Endpoint: slow.URL, Model: "m", Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := timed.ReadPage(context.Background(), png()); reader.ClassOf(err) != reader.Retryable {
		t.Fatalf("a call past its timeout: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := timed.ReadPage(ctx, png()); err == nil {
		t.Fatal("a canceled call must fail")
	}
}

const schema = `{"type":"object","properties":{"total":{"type":"number"}},"required":["total"],"additionalProperties":false}`

func TestExtract(t *testing.T) {
	e := serve(t)
	e.content = `{"data":{"total":1280.5},"citations":[{"pointer":"/total","refs":["2.2"]},{"pointer":"/total","refs":["2.7"]},{"pointer":"","refs":["1.1"]},{"pointer":"/x","refs":[]}]}`
	in := reader.ExtractRequest{
		Schema: json.RawMessage(schema), Instructions: " Amounts are in euros. ", Citations: true,
		Text: "[2.2] Total 1,280.50\n[2.7] Sum 1,280.50", Problems: []string{"/total: expected number"},
		Credential: reader.NewCredential(key),
	}
	got, err := e.extractor(t).Extract(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != `{"total":1280.5}` || got.Constrained || got.Model != "model-that-answered" || got.Usage.InputTokens != 1200 {
		t.Fatalf("result = %+v", got)
	}
	if len(got.Citations) != 1 || strings.Join(got.Citations["/total"], ",") != "2.2,2.7" {
		t.Fatalf("citations = %v", got.Citations)
	}

	ask := e.got.Messages[0].Content[0].Text
	for _, want := range []string{"list the refs", "Amounts are in euros.", schema, "- /total: expected number", "Document:\n[2.2] Total"} {
		if !strings.Contains(ask, want) {
			t.Errorf("the request lacks %q:\n%s", want, ask)
		}
	}
	if e.got.ResponseFormat != nil || e.auth != "Bearer "+key {
		t.Fatalf("an unconstrained call sends no response format: %+v, auth %q", e.got.ResponseFormat, e.auth)
	}
}

func TestExtractConstrainedAndWithoutCitations(t *testing.T) {
	e := serve(t)
	e.content = `{"data":{"total":1},"citations":[{"pointer":"/total","refs":["1.1"]}]}`
	x := e.extractor(t, func(c *Config) { c.Constrain = true })

	got, err := x.Extract(context.Background(), reader.ExtractRequest{Schema: json.RawMessage(schema), Text: "[1.1] Total 1", Constrain: true})
	if err != nil || !got.Constrained || got.Citations != nil {
		t.Fatalf("Extract = %+v, %v; citations were not asked for", got, err)
	}
	f := e.got.ResponseFormat
	if f == nil || f.JSONSchema.Name != "extraction" || !json.Valid(f.JSONSchema.Schema) || !strings.Contains(string(f.JSONSchema.Schema), `"data":`+schema) {
		t.Fatalf("response format = %+v", f)
	}
	if ask := e.got.Messages[0].Content[0].Text; !strings.Contains(ask, "with an empty list of citations") || strings.Contains(ask, "list the refs") || e.auth != "" {
		t.Fatalf("without citations and without a key: auth %q, ask:\n%s", e.auth, ask)
	}

	// The caller decides per schema; the extractor's own setting is a ceiling.
	if got, err := x.Extract(context.Background(), reader.ExtractRequest{Schema: json.RawMessage(schema), Text: "t"}); err != nil || got.Constrained || e.got.ResponseFormat != nil {
		t.Fatalf("a call that does not ask for it is not constrained: %+v, %v", got, err)
	}
}

func TestExtractFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(*endpoint)
		in    func(*reader.ExtractRequest)
		class reader.Class
	}{
		{name: "text past the bound", in: func(r *reader.ExtractRequest) { r.Text = strings.Repeat("x", 101) }, class: reader.Permanent},
		{name: "a schema that is not JSON", in: func(r *reader.ExtractRequest) { r.Schema = json.RawMessage("{") }, class: reader.Permanent},
		{name: "a reply cut short", set: func(e *endpoint) { e.finish = "length" }, class: reader.Invalid},
		{name: "prose", set: func(e *endpoint) { e.content = "The total is 12." }, class: reader.Invalid},
		{name: "no data", set: func(e *endpoint) { e.content = `{"citations":[]}` }, class: reader.Invalid},
		{name: "null data", set: func(e *endpoint) { e.content = `{"data":null,"citations":[]}` }, class: reader.Invalid},
		{name: "server error", set: func(e *endpoint) { e.status = 503 }, class: reader.Retryable},
	} {
		e := serve(t)
		e.content = `{"data":{"total":1},"citations":[]}`
		if tc.set != nil {
			tc.set(e)
		}
		in := reader.ExtractRequest{Schema: json.RawMessage(schema), Text: "[1.1] Total 1"}
		if tc.in != nil {
			tc.in(&in)
		}
		_, err := e.extractor(t, func(c *Config) { c.MaxInput = 100 }).Extract(context.Background(), in)
		if err == nil || reader.ClassOf(err) != tc.class {
			t.Errorf("%s: err = %v, want class %v", tc.name, err, tc.class)
		}
	}
}

// Both types satisfy the interfaces they are built for.
var (
	_ reader.Reader    = (*Reader)(nil)
	_ reader.Extractor = (*Extractor)(nil)
)

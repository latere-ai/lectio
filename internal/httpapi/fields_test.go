// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/extract"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
)

// Extraction through the API, over the durable backend
// (specs/011-structured-extraction.md): a worker in the test's process
// claims the tasks from Postgres, as a worker process does.

// invoiced reads the 3 pages of an invoice: a running header on each, the
// invoice's number on the first, an item on each of the first 2, and the
// total on the last. It counts its calls.
type invoiced struct{ calls atomic.Int64 }

func (*invoiced) Describe() reader.Description {
	return reader.Description{Name: "layout", Accepts: []string{"image/png"}, Image: reader.ImageSpec{DPI: 72, Format: "png"}, Boxes: true, Version: "v1"}
}

func (i *invoiced) ReadPage(_ context.Context, page reader.Page) (reader.Result, error) {
	i.calls.Add(1)
	lines := map[int][]string{
		1: {"Invoice INV-0042", "Item bolt 3"},
		2: {"Item nut 4"},
		3: {"Total: 7"},
	}[page.Number]
	raw := []reader.Raw{{Label: "page_header", Text: "ACME Corp", Box: []float64{100, 20, 900, 60}}}
	for n, l := range lines {
		raw = append(raw, reader.Raw{Label: "text", Text: l, Box: []float64{100, float64(100 + 100*n), 900, float64(180 + 100*n)}})
	}
	return reader.Result{Model: "layout", Usage: document.Usage{Pages: 1}, Blocks: reader.Normalize(raw, reader.Grid{Width: 1000, Height: 1000})}, nil
}

// The lines a text model of the cases reads.
var (
	numberLine = regexp.MustCompile(`(?m)^\[(\d+\.\d+)\] Invoice (\S+)$`)
	totalLine  = regexp.MustCompile(`(?m)^\[(\d+\.\d+)\] Total: (\d+)$`)
	itemLine   = regexp.MustCompile(`(?m)^\[(\d+\.\d+)\] Item (\S+) (\d+)$`)
)

// textModel is the extractor of the cases: it fills the invoice schema from
// the lines of the text it is given, each value cited by its line's ref,
// and nothing for what the text does not state. A case scripts another
// answer for a call when it sets answers.
type textModel struct {
	maxInput int
	answers  func(n int, req reader.ExtractRequest) (reader.ExtractResult, bool)

	mu    sync.Mutex
	asked []reader.ExtractRequest
}

func (m *textModel) Describe() reader.ExtractorDescription {
	return reader.ExtractorDescription{Name: "text", MaxInput: m.maxInput}
}

func (m *textModel) Extract(_ context.Context, req reader.ExtractRequest) (reader.ExtractResult, error) {
	m.mu.Lock()
	m.asked = append(m.asked, req)
	n := len(m.asked)
	m.mu.Unlock()
	if m.answers != nil {
		if res, scripted := m.answers(n, req); scripted {
			return res, nil
		}
	}
	data, cited := map[string]any{}, map[string][]string{}
	if f := numberLine.FindStringSubmatch(req.Text); f != nil {
		data["number"], cited["/number"] = f[2], []string{f[1]}
	}
	if f := totalLine.FindStringSubmatch(req.Text); f != nil {
		total, _ := strconv.Atoi(f[2])
		data["total"], cited["/total"] = total, []string{f[1]}
	}
	var items []any
	for i, f := range itemLine.FindAllStringSubmatch(req.Text, -1) {
		price, _ := strconv.Atoi(f[3])
		items = append(items, map[string]any{"name": f[2], "price": price})
		cited["/items/"+strconv.Itoa(i)+"/name"] = []string{f[1]}
	}
	if items != nil {
		data["items"] = items
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return reader.ExtractResult{}, err
	}
	return reader.ExtractResult{Data: raw, Citations: cited, Model: "text-model", Usage: document.Usage{InputTokens: 100, OutputTokens: 10}}, nil
}

// calls is how many calls the model was sent.
func (m *textModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.asked)
}

// requests is a copy of the calls the model was sent, in order.
func (m *textModel) requests() []reader.ExtractRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.asked)
}

// invoiceSchema is the schema of the cases, and partiesSchema a second one
// over the same document.
const (
	invoiceSchema = `{"type":"object","required":["number"],"properties":{"number":{"type":"string"},"total":{"type":"number"},` +
		`"items":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"price":{"type":"number"}}}}}}`
	totalSchema = `{"type":"object","properties":{"total":{"type":"number"}}}`
)

// extracting is a durable server whose pages are read as an invoice and
// whose extractions are filled by the text model. The case skips where no
// container runtime answers.
func extracting(t *testing.T, model *textModel, change func(*Server, *run.Runner)) (*env, *invoiced) {
	t.Helper()
	durably(t, false)
	over.extractors, over.extractChain = map[string]reader.Extractor{"text": model}, []string{"text"}
	rd := &invoiced{}
	e := serve(t, func(s *Server, r *run.Runner) {
		r.Readers, r.Chain = map[string]reader.Reader{"layout": rd}, []string{"layout"}
		if change != nil {
			change(s, r)
		}
	})
	return e, rd
}

// invoice parses the 3 pages of the invoice and returns the parse's id.
func (e *env) invoice() string {
	e.t.Helper()
	p := e.parsed(e.upload("invoice.tiff", testfixtures.Read(e.t, testfixtures.MultiTIFF)), "")
	if p["state"] != "succeeded" {
		e.t.Fatalf("the invoice was parsed as %v", p)
	}
	return p["id"].(string)
}

// ask asks an extraction of a parse and fails the case unless it is
// accepted as pending.
func (e *env) ask(parse, name, schema string) {
	e.t.Helper()
	got := e.do("POST", "/parses/"+parse+"/fields", `{"name":"`+name+`","schema":`+schema+`}`)
	if got.status != http.StatusAccepted || got.json(e.t)["state"] != "pending" || got.json(e.t)["name"] != name || got.json(e.t)["constrained"] != false {
		e.t.Fatalf("asking the extraction %s: %d %s", name, got.status, got.body)
	}
}

// filled waits for an extraction to end and returns it.
func (e *env) filled(parse, name, query string) map[string]any {
	e.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		got := e.do("GET", "/parses/"+parse+"/fields/"+name+query, nil)
		if got.status != http.StatusOK {
			e.t.Fatalf("reading the extraction %s: %d %s", name, got.status, got.body)
		}
		if f := got.json(e.t); f["state"] != "pending" {
			return f
		}
	}
	e.t.Fatalf("the extraction %s of %s did not end", name, parse)
	return nil
}

// TestASchemaIsCheckedWhenAnExtractionIsAsked: a schema that does not
// compile, is too large or nests too deep is refused when the request
// arrives, with the reason and the member it is about, and so is every
// other member the contract does not take. Nothing is queued for a request
// that was refused, and no model is called.
func TestASchemaIsCheckedWhenAnExtractionIsAsked(t *testing.T) {
	model := &textModel{}
	e, _ := extracting(t, model, nil)
	pid := e.invoice()

	deep := `{"type":"string"}`
	for range extract.MaxSchemaDepth {
		deep = `{"type":"object","properties":{"a":` + deep + `}}`
	}
	large := `{"type":"object","description":"` + strings.Repeat("d", extract.MaxSchemaBytes) + `"}`
	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
		field  string
		reason string
	}{
		"a schema that does not compile": {`{"name":"a","schema":{"type":"object","properties":{"x":{"type":"money"}}}}`, 400, "invalid_schema", "schema", "does not compile"},
		"a schema that is too large":     {`{"name":"a","schema":` + large + `}`, 400, "invalid_schema", "schema", "bytes"},
		"a schema that nests too deep":   {`{"name":"a","schema":` + deep + `}`, 400, "invalid_schema", "schema", "levels deep"},
		"a schema whose root is a list":  {`{"name":"a","schema":{"type":"array"}}`, 400, "invalid_schema", "schema", "root"},
		"a schema that fetches":          {`{"name":"a","schema":{"type":"object","properties":{"x":{"$ref":"https://example.com/s.json"}}}}`, 400, "invalid_schema", "schema", "does not compile"},
		"a schema that is no object":     {`{"name":"a","schema":"object"}`, 400, "invalid_schema", "schema", "not a JSON object"},
		"no schema":                      {`{"name":"a"}`, 400, "invalid_request", "schema", "required"},
		"no name":                        {`{"schema":{"type":"object"}}`, 400, "invalid_request", "name", "lower case"},
		"a name in upper case":           {`{"name":"Invoice","schema":{"type":"object"}}`, 400, "invalid_request", "name", "lower case"},
		"a name with a dot":              {`{"name":"a.b","schema":{"type":"object"}}`, 400, "invalid_request", "name", "lower case"},
		"a name of 64 characters":        {`{"name":"` + strings.Repeat("a", 64) + `","schema":{"type":"object"}}`, 400, "invalid_request", "name", "at most 63"},
		"instructions that are too long": {`{"name":"a","schema":{"type":"object"},"instructions":"` + strings.Repeat("ä", maxInstructions+1) + `"}`, 400, "invalid_request", "instructions", "4000"},
		"a member the contract lacks":    {`{"name":"a","schema":{"type":"object"},"required":true}`, 400, "unknown_field", "required", "required"},
		"an extractor that is none":      {`{"name":"a","schema":{"type":"object"},"extractor":"other"}`, 400, "reader_not_found", "", "other"},
		"no body":                        {``, 400, "invalid_request", "", "JSON"},
	} {
		got := e.do("POST", "/parses/"+pid+"/fields", tc.body)
		reason, _ := at(got.json(t), "error", "details", "reason").(string)
		if got.status != tc.status || got.code(t) != tc.code || fieldOf(t, got) != tc.field || !strings.Contains(reason, tc.reason) {
			t.Errorf("%s: %d %s", name, got.status, got.body)
		}
	}
	// Instructions of exactly the bound, in characters and not in bytes,
	// are taken.
	if got := e.do("POST", "/parses/"+pid+"/fields", `{"name":"long","schema":`+totalSchema+`,"instructions":"`+strings.Repeat("ä", maxInstructions)+`","citations":false,"extractor":"text"}`); got.status != http.StatusAccepted {
		t.Fatalf("instructions at the bound: %d %s", got.status, got.body)
	}
	if f := e.filled(pid, "long", ""); f["state"] != "succeeded" || f["citations"] != nil {
		t.Fatalf("an extraction asked without citations: %v", f)
	}
	if listed := e.do("GET", "/parses/"+pid+"/fields", nil).json(t)["fields"].([]any); len(listed) != 1 || model.calls() != 1 {
		t.Fatalf("after the refusals the parse has %d extractions and the model was called %d times", len(listed), model.calls())
	}

	// A name the parse already has, another caller's parse, a parse that
	// is not there, and a read that names no extraction.
	if got := e.do("POST", "/parses/"+pid+"/fields", `{"name":"long","schema":`+totalSchema+`}`); got.status != http.StatusConflict || got.code(t) != "conflict" {
		t.Fatalf("a name the parse has: %d %s", got.status, got.body)
	}
	for _, route := range []string{"POST /parses/" + pid + "/fields", "GET /parses/" + pid + "/fields", "GET /parses/" + pid + "/fields/long", "GET /parses/prs_none/fields"} {
		method, path, _ := strings.Cut(route, " ")
		who := e.as("bob-token")
		if strings.Contains(path, "prs_none") {
			who = e
		}
		if got := who.do(method, path, `{"name":"b","schema":`+totalSchema+`}`); got.status != http.StatusNotFound || got.code(t) != "parse_not_found" {
			t.Errorf("%s of a parse that is not the caller's: %d %s", route, got.status, got.body)
		}
	}
	if got := e.do("GET", "/parses/"+pid+"/fields/other", nil); got.status != http.StatusNotFound || got.code(t) != "not_found" {
		t.Fatalf("an extraction nobody asked: %d %s", got.status, got.body)
	}
	if got := e.do("GET", "/parses/"+pid+"/fields/long?resolve=perhaps", nil); got.status != http.StatusBadRequest || fieldOf(t, got) != "resolve" {
		t.Fatalf("a resolve that is no boolean: %d %s", got.status, got.body)
	}
}

// TestTwoSchemasAreExtractedFromOneParseAndNoPageIsReadAgain: 2 schemas
// asked of one parse that has ended, one after the other, produce 2
// fields with one call to the text model each and no call to a reader.
// Each object validates against its schema, and every citation resolves to
// a block whose text contains the value. The document lists the fields,
// and its parse is as it was but for what the calls used.
func TestTwoSchemasAreExtractedFromOneParseAndNoPageIsReadAgain(t *testing.T) {
	model := &textModel{}
	e, rd := extracting(t, model, nil)
	pid := e.invoice()
	before, read := e.do("GET", "/parses/"+pid, nil).json(t), rd.calls.Load()
	markdown := string(e.do("GET", "/parses/"+pid+"/document?format=markdown", nil).body)

	e.ask(pid, "invoice", invoiceSchema)
	invoice := e.filled(pid, "invoice", "?resolve=true")
	e.ask(pid, "sum", totalSchema)
	sum := e.filled(pid, "sum", "")
	if rd.calls.Load() != read || read != 3 || model.calls() != 2 {
		t.Fatalf("the reader was called %d times and then %d, and the text model %d times", read, rd.calls.Load(), model.calls())
	}

	if invoice["state"] != "succeeded" || invoice["model"] != "text-model" || invoice["attempts"] != 1.0 || invoice["windows"] != 1.0 ||
		at(invoice, "usage", "input_tokens") != 100.0 || invoice["error"] != nil {
		t.Fatalf("the invoice: %v", invoice)
	}
	schema, err := extract.Compile([]byte(invoiceSchema))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(invoice["data"])
	if err != nil || len(schema.Check(data, false)) != 0 || string(data) != `{"items":[{"name":"bolt","price":3},{"name":"nut","price":4}],"number":"INV-0042","total":7}` {
		t.Fatalf("the object %s, %v, findings %v", data, err, schema.Check(data, false))
	}
	// Every citation names a block, with its page and its box, whose text
	// holds the value that cites it.
	values := map[string]string{"/number": "INV-0042", "/total": "7", "/items/0/name": "bolt", "/items/1/name": "nut"}
	citations := invoice["citations"].(map[string]any)
	if len(citations) != len(values) {
		t.Fatalf("the object cites %v", citations)
	}
	for pointer, value := range values {
		cited := citations[pointer].([]any)
		if len(cited) != 1 {
			t.Fatalf("%s cites %v", pointer, cited)
		}
		ref, _ := at(cited[0], "ref").(string)
		block := e.do("GET", "/parses/"+pid+"/blocks/"+ref, nil).json(t)
		page, _, _ := document.ParseRef(ref)
		if text, _ := block["text"].(string); !strings.Contains(text, value) || at(cited[0], "page") != float64(page) || !slices.Equal(at(cited[0], "box").([]any), block["box"].([]any)) {
			t.Errorf("%s = %s cites %v, the block %v", pointer, value, cited[0], block)
		}
	}
	// Read without resolve, a citation is the block's ref alone.
	if plain := e.filled(pid, "sum", ""); sum["state"] != "succeeded" || !slices.Equal(at(plain, "citations", "/total").([]any), []any{"3.2"}) || at(plain, "data", "total") != 7.0 {
		t.Fatalf("the sum: %v", plain)
	}

	listed := e.do("GET", "/parses/"+pid+"/fields", nil).json(t)["fields"].([]any)
	if len(listed) != 2 || at(listed[0], "name") != "invoice" || at(listed[1], "name") != "sum" || at(listed[1], "data", "total") != 7.0 {
		t.Fatalf("the extractions of the parse: %v", listed)
	}
	doc := e.do("GET", "/parses/"+pid+"/document", nil).json(t)
	if !slices.Equal(doc["fields"].([]any), []any{"invoice", "sum"}) {
		t.Fatalf("the document lists the fields %v", doc["fields"])
	}
	after := e.do("GET", "/parses/"+pid, nil).json(t)
	if after["state"] != before["state"] || at(after, "progress", "pages_done") != at(before, "progress", "pages_done") ||
		at(after, "usage", "input_tokens") != 200.0 || string(e.do("GET", "/parses/"+pid+"/document?format=markdown", nil).body) != markdown {
		t.Fatalf("the extractions moved their parse from %v to %v", before, after)
	}
}

// TestAReplyThatViolatesTheSchemaIsRepairedOrTheFieldFails: a reply that
// violates the schema is repaired within 2 retries, and the field then says
// how often the model was asked and holds the tokens of every call. A
// model that never satisfies the schema leaves the field failed with
// schema_not_satisfied and the rules it broke, and the parse and its
// document as they were.
func TestAReplyThatViolatesTheSchemaIsRepairedOrTheFieldFails(t *testing.T) {
	bad := reader.ExtractResult{Data: json.RawMessage(`{"number":42}`), Model: "text-model", Usage: document.Usage{InputTokens: 100, OutputTokens: 10}}
	var stubborn atomic.Bool
	model := &textModel{}
	model.answers = func(n int, _ reader.ExtractRequest) (reader.ExtractResult, bool) {
		return bad, stubborn.Load() || n == 1
	}
	e, _ := extracting(t, model, nil)
	pid := e.invoice()
	document := e.do("GET", "/parses/"+pid+"/document", nil).body

	e.ask(pid, "repaired", invoiceSchema)
	repaired := e.filled(pid, "repaired", "")
	if repaired["state"] != "succeeded" || repaired["attempts"] != 2.0 || at(repaired, "usage", "input_tokens") != 200.0 || at(repaired, "usage", "output_tokens") != 20.0 ||
		at(repaired, "data", "number") != "INV-0042" {
		t.Fatalf("the repaired extraction: %v", repaired)
	}
	if asked := model.requests(); !strings.Contains(asked[1].Previous, `"number":42`) || len(asked[1].Problems) != 1 || asked[1].Text != asked[0].Text {
		t.Fatalf("the repair was asked %+v", asked[1])
	}

	stubborn.Store(true)
	e.ask(pid, "failed", invoiceSchema)
	failed := e.filled(pid, "failed", "")
	if failed["state"] != "failed" || at(failed, "error", "code") != "schema_not_satisfied" || failed["data"] != nil || failed["attempts"] != 3.0 ||
		at(failed, "usage", "input_tokens") != 300.0 || !strings.Contains(at(failed, "error", "detail").(string), "#/properties/number/type") {
		t.Fatalf("the extraction that never satisfied its schema: %v", failed)
	}
	if model.calls() != 5 {
		t.Fatalf("the model was called %d times, want 2 and 3", model.calls())
	}
	p := e.do("GET", "/parses/"+pid, nil).json(t)
	if p["state"] != "succeeded" || p["error"] != nil || at(p, "progress", "pages_failed") != 0.0 {
		t.Fatalf("a failed extraction moved its parse to %v", p)
	}
	// The document is as it was, and lists the field that was filled alone.
	doc := e.do("GET", "/parses/"+pid+"/document", nil)
	if !slices.Equal(doc.json(t)["fields"].([]any), []any{"repaired"}) || len(doc.body) <= len(document) {
		t.Fatalf("the document after a failed extraction: %s", doc.body)
	}
}

// TestALongDocumentIsExtractedInWindowsThroughTheAPI: a document longer
// than its extractor's input is read in windows, one call each, and the
// merged object validates against the schema and says how many windows it
// was merged from.
func TestALongDocumentIsExtractedInWindowsThroughTheAPI(t *testing.T) {
	// The invoice is 88 bytes of text in 5 blocks, 4 times a window of 22
	// bytes, and no 2 of its blocks fit one window.
	model := &textModel{maxInput: 22}
	e, _ := extracting(t, model, nil)
	pid := e.invoice()
	e.ask(pid, "invoice", invoiceSchema)
	got := e.filled(pid, "invoice", "")
	if got["state"] != "succeeded" || got["windows"] != 5.0 || model.calls() != 5 || got["attempts"] != 1.0 {
		t.Fatalf("the extraction in windows: %v after %d calls", got, model.calls())
	}
	data, err := json.Marshal(got["data"])
	if err != nil || string(data) != `{"items":[{"name":"bolt","price":3},{"name":"nut","price":4}],"number":"INV-0042","total":7}` {
		t.Fatalf("the merged object %s, %v", data, err)
	}
	if at(got, "usage", "input_tokens") != 100*got["windows"].(float64) {
		t.Fatalf("the windows used %v", got["usage"])
	}
	for _, req := range model.requests() {
		if len(req.Text) > 22 {
			t.Fatalf("a call was given %d bytes: %q", len(req.Text), req.Text)
		}
	}
}

// TestAnExtractionAskedWhileItsParseRunsWaitsForTheDocument: an extraction
// may be asked while the parse runs. It is accepted and pending, makes no
// call until the parse has ended, and is then filled from the document.
func TestAnExtractionAskedWhileItsParseRunsWaitsForTheDocument(t *testing.T) {
	model := &textModel{}
	reached, release := make(chan struct{}), make(chan struct{})
	durably(t, false)
	over.extractors, over.extractChain = map[string]reader.Extractor{"text": model}, []string{"text"}
	e := serve(t, func(_ *Server, r *run.Runner) {
		r.Workers = 1
		r.Readers, r.Chain = map[string]reader.Reader{"layout": &halted{reached: reached, release: release}}, []string{"layout"}
	})
	sub := e.do("POST", "/parses", `{"source":{"file":"`+e.upload("invoice.tiff", testfixtures.Read(t, testfixtures.MultiTIFF))+`"}}`)
	pid := sub.json(t)["id"].(string)
	<-reached
	e.ask(pid, "invoice", invoiceSchema)
	if got := e.do("GET", "/parses/"+pid+"/fields/invoice", nil).json(t); got["state"] != "pending" || got["data"] != nil || model.calls() != 0 {
		t.Fatalf("an extraction of a running parse: %v after %d calls", got, model.calls())
	}
	close(release)
	if got := e.filled(pid, "invoice", ""); got["state"] != "succeeded" || at(got, "data", "total") != 7.0 || model.calls() != 1 {
		t.Fatalf("the extraction after its parse ended: %v", got)
	}
	if p := e.ended(pid); p["state"] != "succeeded" {
		t.Fatalf("the parse ended %v", p)
	}
}

// halted is the invoice's reader, stopped at its second page until a case
// lets it go.
type halted struct {
	invoiced
	once             sync.Once
	reached, release chan struct{}
}

func (h *halted) ReadPage(ctx context.Context, page reader.Page) (reader.Result, error) {
	if page.Number == 2 {
		h.once.Do(func() { close(h.reached) })
		<-h.release
	}
	return h.invoiced.ReadPage(ctx, page)
}

// TestAnExtractionIsHeldToTheExtractorsItsCallerMayName: an extraction asks
// parse.create about its stored parse, and the allow's readers are the
// extractors its caller may name. One outside them is refused with
// reader_not_permitted before anything is queued, and an extraction that
// names none follows the routing policy. With no extractor configured
// there is nothing to ask.
func TestAnExtractionIsHeldToTheExtractorsItsCallerMayName(t *testing.T) {
	model := &textModel{}
	rec := &recorder{answer: func(q authz.Request) (authz.Decision, error) {
		if q.Action == authorizer.ActionParseCreate && q.Resource.ID != "" {
			return authz.Decision{Allow: true, Limits: json.RawMessage(`{"readers":["layout"]}`)}, nil
		}
		return authz.Decision{Allow: true}, nil
	}}
	e, _ := extracting(t, model, func(s *Server, _ *run.Runner) { s.Authz = access.NewAuthorizer(rec, configured()) })
	pid := e.invoice()
	rec.take()
	got := e.do("POST", "/parses/"+pid+"/fields", `{"name":"pinned","schema":`+totalSchema+`,"extractor":"text"}`)
	if got.status != http.StatusForbidden || got.code(t) != "reader_not_permitted" || fieldOf(t, got) != "extractor" {
		t.Fatalf("an extractor outside the allow: %d %s", got.status, got.body)
	}
	if asked := rec.take(); len(asked) != 1 || asked[0].Action != authorizer.ActionParseCreate || asked[0].Resource.ID != pid {
		t.Fatalf("the extraction asked %+v", asked)
	}
	e.ask(pid, "routed", totalSchema)
	if f := e.filled(pid, "routed", ""); f["state"] != "succeeded" || model.calls() != 1 {
		t.Fatalf("an extraction that names no extractor: %v", f)
	}

	durably(t, false)
	bare := serve(t, nil)
	other := bare.parsed(bare.upload("scan.png", sheet(t)), "")["id"].(string)
	if got := bare.do("POST", "/parses/"+other+"/fields", `{"name":"a","schema":`+totalSchema+`}`); got.status != http.StatusBadRequest || got.code(t) != "reader_not_found" {
		t.Fatalf("no extractor: %d %s", got.status, got.body)
	}
	if listed := bare.do("GET", "/parses/"+other+"/fields", nil); listed.status != http.StatusOK || len(listed.json(t)["fields"].([]any)) != 0 {
		t.Fatalf("the extractions of a parse nobody asked: %d %s", listed.status, listed.body)
	}
}

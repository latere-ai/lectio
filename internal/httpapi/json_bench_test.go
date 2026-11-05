// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/assemble"
	"latere.ai/x/lectio/internal/store"
)

// benchSink is a response writer that takes a write deadline and a flush,
// as the server's connection does, and keeps what it was sent.
type benchSink struct {
	header http.Header
	body   bytes.Buffer
}

func (s *benchSink) Header() http.Header         { return s.header }
func (s *benchSink) WriteHeader(int)             {}
func (s *benchSink) Write(p []byte) (int, error) { return s.body.Write(p) }
func (s *benchSink) Flush()                      {}
func (s *benchSink) SetWriteDeadline(time.Time) error {
	return nil
}

// benchPages is a document of n pages of blocks each, read and numbered.
func benchPages(n, blocks int) []document.Page {
	out := make([]document.Page, n)
	for p := range out {
		page := document.Page{Number: p + 1, Width: 595, Height: 842, State: document.PageSucceeded, Source: document.SourceReader,
			Reader: "default", Model: "reader-model-2026-03", Attempts: 1}
		for o := 1; o <= blocks; o++ {
			kind := document.KindText
			switch {
			case o == 1:
				kind = document.KindHeading
			case o%7 == 0:
				kind = document.KindListItem
			}
			x, y := 0.08+float64(o%3)*0.01, float64(o%20)*0.045
			b := document.Block{
				Ref: document.Ref(p+1, o), Kind: kind, Order: o, Box: &document.Box{x, y, 0.92 - x, y + 0.04},
				Text: fmt.Sprintf("Clause %d.%d. The supplier shall deliver the goods named in the order within 30 days of its date, and invoices are payable net 45.", p+1, o),
			}
			if kind == document.KindHeading {
				b.Level = 2
			}
			page.Blocks = append(page.Blocks, b)
		}
		out[p] = page
	}
	return out
}

// benchSizes are the block counts that make about 0.5 KB, 10 KB and 200 KB
// of blocks.
var benchSizes = []struct {
	name   string
	blocks int
}{{"small", 3}, {"medium", 55}, {"large", 1100}}

// BenchmarkJSONBlockListMarshal is the response of a blocks listing: one
// JSON object per block, one line each.
func BenchmarkJSONBlockListMarshal(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			blocks := benchPages(1, size.blocks)[0].Blocks
			var sink bytes.Buffer
			encode := func() error {
				sink.Reset()
				for _, block := range blocks {
					raw, err := json.Marshal(block)
					if err != nil {
						return err
					}
					sink.Write(raw)
					sink.WriteByte('\n')
				}
				return nil
			}
			if err := encode(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(sink.Len()))
			for b.Loop() {
				if err := encode(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONChunkListMarshal is the response of a chunks listing: the
// document cut into chunks by section, one JSON object per line.
func BenchmarkJSONChunkListMarshal(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			pages := benchPages(1, size.blocks)
			chunks := assemble.Chunks(pages, assemble.BySection, defaultChunk)
			if len(chunks) == 0 {
				b.Fatal("no chunks")
			}
			var sink bytes.Buffer
			encode := func() error {
				sink.Reset()
				for _, chunk := range chunks {
					raw, err := json.Marshal(chunk)
					if err != nil {
						return err
					}
					sink.Write(raw)
					sink.WriteByte('\n')
				}
				return nil
			}
			if err := encode(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(sink.Len()))
			for b.Loop() {
				if err := encode(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONDocumentIndexEncode is the document index a parse answers
// with: its pages without their blocks, the outline and the usage, written
// by the shared JSON writer.
func BenchmarkJSONDocumentIndexEncode(b *testing.B) {
	for _, size := range []struct {
		name  string
		pages int
	}{{"small", 3}, {"medium", 60}, {"large", 1200}} {
		b.Run(size.name, func(b *testing.B) {
			doc := document.Document{Parse: "prs_01JQ3Y7K8M2N4P6R8T0V2X4Z6B", Usage: document.Usage{Pages: size.pages, InputTokens: int64(size.pages) * 1800, OutputTokens: int64(size.pages) * 900}}
			for _, p := range benchPages(size.pages, 12) {
				doc.Pages = append(doc.Pages, p.Summary())
				doc.Outline = append(doc.Outline, document.Heading{Ref: document.Ref(p.Number, 1), Level: 2, Text: fmt.Sprintf("Section %d. Delivery and payment terms", p.Number), Page: p.Number})
			}
			rec := httptest.NewRecorder()
			httpjson.Write(rec, http.StatusOK, doc)
			want := rec.Body.Len()
			b.ReportAllocs()
			b.SetBytes(int64(want))
			for b.Loop() {
				rec := &benchSink{header: http.Header{}}
				httpjson.Write(rec, http.StatusOK, doc)
				if rec.body.Len() != want {
					b.Fatal("the index changed size")
				}
			}
		})
	}
}

// BenchmarkJSONParseEventStreamEncode is the progress of a parse sent as
// server-sent events: one page event per page that settled, then the
// parse's progress and state.
func BenchmarkJSONParseEventStreamEncode(b *testing.B) {
	for _, size := range []struct {
		name  string
		pages int
	}{{"small", 3}, {"medium", 100}, {"large", 500}} {
		b.Run(size.name, func(b *testing.B) {
			started := time.Date(2026, 3, 14, 9, 30, 0, 0, time.UTC)
			ev := store.Events{
				Seq: int64(size.pages) + 2,
				Parse: store.Parse{
					ID: "prs_01JQ3Y7K8M2N4P6R8T0V2X4Z6B", Owner: "usr_01JQ3Y7K8M", State: "running", File: "fil_01JQ3Y7K8M2N4P6R8T0V2X4Z6B",
					Class: "standard", Reader: "default", Languages: []string{"en"}, Stage: "reading",
					PagesTotal: size.pages * 2, PagesDone: size.pages, CreatedAt: started, StartedAt: &started,
				},
			}
			for i := 1; i <= size.pages; i++ {
				ev.Pages = append(ev.Pages, store.PageEvent{Seq: int64(i), Page: i, State: "succeeded"})
			}
			send := func() (int, error) {
				sink := &benchSink{header: http.Header{}}
				stream := &eventStream{w: sink, rc: http.NewResponseController(sink)}
				if _, err := stream.send(ev); err != nil {
					return 0, err
				}
				return sink.body.Len(), nil
			}
			n, err := send()
			if err != nil || n == 0 {
				b.Fatalf("%d bytes, %v", n, err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(n))
			for b.Loop() {
				if _, err := send(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONSubmitParseRequestDecode is the body of a submit decoded by
// the handlers' request codec, which refuses members the contract does not
// name.
func BenchmarkJSONSubmitParseRequestDecode(b *testing.B) {
	labels := make([]string, 0, maxLabels)
	for i := range maxLabels {
		labels = append(labels, fmt.Sprintf(`"label-%02d":"value of the label %d for the contract batch"`, i, i))
	}
	for _, c := range []struct {
		name string
		body string
	}{
		{"small", `{"source":{"file":"fil_01JQ3Y7K8M2N4P6R8T0V2X4Z6B"},"pages":"1-12","languages":["en"]}`},
		{"medium", `{"source":{"file":"fil_01JQ3Y7K8M2N4P6R8T0V2X4Z6B"},"origin":{"store":"vault","path":"contracts/2026/q1/supply-agreement.pdf","version":"7"},` +
			`"pages":"1-12,20-48","reader":"default","languages":["en","de","fr"],"class":"standard","priority":5,"allow_failed_pages":2,"deadline":"15m","reuse":true,` +
			`"labels":{` + strings.Join(labels, ",") + `}}`},
	} {
		b.Run(c.name, func(b *testing.B) {
			decodeOnce := func() error {
				req := httptest.NewRequest(http.MethodPost, "/v1/parses", strings.NewReader(c.body))
				var out parseRequest
				_, err := decode(httptest.NewRecorder(), req, &out)
				return err
			}
			if err := decodeOnce(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(c.body)))
			for b.Loop() {
				if err := decodeOnce(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

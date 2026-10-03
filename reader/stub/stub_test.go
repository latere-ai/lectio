// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stub

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/reader"
)

func TestReaderIsAFunctionOfThePage(t *testing.T) {
	r := &Reader{}
	page := reader.Page{Number: 4, Data: []byte("page-bytes"), MediaType: "image/png"}

	first, err := r.ReadPage(context.Background(), page)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.ReadPage(context.Background(), page)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("two reads of one page differ: %+v, %+v (%v)", first, second, err)
	}

	if len(first.Blocks) != 3 || first.Model != Name || first.Usage != (document.Usage{Pages: 1, InputTokens: 10, OutputTokens: 3}) {
		t.Fatalf("result = %+v", first)
	}
	title, text, number := first.Blocks[0], first.Blocks[1], first.Blocks[2]
	if title.Kind != document.KindTitle || title.Text != "Page 4" || title.Level != 1 || title.Box == nil {
		t.Fatalf("title = %+v", title)
	}
	if want := "10 bytes of image/png, sha256 " + Digest(page.Data); text.Text != want {
		t.Fatalf("text = %q, want %q", text.Text, want)
	}
	if number.Kind != document.KindPageNumber || number.Text != "4" {
		t.Fatalf("number = %+v", number)
	}
	if r.Calls(4) != 2 || r.Calls(5) != 0 {
		t.Fatalf("calls: page 4 %d, page 5 %d", r.Calls(4), r.Calls(5))
	}
	if d := r.Describe(); d.Name != Name || !d.Boxes || len(d.Accepts) != 2 {
		t.Fatalf("Describe() = %+v", d)
	}
}

func TestReaderFailsOnDemand(t *testing.T) {
	r := &Reader{Fail: func(p reader.Page) error {
		if p.Number == 2 {
			return reader.Errorf(reader.RateLimited, "slow down")
		}
		return nil
	}}
	if _, err := r.ReadPage(context.Background(), reader.Page{Number: 1}); err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if _, err := r.ReadPage(context.Background(), reader.Page{Number: 2}); reader.ClassOf(err) != reader.RateLimited {
		t.Fatalf("page 2: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ReadPage(ctx, reader.Page{Number: 3}); err == nil || r.Calls(3) != 1 {
		t.Fatalf("a canceled read fails and is still counted: %v, %d", err, r.Calls(3))
	}
}

func TestExtractor(t *testing.T) {
	x := Extractor{}
	schema := json.RawMessage(`{"type":"object","properties":{"total":{"type":"string"},"date":{"type":"string"}}}`)

	got, err := x.Extract(context.Background(), reader.ExtractRequest{Schema: schema, Text: "[2.3] Total 12.50\n[2.4] more", Citations: true})
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]string
	if err := json.Unmarshal(got.Data, &data); err != nil || data["total"] != "Total 12.50" || data["date"] != "Total 12.50" {
		t.Fatalf("data = %s (%v)", got.Data, err)
	}
	if want := map[string][]string{"/total": {"2.3"}, "/date": {"2.3"}}; !reflect.DeepEqual(got.Citations, want) {
		t.Fatalf("citations = %v, want %v", got.Citations, want)
	}
	if got.Model != Name || got.Constrained || x.Describe().Name != Name {
		t.Fatalf("result = %+v", got)
	}

	plain, err := x.Extract(context.Background(), reader.ExtractRequest{Schema: schema, Text: "no ref on this line"})
	if err != nil || plain.Citations != nil {
		t.Fatalf("without citations asked for there are none: %+v, %v", plain, err)
	}
	unreffed, err := x.Extract(context.Background(), reader.ExtractRequest{Schema: schema, Text: "[unclosed line", Citations: true})
	if err != nil || len(unreffed.Citations) != 0 {
		t.Fatalf("a line without a ref cites nothing: %+v, %v", unreffed, err)
	}

	if _, err := x.Extract(context.Background(), reader.ExtractRequest{Schema: json.RawMessage("{")}); reader.ClassOf(err) != reader.Permanent {
		t.Fatalf("a schema that is not JSON: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := x.Extract(ctx, reader.ExtractRequest{Schema: schema}); err == nil {
		t.Fatal("a canceled extraction must fail")
	}
}

var (
	_ reader.Reader    = (*Reader)(nil)
	_ reader.Extractor = Extractor{}
)

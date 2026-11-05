// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/lectio/reader"
)

// canned is a transport that answers every request with one body and keeps
// the size of the last request, so a chat call is measured without a socket.
type canned struct {
	reply   []byte
	request int
}

func (c *canned) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	c.request = len(raw)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(c.reply)),
		Request:    req,
	}, nil
}

// completion wraps the model's text into a chat completions reply, as an
// endpoint sends it.
func benchCompletion(b *testing.B, content string) []byte {
	b.Helper()
	raw, err := json.Marshal(map[string]any{
		"id":      "chatcmpl-9f2c41d0a7",
		"object":  "chat.completion",
		"created": 1773500000,
		"model":   "reader-model-2026-03",
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": content},
		}},
		"usage": map[string]any{"prompt_tokens": 1840, "completion_tokens": 912, "total_tokens": 2752},
	})
	if err != nil {
		b.Fatal(err)
	}
	return raw
}

// benchPageReply is what a model returns for a page of n blocks.
func benchPageReply(n int) string {
	var s strings.Builder
	s.WriteString(`{"blocks":[`)
	for i := range n {
		if i > 0 {
			s.WriteByte(',')
		}
		kind := "text"
		switch {
		case i == 0:
			kind = "title"
		case i%9 == 4:
			kind = "list_item"
		case i%17 == 8:
			kind = "table"
		}
		x, y := 80+(i%3)*10, (i*23)%880
		fmt.Fprintf(&s, `{"kind":%q,"text":"Section %d. The supplier shall deliver the goods named in the order within 30 days of its date, and invoices are payable net 45.","description":null,"box":[%d,%d,%d,%d],"level":%s}`,
			kind, i, x, y, 900-x, y+20, map[bool]string{true: "1", false: "null"}[i == 0])
	}
	s.WriteString(`]}`)
	return s.String()
}

// BenchmarkJSONPageReadRoundTrip is one page read through a chat
// completions endpoint: the request with the page image encoded in it, the
// reply decoded, and the model's own JSON decoded out of the reply.
func BenchmarkJSONPageReadRoundTrip(b *testing.B) {
	for _, size := range []struct {
		name   string
		image  int
		blocks int
	}{
		{"small", 200, 3},
		{"medium", 7 << 10, 40},
		{"large", 150 << 10, 110},
	} {
		b.Run(size.name, func(b *testing.B) {
			image := bytes.Repeat([]byte("\x89PNG\r\n\x1a\nlectio-page-pixels-"), size.image/16+1)[:size.image]
			rt := &canned{reply: benchCompletion(b, benchPageReply(size.blocks))}
			r, err := NewReader(Config{Name: "default", Endpoint: "http://chat.invalid/v1", Model: "reader-model", Constrain: true})
			if err != nil {
				b.Fatal(err)
			}
			r.c.http = &http.Client{Transport: rt}
			page := reader.Page{Number: 3, Data: image, MediaType: "image/png", Width: 1240, Height: 1754,
				Languages: []string{"en", "de"}, Credential: reader.NewCredential("sk-bench")}

			got, err := r.ReadPage(context.Background(), page)
			if err != nil || len(got.Blocks) != size.blocks {
				b.Fatalf("%d blocks, %v", len(got.Blocks), err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(rt.request + len(rt.reply)))
			for b.Loop() {
				if _, err := r.ReadPage(context.Background(), page); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONPageReplyFencedDecode is the model's JSON when it comes
// wrapped in a code fence with LaTeX written in single backslashes, which
// is decoded as it arrived, then repaired, then decoded again.
func BenchmarkJSONPageReplyFencedDecode(b *testing.B) {
	for _, size := range []struct {
		name   string
		blocks int
	}{{"small", 3}, {"medium", 40}, {"large", 110}} {
		b.Run(size.name, func(b *testing.B) {
			content := "```json\n" + strings.Replace(benchPageReply(size.blocks),
				`"text":"Section 1.`, `"text":"$\frac{a}{b} = \beta$ Section 1.`, 1) + "\n```"
			if wire, err := decodeBlocks(content, false); err != nil || len(wire) != size.blocks {
				b.Fatalf("%d blocks, %v", len(wire), err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(content)))
			for b.Loop() {
				if _, err := decodeBlocks(content, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

const benchExtractSchema = `{"type":"object","additionalProperties":false,"required":["invoice_number","total","lines"],"properties":{"invoice_number":{"type":"string"},"total":{"type":"number"},"supplier":{"type":"object","properties":{"name":{"type":"string"},"vat":{"type":"string"}}},"lines":{"type":"array","items":{"type":"object","required":["description","quantity","unit_price"],"properties":{"description":{"type":"string"},"quantity":{"type":"number"},"unit_price":{"type":"number"}}}}}}`

// BenchmarkJSONExtractRoundTrip is one extraction window sent to a
// constrained endpoint: the request with the document text and the derived
// schema, the reply, the data and citations decoded, and the data encoded
// again with its nulls dropped.
func BenchmarkJSONExtractRoundTrip(b *testing.B) {
	for _, size := range []struct {
		name  string
		text  int
		lines int
	}{
		{"small", 500, 3},
		{"medium", 10 << 10, 60},
		{"large", 200 << 10, 400},
	} {
		b.Run(size.name, func(b *testing.B) {
			var text strings.Builder
			for i := 0; text.Len() < size.text; i++ {
				fmt.Fprintf(&text, "[%d.%d] Line %d: hydraulic coupling assembly, quantity %d, unit price %d.50 EUR\n", 1+i/40, 1+i%40, i, 1+i%9, 10+i%300)
			}
			var data, cites strings.Builder
			data.WriteString(`{"invoice_number":"INV-2026-004217","total":48213.75,"supplier":{"name":"Hallmark Industrial Supplies GmbH","vat":null},"lines":[`)
			cites.WriteString(`[{"pointer":"/total","refs":["1.1","1.2"]}`)
			for i := 0; i < size.lines; i++ {
				if i > 0 {
					data.WriteByte(',')
				}
				fmt.Fprintf(&data, `{"description":"Hydraulic coupling assembly, series %d","quantity":%d,"unit_price":%d.50}`, i, 1+i%9, 10+i%300)
				fmt.Fprintf(&cites, `,{"pointer":"/lines/%d/description","refs":["%d.%d"]}`, i, 1+i/40, 1+i%40)
			}
			data.WriteString(`]}`)
			cites.WriteString(`]`)
			reply := `{"data":` + data.String() + `,"citations":` + cites.String() + `}`

			rt := &canned{reply: benchCompletion(b, reply)}
			x, err := NewExtractor(Config{Name: "text", Endpoint: "http://chat.invalid/v1", Model: "text-model", Constrain: true, MaxInput: 1 << 20})
			if err != nil {
				b.Fatal(err)
			}
			x.c.http = &http.Client{Transport: rt}
			in := reader.ExtractRequest{
				Schema: json.RawMessage(benchExtractSchema), Instructions: "Amounts are in euros.", Citations: true, Constrain: true,
				Text: text.String(), Credential: reader.NewCredential("sk-bench"),
			}
			if _, err := x.Extract(context.Background(), in); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(rt.request + len(rt.reply)))
			for b.Loop() {
				if _, err := x.Extract(context.Background(), in); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

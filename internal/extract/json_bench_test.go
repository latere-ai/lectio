// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"encoding/json/jsontext"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// benchSchema is the schema of an invoice extraction: a header and a list
// of line items, the shape the extraction fields of a parse are asked for in.
const benchSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["invoice_number", "issued", "supplier", "total", "lines"],
  "properties": {
    "invoice_number": {"type": "string"},
    "issued": {"type": "string"},
    "supplier": {"type": "object", "properties": {"name": {"type": "string"}, "vat": {"type": "string"}, "address": {"type": "string"}}},
    "total": {"type": "number"},
    "currency": {"type": "string"},
    "lines": {"type": "array", "items": {"type": "object", "required": ["description", "quantity", "unit_price"],
      "properties": {"description": {"type": "string"}, "quantity": {"type": "number"}, "unit_price": {"type": "number"}, "tax_rate": {"type": "number"}}}}
  }
}`

// benchSizes are the line counts that make an extraction reply of about
// 0.5 KB, 10 KB and 200 KB.
var benchSizes = []struct {
	name  string
	lines int
}{
	{"small", 3},
	{"medium", 90},
	{"large", 1800},
}

// benchInvoice is the reply of one extraction window with n line items.
func benchInvoice(n int, offset int) []byte {
	var b strings.Builder
	b.WriteString(`{"invoice_number":"INV-2026-004217","issued":"2026-03-14","supplier":{"name":"Hallmark Industrial Supplies GmbH","vat":"DE298374651","address":"Industriestrasse 14, 80939 Muenchen, Germany"},"total":48213.75,"currency":"EUR","lines":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"description":"Hydraulic coupling assembly, series %d, stainless steel, 3/4 inch","quantity":%d,"unit_price":%d.%02d,"tax_rate":0.19}`,
			offset+i, 1+i%12, 20+i%400, (i*7)%100)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// BenchmarkJSONExtractionReplyCheck is the validation of a model's
// extraction reply against the caller's schema: the decode with every
// number narrowed, then the validator.
func BenchmarkJSONExtractionReplyCheck(b *testing.B) {
	schema, err := Compile([]byte(benchSchema))
	if err != nil {
		b.Fatal(err)
	}
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			data := benchInvoice(size.lines, 0)
			if found := schema.Check(data, false); len(found) != 0 {
				b.Fatalf("the reply does not satisfy the schema: %v", found)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if found := schema.Check(data, false); len(found) != 0 {
					b.Fatal(found)
				}
			}
		})
	}
}

// BenchmarkJSONExtractionReplyDecode is the decode alone, which every
// reply and every compiled schema goes through.
func BenchmarkJSONExtractionReplyDecode(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			data := benchInvoice(size.lines, 0)
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if _, flaws, err := decode(data, MaxValueDepth); err != nil || len(flaws) > 0 {
					b.Fatal(err, flaws)
				}
			}
		})
	}
}

// BenchmarkJSONExtractionWindowsMerge is the join of the replies to the
// windows of one long document: each part is decoded, merged and the
// result encoded again.
func BenchmarkJSONExtractionWindowsMerge(b *testing.B) {
	// The join compares every item with those before it, so a reply of
	// 200 KB is not one window of a real extraction; small and medium are.
	for _, size := range []struct {
		name  string
		lines int
	}{{"small", 8}, {"medium", 60}} {
		b.Run(size.name, func(b *testing.B) {
			windows := 4
			parts := make([]Part, windows)
			total := 0
			for i := range parts {
				data := benchInvoice(size.lines/windows, i*size.lines)
				citations := map[string][]string{"/total": {"1." + strconv.Itoa(i+1)}}
				for j := 0; j < size.lines/windows; j += 10 {
					citations["/lines/"+strconv.Itoa(j)+"/description"] = []string{strconv.Itoa(i+1) + "." + strconv.Itoa(j+1)}
				}
				parts[i] = Part{Data: jsontext.Value(data), Citations: citations}
				total += len(data)
			}
			if out := Merge(parts); len(out.Data) == 0 {
				b.Fatal("the merge is empty")
			}
			b.ReportAllocs()
			b.SetBytes(int64(total))
			for b.Loop() {
				if out := Merge(parts); len(out.Data) == 0 {
					b.Fatal("the merge is empty")
				}
			}
		})
	}
}

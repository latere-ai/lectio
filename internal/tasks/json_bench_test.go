// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/document"
)

// benchPageResult is the stored result of one page of n blocks, as a
// worker reports it in a settle.
func benchPageResult(b *testing.B, page, n int) json.RawMessage {
	b.Helper()
	p := document.Page{Number: page, Width: 595, Height: 842, State: document.PageSucceeded, Source: document.SourceReader,
		Reader: "default", Model: "reader-model-2026-03", Attempts: 1,
		Usage: &document.Usage{Pages: 1, InputTokens: 1840, OutputTokens: 912}}
	for o := 1; o <= n; o++ {
		y := float64(o%20) * 0.045
		p.Blocks = append(p.Blocks, document.Block{
			Ref: document.Ref(page, o), Kind: document.KindText, Order: o, Box: &document.Box{0.08, y, 0.92, y + 0.04},
			Text: fmt.Sprintf("Clause %d.%d. The supplier shall deliver the goods named in the order within 30 days of its date, and invoices are payable net 45.", page, o),
		})
	}
	raw, err := json.Marshal(struct {
		Revision int           `json:"revision"`
		Image    string        `json:"image"`
		Page     document.Page `json:"page"`
	}{1, fmt.Sprintf("parses/prs_01JQ3Y7K8M/pages/%d.png", page), p})
	if err != nil {
		b.Fatal(err)
	}
	return raw
}

// benchRequest is a worker's request to the server: the settles of the
// page tasks it finished, the leases it holds, and what it can take next.
func benchRequest(b *testing.B, settles, blocks int) Request {
	b.Helper()
	req := Request{Free: 4, Kinds: []Kind{Page, Extract, Figure}}
	for i := 1; i <= settles; i++ {
		s := Settle{
			Parse: "prs_01JQ3Y7K8M2N4P6R8T0V2X4Z6B", Task: PageID(i), Token: int64(1000 + i), Outcome: Done,
			Usage:  Usage{Calls: 1, InputTokens: 1840, OutputTokens: 912},
			Units:  1,
			Health: Healthy,
			Result: benchPageResult(b, i, blocks),
		}
		if i%11 == 0 {
			s.Outcome, s.RetryAfter, s.Result = Retryable, 1500*time.Millisecond, nil
			s.Error = &Error{Code: "reader_rate_limited", Detail: "the reader answered 429 and asked for a wait"}
		}
		req.Settles = append(req.Settles, s)
		req.Held = append(req.Held, Held{Parse: s.Parse, Task: PageID(settles + i), Token: int64(2000 + i)})
	}
	return req
}

var benchRequestSizes = []struct {
	name            string
	settles, blocks int
}{{"small", 1, 1}, {"medium", 2, 17}, {"large", 28, 35}}

// BenchmarkJSONWorkerRequestMarshal is the message a worker sends to the
// server when it settles its tasks, with each page's result inside it.
func BenchmarkJSONWorkerRequestMarshal(b *testing.B) {
	for _, size := range benchRequestSizes {
		b.Run(size.name, func(b *testing.B) {
			req := benchRequest(b, size.settles, size.blocks)
			raw, err := json.Marshal(req)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for b.Loop() {
				if _, err := json.Marshal(req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONWorkerRequestUnmarshal is the server reading that message,
// then checking it.
func BenchmarkJSONWorkerRequestUnmarshal(b *testing.B) {
	for _, size := range benchRequestSizes {
		b.Run(size.name, func(b *testing.B) {
			raw, err := json.Marshal(benchRequest(b, size.settles, size.blocks))
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for b.Loop() {
				var got Request
				if err := json.Unmarshal(raw, &got); err != nil {
					b.Fatal(err)
				}
				if err := got.Validate(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchReply is the server's answer: the claims it leases to the worker,
// each with the context its task is run in.
func benchReply(claims, pages int) Reply {
	selected := make([]string, pages)
	for i := range selected {
		selected[i] = fmt.Sprint(i + 1)
	}
	manifest := json.RawMessage(`{"pages_total":` + fmt.Sprint(pages) + `,"selected":[` + strings.Join(selected, ",") + `],"media_type":"application/pdf","work":"parses/prs_01JQ3Y7K8M/work.pdf","token":1001}`)
	until := time.Date(2026, 3, 14, 9, 30, 5, 0, time.UTC)
	reply := Reply{SleepUntil: &until, Lost: []Ref{}, Refused: []Ref{}}
	for i := 1; i <= claims; i++ {
		reply.Claims = append(reply.Claims, Claim{
			Parse: "prs_01JQ3Y7K8M2N4P6R8T0V2X4Z6B", Task: PageID(i), Kind: Page, Token: int64(3000 + i),
			Group: "grp_01JQ3Y7K8M", Project: "prj_01JQ3Y7K8M", Attempt: 1, Reader: "default",
			Context: Context{
				Owner: "usr_01JQ3Y7K8M", File: &Source{Key: "files/fil_01JQ3Y7K8M2N4P6R8T0V2X4Z6B", Name: "supply-agreement.pdf", MediaType: "application/pdf"},
				Pages: "1-" + fmt.Sprint(pages), Languages: []string{"en", "de"}, Manifest: manifest, Reuse: "reuse/prs_01JQ3Y7K8M",
			},
		})
	}
	return reply
}

var benchReplySizes = []struct {
	name          string
	claims, pages int
}{{"small", 1, 2}, {"medium", 10, 60}, {"large", 230, 40}}

// BenchmarkJSONLeaseReplyMarshal is the server's answer to a worker that
// asks for work.
func BenchmarkJSONLeaseReplyMarshal(b *testing.B) {
	for _, size := range benchReplySizes {
		b.Run(size.name, func(b *testing.B) {
			reply := benchReply(size.claims, size.pages)
			raw, err := json.Marshal(reply)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for b.Loop() {
				if _, err := json.Marshal(reply); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONLeaseReplyUnmarshal is the worker reading that answer.
func BenchmarkJSONLeaseReplyUnmarshal(b *testing.B) {
	for _, size := range benchReplySizes {
		b.Run(size.name, func(b *testing.B) {
			raw, err := json.Marshal(benchReply(size.claims, size.pages))
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for b.Loop() {
				var got Reply
				if err := json.Unmarshal(raw, &got); err != nil {
					b.Fatal(err)
				}
				if len(got.Claims) != size.claims {
					b.Fatalf("%d claims", len(got.Claims))
				}
			}
		})
	}
}

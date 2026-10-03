// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package stub is a reader and an extractor that make no call. What they
// return is a function of what they are given and nothing else, so a test
// can state the result before the run, and a development server can parse
// a file with no model configured. Their output describes the input; it is
// not a reading of it.
package stub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/reader"
)

// Name is the name both stubs describe themselves with.
const Name = "stub"

// Reader returns the same three blocks for every page: a title naming the
// page, a line of text holding a digest of the page's bytes, and a page
// number. A test that knows the bytes knows the blocks.
type Reader struct {
	// Fail, when set, is called before each read; a non-nil error is
	// returned as the read's error. Tests use it to make a page fail, wait,
	// or be rate limited.
	Fail func(page reader.Page) error

	mu    sync.Mutex
	calls map[int]int
}

// Describe says the stub takes images and returns boxes.
func (r *Reader) Describe() reader.Description {
	return reader.Description{
		Name:    Name,
		Accepts: []string{"image/png", "image/jpeg"},
		Image:   reader.ImageSpec{DPI: 72, Format: "png"},
		Boxes:   true,
	}
}

// ReadPage returns the stub's blocks for the page.
func (r *Reader) ReadPage(ctx context.Context, page reader.Page) (reader.Result, error) {
	r.mu.Lock()
	if r.calls == nil {
		r.calls = map[int]int{}
	}
	r.calls[page.Number]++
	r.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return reader.Result{}, reader.FromTransport(err)
	}
	if r.Fail != nil {
		if err := r.Fail(page); err != nil {
			return reader.Result{}, err
		}
	}
	blocks := reader.Normalize([]reader.Raw{
		{Label: "title", Text: fmt.Sprintf("Page %d", page.Number), Box: []float64{100, 80, 900, 160}, Level: 1},
		{Label: "text", Text: fmt.Sprintf("%d bytes of %s, sha256 %s", len(page.Data), page.MediaType, Digest(page.Data)), Box: []float64{100, 200, 900, 300}},
		{Label: "page_number", Text: fmt.Sprint(page.Number), Box: []float64{460, 940, 540, 980}},
	}, reader.Grid{Width: 1000, Height: 1000})
	return reader.Result{
		Blocks: blocks,
		Model:  Name,
		Usage:  document.Usage{Pages: 1, InputTokens: int64(len(page.Data)), OutputTokens: 3},
	}, nil
}

// Calls reports how many times a page was read, which is what a test of
// retries and restarts counts.
func (r *Reader) Calls(page int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[page]
}

// Digest is the short digest the stub writes into a page's text.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// Extractor fills every property a schema's root declares with the first
// line of the text, and cites that line's ref. It reads no more of the
// schema than its root properties.
type Extractor struct{}

// Describe says the stub has no input bound and constrains nothing.
func (Extractor) Describe() reader.ExtractorDescription {
	return reader.ExtractorDescription{Name: Name}
}

// Extract returns the stub's object for the schema.
func (Extractor) Extract(ctx context.Context, in reader.ExtractRequest) (reader.ExtractResult, error) {
	if err := ctx.Err(); err != nil {
		return reader.ExtractResult{}, reader.FromTransport(err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(in.Schema, &schema); err != nil {
		return reader.ExtractResult{}, reader.Errorf(reader.Permanent, "the schema is not JSON")
	}

	line, _, _ := strings.Cut(in.Text, "\n")
	ref, value := "", strings.TrimSpace(line)
	if rest, found := strings.CutPrefix(value, "["); found {
		if r, v, closed := strings.Cut(rest, "]"); closed {
			ref, value = r, strings.TrimSpace(v)
		}
	}

	data := make(map[string]string, len(schema.Properties))
	out := reader.ExtractResult{Model: Name, Usage: document.Usage{InputTokens: int64(len(in.Text)), OutputTokens: int64(len(schema.Properties))}}
	if in.Citations {
		out.Citations = map[string][]string{}
	}
	for name := range schema.Properties {
		data[name] = value
		if in.Citations && ref != "" {
			out.Citations["/"+name] = []string{ref}
		}
	}
	// A map of strings always encodes.
	out.Data, _ = json.Marshal(data)
	return out, nil
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/reader"
)

// PromptVersion names the instruction a page is read with. It is part of
// what makes two parses of the same file comparable, so a change to the
// instruction changes it.
const PromptVersion = "1"

// grid is the coordinate range the model is asked to place boxes on.
// Integers on a fixed grid are placed more reliably than fractions.
const grid = 1000

// instruction is what the model is asked for a page.
var instruction = `You are reading one page of a document. Return every region of the page as a block, in the order a person would read them.

For each block give:
- kind: one of ` + kindList + `.
- text: the region's content, transcribed exactly. Do not summarize, translate, correct, repeat, or invent text. Leave out what cannot be read. Write a table as HTML with rowspan and colspan, a formula as LaTeX, and a figure as one sentence saying what it shows.
- box: the region as [x0, y0, x1, y1] on a grid where the page is 1000 wide and 1000 high and the origin is the top left corner.
- level: the depth of a title or a heading, from 1 to 6, and null for every other kind.

Mark running headers, running footers and page numbers with their own kinds. A page with nothing on it has no blocks.

Reply with one JSON object and nothing else: {"blocks": [{"kind": "...", "text": "...", "box": [x0, y0, x1, y1], "level": null}]}`

// kindList is the closed set of kinds as the instruction lists it.
var kindList = func() string {
	names := make([]string, 0, len(document.Kinds()))
	for _, k := range document.Kinds() {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
}()

// pageSchema is the reply schema of a page, in the subset of JSON Schema
// that constrained decoding accepts: every property required, no additional
// properties, and no bounds on the box's length, which validation checks.
var pageSchema = func() json.RawMessage {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"blocks"},
		"properties": map[string]any{
			"blocks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"kind", "text", "box", "level"},
					"properties": map[string]any{
						"kind":  map[string]any{"type": "string", "enum": document.Kinds()},
						"text":  map[string]any{"type": "string"},
						"box":   map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
						"level": map[string]any{"type": []string{"integer", "null"}},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		panic(err) // the schema is a literal; it cannot fail to encode
	}
	return raw
}()

// imageTypes are the media types a chat reader takes.
var imageTypes = []string{"image/png", "image/jpeg"}

// Reader reads a page through chat completions.
type Reader struct{ c *client }

// NewReader builds a reader. It refuses a configuration with no name, no
// model, or no usable endpoint.
func NewReader(cfg Config) (*Reader, error) {
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Reader{c: c}, nil
}

// Describe says what the reader accepts and returns.
func (r *Reader) Describe() reader.Description {
	accepts := slices.Clone(imageTypes)
	if r.c.cfg.Image.Format == "jpeg" {
		slices.Reverse(accepts)
	}
	return reader.Description{Name: r.c.cfg.Name, Accepts: accepts, Image: r.c.cfg.Image, Boxes: true}
}

// wireBlock is one block as the model writes it.
type wireBlock struct {
	Kind  string    `json:"kind"`
	Text  string    `json:"text"`
	Box   []float64 `json:"box"`
	Level *int      `json:"level"`
}

// ReadPage sends the page to the model and returns its blocks.
func (r *Reader) ReadPage(ctx context.Context, page reader.Page) (reader.Result, error) {
	if !slices.Contains(imageTypes, page.MediaType) {
		return reader.Result{}, reader.Errorf(reader.Permanent, "a chat reader takes a PNG or a JPEG, and the page is %s", page.MediaType)
	}
	ask := instruction
	if len(page.Languages) > 0 {
		ask += "\n\nThe page is most likely written in: " + strings.Join(page.Languages, ", ") + "."
	}
	req := request{
		Model:     r.c.cfg.Model,
		MaxTokens: r.c.cfg.MaxOutputTokens,
		Messages: []message{{Role: "user", Content: []part{
			{Type: "text", Text: ask},
			{Type: "image_url", ImageURL: &imageURL{URL: "data:" + page.MediaType + ";base64," + base64.StdEncoding.EncodeToString(page.Data)}},
		}}},
	}
	if r.c.cfg.Constrain {
		req.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: jsonSchema{Name: "page", Strict: true, Schema: pageSchema}}
	}

	done, err := r.c.complete(ctx, page.Credential, req)
	if err != nil {
		return reader.Result{}, err
	}
	wire, err := decodeBlocks(done.content)
	if err != nil {
		return reader.Result{}, err
	}

	raws := make([]reader.Raw, 0, len(wire))
	for _, w := range wire {
		raw := reader.Raw{Label: w.Kind, Text: w.Text, Box: w.Box}
		if w.Level != nil {
			raw.Level = *w.Level
		}
		raws = append(raws, raw)
	}
	blocks := reader.Normalize(raws, reader.Grid{Width: grid, Height: grid})
	if done.truncated && len(blocks) > 0 {
		last := &blocks[len(blocks)-1]
		last.Flags = append(last.Flags, document.FlagTruncated)
	}

	usage := done.usage
	usage.Pages = 1
	return reader.Result{Blocks: blocks, Model: done.model, Usage: usage, Truncated: done.truncated}, nil
}

// decodeBlocks reads the model's reply. The instruction asks for an object
// holding the blocks; a model that returns the bare list is read too.
func decodeBlocks(content string) ([]wireBlock, error) {
	var wrapped struct {
		Blocks *[]wireBlock `json:"blocks"`
	}
	if decode(content, &wrapped) == nil && wrapped.Blocks != nil {
		return *wrapped.Blocks, nil
	}
	var bare []wireBlock
	if decode(content, &bare) == nil {
		return bare, nil
	}
	return nil, reader.Errorf(reader.Invalid, "the reply is not the JSON the instruction asks for")
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package chat

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/prompts"
	"latere.ai/x/lectio/reader"
)

// grid is the coordinate range a model is asked to place boxes on when it
// is not asked for pixels. Integers on a fixed grid are placed more
// reliably than fractions.
const grid = 1000

// kinds is the closed set of kinds, by name, as the page prompt lists it.
var kinds = func() []string {
	names := make([]string, 0, len(document.Kinds()))
	for _, k := range document.Kinds() {
		names = append(names, string(k))
	}
	return names
}()

// ask is what the page prompt is rendered with for a configuration:
// everything but the page's languages.
func ask(cfg Config) prompts.PageData {
	d := prompts.PageData{Kinds: kinds, Grid: grid, Pixels: cfg.Boxes.Space == SpacePixels, BoxOrder: []string{"x0", "y0", "x1", "y1"}}
	if cfg.Boxes.Order == OrderYX {
		d.BoxOrder = []string{"y0", "x0", "y1", "x1"}
	}
	return d
}

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
					"required":             []string{"kind", "text", "description", "box", "level"},
					"properties": map[string]any{
						"kind":        map[string]any{"type": "string", "enum": document.Kinds()},
						"text":        map[string]any{"type": "string"},
						"description": map[string]any{"type": []string{"string", "null"}},
						"box":         map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
						"level":       map[string]any{"type": []string{"integer", "null"}},
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
	return reader.Description{Name: r.c.cfg.Name, Accepts: accepts, Image: r.c.cfg.Image, Boxes: true, Version: r.version()}
}

// version names what this reader's configuration decides about a page's
// result: the model, the prompt as this reader asks it, whether the reply
// is constrained, how the page is rendered, and the temperature.
func (r *Reader) version() string {
	cfg := r.c.cfg
	temperature := "default"
	if cfg.Temperature != nil {
		temperature = strconv.FormatFloat(*cfg.Temperature, 'g', -1, 64)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		cfg.Model, prompts.PageVersion(ask(cfg)), strconv.FormatBool(cfg.Constrain), temperature,
		strconv.Itoa(cfg.Image.DPI), strconv.Itoa(cfg.Image.LongEdge), cfg.Image.Format,
	}, "\x00")))
	return hex.EncodeToString(sum[:6])
}

// wireBlock is one block as the model writes it.
type wireBlock struct {
	Kind        string    `json:"kind"`
	Text        string    `json:"text"`
	Description *string   `json:"description"`
	Box         []float64 `json:"box"`
	Level       *int      `json:"level"`
}

// ReadPage sends the page to the model and returns its blocks.
func (r *Reader) ReadPage(ctx context.Context, page reader.Page) (reader.Result, error) {
	if !slices.Contains(imageTypes, page.MediaType) {
		return reader.Result{}, reader.Errorf(reader.Permanent, "a chat reader takes a PNG or a JPEG, and the page is %s", page.MediaType)
	}
	cfg := r.c.cfg
	data := ask(cfg)
	data.Languages = page.Languages
	instruction, err := prompts.Page(data)
	if err != nil {
		return reader.Result{}, &reader.Error{Class: reader.Misconfigured, Detail: "the page prompt does not render", Err: err}
	}
	req := r.c.request([]part{
		{Type: "text", Text: instruction},
		{Type: "image_url", ImageURL: &imageURL{URL: "data:" + page.MediaType + ";base64," + base64.StdEncoding.EncodeToString(page.Data)}},
	})
	if cfg.Constrain {
		req.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: jsonSchema{Name: "page", Strict: true, Schema: pageSchema}}
	}

	done, err := r.c.complete(ctx, page.Credential, req)
	if err != nil {
		return reader.Result{}, err
	}
	wire, err := decodeBlocks(done.content, done.truncated)
	if err != nil {
		return reader.Result{}, err
	}

	// The numbers come back in the order and the space this reader asked
	// for; the object model takes x before y and a fraction of the page.
	space := reader.Grid{Width: grid, Height: grid}
	if cfg.Boxes.Space == SpacePixels {
		space = reader.Grid{Width: float64(page.Width), Height: float64(page.Height)}
	}
	raws := make([]reader.Raw, 0, len(wire))
	for _, w := range wire {
		raw := reader.Raw{Label: w.Kind, Text: w.Text, Box: w.Box}
		if cfg.Boxes.Order == OrderYX && len(raw.Box) == 4 {
			raw.Box = []float64{raw.Box[1], raw.Box[0], raw.Box[3], raw.Box[2]}
		}
		if w.Description != nil {
			raw.Description = *w.Description
		}
		if w.Level != nil {
			raw.Level = *w.Level
		}
		raws = append(raws, raw)
	}
	blocks := reader.Normalize(raws, space)
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
//
// A reply that was cut at the output limit is not JSON any more. What it
// holds up to its last complete block is still a reading of the top of the
// page, so that much is kept and the page is marked as cut, and the rest
// of the page is not silently taken for read.
func decodeBlocks(content string, truncated bool) ([]wireBlock, error) {
	wire, err := decodeWhole(content)
	if err != nil && truncated {
		wire, err = decodeWhole(complete(content))
	}
	if err != nil {
		return nil, err
	}
	// A formula written with single backslashes decodes without an error
	// into control characters. When that shows, the reply is decoded again
	// with its backslashes repaired.
	if slices.ContainsFunc(wire, func(w wireBlock) bool { return corrupt(w.Text) }) {
		if repaired, rerr := decodeWhole(repairEscapes(content)); rerr == nil {
			return repaired, nil
		}
		if repaired, rerr := decodeWhole(complete(repairEscapes(content))); rerr == nil && truncated {
			return repaired, nil
		}
	}
	return wire, nil
}

func decodeWhole(content string) ([]wireBlock, error) {
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

// complete closes a reply that was cut inside its list of blocks: it keeps
// the reply up to the last block that was written whole and closes what was
// open. A reply with no whole block comes back as it was.
func complete(content string) string {
	depth, inString, escaped := 0, false, false
	list, end, closing := -1, -1, ""
	for i := 0; i < len(content); i++ {
		c := content[i]
		switch {
		case escaped:
			escaped = false
		case inString:
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '[' && list < 0:
			// The first list is the list of blocks, at the top of a bare
			// reply or one object down in a wrapped one.
			list = depth
			closing = "]" + strings.Repeat("}", depth)
			depth++
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
			if c == '}' && list >= 0 && depth == list+1 {
				end = i + 1
			}
		}
	}
	if end < 0 {
		return content
	}
	return content[:end] + closing
}

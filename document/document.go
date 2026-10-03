// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package document is the object model a parse produces: a document is
// pages, a page is blocks, and a block is one region of a page with one
// kind, a position, and its text. Everything that reads a page, natively or
// through a model, returns these types, and the HTTP API serves them as
// they are. The design is specs/002-object-model.md.
package document

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Kind is what a block is. The set is closed: a reader's label outside it
// becomes KindText, and adding a kind is a change to the spec.
type Kind string

// The kinds a block may have.
const (
	KindTitle      Kind = "title"
	KindHeading    Kind = "heading"
	KindText       Kind = "text"
	KindListItem   Kind = "list_item"
	KindTable      Kind = "table"
	KindFigure     Kind = "figure"
	KindFormula    Kind = "formula"
	KindForm       Kind = "form"
	KindKeyValue   Kind = "key_value"
	KindCaption    Kind = "caption"
	KindFootnote   Kind = "footnote"
	KindPageHeader Kind = "page_header"
	KindPageFooter Kind = "page_footer"
	KindPageNumber Kind = "page_number"
	KindSignature  Kind = "signature"
	KindBarcode    Kind = "barcode"
	KindCode       Kind = "code"
)

var kinds = []Kind{
	KindTitle, KindHeading, KindText, KindListItem, KindTable, KindFigure,
	KindFormula, KindForm, KindKeyValue, KindCaption, KindFootnote,
	KindPageHeader, KindPageFooter, KindPageNumber, KindSignature,
	KindBarcode, KindCode,
}

// Kinds returns every kind, in the order the spec lists them.
func Kinds() []Kind { return slices.Clone(kinds) }

// Valid reports whether k is in the set.
func (k Kind) Valid() bool { return slices.Contains(kinds, k) }

// CoerceKind maps a label a reader returned onto the set. A label outside it
// becomes KindText and ok is false, so the caller can flag the block.
func CoerceKind(label string) (kind Kind, ok bool) {
	k := Kind(strings.ToLower(strings.TrimSpace(label)))
	if k.Valid() {
		return k, true
	}
	return KindText, false
}

// Textual reports whether a block of this kind is running text: the kinds
// the header and footer pass and the renderings treat as prose.
func (k Kind) Textual() bool {
	switch k {
	case KindTitle, KindHeading, KindText, KindListItem, KindCaption,
		KindFootnote, KindPageHeader, KindPageFooter, KindPageNumber:
		return true
	}
	return false
}

// Box is a region of a page as [x0, y0, x1, y1], each a fraction of the
// page's width or height, with the origin at the top left. One coordinate
// system for every source is what lets a client draw an overlay without
// knowing how the page was rendered.
type Box [4]float64

// Valid reports whether the box lies inside the page and has an area.
func (b Box) Valid() bool {
	for _, v := range b {
		if v < 0 || v > 1 {
			return false
		}
	}
	return b[0] < b[2] && b[1] < b[3]
}

// Normalize repairs a box a reader returned: corners given in the wrong
// order are swapped and coordinates outside the page are clamped onto it.
// changed reports whether anything moved. ok is false when nothing usable is
// left, which is a box with no area.
func (b Box) Normalize() (out Box, changed, ok bool) {
	out = b
	if out[0] > out[2] {
		out[0], out[2] = out[2], out[0]
	}
	if out[1] > out[3] {
		out[1], out[3] = out[3], out[1]
	}
	for i, v := range out {
		out[i] = min(max(v, 0), 1)
	}
	return out, out != b, out.Valid()
}

// Flag records what validation did to a block, so a caller can tell a block
// a reader returned as it is from one that was repaired.
type Flag string

// The flags validation sets.
const (
	// FlagBoxClamped: the box was outside the page or inverted and was repaired.
	FlagBoxClamped Flag = "box_clamped"
	// FlagKindCoerced: the reader's label was outside the set and became text.
	FlagKindCoerced Flag = "kind_coerced"
	// FlagTruncated: the reader's reply ended at its output limit.
	FlagTruncated Flag = "truncated"
)

// Block is one region of a page.
type Block struct {
	// Ref addresses the block for the life of the parse: "<page>.<order>".
	Ref  string `json:"ref"`
	Kind Kind   `json:"kind"`
	// Order is the reading order within the page, dense from 1.
	Order int `json:"order"`
	// Box is nil when the position is unknown, which is every block of a
	// page whose format carried its own structure.
	Box *Box `json:"box"`
	// Text is what is printed in the region, as plain text. For a table it
	// is the cells joined row by row; for a figure it is the words printed
	// inside it. Text is transcription and nothing else.
	Text string `json:"text"`
	// Description is what a reader says a figure shows. It is the reader's
	// own prose, never transcription, so it is kept apart from Text: it is
	// not what the document says and is never cited as such.
	Description string `json:"description,omitempty"`
	// Level is the heading depth, 1 to 6, on a title or a heading.
	Level int `json:"level,omitempty"`
	// Table is the structure of a block of KindTable.
	Table *Table `json:"table,omitempty"`
	// Repeated marks a running header or footer after its first occurrence.
	Repeated bool   `json:"repeated,omitempty"`
	Flags    []Flag `json:"flags,omitempty"`
}

// Table is the structure of a table block. HTML is the markup the reader or
// the native extractor produced, kept verbatim because it is the one form
// that carries merged cells without loss.
type Table struct {
	Rows  int    `json:"rows"`
	Cols  int    `json:"cols"`
	Cells []Cell `json:"cells,omitempty"`
	HTML  string `json:"html,omitempty"`
}

// Cell is one cell of a table. Row and Col count from 0.
type Cell struct {
	Row     int `json:"row"`
	Col     int `json:"col"`
	RowSpan int `json:"row_span,omitempty"`
	ColSpan int `json:"col_span,omitempty"`
	// Header marks a cell of a header row or column.
	Header bool   `json:"header,omitempty"`
	Text   string `json:"text"`
	Box    *Box   `json:"box,omitempty"`
}

// Ref renders a block's address.
func Ref(page, order int) string {
	return strconv.Itoa(page) + "." + strconv.Itoa(order)
}

// ParseRef reads a block's address back into its page and order.
func ParseRef(ref string) (page, order int, err error) {
	p, o, found := strings.Cut(ref, ".")
	if !found {
		return 0, 0, fmt.Errorf("ref %q is not <page>.<order>", ref)
	}
	page, perr := strconv.Atoi(p)
	order, oerr := strconv.Atoi(o)
	if perr != nil || oerr != nil || page < 1 || order < 1 {
		return 0, 0, fmt.Errorf("ref %q is not <page>.<order>", ref)
	}
	return page, order, nil
}

// Number puts a page's blocks in reading order and gives them their
// addresses: it sorts by the order the reader gave, keeping the reader's
// sequence for equal values, then renumbers densely from 1 and sets Ref.
// It returns the same slice.
func Number(page int, blocks []Block) []Block {
	slices.SortStableFunc(blocks, func(a, b Block) int { return a.Order - b.Order })
	for i := range blocks {
		blocks[i].Order = i + 1
		blocks[i].Ref = Ref(page, i+1)
	}
	return blocks
}

// PageState is where a page is in its life.
type PageState string

// The states of a page.
const (
	PagePending   PageState = "pending"
	PageSucceeded PageState = "succeeded"
	PageFailed    PageState = "failed"
)

// PageSource says who produced a page's blocks.
type PageSource string

// The sources of a page's blocks.
const (
	// SourceReader: a model read the page from its image.
	SourceReader PageSource = "reader"
	// SourceNative: the file format carried its own structure.
	SourceNative PageSource = "native"
)

// Error is why a page, a field, or a parse failed: a code a caller branches
// on and a sentence for a developer.
type Error struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// Usage is what a piece of work consumed. Cost and Currency are set only
// when the model endpoint reported a cost.
type Usage struct {
	Pages        int    `json:"pages,omitempty"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	Cost         string `json:"cost,omitempty"`
	Currency     string `json:"currency,omitempty"`
}

// Add returns the sum of two usages. Costs are left to whoever knows the
// currency: a sum of decimal strings is not this package's to compute.
func (u Usage) Add(v Usage) Usage {
	u.Pages += v.Pages
	u.InputTokens += v.InputTokens
	u.OutputTokens += v.OutputTokens
	return u
}

// Page is one page, sheet, slide, or image frame, in source order.
type Page struct {
	// Number counts from 1.
	Number int `json:"number"`
	// Width and Height are in points (1/72 inch) for paged formats and in
	// pixels for images. Boxes do not depend on them.
	Width    float64    `json:"width"`
	Height   float64    `json:"height"`
	Rotation int        `json:"rotation,omitempty"`
	State    PageState  `json:"state"`
	Source   PageSource `json:"source,omitempty"`
	// Reader and Model name what read the page; both are empty on a native page.
	Reader   string `json:"reader,omitempty"`
	Model    string `json:"model,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	// Truncated reports that the reader's reply ended at its output limit:
	// the blocks are what it finished, and the page may hold more.
	Truncated bool    `json:"truncated,omitempty"`
	Blocks    []Block `json:"blocks"`
	Usage     *Usage  `json:"usage,omitempty"`
	Error     *Error  `json:"error,omitempty"`
}

// Validate reports the first thing about a page that the model forbids: a
// number below 1, a block whose kind is outside the set, an order that is
// not dense from 1, a ref that does not match its page and order, a box
// outside the page, a table on a block that is not a table, or blocks on a
// page that did not succeed.
func (p Page) Validate() error {
	if p.Number < 1 {
		return fmt.Errorf("page number %d is below 1", p.Number)
	}
	if p.State != PageSucceeded && len(p.Blocks) > 0 {
		return fmt.Errorf("page %d is %s and holds %d blocks", p.Number, p.State, len(p.Blocks))
	}
	for i, b := range p.Blocks {
		if err := b.validate(p.Number, i+1); err != nil {
			return fmt.Errorf("page %d: %w", p.Number, err)
		}
	}
	return nil
}

func (b Block) validate(page, order int) error {
	switch {
	case !b.Kind.Valid():
		return fmt.Errorf("block %d has unknown kind %q", order, b.Kind)
	case b.Order != order:
		return fmt.Errorf("block %d has order %d", order, b.Order)
	case b.Ref != Ref(page, order):
		return fmt.Errorf("block %d has ref %q", order, b.Ref)
	case b.Box != nil && !b.Box.Valid():
		return fmt.Errorf("block %s has a box outside the page or without area", b.Ref)
	case b.Level < 0 || b.Level > 6:
		return fmt.Errorf("block %s has level %d", b.Ref, b.Level)
	case b.Table != nil && b.Kind != KindTable:
		return fmt.Errorf("block %s is %s and holds a table", b.Ref, b.Kind)
	}
	return nil
}

// Span is a table that continues across pages, as a join of the table
// blocks that hold its parts. The parts stay where they are.
type Span struct {
	ID    string   `json:"id"`
	Parts []string `json:"parts"`
	Rows  int      `json:"rows"`
	Cols  int      `json:"cols"`
}

// Heading is one entry of a document's outline.
type Heading struct {
	Ref   string `json:"ref"`
	Level int    `json:"level"`
	Text  string `json:"text"`
	Page  int    `json:"page"`
}

// Chunk is a run of blocks rendered as text, for retrieval.
type Chunk struct {
	ID     string   `json:"id"`
	Text   string   `json:"text"`
	Pages  []int    `json:"pages"`
	Blocks []string `json:"blocks"`
}

// FieldState is where an extraction is: asked for and not done yet, or how
// it ended.
type FieldState string

// The states of a field.
const (
	FieldPending   FieldState = "pending"
	FieldSucceeded FieldState = "succeeded"
	FieldFailed    FieldState = "failed"
)

// Field is the result of one extraction: an object in the shape of the
// caller's schema, and for each value the blocks it was read from.
type Field struct {
	Name  string          `json:"name"`
	State FieldState      `json:"state"`
	Data  json.RawMessage `json:"data,omitempty"`
	// Citations maps a JSON pointer into Data to the refs of the blocks the
	// value came from.
	Citations map[string][]string `json:"citations,omitempty"`
	Model     string              `json:"model,omitempty"`
	// Constrained reports whether the schema was enforced by the model's
	// decoding and not only stated in the prompt.
	Constrained bool   `json:"constrained"`
	Attempts    int    `json:"attempts,omitempty"`
	Windows     int    `json:"windows,omitempty"`
	Usage       *Usage `json:"usage,omitempty"`
	Error       *Error `json:"error,omitempty"`
}

// PageSummary is a page without its blocks, as the document index lists it.
type PageSummary struct {
	Number int        `json:"number"`
	State  PageState  `json:"state"`
	Source PageSource `json:"source,omitempty"`
	Blocks int        `json:"blocks"`
	Error  *Error     `json:"error,omitempty"`
}

// Summary reduces a page to its entry in the document index.
func (p Page) Summary() PageSummary {
	return PageSummary{Number: p.Number, State: p.State, Source: p.Source, Blocks: len(p.Blocks), Error: p.Error}
}

// Document is the index of a parse's result: what assembly found across the
// pages. It lists the pages without their blocks; a page's blocks are read
// by page, which is what keeps a long document out of memory.
type Document struct {
	Parse   string        `json:"parse"`
	Pages   []PageSummary `json:"pages"`
	Spans   []Span        `json:"spans,omitempty"`
	Outline []Heading     `json:"outline,omitempty"`
	Usage   Usage         `json:"usage"`
	// Renderings and Fields name what else the parse wrote: "markdown",
	// "text", and the names of the extractions.
	Renderings []string `json:"renderings,omitempty"`
	Fields     []string `json:"fields,omitempty"`
}

// ErrNoBlock is returned by Find when a page holds no block with the ref.
var ErrNoBlock = errors.New("no such block")

// Find returns the block of a page with the given ref.
func (p Page) Find(ref string) (Block, error) {
	page, order, err := ParseRef(ref)
	if err != nil {
		return Block{}, err
	}
	if page != p.Number || order > len(p.Blocks) {
		return Block{}, ErrNoBlock
	}
	return p.Blocks[order-1], nil
}

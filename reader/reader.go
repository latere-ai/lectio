// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package reader is the seam between Lectio and whatever reads a page or
// fills a schema: a vision-language model, an OCR engine, a text model.
// Nothing else in the module knows that a model exists.
//
// There are three interfaces, one per kind of outbound call. A Reader
// turns one page into blocks. An Extractor turns a document's text into an
// object in the shape of a schema. A Describer says what one figure shows.
// All are stateless: one call in, one result out, with no memory of the
// document and no knowledge of queues, tenants, or storage. That is what
// makes a model a line of configuration and a page the unit of work.
//
// An adapter is an implementation of one or more interfaces over one wire
// format. The module ships 4: chat, for any endpoint that speaks
// OpenAI-compatible chat completions with image input; layout, for an OCR
// engine behind a small HTTP contract of its own; text, which reads a page
// from the text its file carries and calls nothing; and stub, which is
// deterministic and makes no call. An adapter for another engine lives
// outside this module and needs nothing but this package and document.
//
// The design is specs/008-readers.md.
package reader

import (
	"context"
	"encoding/json"
	"log/slog"

	"latere.ai/x/lectio/document"
)

// Reader turns one page into blocks.
type Reader interface {
	// Describe says what the reader accepts and what it returns. It is
	// constant for the life of the reader and makes no call: the caller
	// reads it to prepare the page and to know what to expect back.
	Describe() Description

	// ReadPage reads one page. The page's Data is in one of the media types
	// Describe accepts, prepared the way Describe asks.
	//
	// The blocks come back in reading order with their kind, text, and box,
	// already brought onto the object model by Normalize. Refs are not set:
	// the caller numbers the page. An error is one of this package's
	// classes (see Error), so the caller never inspects a status code.
	ReadPage(ctx context.Context, page Page) (Result, error)
}

// Description is what a reader accepts and returns.
type Description struct {
	// Name is the reader's configured name, the one a parse pins and a page
	// result records.
	Name string `json:"name"`

	// Accepts lists the media types of Page.Data the reader takes, most
	// preferred first: "image/png", "image/jpeg", or "application/pdf" for
	// a reader that takes a PDF holding exactly one page.
	Accepts []string `json:"accepts"`

	// Image says how a page is rendered for this reader when it takes
	// images. The image the caller stores is the image the reader saw.
	Image ImageSpec `json:"image"`

	// Boxes reports whether the reader returns a position for each block.
	// A reader that returns text alone still fits: its blocks have no box.
	Boxes bool `json:"boxes"`

	// Kinds lists the kinds the reader can return. Empty means any.
	Kinds []document.Kind `json:"kinds,omitempty"`

	// Version names everything in the reader's configuration that changes
	// what it returns for the same page: its model, its prompt, how it
	// asks for boxes, its parameters. Two readers with the same version
	// read a page the same way, which is what lets an earlier parse stand
	// in for a new one. A reader that cannot say leaves it empty, and its
	// pages are never reused.
	Version string `json:"version,omitempty"`

	// Text reports that the reader reads what the file itself holds of a
	// page: the caller hands it Page.Text for a page of a format that
	// carries its text, beside the image.
	Text bool `json:"text,omitempty"`
}

// ImageSpec is how a page is rendered into an image.
type ImageSpec struct {
	// DPI is the resolution a paged format is rendered at.
	DPI int `json:"dpi"`
	// LongEdge bounds the longer side in pixels; a larger render is scaled
	// down to it. Zero is no bound.
	LongEdge int `json:"long_edge,omitempty"`
	// Format is "png" or "jpeg".
	Format string `json:"format"`
}

// Page is one page as a reader receives it.
type Page struct {
	// Number is the page's number in its document, from 1. It is passed for
	// logs and traces; a reader must not depend on it.
	Number int

	// Data is the page: an image, or a PDF holding exactly this page.
	Data      []byte
	MediaType string

	// Width and Height are the image's size in pixels, zero for a PDF.
	Width, Height int

	// Languages are hints, most likely first. May be empty.
	Languages []string

	// Text is what the file itself holds of the page, for a reader whose
	// description asks for it. It is nil for every other reader, for a
	// format that carries no text of its own, such as an image, and for a
	// page whose text could not be read within the bounds a page is held
	// to.
	Text *PageText

	// Credential is the key this call is made with. It decides who the
	// model endpoint charges, so it arrives per call and is never part of
	// a reader's configuration.
	Credential Credential
}

// Result is what a reader returns for one page.
type Result struct {
	// Blocks are in reading order, with Order set from 1 and Ref empty.
	Blocks []document.Block

	// Model is what the endpoint says answered, which may differ from what
	// was asked for.
	Model string

	// Usage is what the call consumed. Pages is 1.
	Usage document.Usage

	// Truncated reports that the reply ended at the model's output limit,
	// so the page's last block may be cut short.
	Truncated bool

	// TextLayer reports that the blocks were built from Page.Text alone
	// and no model was called: the page's characters are the file's own,
	// and its structure is what their positions and type show. The page
	// then says so as its source.
	TextLayer bool
}

// Extractor turns a document's text into an object in the shape of a
// schema, and says which blocks each value came from.
type Extractor interface {
	// Describe says what the extractor accepts. It makes no call.
	Describe() ExtractorDescription

	// Extract fills the schema from the text. The result's Data is the
	// model's object as it returned it: the caller validates it against the
	// schema and decides whether to ask again.
	Extract(ctx context.Context, req ExtractRequest) (ExtractResult, error)
}

// ExtractorDescription is what an extractor accepts.
type ExtractorDescription struct {
	Name string `json:"name"`

	// MaxInput bounds the text of one call, in characters. A document longer
	// than this is extracted in windows by the caller. Zero is no bound.
	MaxInput int `json:"max_input,omitempty"`

	// Constrained reports whether the extractor can have the model's
	// decoding enforce a schema, and not only state it in the prompt.
	Constrained bool `json:"constrained"`
}

// ExtractRequest is one extraction call.
type ExtractRequest struct {
	// Schema is the caller's JSON Schema, whose root is an object.
	Schema json.RawMessage

	// Instructions are the caller's guidance for the model. May be empty.
	Instructions string

	// Text is the document, or one window of it, in reading order. Each
	// block begins with its ref in brackets, which is what a citation
	// names. A block may run over several lines, as a table does.
	Text string

	// Citations asks for the refs each value was read from.
	Citations bool

	// Constrain asks the extractor to enforce the schema in decoding. The
	// caller sets it only for a schema that constrained decoding supports.
	Constrain bool

	// Previous is the reply an earlier attempt at the same request gave,
	// and Problems are a validator's findings on it. Set, they ask the
	// model to repair that reply: it is shown what it wrote and what was
	// wrong with it, and asked to change only that.
	Previous string
	Problems []string

	Credential Credential
}

// ExtractResult is what an extractor returns.
type ExtractResult struct {
	// Data is the object the model returned for the caller's schema.
	Data json.RawMessage

	// Citations maps a JSON pointer into Data to the refs the value was
	// read from. Refs are as the model gave them; the caller drops the
	// ones that do not exist.
	Citations map[string][]string

	Model string
	Usage document.Usage

	// Constrained reports whether the schema was enforced in decoding.
	Constrained bool
}

// Credential is the key a call is made with. Its value is reachable only
// through Reveal: printing, logging, or marshaling a Credential shows a
// placeholder, so a key cannot leak through an error, a log line, or a
// stored result by accident.
type Credential struct{ key string }

// redacted is what a Credential shows everywhere but Reveal.
const redacted = "[credential]"

// NewCredential wraps a key.
func NewCredential(key string) Credential { return Credential{key: key} }

// Reveal returns the key. Call it only where the key is put on the wire.
func (c Credential) Reveal() string { return c.key }

// IsZero reports whether no key is set.
func (c Credential) IsZero() bool { return c.key == "" }

// String shows a placeholder.
func (c Credential) String() string { return redacted }

// GoString shows a placeholder, so %#v does not print the key.
func (c Credential) GoString() string { return redacted }

// MarshalJSON writes a placeholder.
func (c Credential) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// LogValue shows a placeholder to structured logging.
func (c Credential) LogValue() slog.Value { return slog.StringValue(redacted) }

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package text reads a page from the text its file carries. It calls no
// model and no service: what it returns is a function of the words the
// file holds for the page, where each is drawn and in what type, and of
// what else the page paints (reader.PageText). A typeset PDF holds all of
// that, so a page of one costs nothing to read and its characters are the
// file's own.
//
// The reader builds paragraphs, headings where the size and the weight of
// the type say so, list items, tables whose ruling closes every cell, and
// a figure where the page paints one. It reads no meaning: it does not
// say what a figure shows, and it writes a formula as the characters the
// file holds.
//
// What it cannot read without guessing it declines, with the class
// reader.Refused, and the page goes to the next reader in the chain. A
// page it declines costs one call to that reader; a page it reads wrongly
// costs the result. So every rule below leans toward declining, and each
// bound it judges by is a constant of this package with a comment that
// says what it trades.
//
// A page is declined when:
//
//   - the file carries no text for it, or none that is drawn on it;
//   - its text was not read whole within the bounds a page is held to;
//   - a character has no Unicode mapping, or the text fails a measure of
//     being text: its share of letters and digits, the sequences a
//     double decoding leaves, the share of words without a vowel;
//   - a word is drawn at an angle;
//   - a word does not show in the page's image where the file places it:
//     it lies under a shape painted over it, or is drawn in the paper's
//     color;
//   - what the page paints beside its text takes up more of the page than
//     a figure in a page of text does, or holds more text than a figure
//     does;
//   - ruling does not close into a table, or a word lies across it;
//   - text stands side by side and is neither columns of prose nor the
//     cells of a ruled table.
package text

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/reader"
)

// Config is how a text reader is set up. It has no endpoint, no model and
// no key.
type Config struct {
	// Name is the reader's configured name.
	Name string

	// Image is how the page is rendered for the result: what a caller of
	// the result sees the page as, and what a figure is cut from. The
	// reader reads nothing off the image. It looks at it for one thing:
	// that each word shows where the file places it. Zero values take 160
	// dpi, a long edge of 2,048 pixels, and PNG.
	Image reader.ImageSpec
}

// rules names the revision of the rules a page is read by. It is part of
// the reader's version, with every bound below, so a change to how a page
// is read keeps a result of the earlier rules from standing in for it.
const rules = 1

// Reader reads a page from the text its file carries.
type Reader struct {
	cfg     Config
	version string
}

// New returns a text reader.
func New(cfg Config) (*Reader, error) {
	if cfg.Name == "" {
		return nil, errors.New("text: the reader has no name")
	}
	if cfg.Image.DPI <= 0 {
		cfg.Image.DPI = 160
	}
	if cfg.Image.LongEdge == 0 {
		cfg.Image.LongEdge = 2048
	}
	switch cfg.Image.Format {
	case "":
		cfg.Image.Format = "png"
	case "png", "jpeg":
	default:
		return nil, fmt.Errorf("text: image.format is %q, want png or jpeg", cfg.Image.Format)
	}
	// Everything that changes what the reader returns for a page: the
	// rules, the bounds they judge by, and the image the result holds.
	sum := sha256.Sum256(fmt.Appendf(nil, "text\x00%d\x00%v\x00%d\x00%d\x00%s", rules, bounds, cfg.Image.DPI, cfg.Image.LongEdge, cfg.Image.Format))
	return &Reader{cfg: cfg, version: hex.EncodeToString(sum[:8])}, nil
}

// kinds are the kinds the reader returns. It names no furniture: a running
// header and a page number are found across pages, by assembly.
var kinds = []document.Kind{
	document.KindTitle, document.KindHeading, document.KindText, document.KindListItem,
	document.KindTable, document.KindFigure, document.KindCaption, document.KindFootnote,
}

// Describe says the reader asks for the page's own text and calls no model.
func (r *Reader) Describe() reader.Description {
	accepts := []string{"image/png", "image/jpeg"}
	if r.cfg.Image.Format == "jpeg" {
		accepts = []string{"image/jpeg", "image/png"}
	}
	return reader.Description{
		Name: r.cfg.Name, Accepts: accepts, Image: r.cfg.Image, Boxes: true,
		Kinds: append([]document.Kind(nil), kinds...), Version: r.version, Text: true, Local: true,
	}
}

// ReadPage builds the page's blocks from the text its file carries, or
// declines the page.
func (r *Reader) ReadPage(ctx context.Context, page reader.Page) (reader.Result, error) {
	if err := ctx.Err(); err != nil {
		return reader.Result{}, reader.FromTransport(err)
	}
	if page.Text == nil {
		return reader.Result{}, decline("the file carries no text of its own for the page")
	}
	raws, err := read(page)
	if err != nil {
		return reader.Result{}, err
	}
	blocks := reader.Normalize(raws, reader.Grid{Width: page.Text.Width, Height: page.Text.Height})
	return reader.Result{Blocks: blocks, Usage: document.Usage{Pages: 1}, TextLayer: true}, nil
}

// decline is the error of a page this reader cannot be the one to read.
// why is one fixed sentence: it never holds anything the page says.
func decline(why string) error {
	return reader.Errorf(reader.Refused, "%s", why)
}

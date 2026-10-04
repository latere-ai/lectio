// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package figures holds what describing a parse's figures means, whoever
// runs it: which blocks of a page a run takes, what a figure's caption is,
// what names a description so that it is made once, and how a
// description is written onto its block. The in-process runner of a
// development server and the tasks of the durable one both describe
// figures through it, so the 2 servers cannot come to answer differently.
// The design is specs/003-api.md and specs/008-readers.md.
package figures

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/reader"
)

// MaxRun is the most figures one run describes. A run is started by one
// request, which finds the figures in the pages it takes, and in the
// durable server by one statement, which writes a row and a task for each
// under the lock every worker's exchange waits for. At 1,000 that is
// 2,000 rows, fewer than the task rows the prepare of a parse at the
// default limit on pages writes under the same lock, and 1,000 calls to a
// describer, which a run's deadline has time for. A parse that holds more
// is described in several runs, each over a range of its pages: a figure
// keeps its description from run to run.
const MaxRun = 1000

// Bounded refuses a run that found more figures than MaxRun, with the code
// a caller that named too many pages for one request is answered with. The
// count is of what the run would describe, so figures that already hold a
// description do not count unless the run describes them again.
func Bounded(found int) error {
	if found > MaxRun {
		return fault.New(fault.TooManyPages,
			"the pages of the run hold more than %d figures to describe, and a run describes at most %d: name fewer pages", MaxRun, MaxRun)
	}
	return nil
}

// Figure is one figure a run sets out to describe.
type Figure struct {
	// Ref is the ref of the figure's block, and Page the page it is on.
	Ref  string
	Page int
	// Box is where the figure is on its page's image.
	Box document.Box
	// Caption is the caption the figure has on its page, when it has one.
	Caption string
}

// Of lists the figures of a page a run describes: every block of kind
// figure that has a box and has no description yet. redo takes the ones
// that have one too. Whether the page has an image to cut them from is the
// caller's to know.
func Of(page document.Page, redo bool) []Figure {
	var out []Figure
	for i, b := range page.Blocks {
		if b.Kind != document.KindFigure || b.Box == nil || (b.Description != "" && !redo) {
			continue
		}
		out = append(out, Figure{Ref: b.Ref, Page: page.Number, Box: *b.Box, Caption: Caption(page.Blocks, i)})
	}
	return out
}

// Caption returns the caption of the figure at position i of a page's
// blocks: the block after it when that is a caption, else the one before.
// A figure with neither has none.
func Caption(blocks []document.Block, i int) string {
	if i+1 < len(blocks) && blocks[i+1].Kind == document.KindCaption {
		return blocks[i+1].Text
	}
	if i > 0 && blocks[i-1].Kind == document.KindCaption {
		return blocks[i-1].Text
	}
	return ""
}

// Key names what describing a figure means: the file's bytes, the page,
// the figure's place on it, its caption, the languages hinted, and every
// describer that may come to describe it, by its configured name and its
// version. 2 figures with one key are described the same, so a description
// made once stands for both. chain names the describers in the order they
// are tried, each of which is in describers. A file with no digest, and a
// describer that names no version, promise nothing, and then there is no
// key.
func Key(contentSHA string, f Figure, languages, chain []string, describers map[string]reader.Describer) string {
	if contentSHA == "" {
		return ""
	}
	parts := []string{
		contentSHA, strconv.Itoa(f.Page), fmt.Sprintf("%.5f,%.5f,%.5f,%.5f", f.Box[0], f.Box[1], f.Box[2], f.Box[3]),
		f.Caption, strings.Join(languages, ","),
	}
	for _, name := range chain {
		version := describers[name].Describe().Version
		if version == "" {
			return ""
		}
		parts = append(parts, name+"="+version)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// Description is what a describer said of one figure, as it is kept.
type Description struct {
	// Type is what kind of figure it is, one of reader's Figure constants.
	Type string `json:"type"`
	// Description says what the figure shows, in the describer's own words.
	Description string `json:"description"`
	// Labels are the words printed inside the figure, in reading order.
	Labels []string `json:"labels,omitempty"`
	// Model is what the endpoint said answered.
	Model string `json:"model,omitempty"`
}

// Kept is a describer's result as it is kept: without what the call used,
// which is the call's and not the figure's.
func Kept(res reader.FigureResult) Description {
	return Description{Type: res.Type, Description: res.Description, Labels: res.Labels, Model: res.Model}
}

// Onto writes a description onto a figure's block. The labels printed in
// the figure become the block's text when the page's reader gave it none:
// a text the reader transcribed is kept, since it came with the page.
func (d Description) Onto(b *document.Block) {
	b.Description = d.Description
	b.Figure = &document.Figure{Type: d.Type, Model: d.Model}
	if b.Text == "" {
		b.Text = strings.Join(d.Labels, "\n")
	}
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"strings"
	"unicode/utf8"

	"latere.ai/x/lectio/internal/fault"
)

// MaxWindows is the most windows one extraction reads. Each is a call to a
// model, so the bound is what a document of any length can cost: a document
// whose text does not fit fails its extraction and calls no model.
const MaxWindows = 32

// Window is one part of a document as one call is given it.
type Window struct {
	// Text is the part: its blocks, each led by its ref.
	Text string `json:"text"`
	// Refs are the refs of those blocks, which are the refs a reply to this
	// window may cite.
	Refs []string `json:"refs"`
}

// Windows cuts a document into the parts an extractor reads it in. budget
// is the most text one call takes, in bytes; zero or less is no bound, and
// the document is then one window.
//
// A window holds consecutive sections, a section being a title or a heading
// and what follows it, and as many as fit. A section that does not fit in
// what is left of a window begins the next. A section longer than a window
// is cut between 2 of its blocks, and a block longer than a window is cut
// in its text, each part led by the block's ref. A document with no text
// has no window. One that needs more than MaxWindows is refused with
// too_many_pages.
func Windows(pieces []Piece, budget int) ([]Window, error) {
	if len(pieces) == 0 {
		return nil, nil
	}
	var (
		out  []Window
		text strings.Builder
		refs []string
	)
	shut := func() {
		if text.Len() > 0 {
			out = append(out, Window{Text: text.String(), Refs: refs})
			text.Reset()
			refs = nil
		}
	}
	add := func(p Piece, l string) {
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(l)
		if len(refs) == 0 || refs[len(refs)-1] != p.Ref {
			refs = append(refs, p.Ref)
		}
	}
	// fits reports whether n more bytes, and the newline before them, fit in
	// the window that is open.
	fits := func(n int) bool {
		return budget <= 0 || text.Len() == 0 && n <= budget || text.Len() > 0 && text.Len()+1+n <= budget
	}

	for start := 0; start < len(pieces); {
		// The section that begins here ends before the next heading.
		end, size := start+1, len(line(pieces[start].Ref, pieces[start].Text))
		for ; end < len(pieces) && !pieces[end].Section; end++ {
			size += 1 + len(line(pieces[end].Ref, pieces[end].Text))
		}
		if !fits(size) {
			shut()
		}
		for _, p := range pieces[start:end] {
			l := line(p.Ref, p.Text)
			if !fits(len(l)) {
				shut()
			}
			if fits(len(l)) {
				add(p, l)
				continue
			}
			// The block alone is longer than a window: it is cut in its
			// text, at a character's boundary, and every part begins a
			// window of its own with the block's ref.
			lead := line(p.Ref, "")
			room := max(budget-len(lead), utf8.UTFMax)
			for rest := p.Text; rest != ""; {
				cut := min(room, len(rest))
				for cut < len(rest) && !utf8.RuneStart(rest[cut]) {
					cut--
				}
				add(p, lead+rest[:cut])
				rest = rest[cut:]
				if rest != "" {
					shut()
				}
			}
		}
		start = end
	}
	shut()
	if len(out) > MaxWindows {
		return nil, fault.New(fault.TooManyPages, "the document's text takes %d windows of the extractor's input, and an extraction reads at most %d", len(out), MaxWindows)
	}
	return out, nil
}

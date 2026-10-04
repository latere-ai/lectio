// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"encoding/json"
	"maps"
	"slices"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/reader"
)

// MaxRepairs is how often a reply that does not satisfy the schema is sent
// back to the model, with what was wrong with it, before the extraction
// fails. The count is per window.
const MaxRepairs = 2

// Input is the document as one extraction reads it, cut for the extractor
// that reads it. It is made once, by the first call's claim, and every
// later call reads from it, so the calls of one extraction are over one
// reading of the document whatever the pages become meanwhile.
type Input struct {
	// Extractor is the extractor the document was cut for.
	Extractor string `json:"extractor"`
	// Windows are the parts of the document, in reading order. A document
	// with no text has none.
	Windows []Window `json:"windows"`
}

// Plan cuts a document for an extractor: budget is the most text one call
// to it takes, in bytes, and zero is no bound.
func Plan(extractor string, pages []document.Page, budget int) (Input, error) {
	windows, err := Windows(Pieces(pages), budget)
	if err != nil {
		return Input{}, err
	}
	if windows == nil {
		windows = []Window{}
	}
	return Input{Extractor: extractor, Windows: windows}, nil
}

// Progress is where an extraction stands between 2 of its calls.
type Progress struct {
	// Input is the object key of the Input the extraction reads from.
	Input string `json:"input"`
	// Extractor is the extractor the calls so far were made to.
	Extractor string `json:"extractor"`

	// Parts are the accepted replies to the first windows, in order. The
	// window the next call reads is the one after them.
	Parts []Part `json:"parts"`

	// Previous is the reply the last call gave for that window when it did
	// not satisfy the schema, Problems what the validator found in it, and
	// Repairs how often that window's reply was sent back so far.
	Previous string   `json:"previous,omitempty"`
	Problems []string `json:"problems,omitempty"`
	Repairs  int      `json:"repairs,omitempty"`

	// Repaired counts the repairs over every window, Model is what the
	// endpoint said answered last, and Constrained whether the schema was
	// sent for enforcement.
	Repaired    int    `json:"repaired,omitempty"`
	Model       string `json:"model,omitempty"`
	Constrained bool   `json:"constrained,omitempty"`
}

// Summary is what a field says of how it was filled, beside its object:
// what answered, whether the schema was sent for enforcement, how many
// times the model was asked for a reply that satisfies the schema, 1 and
// each repair, and how many windows the document was read in.
type Summary struct {
	Model       string `json:"model,omitempty"`
	Constrained bool   `json:"constrained"`
	Attempts    int    `json:"attempts"`
	Windows     int    `json:"windows"`
}

// Summary is the summary of an extraction that stands where p does, over
// the windows of in. An extraction that made no call, of a document with no
// text, made no attempt.
func (p Progress) Summary(in Input) Summary {
	s := Summary{Model: p.Model, Constrained: p.Constrained, Windows: len(in.Windows)}
	if len(in.Windows) > 0 {
		s.Attempts = 1 + p.Repaired
	}
	return s
}

// Ask is what the next call of an extraction is asked, beside the schema
// and the caller's instructions: the window it reads, and, when it repairs
// a reply, that reply and what was wrong with it.
func (p Progress) Ask(in Input) (text, previous string, problems []string) {
	return in.Windows[len(p.Parts)].Text, p.Previous, p.Problems
}

// Step is what an extraction does after a call.
type Step int

// The steps.
const (
	// Next: the reply was accepted and another window follows.
	Next Step = iota
	// Repair: the reply does not satisfy the schema, and is sent back.
	Repair
	// Filled: every window was read and the object satisfies the schema.
	Filled
	// Unsatisfied: the object does not satisfy the schema and no repair is
	// left. The extraction fails with schema_not_satisfied.
	Unsatisfied
)

// Take reads the reply to the call p asked for and moves the extraction on.
// It answers what comes next, with the result when the extraction is
// filled and the validator's findings when it is not satisfied.
//
// A reply is held to the schema when it arrives. A document read in one
// window is held to all of it. A window of a longer document is held to
// everything but what another window may hold, a required member and the
// least a list or an object may hold, and the merged object is then held to
// the whole schema. A reply that fails is repaired up to MaxRepairs times
// with the same window, and one that would cost more than MaxCheckWork to
// check fails the extraction at once. A merged object that fails is not repaired: no one
// call reads the whole document, so none could mend it.
func Take(schema *Schema, in Input, p *Progress, res reader.ExtractResult) (Step, Result, []Finding) {
	window := in.Windows[len(p.Parts)]
	whole := len(in.Windows) == 1
	p.Model, p.Constrained = res.Model, res.Constrained

	if findings := schema.Check(res.Data, !whole); len(findings) > 0 {
		// An object that was not held to the schema is not sent back: the
		// cost is in the schema and the document, and a repair would be
		// another call that ends the same way.
		if p.Repairs >= MaxRepairs || unchecked(findings) {
			return Unsatisfied, Result{}, findings
		}
		p.Repairs++
		p.Repaired++
		p.Previous, p.Problems = reply(res), Problems(findings)
		return Repair, Result{}, nil
	}
	p.Parts = append(p.Parts, Part{Data: res.Data, Citations: Cited(res.Data, res.Citations, window.Refs)})
	p.Previous, p.Problems, p.Repairs = "", nil, 0
	if len(p.Parts) < len(in.Windows) {
		return Next, Result{}, nil
	}
	merged := Merge(p.Parts)
	if !whole {
		if findings := schema.Check(merged.Data, false); len(findings) > 0 {
			return Unsatisfied, Result{}, findings
		}
	}
	return Filled, merged, nil
}

// Empty is the extraction of a document with no text: no call is made, and
// the object holds nothing. It is filled when the schema asks for nothing,
// and not satisfied otherwise.
func Empty(schema *Schema) (Step, Result, []Finding) {
	empty := Result{Data: json.RawMessage(`{}`)}
	if findings := schema.Check(empty.Data, false); len(findings) > 0 {
		return Unsatisfied, Result{}, findings
	}
	return Filled, empty, nil
}

// reply is what a call returned, in the shape the model was asked to reply
// in, so a repair shows the model what it wrote.
func reply(res reader.ExtractResult) string {
	type citation struct {
		Pointer string   `json:"pointer"`
		Refs    []string `json:"refs"`
	}
	wire := struct {
		Data      json.RawMessage `json:"data"`
		Citations []citation      `json:"citations"`
	}{Data: res.Data, Citations: []citation{}}
	if !json.Valid(res.Data) {
		wire.Data = json.RawMessage(`null`)
	}
	for _, pointer := range slices.Sorted(maps.Keys(res.Citations)) {
		wire.Citations = append(wire.Citations, citation{Pointer: pointer, Refs: res.Citations[pointer]})
	}
	return string(encode(wire))
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package quality

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Class says how a corpus file comes to its blocks, which decides the bars
// its scores are held to.
type Class string

// The classes of a corpus file.
const (
	// Exact: the file is read from its own structure, with no model, so
	// what comes out is what the file holds.
	Exact Class = "exact"
	// Typeset: pages rendered from their source with no loss, read by a
	// model.
	Typeset Class = "typeset"
	// Scan: pages that are images of a page at a scanner's resolution,
	// read by a model.
	Scan Class = "scan"
	// Converted: a file an office suite converts before it is read.
	Converted Class = "converted"
)

// Bars is what the scores of a file must reach: CER at most, every other
// measure at least. A measure a truth does not have, cells with no table
// and boxes with no box, has no bar to miss.
type Bars struct {
	CER   float64 `json:"cer"`
	Kinds float64 `json:"kinds"`
	Cells float64 `json:"cells"`
	Order float64 `json:"order"`
	Boxes float64 `json:"boxes"`
}

// classes lists the classes in the order a table of bars prints them.
var classes = []Class{Exact, Typeset, Scan, Converted}

// bars are the bars of each class. They are bars for the corpus of this
// repository, taken from what a competent page-reading model reaches on
// clean typeset pages, and no claim about any other document.
var bars = map[Class]Bars{
	Exact:     {CER: 0, Kinds: 1, Cells: 1, Order: 1, Boxes: 1},
	Typeset:   {CER: 0.02, Kinds: 0.90, Cells: 0.95, Order: 0.95, Boxes: 0.90},
	Scan:      {CER: 0.05, Kinds: 0.90, Cells: 0.95, Order: 0.95, Boxes: 0.85},
	Converted: {CER: 0.02, Kinds: 0.90, Cells: 0.95, Order: 0.95, Boxes: 0.90},
}

// BarsOf returns the bars of a class. ok is false for a class nobody
// defined.
func BarsOf(c Class) (b Bars, ok bool) {
	b, ok = bars[c]
	return b, ok
}

// Miss is one measure that did not reach its bar.
type Miss struct {
	Measure string  `json:"measure"`
	Value   float64 `json:"value"`
	Bar     float64 `json:"bar"`
}

// String says the miss in one line.
func (m Miss) String() string {
	if m.Measure == "cer" {
		return fmt.Sprintf("cer %s is over %s", percent(m.Value, 2), percent(m.Bar, 2))
	}
	return fmt.Sprintf("%s %s is under %s", m.Measure, percent(m.Value, 1), percent(m.Bar, 1))
}

// Misses lists the measures of s that do not reach b.
func (b Bars) Misses(s Scores) []Miss {
	var out []Miss
	if s.CER > b.CER {
		out = append(out, Miss{"cer", s.CER, b.CER})
	}
	least := func(measure string, value *float64, bar float64) {
		if value != nil && *value < bar {
			out = append(out, Miss{measure, *value, bar})
		}
	}
	least("kinds", &s.Kinds, b.Kinds)
	least("cells", s.Cells, b.Cells)
	least("order", &s.Order, b.Order)
	least("boxes", s.Boxes, b.Boxes)
	return out
}

// BarsTable is the bars of every class as a Markdown table, the one form
// they are stated in: a report prints it and the documentation holds it.
func BarsTable() string {
	var b strings.Builder
	b.WriteString("| Class | CER at most | Kinds at least | Cells at least | Order at least | Boxes at least |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|\n")
	for _, c := range classes {
		v := bars[c]
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s |\n", c,
			percent(v.CER, 0), percent(v.Kinds, 0), percent(v.Cells, 0), percent(v.Order, 0), percent(v.Boxes, 0))
	}
	return b.String()
}

// Result is one file of a run: its scores, what reading it took, and the
// bars it missed.
type Result struct {
	File   string `json:"file"`
	Format string `json:"format"`
	Class  Class  `json:"class"`

	// Error is why the file has no scores: its parse failed or did not end.
	Error string `json:"error,omitempty"`

	Scores Scores `json:"scores"`

	// Seconds is the time from the submit of the parse to its end, and
	// SecondsPerPage that time over the pages of the file.
	Seconds        float64 `json:"seconds"`
	SecondsPerPage float64 `json:"seconds_per_page"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	// Model is what the reader's endpoint said answered.
	Model string `json:"model,omitempty"`

	Misses []Miss `json:"misses,omitempty"`
}

// Report is a run of the corpus.
type Report struct {
	// Started is when the run began, in RFC 3339.
	Started string `json:"started"`
	// Reader is the name of the reader the pages were read with, and Server
	// says which server ran the parses.
	Reader string         `json:"reader"`
	Server string         `json:"server"`
	Bars   map[Class]Bars `json:"bars"`
	Files  []Result       `json:"files"`
	// Skipped names the files of the corpus the run left out, each with
	// why, so a report of fewer files does not read as the whole corpus.
	Skipped []string `json:"skipped,omitempty"`
}

// NewReport starts a report that carries the bars it is judged by.
func NewReport(started, reader, server string) *Report {
	return &Report{Started: started, Reader: reader, Server: server, Bars: bars}
}

// Add scores one file into the report: its misses are the bars of its class
// that its scores do not reach. A file with an error has no scores and
// misses by that alone.
func (r *Report) Add(res Result) {
	if res.Error == "" {
		if b, ok := BarsOf(res.Class); ok {
			res.Misses = b.Misses(res.Scores)
		} else {
			res.Error = fmt.Sprintf("class %q has no bars", res.Class)
		}
	}
	if res.Scores.Pages > 0 {
		res.SecondsPerPage = res.Seconds / float64(res.Scores.Pages)
	}
	r.Files = append(r.Files, res)
}

// Failed lists the files that did not reach their bars, each with why.
func (r *Report) Failed() []string {
	var out []string
	for _, f := range r.Files {
		switch {
		case f.Error != "":
			out = append(out, f.File+": "+f.Error)
		case len(f.Misses) > 0:
			whys := make([]string, 0, len(f.Misses))
			for _, m := range f.Misses {
				whys = append(whys, m.String())
			}
			out = append(out, f.File+": "+strings.Join(whys, "; "))
		}
	}
	return out
}

// JSON is the report as a file of the same numbers the Markdown prints.
func (r *Report) JSON() ([]byte, error) {
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// Markdown is the report for a person: one row per file, the bars, and
// under them what each file missed and the notes of its score.
func (r *Report) Markdown() string {
	var b strings.Builder
	b.WriteString("# Quality run\n\n")
	fmt.Fprintf(&b, "Started %s. Reader `%s`. Server: %s.\n\n", r.Started, r.Reader, r.Server)
	b.WriteString("| File | Format | Class | Pages | Blocks | CER | Kinds | Cells | Order | Boxes | Seconds per page | Tokens in | Tokens out | Bars |\n")
	b.WriteString("|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|\n")
	for _, f := range r.Files {
		if f.Error != "" {
			fmt.Fprintf(&b, "| `%s` | %s | `%s` | | | | | | | | | | | failed |\n", f.File, f.Format, f.Class)
			continue
		}
		s := f.Scores
		verdict := "met"
		if len(f.Misses) > 0 {
			verdict = "missed"
		}
		fmt.Fprintf(&b, "| `%s` | %s | `%s` | %d | %d | %s | %s | %s | %s | %s | %.1f | %d | %d | %s |\n",
			f.File, f.Format, f.Class, s.Pages, s.Blocks, percent(s.CER, 2), percent(s.Kinds, 1),
			optional(s.Cells), percent(s.Order, 1), optional(s.Boxes), f.SecondsPerPage, f.InputTokens, f.OutputTokens, verdict)
	}
	fmt.Fprintf(&b, "\n%d of %d files meet their bars.\n", len(r.Files)-len(r.Failed()), len(r.Files))
	if len(r.Skipped) > 0 {
		b.WriteString("\nLeft out of this run:\n\n")
		for _, why := range r.Skipped {
			fmt.Fprintf(&b, "- %s\n", why)
		}
	}
	b.WriteByte('\n')

	b.WriteString("## Bars\n\n")
	b.WriteString(BarsTable())

	b.WriteString("\n## Misses and notes\n")
	for _, f := range r.Files {
		if f.Error == "" && len(f.Misses) == 0 && len(f.Scores.Notes) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n### `%s`\n\n", f.File)
		if f.Error != "" {
			fmt.Fprintf(&b, "- failed: %s\n", f.Error)
		}
		for _, m := range f.Misses {
			fmt.Fprintf(&b, "- missed: %s\n", m)
		}
		for _, n := range f.Scores.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	return b.String()
}

// percent writes a share as a percentage with the given decimals.
func percent(v float64, decimals int) string {
	return fmt.Sprintf("%.*f%%", decimals, 100*v)
}

// optional writes a measure a truth may not have: a dash when it has none.
func optional(v *float64) string {
	if v == nil {
		return "-"
	}
	return percent(*v, 1)
}

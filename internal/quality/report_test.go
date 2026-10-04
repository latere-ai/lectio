// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package quality

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func share1(v float64) *float64 { return &v }

func TestEveryClassHasBarsAndExactAllowsNothing(t *testing.T) {
	for _, c := range classes {
		b, ok := BarsOf(c)
		if !ok || b.CER < 0 || b.CER > 0.05 || b.Kinds < 0.9 || b.Cells < 0.95 || b.Order < 0.95 || b.Boxes < 0.85 {
			t.Errorf("%s: bars %+v, %v", c, b, ok)
		}
	}
	if len(classes) != len(bars) {
		t.Fatalf("%d classes are listed and %d have bars", len(classes), len(bars))
	}
	if b, _ := BarsOf(Exact); b != (Bars{CER: 0, Kinds: 1, Cells: 1, Order: 1, Boxes: 1}) {
		t.Fatalf("a file read from itself is held to no error: %+v", b)
	}
	if _, ok := BarsOf("guessed"); ok {
		t.Fatal("a class nobody defined has bars")
	}
}

func TestMissesAreTheMeasuresUnderTheirBar(t *testing.T) {
	typeset, _ := BarsOf(Typeset)
	for name, tc := range map[string]struct {
		scores Scores
		want   []string
	}{
		"at every bar":       {Scores{CER: 0.02, Kinds: 0.9, Order: 0.95, Cells: share1(0.95), Boxes: share1(0.9)}, nil},
		"no table, no boxes": {Scores{CER: 0.001, Kinds: 1, Order: 1}, nil},
		"every measure under": {Scores{CER: 0.0213, Kinds: 0.899, Order: 0.9, Cells: share1(0.5), Boxes: share1(0.89)}, []string{
			"cer 2.13% is over 2.00%", "kinds 89.9% is under 90.0%", "cells 50.0% is under 95.0%",
			"order 90.0% is under 95.0%", "boxes 89.0% is under 90.0%",
		}},
	} {
		var got []string
		for _, m := range typeset.Misses(tc.scores) {
			got = append(got, m.String())
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestBarsTableStatesEveryClassOnce(t *testing.T) {
	want := "| Class | CER at most | Kinds at least | Cells at least | Order at least | Boxes at least |\n" +
		"|---|---:|---:|---:|---:|---:|\n" +
		"| `exact` | 0% | 100% | 100% | 100% | 100% |\n" +
		"| `typeset` | 2% | 90% | 95% | 95% | 90% |\n" +
		"| `scan` | 5% | 90% | 95% | 95% | 85% |\n" +
		"| `converted` | 2% | 90% | 95% | 95% | 90% |\n"
	if got := BarsTable(); got != want {
		t.Fatalf("the table of bars:\n%s\nwant:\n%s", got, want)
	}
}

func TestAReportSaysWhichFilesMeetTheirBars(t *testing.T) {
	r := NewReport("2026-10-04T04:00:00Z", "engine", "durable, role all")
	r.Add(Result{
		File: "survey.pdf", Format: "typeset PDF", Class: Typeset, Seconds: 30, InputTokens: 9000, OutputTokens: 3000, Model: "some-model",
		Scores: Scores{Pages: 3, Blocks: 40, CER: 0.0041, Kinds: 0.975, Order: 1, Cells: share1(1), Boxes: share1(0.95)},
	})
	r.Add(Result{
		File: "scan.pdf", Format: "scanned PDF", Class: Scan, Seconds: 44,
		Scores: Scores{Pages: 4, Blocks: 40, CER: 0.08, Kinds: 0.85, Order: 1, Cells: share1(1), Notes: []string{`1.2 title is found as heading: "Tide"`}},
	})
	r.Add(Result{File: "notes.txt", Format: "plain text", Class: Exact, Scores: Scores{Pages: 1, Blocks: 3, Kinds: 1, Order: 1, Notes: []string{"a note on a file that met its bars"}}})
	r.Add(Result{File: "slides.pptx", Format: "presentation", Class: Converted, Error: "the parse ended failed: unsupported_media_type"})
	r.Add(Result{File: "odd.bin", Format: "unknown", Class: "guessed", Scores: Scores{Pages: 1}})
	r.Add(Result{File: "empty.txt", Format: "plain text", Class: Exact, Scores: Scores{Kinds: 1, Order: 1}})

	failed := r.Failed()
	want := []string{
		"scan.pdf: cer 8.00% is over 5.00%; kinds 85.0% is under 90.0%",
		"slides.pptx: the parse ended failed: unsupported_media_type",
		`odd.bin: class "guessed" has no bars`,
	}
	if !reflect.DeepEqual(failed, want) {
		t.Fatalf("failed = %q, want %q", failed, want)
	}
	if r.Files[0].SecondsPerPage != 10 || r.Files[1].SecondsPerPage != 11 || r.Files[5].SecondsPerPage != 0 {
		t.Fatalf("seconds per page: %v, %v, %v", r.Files[0].SecondsPerPage, r.Files[1].SecondsPerPage, r.Files[5].SecondsPerPage)
	}

	md := r.Markdown()
	for _, line := range []string{
		"Started 2026-10-04T04:00:00Z. Reader `engine`. Server: durable, role all.",
		"| `survey.pdf` | typeset PDF | `typeset` | 3 | 40 | 0.41% | 97.5% | 100.0% | 100.0% | 95.0% | 10.0 | 9000 | 3000 | met |",
		"| `scan.pdf` | scanned PDF | `scan` | 4 | 40 | 8.00% | 85.0% | 100.0% | 100.0% | - | 11.0 | 0 | 0 | missed |",
		"| `notes.txt` | plain text | `exact` | 1 | 3 | 0.00% | 100.0% | - | 100.0% | - | 0.0 | 0 | 0 | met |",
		"| `slides.pptx` | presentation | `converted` | | | | | | | | | | | failed |",
		"3 of 6 files meet their bars.",
		"| `scan` | 5% | 90% | 95% | 95% | 85% |",
		"### `scan.pdf`\n\n- missed: cer 8.00% is over 5.00%\n- missed: kinds 85.0% is under 90.0%\n- 1.2 title is found as heading: \"Tide\"\n",
		"### `notes.txt`\n\n- a note on a file that met its bars\n",
		"### `slides.pptx`\n\n- failed: the parse ended failed: unsupported_media_type\n",
	} {
		if !strings.Contains(md, line) {
			t.Errorf("the report does not hold %q:\n%s", line, md)
		}
	}
	if strings.Contains(md, "### `survey.pdf`") || strings.Contains(md, "### `empty.txt`") {
		t.Errorf("a file with nothing to say has a section:\n%s", md)
	}

	raw, err := r.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Files) != 6 || back.Reader != "engine" || back.Bars[Scan].CER != 0.05 || back.Files[0].Model != "some-model" ||
		back.Files[1].Misses[0] != (Miss{"cer", 0.08, 0.05}) || back.Files[0].Scores.Boxes == nil || back.Files[1].Scores.Boxes != nil {
		t.Fatalf("the report as JSON does not hold the same numbers:\n%s", raw)
	}
	if !strings.HasSuffix(string(raw), "}\n") {
		t.Fatal("the JSON file does not end in a newline")
	}
}

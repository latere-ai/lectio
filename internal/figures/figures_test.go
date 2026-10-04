// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package figures

import (
	"context"
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// versioned is a describer of the version a test gives it.
type versioned struct{ version string }

func (v versioned) Describe() reader.DescriberDescription {
	return reader.DescriberDescription{Version: v.version}
}

func (versioned) DescribeFigure(context.Context, reader.FigureRequest) (reader.FigureResult, error) {
	return reader.FigureResult{}, nil
}

// TestARunTakesTheFiguresThatHaveABoxAndNoDescription: of a page's blocks a
// run takes the figures that have a place on the page and that nobody
// described, each with the caption beside it: the block after it, else the
// one before. A run that describes again takes the described ones too.
func TestARunTakesTheFiguresThatHaveABoxAndNoDescription(t *testing.T) {
	box := &document.Box{0.1, 0.2, 0.5, 0.6}
	page := document.Page{Number: 3, Blocks: []document.Block{
		{Ref: "3.1", Kind: document.KindCaption, Text: "Figure 1: above."},
		{Ref: "3.2", Kind: document.KindFigure, Box: box},
		{Ref: "3.3", Kind: document.KindText, Text: "A paragraph.", Box: box},
		{Ref: "3.4", Kind: document.KindFigure, Box: box},
		{Ref: "3.5", Kind: document.KindCaption, Text: "Figure 2: below."},
		{Ref: "3.6", Kind: document.KindFigure, Box: box, Description: "Described before."},
		{Ref: "3.7", Kind: document.KindFigure},
		{Ref: "3.8", Kind: document.KindFigure, Box: box},
	}}
	got := Of(page, false)
	if len(got) != 3 || got[0] != (Figure{Ref: "3.2", Page: 3, Box: *box, Caption: "Figure 1: above."}) ||
		got[1].Ref != "3.4" || got[1].Caption != "Figure 2: below." || got[2].Ref != "3.8" || got[2].Caption != "" {
		t.Fatalf("a run takes %+v", got)
	}
	// A caption that follows a figure is that figure's, and the one before
	// it is taken only when none follows.
	if again := Of(page, true); len(again) != 4 || again[2].Ref != "3.6" || again[2].Caption != "Figure 2: below." {
		t.Fatalf("a run that describes again takes %+v", again)
	}
	for name, tc := range map[string]struct {
		blocks []document.Block
		want   string
	}{
		"the caption after it":  {[]document.Block{{Kind: document.KindCaption, Text: "before"}, {Kind: document.KindFigure}, {Kind: document.KindCaption, Text: "after"}}, "after"},
		"the caption before it": {[]document.Block{{Kind: document.KindCaption, Text: "before"}, {Kind: document.KindFigure}, {Kind: document.KindText, Text: "body"}}, "before"},
		"no caption":            {[]document.Block{{Kind: document.KindText}, {Kind: document.KindFigure}}, ""},
	} {
		if got := Caption(tc.blocks, 1); got != tc.want {
			t.Errorf("%s: %q", name, got)
		}
	}
}

// TestAKeyNamesWhatDescribingAFigureMeans: the same bytes, page, place,
// caption, languages and describers have one key, a change in any of them
// has another, and a file with no digest or a describer with no version
// has none.
func TestAKeyNamesWhatDescribingAFigureMeans(t *testing.T) {
	f := Figure{Ref: "3.2", Page: 3, Box: document.Box{0.1, 0.2, 0.5, 0.6}, Caption: "Figure 1"}
	describers := map[string]reader.Describer{
		"vision": versioned{"v1"}, "newer": versioned{"v2"}, "second": versioned{"v1"}, "silent": versioned{""},
	}
	chain := []string{"vision"}
	key := Key("abc", f, []string{"de"}, chain, describers)
	if len(key) != 64 || key != Key("abc", f, []string{"de"}, chain, describers) {
		t.Fatalf("the key is %q", key)
	}
	moved, captioned, paged := f, f, f
	moved.Box[0], captioned.Caption, paged.Page = 0.11, "Figure 2", 4
	for name, other := range map[string]string{
		"other bytes":       Key("abd", f, []string{"de"}, chain, describers),
		"another place":     Key("abc", moved, []string{"de"}, chain, describers),
		"another caption":   Key("abc", captioned, []string{"de"}, chain, describers),
		"another page":      Key("abc", paged, []string{"de"}, chain, describers),
		"other languages":   Key("abc", f, []string{"en"}, chain, describers),
		"another version":   Key("abc", f, []string{"de"}, []string{"newer"}, describers),
		"another describer": Key("abc", f, []string{"de"}, []string{"second"}, describers),
		"a longer chain":    Key("abc", f, []string{"de"}, []string{"vision", "second"}, describers),
	} {
		if other == key || other == "" {
			t.Errorf("%s has the key %q", name, other)
		}
	}
	// The ref is the block's address and no part of what is described.
	renamed := f
	renamed.Ref = "3.9"
	if Key("abc", renamed, []string{"de"}, chain, describers) != key {
		t.Error("the same figure under another ref has another key")
	}
	if Key("", f, nil, chain, describers) != "" || Key("abc", f, nil, []string{"vision", "silent"}, describers) != "" {
		t.Error("a figure nothing is promised about has a key")
	}
}

// TestADescriptionIsWrittenOntoItsBlock: a description fills the block's
// description and what kind of figure it is, and the words printed in the
// figure become the block's text only when the page's reader gave it none.
func TestADescriptionIsWrittenOntoItsBlock(t *testing.T) {
	res, err := (&stub.Describer{}).DescribeFigure(context.Background(), reader.FigureRequest{Data: []byte("png"), Width: 3, Height: 2})
	if err != nil {
		t.Fatal(err)
	}
	kept := Kept(res)
	if kept.Description != res.Description || kept.Type != reader.FigureOther || kept.Model != stub.Name || len(kept.Labels) != 1 {
		t.Fatalf("a result is kept as %+v", kept)
	}
	bare := document.Block{Kind: document.KindFigure}
	kept.Onto(&bare)
	if bare.Description != res.Description || bare.Figure == nil || bare.Figure.Type != reader.FigureOther || bare.Figure.Model != stub.Name || bare.Text != "3 bytes" {
		t.Fatalf("a figure with no text becomes %+v", bare)
	}
	read := document.Block{Kind: document.KindFigure, Text: "Q1 Q2"}
	Description{Type: reader.FigureChart, Description: "A chart.", Labels: []string{"one", "two"}}.Onto(&read)
	if read.Text != "Q1 Q2" || read.Description != "A chart." || read.Figure.Type != reader.FigureChart {
		t.Fatalf("a figure whose reader gave it text becomes %+v", read)
	}
}

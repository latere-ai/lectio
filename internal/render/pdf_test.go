// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"context"
	"image"
	"testing"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
)

// engine is one PDF engine for the package's tests: loading it compiles
// the WebAssembly module, which is the slow part and is done once.
var engine = &PDF{Instances: 2}

func describe(dpi, longEdge int, format string) reader.Description {
	return reader.Description{Accepts: []string{"image/png", "image/jpeg"}, Image: reader.ImageSpec{DPI: dpi, LongEdge: longEdge, Format: format}}
}

func TestAPDFPageIsRenderedAtTheReadersResolution(t *testing.T) {
	pdf := testfixtures.Read(t, testfixtures.TextPDF)
	for name, tc := range map[string]struct {
		want          reader.Description
		width, height int
		mediaType     string
	}{
		// A letter page is 8.5 by 11 inches.
		"at 72 dpi, one pixel per point":  {describe(72, 0, "png"), 612, 792, "image/png"},
		"at 144 dpi":                      {describe(144, 0, "png"), 1224, 1584, "image/png"},
		"no resolution named":             {describe(0, 0, ""), 1360, 1760, "image/png"},
		"within a long edge":              {describe(144, 792, "png"), 612, 792, "image/png"},
		"as a JPEG, for a reader of them": {describe(72, 0, "jpeg"), 612, 792, "image/jpeg"},
	} {
		got, err := engine.Render(context.Background(), pdf, detect.MIMEPDF, 1, tc.want)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Width != tc.width || got.Height != tc.height || got.MediaType != tc.mediaType || got.Blank {
			t.Errorf("%s: %dx%d %s, blank %v", name, got.Width, got.Height, got.MediaType, got.Blank)
		}
		img, format, err := image.Decode(bytes.NewReader(got.Data))
		if err != nil || "image/"+format != tc.mediaType || img.Bounds().Dx() != tc.width {
			t.Errorf("%s: the bytes are %s, %v", name, format, err)
		}
	}

	// The page holds a black bar from 72 to 540 points across, 168 to 192
	// points down from the top: the rendering is of this page, the right
	// way up.
	got, err := engine.Render(context.Background(), pdf, detect.MIMEPDF, 1, describe(72, 0, "png"))
	if err != nil {
		t.Fatal(err)
	}
	img, _, _ := image.Decode(bytes.NewReader(got.Data))
	if r, g, b, _ := img.At(300, 180).RGBA(); r > 0x1000 || g > 0x1000 || b > 0x1000 {
		t.Errorf("the bar is not where the page draws it: %x %x %x", r, g, b)
	}
	if r, _, _, _ := img.At(300, 400).RGBA(); r < 0xf000 {
		t.Errorf("the paper is not white: %x", r)
	}

	// A page with nothing on it is seen as blank, and needs no reader.
	empty, err := engine.Render(context.Background(), pdf, detect.MIMEPDF, 2, describe(72, 0, "png"))
	if err != nil || !empty.Blank || empty.Width != 612 {
		t.Fatalf("the empty page: %+v, %v", empty.Blank, err)
	}
}

func TestAPDFThatCannotBeRenderedSaysWhy(t *testing.T) {
	pdf := testfixtures.Read(t, testfixtures.TextPDF)
	for name, tc := range map[string]struct {
		data      []byte
		mediaType string
		page      int
		want      fault.Code
	}{
		"a page past the end":   {pdf, detect.MIMEPDF, 3, fault.InvalidPages},
		"bytes that are no PDF": {[]byte("%PDF-1.4\nnothing follows"), detect.MIMEPDF, 1, fault.DocumentCorrupt},
		"another format":        {pdf, detect.MIMEPNG, 1, fault.UnsupportedMediaType},
	} {
		if _, err := engine.Render(context.Background(), tc.data, tc.mediaType, tc.page, describe(72, 0, "png")); fault.CodeOf(err) != tc.want {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A render that was canceled before it began takes no engine.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := engine.Render(ctx, pdf, detect.MIMEPDF, 1, describe(72, 0, "png")); err == nil {
		t.Error("a canceled render")
	}
}

func TestResolutionKeepsAPageWithinItsBounds(t *testing.T) {
	for name, tc := range map[string]struct {
		width, height float64
		want          reader.ImageSpec
		dpi           int
	}{
		"the reader's resolution":            {612, 792, reader.ImageSpec{DPI: 200}, 200},
		"none named":                         {612, 792, reader.ImageSpec{}, defaultDPI},
		"lowered to the long edge":           {612, 792, reader.ImageSpec{DPI: 200, LongEdge: 1100}, 100},
		"a long edge the page is within":     {612, 792, reader.ImageSpec{DPI: 100, LongEdge: 4000}, 100},
		"a poster, lowered to the bitmap":    {7200, 7200, reader.ImageSpec{DPI: 300}, 63},
		"a page too large for any bitmap":    {7.2e6, 7.2e6, reader.ImageSpec{DPI: 300}, 1},
		"a landscape page, by its long edge": {792, 612, reader.ImageSpec{DPI: 200, LongEdge: 1100}, 100},
	} {
		if got := resolution(tc.width, tc.height, tc.want); got != tc.dpi {
			t.Errorf("%s: %d dpi, want %d", name, got, tc.dpi)
		}
	}
}

func TestPagesRendersEveryFormat(t *testing.T) {
	r := &Pages{PDF: engine}
	got, err := r.Render(context.Background(), testfixtures.Read(t, testfixtures.TextPDF), detect.MIMEPDF, 1, describe(72, 0, "png"))
	if err != nil || got.Width != 612 {
		t.Fatalf("a PDF: %+v, %v", got.Width, err)
	}
	got, err = r.Render(context.Background(), testfixtures.Read(t, testfixtures.PNG), detect.MIMEPNG, 1, describe(72, 0, "png"))
	if err != nil || got.MediaType != "image/png" {
		t.Fatalf("an image: %+v, %v", got.MediaType, err)
	}
	if fresh := NewPages(); fresh.PDF == nil || fresh.PDF.Close() != nil {
		t.Fatal("a new renderer has a PDF engine, and one never used closes at once")
	}
}

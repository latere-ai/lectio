// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
)

// picture is a PNG of the given size: white, with a dark square in the
// middle unless it is to be blank.
func picture(t *testing.T, w, h int, blank bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{250, 250, 250, 255}
			if !blank && x > w/3 && x < 2*w/3 && y > h/3 && y < 2*h/3 {
				c = color.RGBA{20, 20, 20, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func size(t *testing.T, data []byte) (int, int, string) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height, format
}

var (
	takesPNG  = reader.Description{Accepts: []string{"image/png", "image/jpeg"}, Image: reader.ImageSpec{Format: "png"}}
	takesJPEG = reader.Description{Accepts: []string{"image/jpeg"}, Image: reader.ImageSpec{Format: "jpeg"}}
)

func TestAnImageTheReaderAcceptsIsPassedAsItIs(t *testing.T) {
	src := picture(t, 400, 300, false)
	got, err := Images{}.Render(context.Background(), src, detect.MIMEPNG, 1, takesPNG)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Data, src) || got.MediaType != "image/png" || got.Width != 400 || got.Height != 300 || got.Blank {
		t.Fatalf("got %d bytes of %s, %dx%d, blank %v", len(got.Data), got.MediaType, got.Width, got.Height, got.Blank)
	}

	jpg := testfixtures.Read(t, testfixtures.JPEG)
	kept, err := Images{}.Render(context.Background(), jpg, detect.MIMEJPEG, 1, takesPNG)
	if err != nil || !bytes.Equal(kept.Data, jpg) || kept.MediaType != "image/jpeg" {
		t.Fatalf("a JPEG a reader accepts is not re-encoded: %s, %v", kept.MediaType, err)
	}
}

func TestAnImageIsTranscodedForAReaderThatDoesNotTakeIt(t *testing.T) {
	got, err := Images{}.Render(context.Background(), picture(t, 400, 300, false), detect.MIMEPNG, 1, takesJPEG)
	if err != nil {
		t.Fatal(err)
	}
	w, h, format := size(t, got.Data)
	if got.MediaType != "image/jpeg" || format != "jpeg" || w != 400 || h != 300 {
		t.Fatalf("got %s (%s) %dx%d", got.MediaType, format, w, h)
	}
}

func TestAnImagePastTheLongEdgeIsScaledDown(t *testing.T) {
	want := takesPNG
	want.Image.LongEdge = 100
	for name, src := range map[string][]byte{"wide": picture(t, 400, 200, false), "tall": picture(t, 200, 400, false)} {
		got, err := Images{}.Render(context.Background(), src, detect.MIMEPNG, 1, want)
		if err != nil {
			t.Fatal(err)
		}
		w, h, _ := size(t, got.Data)
		if max(w, h) != 100 || min(w, h) != 50 || got.Width != w || got.Height != h {
			t.Errorf("%s: scaled to %dx%d, reported %dx%d", name, w, h, got.Width, got.Height)
		}
	}

	// An image within the bound is left alone.
	small := picture(t, 80, 60, false)
	got, err := Images{}.Render(context.Background(), small, detect.MIMEPNG, 1, want)
	if err != nil || !bytes.Equal(got.Data, small) {
		t.Fatalf("an image within the bound is not scaled: %v", err)
	}
}

func TestABlankPageIsSeen(t *testing.T) {
	got, err := Images{}.Render(context.Background(), picture(t, 200, 200, true), detect.MIMEPNG, 1, takesPNG)
	if err != nil || !got.Blank {
		t.Fatalf("a page of one color is blank: %+v, %v", got.Blank, err)
	}

	// Compression noise around one color is still blank.
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			v := uint8(248 + (x+y)%3)
			img.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	noisy, err := Images{}.Render(context.Background(), buf.Bytes(), detect.MIMEJPEG, 1, takesPNG)
	if err != nil || !noisy.Blank {
		t.Fatalf("noise within the tolerance is blank: %+v, %v", noisy.Blank, err)
	}
}

// A page that holds one short word is not blank. A sampled grid of points
// missed it, the page was never shown to a reader, and the word was lost.
func TestAPageWithOneSmallMarkIsNotBlank(t *testing.T) {
	// A letter page at 160 dpi with a mark the size of a short word in
	// small type, placed between the points a 32 by 32 grid would look at.
	img := image.NewGray(image.Rect(0, 0, 1360, 1760))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for y := 870; y < 884; y++ {
		for x := 670; x < 700; x++ {
			img.SetGray(x, y, color.Gray{0})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	got, err := Images{}.Render(context.Background(), buf.Bytes(), detect.MIMEPNG, 1, takesPNG)
	if err != nil || got.Blank {
		t.Fatalf("a page with a small mark is taken for blank: %+v, %v", got.Blank, err)
	}

	// The same through the PDF engine: a page whose only content is one
	// word in 9 point type.
	page, err := engine.Render(context.Background(), oneWordPDF(), detect.MIMEPDF, 1, describe(160, 0, "png"))
	if err != nil || page.Blank {
		t.Fatalf("a PDF page with one word is taken for blank: %+v, %v", page.Blank, err)
	}
}

// oneWordPDF is a letter page that holds the word "Approved." in 9 point
// type and nothing else.
func oneWordPDF() []byte {
	content := "BT /F1 9 Tf 300 400 Td (Approved.) Tj ET\n"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, at := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", at)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

func TestATIFFIsRenderedFrameByFrame(t *testing.T) {
	tiff := testfixtures.Read(t, testfixtures.MultiTIFF) // three frames: 10x10, 10x10, 20x20
	for frame, edge := range map[int]int{1: 10, 3: 20} {
		got, err := Images{}.Render(context.Background(), tiff, detect.MIMETIFF, frame, takesPNG)
		if err != nil {
			t.Fatal(err)
		}
		w, h, format := size(t, got.Data)
		if format != "png" || got.MediaType != "image/png" || w != edge || h != edge {
			t.Errorf("frame %d: %s %dx%d, want %dx%d", frame, format, w, h, edge, edge)
		}
	}
	if _, err := (Images{}).Render(context.Background(), tiff, detect.MIMETIFF, 4, takesPNG); err == nil {
		t.Fatal("a frame past the last must fail")
	}
}

func TestWhatCannotBeRendered(t *testing.T) {
	for name, tc := range map[string]struct {
		data      []byte
		mediaType string
		page      int
		code      fault.Code
	}{
		"a PDF":                    {testfixtures.Read(t, testfixtures.MinimalPDF), detect.MIMEPDF, 1, fault.UnsupportedMediaType},
		"page 2 of an image":       {picture(t, 10, 10, false), detect.MIMEPNG, 2, fault.InvalidPages},
		"bytes that are not a PNG": {[]byte("not an image"), detect.MIMEPNG, 1, fault.DocumentCorrupt},
	} {
		_, err := Images{}.Render(context.Background(), tc.data, tc.mediaType, tc.page, takesPNG)
		if fault.CodeOf(err) != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Images{}).Render(ctx, picture(t, 10, 10, false), detect.MIMEPNG, 1, takesPNG); err == nil {
		t.Fatal("a canceled render must fail")
	}
}

var _ Renderer = Images{}

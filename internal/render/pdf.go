// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"image/png"
	"math"
	"sync"

	pdfium "github.com/klippa-app/go-pdfium"
	pdfiumerrors "github.com/klippa-app/go-pdfium/errors"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/reader"
)

// The bounds of one rendered page.
const (
	// defaultDPI is the resolution a page is rendered at when the reader
	// names none.
	defaultDPI = 160

	// maxPixels bounds one page's bitmap. A PDF may declare a page of any
	// size, and a page of a few meters at a scan's resolution is gigabytes;
	// past the bound the resolution is lowered until the page fits.
	maxPixels = 40_000_000
)

// PDF renders the pages of a PDF with PDFium, compiled to WebAssembly and
// run inside the process. There is no C library to link and no second
// service to run, and a file that breaks the engine breaks one sandboxed
// instance and not the worker. The zero value is ready to use; the engine
// is loaded on the first render.
type PDF struct {
	// Instances is how many pages may be rendered at once. Each instance
	// holds its own memory. Zero takes 4.
	Instances int

	once sync.Once
	pool pdfium.Pool
	err  error
}

func (p *PDF) load() {
	n := p.Instances
	if n <= 0 {
		n = 4
	}
	// The pool outlives every render, so it is not bound to a call's context.
	p.pool, p.err = webassembly.Init(webassembly.Config{Context: context.Background(), MinIdle: 1, MaxIdle: n, MaxTotal: n})
}

// Close unloads the engine. A PDF that was never used closes at once.
func (p *PDF) Close() error {
	p.once.Do(func() {})
	if p.pool == nil {
		return nil
	}
	return p.pool.Close()
}

// Render returns page n of a PDF as an image the reader accepts, rendered
// at the reader's resolution, lowered when the page would pass the
// reader's long edge or the bound on a bitmap.
func (p *PDF) Render(ctx context.Context, data []byte, mediaType string, n int, want reader.Description) (Image, error) {
	if err := ctx.Err(); err != nil {
		return Image{}, err
	}
	if mediaType != detect.MIMEPDF {
		return Image{}, fault.New(fault.UnsupportedMediaType, "the PDF renderer renders no page of %s", mediaType)
	}
	p.once.Do(p.load)
	if p.err != nil {
		return Image{}, p.err
	}
	engine, err := p.pool.GetInstanceWithContext(ctx)
	if err != nil {
		return Image{}, err
	}
	defer func() { _ = engine.Close() }()

	opened, err := engine.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		if errors.Is(err, pdfiumerrors.ErrPassword) || errors.Is(err, pdfiumerrors.ErrSecurity) {
			return Image{}, fault.New(fault.UnsupportedMediaType, "the PDF is encrypted and opens only with a password")
		}
		return Image{}, fault.Wrap(fault.DocumentCorrupt, err, "the PDF could not be opened")
	}
	defer func() { _, _ = engine.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: opened.Document}) }()

	page := requests.Page{ByIndex: &requests.PageByIndex{Document: opened.Document, Index: n - 1}}
	size, err := engine.GetPageSize(&requests.GetPageSize{Page: page})
	if err != nil {
		return Image{}, fault.New(fault.InvalidPages, "the PDF has no page %d", n)
	}
	rendered, err := engine.RenderPageInDPI(&requests.RenderPageInDPI{Page: page, DPI: resolution(size.Width, size.Height, want.Image)})
	if err != nil {
		return Image{}, fault.Wrap(fault.DocumentCorrupt, err, "page %d could not be rendered", n)
	}
	// The bitmap lives in the engine's memory until it is released, so it
	// is encoded before that.
	defer rendered.Cleanup()
	img := rendered.Result.RenderedImage
	bounds := img.Bounds()
	out := Image{Width: bounds.Dx(), Height: bounds.Dy(), Blank: blank(img)}

	var buf bytes.Buffer
	if want.Image.Format == "jpeg" {
		out.MediaType = detect.MIMEJPEG
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	} else {
		out.MediaType = detect.MIMEPNG
		err = png.Encode(&buf, img)
	}
	if err != nil {
		return Image{}, err
	}
	out.Data = buf.Bytes()
	return out, nil
}

// resolution is the resolution a page of the given size, in points, is
// rendered at: the reader's, lowered until the page's long edge is within
// the reader's bound and its bitmap within the bound on pixels. Rendering
// at the lower resolution, and not scaling a larger bitmap down, keeps
// text as sharp as the size allows and never allocates the larger one.
func resolution(width, height float64, want reader.ImageSpec) int {
	dpi := float64(want.DPI)
	if dpi <= 0 {
		dpi = defaultDPI
	}
	long := max(width, height) / 72 * dpi
	if want.LongEdge > 0 && long > float64(want.LongEdge) {
		dpi *= float64(want.LongEdge) / long
	}
	if pixels := (width / 72 * dpi) * (height / 72 * dpi); pixels > maxPixels {
		dpi *= math.Sqrt(maxPixels / pixels)
	}
	return max(1, int(dpi))
}

// Pages renders every format a reader is shown: an image and the frames of
// a TIFF as they are, and the pages of a PDF with PDFium.
type Pages struct {
	Images Images
	PDF    *PDF
}

// NewPages returns a renderer for every format. The PDF engine is loaded
// when the first PDF page is rendered.
func NewPages() *Pages { return &Pages{PDF: &PDF{}} }

// Render returns the page as an image the reader accepts.
func (r *Pages) Render(ctx context.Context, data []byte, mediaType string, n int, want reader.Description) (Image, error) {
	if mediaType == detect.MIMEPDF {
		return r.PDF.Render(ctx, data, mediaType, n, want)
	}
	return r.Images.Render(ctx, data, mediaType, n, want)
}

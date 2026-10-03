// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"sync"
	"time"

	pdfium "github.com/klippa-app/go-pdfium"
	pdfiumerrors "github.com/klippa-app/go-pdfium/errors"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/reader"
)

// The bounds of one rendered page.
const (
	// defaultDPI is the resolution a page is rendered at when the reader
	// names none.
	defaultDPI = 160

	// maxPixels bounds one page's bitmap, and the pixels of an image that
	// is decoded. A PDF may declare a page of any size and an image file
	// any dimensions, and a few thousand bytes can ask for gigabytes; past
	// the bound a PDF page's resolution is lowered until it fits, and an
	// image is refused.
	maxPixels = 40_000_000

	// memoryPages bounds the memory of one engine instance, in WebAssembly
	// pages of 64 KiB: 512 MiB. An instance holds the page's bitmap
	// (maxPixels at four bytes each, 160 MB) and what the engine allocates
	// to parse the page and decode its fonts and images; the file itself
	// stays outside, read through a callback a block at a time. A page that
	// asks for more is one written to exhaust its reader, and the bound is
	// what keeps it from exhausting the worker instead.
	memoryPages = 8192

	// defaultTimeout is how long one page may take to render when the
	// renderer names no other bound. A page of a real document renders in
	// milliseconds; one that takes this long was built to.
	defaultTimeout = 30 * time.Second
)

// PDF renders the pages of a PDF with PDFium, compiled to WebAssembly and
// run inside the process. There is no C library to link and no second
// service to run. The engine sees nothing of the host: no file, no
// network, no environment. Each page is rendered by an instance of its
// own with a bound on its memory and on its time, and the instance is
// discarded afterwards, so a file that breaks the engine, or was written
// to exhaust it, costs one bounded instance and not the worker. The zero
// value is ready to use; the engine is loaded on the first render.
type PDF struct {
	// Instances is how many pages may be rendered at once. Each instance
	// holds its own memory. Zero takes 4.
	Instances int

	// Timeout bounds the rendering of one page. Zero takes 30 seconds.
	Timeout time.Duration

	once  sync.Once
	slots chan *slot
	cache wazero.CompilationCache
}

// slot is one engine: a runtime that runs one instance at a time. Each
// slot has its own, so that one can be stopped without touching another.
type slot struct {
	pool   pdfium.Pool
	stop   context.CancelFunc
	memory *arena
}

func (p *PDF) load() {
	n := p.Instances
	if n <= 0 {
		n = 4
	}
	// The module is compiled once and every slot's runtime shares the
	// result, so starting a slot again after one was stopped is cheap.
	p.cache = wazero.NewCompilationCache()
	p.slots = make(chan *slot, n)
	for range n {
		p.slots <- &slot{}
	}
}

// start loads a slot's engine.
func (s *slot) start(cache wazero.CompilationCache) error {
	s.memory = newArena()
	// Ending this context ends whatever the engine is running, from the
	// inside: the running call sees it and returns. It also carries how an
	// instance's memory is allocated.
	ctx, stop := context.WithCancel(experimental.WithMemoryAllocator(context.Background(), s.memory.allocator()))
	pool, err := webassembly.Init(webassembly.Config{
		Context: ctx,
		MaxIdle: 1, MaxTotal: 1,
		// An empty file system. Left unset, the engine is given the host's
		// whole file system, and a PDF is a program for the engine.
		FSConfig: wazero.NewFSConfig(),
		RuntimeConfig: wazero.NewRuntimeConfig().
			// The engine's module needs exception handling.
			WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling).
			WithCompilationCache(cache).
			WithMemoryLimitPages(memoryPages).
			// Lets a running call be ended by its context.
			WithCloseOnContextDone(true),
		// What the engine prints is not the server's log.
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		stop()
		return err
	}
	s.pool, s.stop = pool, stop
	return nil
}

// discard unloads a slot's engine and returns its memory. The caller knows
// no call on the engine is running.
func (s *slot) discard() {
	if s.pool != nil {
		s.stop()
		_ = s.pool.Close()
		s.memory.release()
	}
	*s = slot{}
}

// Close unloads the engine. A PDF that was never used closes at once. No
// render may be running.
func (p *PDF) Close() error {
	p.once.Do(func() {})
	if p.slots == nil {
		return nil
	}
	for range cap(p.slots) {
		(<-p.slots).discard()
	}
	return p.cache.Close(context.Background())
}

// errSlow marks work the engine did not finish in the time it is given.
var errSlow = errors.New("the engine did not finish in time")

// onInstance runs work on an engine instance of its own and returns what
// it returns. The work runs beside the caller, not under it: when ctx
// ends, or the work outlives the renderer's time, the engine is told to
// stop, which ends the running call wherever it is, and the caller returns
// at once with ctx's error or errSlow. An instance is never used twice.
func onInstance[T any](ctx context.Context, p *PDF, work func(pdfium.Pdfium) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	p.once.Do(p.load)
	var s *slot
	select {
	case s = <-p.slots:
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	if s.pool == nil {
		if err := s.start(p.cache); err != nil {
			p.slots <- s
			return zero, err
		}
	}
	instance, err := s.pool.GetInstanceWithContext(ctx)
	if err != nil {
		p.slots <- s
		return zero, err
	}

	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		// An engine that was stopped, or ran out of memory, may fail in
		// any call made on it afterwards, the cleanup included.
		defer func() {
			if recover() != nil {
				done <- outcome{err: fault.New(fault.DocumentCorrupt, "the PDF engine stopped while reading the file")}
			}
		}()
		value, err := work(instance)
		done <- outcome{value, err}
	}()

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case out := <-done:
		// Closing discards the instance; its memory goes back now that
		// nothing runs on it.
		_ = instance.Close()
		s.memory.release()
		p.slots <- s
		return out.value, out.err
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = errSlow
	}
	// The engine is told to stop and the caller does not wait for it. The
	// slot comes back, with a new engine to load, once the work has let go
	// of the old one.
	s.stop()
	go func() {
		<-done
		s.discard()
		p.slots <- s
	}()
	return zero, err
}

// open opens a PDF on an engine instance. The caller closes the document.
// The engine reads the file through a reader, the blocks it needs and no
// more, so opening a large file to render one page copies nothing.
func open(instance pdfium.Pdfium, data []byte) (*requests.FPDF_CloseDocument, error) {
	opened, err := instance.OpenDocument(&requests.OpenDocument{FileReader: bytes.NewReader(data), FileReaderSize: int64(len(data))})
	if err != nil {
		if errors.Is(err, pdfiumerrors.ErrPassword) || errors.Is(err, pdfiumerrors.ErrSecurity) {
			return nil, fault.New(fault.UnsupportedMediaType, "the PDF is encrypted and opens only with a password")
		}
		return nil, fault.Wrap(fault.DocumentCorrupt, err, "the PDF could not be opened")
	}
	return &requests.FPDF_CloseDocument{Document: opened.Document}, nil
}

// Count returns how many pages a PDF has, as the engine that renders them
// counts them. This is the count a parse goes by: the engine walks the page
// tree the way a viewer does, where a count read off the file's bytes can
// be led to say more or fewer.
func (p *PDF) Count(ctx context.Context, data []byte) (int, error) {
	n, err := onInstance(ctx, p, func(instance pdfium.Pdfium) (int, error) {
		doc, err := open(instance, data)
		if err != nil {
			return 0, err
		}
		defer func() { _, _ = instance.FPDF_CloseDocument(doc) }()
		counted, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
		if err != nil {
			return 0, fault.Wrap(fault.DocumentCorrupt, err, "the PDF's pages could not be counted")
		}
		return counted.PageCount, nil
	})
	if errors.Is(err, errSlow) {
		return 0, fault.New(fault.DocumentCorrupt, "the PDF's pages could not be counted in the time a file is given")
	}
	if err == nil && n < 1 {
		return 0, fault.New(fault.DocumentCorrupt, "the PDF has no pages")
	}
	return n, err
}

// Render returns page n of a PDF as an image the reader accepts, rendered
// at the reader's resolution, lowered when the page would pass the
// reader's long edge or the bound on a bitmap. A page the engine cannot
// render within its memory and its time fails with fault.DocumentCorrupt.
func (p *PDF) Render(ctx context.Context, data []byte, mediaType string, n int, want reader.Description) (Image, error) {
	if err := ctx.Err(); err != nil {
		return Image{}, err
	}
	if mediaType != detect.MIMEPDF {
		return Image{}, fault.New(fault.UnsupportedMediaType, "the PDF renderer renders no page of %s", mediaType)
	}
	out, err := onInstance(ctx, p, func(instance pdfium.Pdfium) (Image, error) {
		doc, err := open(instance, data)
		if err != nil {
			return Image{}, err
		}
		defer func() { _, _ = instance.FPDF_CloseDocument(doc) }()

		counted, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
		if err != nil || n < 1 || n > counted.PageCount {
			return Image{}, fault.New(fault.InvalidPages, "the PDF has no page %d", n)
		}
		// The engine does not say why a page fails. One it cannot read and
		// one that asks for more memory than an instance has fail alike.
		page := requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: n - 1}}
		size, err := instance.GetPageSize(&requests.GetPageSize{Page: page})
		if err != nil {
			return Image{}, fault.Wrap(fault.DocumentCorrupt, err, "page %d could not be loaded: it is damaged, or it needs more memory than a render is given", n)
		}
		rendered, err := instance.RenderPageInDPI(&requests.RenderPageInDPI{Page: page, DPI: resolution(size.Width, size.Height, want.Image)})
		if err != nil {
			return Image{}, fault.Wrap(fault.DocumentCorrupt, err, "page %d could not be rendered: it is damaged, or it needs more memory than a render is given", n)
		}
		// The bitmap lives in the engine's memory until it is released, so
		// it is encoded before that.
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
	})
	if errors.Is(err, errSlow) {
		return Image{}, fault.New(fault.DocumentCorrupt, "page %d did not render in the time a page is given", n)
	}
	return out, err
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
// when the first PDF is counted or rendered.
func NewPages() *Pages { return &Pages{PDF: &PDF{}} }

// Render returns the page as an image the reader accepts.
func (r *Pages) Render(ctx context.Context, data []byte, mediaType string, n int, want reader.Description) (Image, error) {
	if mediaType == detect.MIMEPDF {
		return r.PDF.Render(ctx, data, mediaType, n, want)
	}
	return r.Images.Render(ctx, data, mediaType, n, want)
}

// CountPDF returns how many pages a PDF has, counted by the engine that
// renders them.
func (r *Pages) CountPDF(ctx context.Context, data []byte) (int, error) {
	return r.PDF.Count(ctx, data)
}

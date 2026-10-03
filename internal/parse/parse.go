// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package parse is the work of a parse, as steps that hold no state between
// calls. Prepare runs once per file: it decides what the file is, opens and
// converts what needs it, counts the pages, and selects the ones to read.
// ReadPage runs once per page: it renders that page, has a reader read it,
// and checks the answer.
//
// The split is the point. A page is the unit of work: whoever schedules
// these steps, in one process or across many, can run pages in any order,
// retry one without the others, and stop between any two. Nothing here
// knows about queues, tenants, or storage. The design is
// specs/005-parse-graph.md.
package parse

import (
	"context"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/intake/unwrap"
	"latere.ai/x/lectio/internal/native"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/reader"
)

// Converter turns an office format into one the pipeline reads: a legacy
// word-processing file into the current format, a presentation or a rich
// text file into PDF. It is optional: a pipeline with none refuses the
// formats that need one.
type Converter interface {
	// Convert returns data, of media type from, as media type to.
	Convert(ctx context.Context, data []byte, from, to string) ([]byte, error)
}

// Manifest is what Prepare found out about a file: everything a page's
// work needs to know that is not the page itself.
type Manifest struct {
	// MediaType is what the working copy is, after any container was opened
	// and any conversion was done.
	MediaType string `json:"media_type"`

	// PagesTotal is how many pages the file has. Selected lists the ones to
	// read, counted from 1, in order.
	PagesTotal int   `json:"pages_total"`
	Selected   []int `json:"selected"`

	// Source says who produces the pages' blocks: a reader, or the format
	// itself.
	Source document.PageSource `json:"source"`
}

// Pipeline holds what the steps depend on.
type Pipeline struct {
	// Limits bound one file.
	Limits pages.Limits

	// Renderer makes the image of a page.
	Renderer render.Renderer

	// Converter converts office formats. Nil refuses them.
	Converter Converter
}

// Prepared is the result of Prepare.
type Prepared struct {
	Manifest Manifest

	// Working is the file as the pages are read from: the source, or what
	// it was opened or converted into.
	Working []byte

	// Native holds the selected pages of a format that carries its own
	// structure. For such a file there is nothing left to read; for any
	// other it is empty.
	Native []document.Page
}

// Prepare does the work that is done once per file. selection is the
// caller's page selection, empty for every page.
func (p *Pipeline) Prepare(ctx context.Context, data []byte, declared detect.DeclaredType, selection string) (Prepared, error) {
	if p.Limits.MaxBytes > 0 && int64(len(data)) > p.Limits.MaxBytes {
		return Prepared{}, fault.New(fault.FileTooLarge, "the file is %d bytes, over the limit of %d", len(data), p.Limits.MaxBytes)
	}

	mediaType, err := detect.Detect(data, declared)
	if err != nil {
		return Prepared{}, err
	}
	// A signed container holds another file, which may be a container too.
	for range unwrap.MaxDepth {
		if class, _ := detect.ClassOf(mediaType); class != detect.ClassUnwrapP7M {
			break
		}
		if data, err = unwrap.Unwrap(data); err != nil {
			return Prepared{}, err
		}
		// What is inside says what it is; the container's name does not.
		if mediaType, err = detect.Detect(data, detect.DeclaredType{}); err != nil {
			return Prepared{}, err
		}
	}

	class, _ := detect.ClassOf(mediaType)
	switch class {
	case detect.ClassConvertToPDF:
		if data, mediaType, err = p.convert(ctx, data, mediaType, detect.MIMEPDF); err != nil {
			return Prepared{}, err
		}
		class = detect.ClassReader
	case detect.ClassConvertDOCToDOCX:
		if data, mediaType, err = p.convert(ctx, data, mediaType, detect.MIMEDOCX); err != nil {
			return Prepared{}, err
		}
		class = detect.ClassNativeDOCX
	}

	out := Prepared{Manifest: Manifest{MediaType: mediaType}, Working: data}
	if class != detect.ClassReader {
		return p.prepareNative(ctx, out, selection)
	}

	out.Manifest.Source = document.SourceReader
	switch mediaType {
	case detect.MIMEPDF:
		out.Manifest.PagesTotal, err = pages.CountPDF(data)
	case detect.MIMETIFF:
		out.Manifest.PagesTotal, err = pages.CountTIFF(data)
	default:
		out.Manifest.PagesTotal = 1
	}
	if err != nil {
		return Prepared{}, err
	}
	if err := pages.EnforceMax(out.Manifest.PagesTotal, p.Limits); err != nil {
		return Prepared{}, err
	}
	if out.Manifest.Selected, err = pages.Select(selection, out.Manifest.PagesTotal); err != nil {
		return Prepared{}, err
	}
	return out, nil
}

// prepareNative reads a format that carries its own structure. Its pages
// exist as soon as the file is read, so they are returned with the
// manifest.
func (p *Pipeline) prepareNative(ctx context.Context, out Prepared, selection string) (Prepared, error) {
	if !native.Reads(out.Manifest.MediaType) {
		return Prepared{}, fault.New(fault.UnsupportedMediaType, "%s is recognized and not read by this build", out.Manifest.MediaType)
	}
	all, err := native.Pages(ctx, out.Working, out.Manifest.MediaType)
	if err != nil {
		return Prepared{}, err
	}
	out.Manifest.Source = document.SourceNative
	out.Manifest.PagesTotal = len(all)
	if err := pages.EnforceMax(len(all), p.Limits); err != nil {
		return Prepared{}, err
	}
	if out.Manifest.Selected, err = pages.Select(selection, len(all)); err != nil {
		return Prepared{}, err
	}
	for _, n := range out.Manifest.Selected {
		out.Native = append(out.Native, all[n-1])
	}
	return out, nil
}

func (p *Pipeline) convert(ctx context.Context, data []byte, from, to string) ([]byte, string, error) {
	if p.Converter == nil {
		return nil, "", fault.New(fault.UnsupportedMediaType, "%s needs conversion, and this server runs no converter", from)
	}
	out, err := p.Converter.Convert(ctx, data, from, to)
	if err != nil {
		return nil, "", err
	}
	return out, to, nil
}

// Page is the result of ReadPage: the page, and the image the reader saw.
type Page struct {
	Page  document.Page
	Image render.Image
}

// PageOptions are what a read is made with beyond the page itself.
type PageOptions struct {
	Languages  []string
	Credential reader.Credential
}

// ReadPage does the work of one page: render it, have the reader read it,
// check the answer, and number the blocks. A page with nothing on it is
// returned without a call.
//
// An error from the reader comes back as the reader classified it, so the
// caller can tell what to do next: wait, try again, or give up on the page.
// A reply that is not usable is a reader.Invalid error.
func (p *Pipeline) ReadPage(ctx context.Context, m Manifest, working []byte, n int, r reader.Reader, opt PageOptions) (Page, error) {
	desc := r.Describe()
	img, err := p.Renderer.Render(ctx, working, m.MediaType, n, desc)
	if err != nil {
		return Page{}, err
	}
	page := document.Page{
		Number: n, Width: float64(img.Width), Height: float64(img.Height),
		State: document.PageSucceeded, Source: document.SourceReader, Blocks: []document.Block{},
	}
	if img.Blank {
		page.Usage = &document.Usage{Pages: 1}
		return Page{Page: page, Image: img}, nil
	}

	res, err := r.ReadPage(ctx, reader.Page{
		Number: n, Data: img.Data, MediaType: img.MediaType, Width: img.Width, Height: img.Height,
		Languages: opt.Languages, Credential: opt.Credential,
	})
	if err != nil {
		return Page{}, err
	}
	if err := reader.Check(res, false); err != nil {
		return Page{}, err
	}

	page.Reader, page.Model = desc.Name, res.Model
	page.Blocks = document.Number(n, res.Blocks)
	usage := res.Usage
	page.Usage = &usage
	if err := page.Validate(); err != nil {
		return Page{}, &reader.Error{Class: reader.Invalid, Detail: "the reader's blocks do not fit the object model", Err: err}
	}
	return Page{Page: page, Image: img}, nil
}

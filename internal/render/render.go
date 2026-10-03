// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package render makes the image of a page that a reader is given. Each
// call renders one page, so a caller never holds more than one page image
// for the work it is doing, however long the document is. The image a
// caller stores is the image the reader saw, which is what makes a block's
// box drawn over it line up.
package render

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"slices"

	"golang.org/x/image/draw"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/intake/tiffx"
	"latere.ai/x/lectio/reader"
)

// Image is one page as an image.
type Image struct {
	Data      []byte
	MediaType string
	// Width and Height are in pixels.
	Width, Height int
	// Blank reports that every sampled pixel has the same color: a page
	// with nothing on it, which needs no reader.
	Blank bool
}

// Renderer makes the image of one page of a file.
type Renderer interface {
	// Render returns page n, counted from 1, of a file of the given media
	// type, prepared the way the reader's description asks: at its
	// resolution, within its long edge, in a format it accepts.
	Render(ctx context.Context, data []byte, mediaType string, n int, want reader.Description) (Image, error)
}

// Images renders the formats that are images already: PNG, JPEG, and each
// frame of a TIFF. It refuses a PDF: rendering one needs an engine this
// package does not hold.
type Images struct{}

// Render returns the page as an image the reader accepts.
func (Images) Render(ctx context.Context, data []byte, mediaType string, n int, want reader.Description) (Image, error) {
	if err := ctx.Err(); err != nil {
		return Image{}, err
	}
	switch mediaType {
	case detect.MIMEPNG, detect.MIMEJPEG:
		if n != 1 {
			return Image{}, fault.New(fault.InvalidPages, "an image has one page, and page %d was asked for", n)
		}
	case detect.MIMETIFF:
		frame, _, _, err := tiffx.FramePNG(data, n)
		if err != nil {
			return Image{}, err
		}
		data, mediaType = frame, detect.MIMEPNG
	default:
		return Image{}, fault.New(fault.UnsupportedMediaType, "this build renders no page of %s", mediaType)
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Image{}, fault.Wrap(fault.DocumentCorrupt, err, "the image could not be decoded")
	}
	bounds := src.Bounds()
	out := Image{Data: data, MediaType: mediaType, Width: bounds.Dx(), Height: bounds.Dy(), Blank: blank(src)}

	long := max(out.Width, out.Height)
	fits := want.Image.LongEdge <= 0 || long <= want.Image.LongEdge
	if fits && slices.Contains(want.Accepts, mediaType) {
		// The file's own bytes are what the reader takes: nothing is lost
		// to a second encoding.
		return out, nil
	}

	if !fits {
		scale := float64(want.Image.LongEdge) / float64(long)
		dst := image.NewRGBA(image.Rect(0, 0, max(1, int(float64(out.Width)*scale)), max(1, int(float64(out.Height)*scale))))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Src, nil)
		src, out.Width, out.Height = dst, dst.Bounds().Dx(), dst.Bounds().Dy()
	}

	var buf bytes.Buffer
	if want.Image.Format == "jpeg" {
		out.MediaType = detect.MIMEJPEG
		err = jpeg.Encode(&buf, src, &jpeg.Options{Quality: 90})
	} else {
		out.MediaType = detect.MIMEPNG
		err = png.Encode(&buf, src)
	}
	if err != nil {
		return Image{}, err
	}
	out.Data = buf.Bytes()
	return out, nil
}

// blank reports whether an image is one color. It samples a grid of points
// and compares each with the first, within a tolerance that absorbs the
// noise of a scan and of lossy compression.
func blank(img image.Image) bool {
	const grid, tolerance = 32, 0x0600 // on the 16-bit scale color.RGBA returns
	b := img.Bounds()
	r0, g0, b0, _ := img.At(b.Min.X, b.Min.Y).RGBA()
	for i := range grid {
		for j := range grid {
			x := b.Min.X + (b.Dx()-1)*i/(grid-1)
			y := b.Min.Y + (b.Dy()-1)*j/(grid-1)
			r, g, bl, _ := img.At(x, y).RGBA()
			if differ(r, r0, tolerance) || differ(g, g0, tolerance) || differ(bl, b0, tolerance) {
				return false
			}
		}
	}
	return true
}

func differ(a, b, tolerance uint32) bool {
	if a > b {
		return a-b > tolerance
	}
	return b-a > tolerance
}

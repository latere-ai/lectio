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
	// Blank reports that every pixel has the same color: a page with
	// nothing on it, which needs no reader.
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
// frame of a TIFF. It refuses a PDF, which PDF renders.
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

	// An image states its size in its header, and decoding allocates for
	// what it states: a few kilobytes can ask for gigabytes. The header is
	// read first and a size past the bound is refused before any pixel is
	// decoded.
	stated, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Image{}, fault.Wrap(fault.DocumentCorrupt, err, "the image could not be decoded")
	}
	if int64(stated.Width)*int64(stated.Height) > maxPixels {
		return Image{}, fault.New(fault.FileTooLarge, "the image is %d by %d pixels, over the bound of %d pixels on one page", stated.Width, stated.Height, maxPixels)
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

// blank reports whether an image is one color: every pixel is within a
// tolerance of the first, which absorbs the noise of lossy compression.
// Every pixel is looked at. A sample would miss a page that holds one
// short line, and a page taken for blank is never shown to a reader, so
// what it held would be lost without a trace. A page with content ends the
// scan at its first mark, so the whole image is read only when it is empty.
func blank(img image.Image) bool {
	const tolerance = 0x0600 // on the 16-bit scale color.RGBA returns
	b := img.Bounds()
	r0, g0, b0, _ := img.At(b.Min.X, b.Min.Y).RGBA()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
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

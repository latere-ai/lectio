// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"image"
	"image/draw"
	"image/png"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
)

// Crop cuts the region a box covers out of a page's image and returns it
// as a PNG. The page image is the one a reader saw, and a box is a
// fraction of it, so the crop of a block's box is that block as it looks
// on the page: a figure, a table, a signature.
//
// The region is rounded outward to whole pixels, so a thin region is never
// lost to rounding. A box that covers no pixel of the image is refused.
func Crop(page Image, box document.Box) (Image, error) {
	stated, _, err := image.DecodeConfig(bytes.NewReader(page.Data))
	if err != nil {
		return Image{}, fault.Wrap(fault.DocumentCorrupt, err, "the page image could not be decoded")
	}
	if int64(stated.Width)*int64(stated.Height) > maxPixels {
		return Image{}, fault.New(fault.FileTooLarge, "the page image is %d by %d pixels, over the bound of %d pixels on one page", stated.Width, stated.Height, maxPixels)
	}
	src, _, err := image.Decode(bytes.NewReader(page.Data))
	if err != nil {
		return Image{}, fault.Wrap(fault.DocumentCorrupt, err, "the page image could not be decoded")
	}

	bounds := src.Bounds()
	w, h := float64(bounds.Dx()), float64(bounds.Dy())
	region := image.Rect(
		bounds.Min.X+int(box[0]*w), bounds.Min.Y+int(box[1]*h),
		bounds.Min.X+ceil(box[2]*w), bounds.Min.Y+ceil(box[3]*h),
	).Intersect(bounds)
	if region.Empty() {
		return Image{}, fault.New(fault.InvalidRequest, "the box covers no pixel of the page image")
	}

	cut := image.NewRGBA(image.Rect(0, 0, region.Dx(), region.Dy()))
	draw.Draw(cut, cut.Bounds(), src, region.Min, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, cut); err != nil {
		return Image{}, err
	}
	return Image{Data: buf.Bytes(), MediaType: "image/png", Width: region.Dx(), Height: region.Dy()}, nil
}

// ceil rounds a pixel coordinate up.
func ceil(v float64) int {
	n := int(v)
	if float64(n) < v {
		n++
	}
	return n
}

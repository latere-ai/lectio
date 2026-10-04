// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package text

import (
	"bytes"
	"image"
	"image/draw"
	_ "image/jpeg" // a page's image is a JPEG
	_ "image/png"  // or a PNG
	"math"
	"unicode"

	"latere.ai/x/lectio/reader"
)

// The bounds a word is held to be seen by. A file can hold a word that
// its page does not show: one drawn in the paper's color, or one under a
// shape painted over it, as a name under a bar of black is. The positions
// do not say so, since nothing in them says what was painted last. The
// page's image does: where a word is drawn, the image is not one flat
// color.
const (
	// flat is the most the samples of a word's place in the page's image
	// may differ, of 255, for the place to be one flat color. Print on
	// paper differs by 200 and more, and the noise of a compressed image
	// by under 10. Raised, pale type on a tint counts as not drawn and its
	// page is declined; lowered, the noise of an image hides a covered
	// word.
	flat = 24

	// seenLength is the fewest letters and digits a word holds to be
	// looked for in the image. A period or a dash is a few samples of ink
	// at a low resolution and can fall between them, and a line of
	// underscores lies under the height a word is looked for at. Raised, a
	// short covered word is read; lowered, a page is declined for a mark
	// too small to see.
	seenLength = 3

	// seenBand is the height above its baseline, in sizes, that a word is
	// looked for in: the height of a small letter, where every letter and
	// every digit leaves ink. The whole line of type is not looked at,
	// since a bar painted over a word covers its letters and may leave
	// the paper above and below them. Raised toward the line's height, a
	// word under such a bar counts as drawn.
	seenBand = 0.5
)

// seen holds the words of a page to its image: each word of seenLength
// letters and digits or more must show where the file places it. It
// declines a page with a word that does not, and a page whose image
// cannot be looked at. A page handed over with no image is not judged.
func seen(page reader.Page, words []reader.Word) error {
	if len(page.Data) == 0 {
		return nil
	}
	decoded, _, err := image.Decode(bytes.NewReader(page.Data))
	if err != nil {
		return decline("the page's image could not be looked at, so its text cannot be held to what the page shows")
	}
	// One layout of samples for every format an image decodes to.
	bounds := decoded.Bounds()
	img := image.NewRGBA(bounds)
	draw.Draw(img, bounds, decoded, bounds.Min, draw.Src)

	across, down := float64(bounds.Dx())/page.Text.Width, float64(bounds.Dy())/page.Text.Height
	for _, w := range words {
		letters := 0
		for _, r := range w.Text {
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				letters++
			}
		}
		if letters < seenLength {
			continue
		}
		// Where the word's small letters stand, in samples, cut to the
		// image.
		place := image.Rect(
			int(math.Floor(w.Box.X0*across)), int(math.Floor((w.Baseline-seenBand*w.Size)*down)),
			int(math.Ceil(w.Box.X1*across)), int(math.Ceil(w.Baseline*down)),
		).Add(bounds.Min).Intersect(bounds)
		if !place.Empty() && uniform(img, place) {
			return decline("a word of the page is not drawn where the file places it: it lies under a shape, or is drawn in the paper's color")
		}
	}
	return nil
}

// uniform reports whether a part of an image is one flat color: no
// channel of its samples differs by more than flat.
func uniform(img *image.RGBA, part image.Rectangle) bool {
	lo, hi := [3]uint8{255, 255, 255}, [3]uint8{}
	for y := part.Min.Y; y < part.Max.Y; y++ {
		row := img.Pix[img.PixOffset(part.Min.X, y):img.PixOffset(part.Max.X, y)]
		for i := 0; i+3 < len(row); i += 4 {
			for c := range 3 {
				lo[c], hi[c] = min(lo[c], row[i+c]), max(hi[c], row[i+c])
				if hi[c]-lo[c] > flat {
					return false
				}
			}
		}
	}
	return true
}

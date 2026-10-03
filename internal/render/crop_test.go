// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
)

// quadrants is a 200 by 100 image whose left half is red and right half
// blue, as a PNG or a JPEG.
func quadrants(t *testing.T, asJPEG bool) Image {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 200, 100))
	for y := range 100 {
		for x := range 200 {
			c := color.RGBA{220, 30, 30, 255}
			if x >= 100 {
				c = color.RGBA{30, 30, 220, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	mediaType := "image/png"
	if asJPEG {
		mediaType = "image/jpeg"
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
			t.Fatal(err)
		}
	} else if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return Image{Data: buf.Bytes(), MediaType: mediaType, Width: 200, Height: 100}
}

func TestCropCutsTheRegionABoxCovers(t *testing.T) {
	for name, page := range map[string]Image{"a PNG": quadrants(t, false), "a JPEG": quadrants(t, true)} {
		// The right half, lower three quarters.
		got, err := Crop(page, document.Box{0.5, 0.25, 1, 1})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.MediaType != "image/png" || got.Width != 100 || got.Height != 75 {
			t.Fatalf("%s: %s, %d by %d", name, got.MediaType, got.Width, got.Height)
		}
		img, err := png.Decode(bytes.NewReader(got.Data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Every corner of the crop is the blue half.
		for _, at := range []image.Point{{2, 2}, {97, 2}, {2, 72}, {97, 72}} {
			if r, _, b, _ := img.At(at.X, at.Y).RGBA(); b < 0x8000 || r > 0x5000 {
				t.Errorf("%s: the crop at %v is not from the right half: r=%x b=%x", name, at, r, b)
			}
		}
	}

	// A region thinner than a pixel is rounded outward and kept.
	thin, err := Crop(quadrants(t, false), document.Box{0.501, 0.501, 0.502, 0.502})
	if err != nil || thin.Width != 1 || thin.Height != 1 {
		t.Fatalf("a thin region: %d by %d, %v", thin.Width, thin.Height, err)
	}
}

func TestCropRefusesWhatItCannotCut(t *testing.T) {
	page := quadrants(t, false)
	for name, tc := range map[string]struct {
		page Image
		box  document.Box
		want fault.Code
	}{
		"a box with no area":                {page, document.Box{0.5, 0.5, 0.5, 0.5}, fault.InvalidRequest},
		"bytes that are no image":           {Image{Data: []byte("not an image")}, document.Box{0, 0, 1, 1}, fault.DocumentCorrupt},
		"an image cut off after its header": {Image{Data: page.Data[:60]}, document.Box{0, 0, 1, 1}, fault.DocumentCorrupt},
	} {
		if _, err := Crop(tc.page, tc.box); fault.CodeOf(err) != tc.want {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An image that declares more pixels than a page may have is refused
	// from its header, as a page is: a PNG signature and a header chunk
	// for 30000 by 30000 gray pixels, and nothing after it.
	var huge bytes.Buffer
	huge.WriteString("\x89PNG\r\n\x1a\n")
	header := []byte{0, 0, 0x75, 0x30, 0, 0, 0x75, 0x30, 8, 0, 0, 0, 0}
	_ = binary.Write(&huge, binary.BigEndian, uint32(len(header)))
	chunk := append([]byte("IHDR"), header...)
	huge.Write(chunk)
	_ = binary.Write(&huge, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	if _, err := Crop(Image{Data: huge.Bytes()}, document.Box{0, 0, 1, 1}); fault.CodeOf(err) != fault.FileTooLarge {
		t.Errorf("an image that declares 30000 by 30000 pixels: %v", err)
	}
}

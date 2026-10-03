// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tiffx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"

	"golang.org/x/image/tiff"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/testfixtures"
)

// singleFrame encodes a w x h single-frame TIFF.
func singleFrame(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := tiff.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode tiff: %v", err)
	}
	return buf.Bytes()
}

func TestCountFrames(t *testing.T) {
	if n, err := CountFrames(testfixtures.Read(t, testfixtures.MultiTIFF)); err != nil || n != 3 {
		t.Fatalf("CountFrames = %d, %v; want 3, nil", n, err)
	}
	if n, err := CountFrames(singleFrame(t, 40, 30)); err != nil || n != 1 {
		t.Fatalf("CountFrames(single) = %d, %v; want 1, nil", n, err)
	}
}

// TestCountFramesReadsBigEndian covers the "MM" byte order: the fixture and
// the encoder both write little-endian.
func TestCountFramesReadsBigEndian(t *testing.T) {
	b := []byte{'M', 'M', 0, 42, 0, 0, 0, 8} // header, first IFD at offset 8
	b = append(b, 0, 0)                      // an IFD with no entries
	b = append(b, 0, 0, 0, 0)                // and no next IFD
	if n, err := CountFrames(b); err != nil || n != 1 {
		t.Fatalf("CountFrames = %d, %v; want 1, nil", n, err)
	}
}

func TestCountFramesCorrupt(t *testing.T) {
	multi := testfixtures.Read(t, testfixtures.MultiTIFF)
	cases := map[string][]byte{
		"empty":        nil,
		"short":        []byte("II*\x00"),
		"bad order":    append([]byte("XX*\x00\x08\x00\x00\x00"), 0, 0),
		"bad magic":    []byte("II\x2b\x00\x08\x00\x00\x00\x00\x00"),
		"ifd past end": []byte("II*\x00\xff\xff\xff\xff"),
		"no ifd":       []byte("II*\x00\x00\x00\x00\x00"),
		"truncated":    multi[:len(multi)/2],
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := CountFrames(in)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("err = %v, want ErrCorrupt", err)
			}
			if got := fault.CodeOf(err); got != fault.DocumentCorrupt {
				t.Errorf("code = %q, want %q", got, fault.DocumentCorrupt)
			}
		})
	}
}

// TestCountFramesRejectsIFDCycle covers the self-referential chain: without
// the set of visited offsets the walk never terminates.
func TestCountFramesRejectsIFDCycle(t *testing.T) {
	b := singleFrame(t, 8, 8)
	order := binary.LittleEndian
	off := order.Uint32(b[4:8])
	entries := order.Uint16(b[off : off+2])
	next := int(off) + 2 + int(entries)*12
	order.PutUint32(b[next:next+4], off) // point the IFD at itself

	if _, err := CountFrames(b); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// TestFramePNGSelectsTheRequestedFrame is the core of multi-frame support:
// each IFD must be isolated and decoded on its own. The fixture's third frame
// is 20x20 while the first two are 10x10, so a walk that always lands on
// frame 1, which is what decoding the whole file does, cannot pass this.
func TestFramePNGSelectsTheRequestedFrame(t *testing.T) {
	b := testfixtures.Read(t, testfixtures.MultiTIFF)
	want := []struct{ w, h int }{{10, 10}, {10, 10}, {20, 20}}

	for i, dims := range want {
		out, w, h, err := FramePNG(b, i+1)
		if err != nil {
			t.Fatalf("frame %d: %v", i+1, err)
		}
		if w != dims.w || h != dims.h {
			t.Errorf("frame %d dims = %dx%d, want %dx%d", i+1, w, h, dims.w, dims.h)
		}
		img, err := png.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("frame %d is not a decodable PNG: %v", i+1, err)
		}
		if got := img.Bounds(); got.Dx() != dims.w || got.Dy() != dims.h {
			t.Errorf("frame %d PNG bounds = %v, want %dx%d", i+1, got, dims.w, dims.h)
		}
	}
}

// TestFramePNGOutOfRange covers a frame number the file does not have. It is
// the caller's mistake and not the document's, so the error carries no fault
// code and reads as internal.
func TestFramePNGOutOfRange(t *testing.T) {
	b := testfixtures.Read(t, testfixtures.MultiTIFF)
	for _, n := range []int{0, -1, 4} {
		_, _, _, err := FramePNG(b, n)
		if err == nil {
			t.Errorf("FramePNG(frame %d) = nil error, want out of range", n)
			continue
		}
		if got := fault.CodeOf(err); got != fault.Internal {
			t.Errorf("FramePNG(frame %d) code = %q, want %q", n, got, fault.Internal)
		}
	}
}

func TestFramePNGCorruptContainer(t *testing.T) {
	_, _, _, err := FramePNG([]byte("not a tiff at all"), 1)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
	if got := fault.CodeOf(err); got != fault.DocumentCorrupt {
		t.Errorf("code = %q, want %q", got, fault.DocumentCorrupt)
	}
}

// TestFramePNGDoesNotMutateInput guards the copy-and-rewrite isolation: the
// caller's buffer is the document, and later frames still have to be readable
// from it.
func TestFramePNGDoesNotMutateInput(t *testing.T) {
	b := testfixtures.Read(t, testfixtures.MultiTIFF)
	before := bytes.Clone(b)
	if _, _, _, err := FramePNG(b, 2); err != nil {
		t.Fatalf("frame 2: %v", err)
	}
	if !bytes.Equal(b, before) {
		t.Fatal("FramePNG modified the input buffer")
	}
	if _, w, h, err := FramePNG(b, 3); err != nil || w != 20 || h != 20 {
		t.Fatalf("frame 3 after frame 2 = %dx%d, %v; want 20x20, nil", w, h, err)
	}
}

// ifd builds a little-endian single-IFD TIFF from LONG entries of one value
// each, with no pixel data. It costs nothing to construct, whatever
// dimensions it declares.
func ifd(entries ...[2]uint32) []byte {
	le := binary.LittleEndian
	b := []byte{'I', 'I'}
	b = le.AppendUint16(b, 42)
	b = le.AppendUint32(b, 8)
	b = le.AppendUint16(b, uint16(len(entries)))
	for _, e := range entries {
		b = le.AppendUint16(b, uint16(e[0])) // tag
		b = le.AppendUint16(b, 4)            // type LONG
		b = le.AppendUint32(b, 1)            // count
		b = le.AppendUint32(b, e[1])         // value
	}
	return le.AppendUint32(b, 0)
}

// hugeFrame declares a w x h frame. Decoding it would take w*h*4 bytes.
func hugeFrame(w, h uint32) []byte {
	return ifd([2]uint32{256, w}, [2]uint32{257, h}) // ImageWidth, ImageLength
}

// TestFramePNGRejectsOversizeFrame pins the decode bound. The declared
// dimensions come from the file and decoding holds the whole frame in memory,
// so without the bound a few dozen header bytes would drive a worker into a
// multi-gigabyte allocation.
func TestFramePNGRejectsOversizeFrame(t *testing.T) {
	_, _, _, err := FramePNG(hugeFrame(40000, 40000), 1) // 1.6e9 pixels
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	if got := fault.CodeOf(err); got != fault.FileTooLarge {
		t.Errorf("code = %q, want %q", got, fault.FileTooLarge)
	}
}

// TestFramePNGAdmitsPrintScan keeps the bound above real scanned pages: A4 at
// 600 dpi is about 34.8 megapixels and must not be refused for its size. The
// frame carries no pixel data, so it fails to decode, and that failure is the
// document's.
func TestFramePNGAdmitsPrintScan(t *testing.T) {
	_, _, _, err := FramePNG(hugeFrame(4960, 7016), 1)
	if errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("a 600 dpi A4 scan was refused by the pixel bound: %v", err)
	}
	if got := fault.CodeOf(err); got != fault.DocumentCorrupt {
		t.Errorf("code = %q, want %q for a frame with no pixel data", got, fault.DocumentCorrupt)
	}
}

// TestFramePNGUnreadableFrameHeader covers an IFD the container walk accepts
// and the decoder does not: its one entry has a data type the format does not
// define.
func TestFramePNGUnreadableFrameHeader(t *testing.T) {
	b := ifd([2]uint32{256, 10})
	binary.LittleEndian.PutUint16(b[12:14], 99) // the entry's type field
	_, _, _, err := FramePNG(b, 1)
	if err == nil {
		t.Fatal("FramePNG = nil error, want a failure for an unreadable frame header")
	}
	if got := fault.CodeOf(err); got != fault.DocumentCorrupt {
		t.Errorf("code = %q, want %q", got, fault.DocumentCorrupt)
	}
}

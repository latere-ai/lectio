// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package tiffx reads the TIFF container. It walks the chain of image file
// directories (IFDs) to count the frames a multi-frame TIFF carries, and
// extracts any one frame as a standalone PNG. Multi-frame TIFF is the usual
// container for a scanned document, one frame per page, and
// golang.org/x/image/tiff decodes only the first frame of a file, so counting
// the pages and producing the image of one page both go through here.
package tiffx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image/png"

	// The import also registers the TIFF format with the image package, so
	// image.Decode and image.DecodeConfig read a TIFF anywhere in a process
	// that links this package.
	"golang.org/x/image/tiff"

	"latere.ai/x/lectio/internal/fault"
)

// ErrCorrupt is under every failure for input that is not a readable TIFF: a
// bad byte-order mark or magic number, a truncated IFD, or a cyclic IFD
// chain. The failure's code is fault.DocumentCorrupt.
var ErrCorrupt = errors.New("tiff: structure is unreadable")

// ErrFrameTooLarge is under the failure for a frame whose declared pixel area
// is over MaxFramePixels. The failure's code is fault.FileTooLarge.
var ErrFrameTooLarge = errors.New("tiff: frame is too large to decode")

// MaxFramePixels bounds the decoded area of one frame. Decoding holds the
// whole frame in memory and the declared dimensions come from the file, so
// the bound is checked from the frame's header before any pixel buffer is
// allocated. 40 megapixels admits an A4 page scanned at 600 dpi, about 34.8
// megapixels, so a full-quality print scan passes.
const MaxFramePixels = 40_000_000

// header is the 8-byte TIFF header: byte order, the magic number 42, and the
// offset of the first IFD.
type header struct {
	order binary.ByteOrder
	first uint32
}

// corrupt is the failure for a container that cannot be walked.
func corrupt() error {
	return fault.Wrap(fault.DocumentCorrupt, ErrCorrupt, "TIFF structure is unreadable")
}

func readHeader(b []byte) (header, error) {
	if len(b) < 8 {
		return header{}, corrupt()
	}
	var h header
	switch {
	case b[0] == 'I' && b[1] == 'I':
		h.order = binary.LittleEndian
	case b[0] == 'M' && b[1] == 'M':
		h.order = binary.BigEndian
	default:
		return header{}, corrupt()
	}
	if h.order.Uint16(b[2:4]) != 42 {
		return header{}, corrupt()
	}
	h.first = h.order.Uint32(b[4:8])
	return h, nil
}

// frameOffsets returns the file offset of every IFD, in chain order. Each IFD
// is one frame. A cycle or an offset past the end of the buffer is corrupt, so
// the walk is bounded by the input and not by the chain it is reading.
func frameOffsets(b []byte) (header, []uint32, error) {
	h, err := readHeader(b)
	if err != nil {
		return header{}, nil, err
	}
	var offsets []uint32
	seen := map[uint32]bool{}
	for off := h.first; off != 0; {
		if seen[off] || int(off)+2 > len(b) {
			return header{}, nil, corrupt()
		}
		seen[off] = true
		entries := h.order.Uint16(b[off : off+2])
		next := int(off) + 2 + int(entries)*12
		if next+4 > len(b) {
			return header{}, nil, corrupt()
		}
		offsets = append(offsets, off)
		off = h.order.Uint32(b[next : next+4])
	}
	if len(offsets) == 0 {
		return header{}, nil, corrupt()
	}
	return h, offsets, nil
}

// CountFrames returns how many frames a TIFF carries, one per IFD. A
// single-frame TIFF returns 1. Input that is not a readable TIFF fails with
// fault.DocumentCorrupt over ErrCorrupt.
func CountFrames(b []byte) (int, error) {
	_, offsets, err := frameOffsets(b)
	if err != nil {
		return 0, err
	}
	return len(offsets), nil
}

// FramePNG decodes the 1-based frame n and encodes it as a PNG. It returns
// the encoded bytes and the frame's width and height in pixels.
//
// The frame is isolated by rewriting a copy of the whole file: the header
// points at frame n's IFD and that IFD ends the chain. Every other offset in
// the file (strip data, color maps) stays valid because the copy keeps the
// original layout, so no tag is rewritten and b is not modified.
//
// A container that cannot be walked and a frame that cannot be decoded fail
// with fault.DocumentCorrupt, and a frame over MaxFramePixels fails with
// fault.FileTooLarge. A frame number outside the file and a PNG that cannot
// be encoded are errors with no fault code: neither is something the owner of
// the file can act on.
func FramePNG(b []byte, n int) (out []byte, width, height int, err error) {
	h, offsets, err := frameOffsets(b)
	if err != nil {
		return nil, 0, 0, err
	}
	if n < 1 || n > len(offsets) {
		return nil, 0, 0, fmt.Errorf("tiff: frame %d out of range (%d frames)", n, len(offsets))
	}

	single := bytes.Clone(b)
	off := offsets[n-1]
	h.order.PutUint32(single[4:8], off)
	entries := h.order.Uint16(single[off : off+2])
	next := int(off) + 2 + int(entries)*12
	h.order.PutUint32(single[next:next+4], 0)

	// DecodeConfig reads only the IFD, so the bound is enforced before Decode
	// allocates the pixel buffer.
	cfg, err := tiff.DecodeConfig(bytes.NewReader(single))
	if err != nil {
		return nil, 0, 0, fault.Wrap(fault.DocumentCorrupt, err, "TIFF frame %d has an unreadable header", n)
	}
	if pixels := int64(cfg.Width) * int64(cfg.Height); pixels > MaxFramePixels {
		return nil, 0, 0, fault.Wrap(fault.FileTooLarge, ErrFrameTooLarge,
			"TIFF frame %d declares %d pixels, limit %d", n, pixels, MaxFramePixels)
	}

	img, err := tiff.Decode(bytes.NewReader(single))
	if err != nil {
		return nil, 0, 0, fault.Wrap(fault.DocumentCorrupt, err, "TIFF frame %d cannot be decoded", n)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, 0, 0, fmt.Errorf("tiff: encode frame %d: %w", n, err)
	}
	bounds := img.Bounds()
	return buf.Bytes(), bounds.Dx(), bounds.Dy(), nil
}

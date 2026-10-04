// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package reader

// PageText is what a file itself holds of one page, beside the image a
// renderer makes of it: the words the page carries, each with where it is
// drawn and in what type, and where the page paints anything that is not
// text. A typeset PDF holds all of it; a scan holds a picture and no word.
//
// A reader that asks for it (Description.Text) can build a page's blocks
// from it with no model, and can tell from it when it must not: the words
// say what the page reads, and the rectangles and drawings say what the
// page shows that no word accounts for.
//
// Every position is in points, 1/72 inch, from the top left corner of the
// page as it is shown, x to the right and y downward: the page after its
// rotation and its crop, which is the page a renderer draws. A position
// over the page's width and height is a block's box.
type PageText struct {
	// Width and Height are the page's size in points.
	Width, Height float64

	// Words are the page's words in the order the file draws them, which
	// is not always the order they are read in.
	Words []Word

	// Rects are the upright rectangles the page paints: a line of a
	// table's ruling or a separator as a rectangle as thin as the line is
	// thick, a frame as its 4 sides, and a filled area such as a cell's
	// background as it is.
	Rects []Rect

	// Drawings are the boxes around everything else the page paints: an
	// image, a shading, and a shape that is not an upright rectangle.
	Drawings []Rect

	// Partial reports that the page holds more characters or more painted
	// objects than are read of one page, so that Words, Rects and Drawings
	// are not the whole of it.
	Partial bool
}

// Rect is an upright rectangle on a page: X0 and Y0 its top left corner,
// X1 and Y1 its bottom right one.
type Rect struct{ X0, Y0, X1, Y1 float64 }

// Width is the rectangle's extent across the page.
func (r Rect) Width() float64 { return r.X1 - r.X0 }

// Height is the rectangle's extent down the page.
func (r Rect) Height() float64 { return r.Y1 - r.Y0 }

// Word is a run of characters the file draws with no space between them.
type Word struct {
	// Text is the word as the file maps its characters to Unicode. A
	// character the file maps to nothing is U+FFFD.
	Text string

	// Box is where the word is drawn. Across the page it spans the word's
	// glyphs; down the page it spans the line of type, from the font's
	// ascent to its descent, so that the words of one line share their
	// extent whatever letters they hold.
	Box Rect

	// Baseline is the y the word stands on.
	Baseline float64

	// Size is the size of the word's type in points, as it is drawn.
	Size float64

	// Bold and Italic are what the name and the flags of the word's font
	// say of it. A font that says neither is neither.
	Bold, Italic bool

	// Hidden reports that the file draws the word with no fill and no
	// stroke: text that is in the file and not on the page, as the text a
	// recognition pass lays under a scan is.
	Hidden bool

	// Turned reports that the word does not run from left to right along
	// the page: it is drawn at an angle, upward, downward or upside down.
	Turned bool

	// Unmapped counts the characters of the word the file maps to no
	// Unicode character.
	Unmapped int
}

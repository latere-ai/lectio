// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package testfixtures

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Logbook returns a PDF of as many pages as asked for that carries its
// text: a harbor log with one entry to a page. A page holds a heading that
// names its entry, 2 paragraphs, and its number at the foot, set in the
// standard fonts, which a file names and does not hold. The pages listed
// in pictures hold a picture of a page and no text, as a page scanned
// into a typeset file does.
//
// The file is written here so that a test can ask for any length: one of
// 300 pages is under 300 KB. Every sentence in it was written for this
// repository, and LogbookEntry says what each page reads.
func Logbook(pages int, pictures ...int) []byte {
	var out bytes.Buffer
	var offsets []int
	object := func(body string) int {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
		return len(offsets)
	}
	stream := func(dict, body string) string {
		return fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", dict, len(body), body)
	}
	out.WriteString("%PDF-1.4\n")

	// The objects every page shares. The page tree is object 2, written
	// once its kids are known, so its place is kept.
	object("<< /Type /Catalog /Pages 2 0 R >>")
	tree := object("")
	offsets[tree-1] = -1
	upright := object("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	bold := object("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold >>")
	// The picture: 32 by 32 samples of gray in bands, so that a page that
	// holds it is not one color.
	samples := make([]byte, 32*32)
	for i := range samples {
		samples[i] = byte(40 + 180*((i/32/4+i%32/4)%2))
	}
	picture := object(stream("/Type /XObject /Subtype /Image /Width 32 /Height 32 /ColorSpace /DeviceGray /BitsPerComponent 8", string(samples)))
	resources := fmt.Sprintf("<< /Font << /F1 %d 0 R /F2 %d 0 R >> /XObject << /Im %d 0 R >> >>", upright, bold, picture)

	var kids []string
	for n := 1; n <= pages; n++ {
		content := "q 468 0 0 468 72 200 cm /Im Do Q"
		if !slices.Contains(pictures, n) {
			heading, paragraphs := LogbookEntry(n)
			var page strings.Builder
			fmt.Fprintf(&page, "BT /F2 16 Tf 72 708 Td (%s) Tj ET\n", heading)
			y := 676
			for _, paragraph := range paragraphs {
				for _, line := range wrapped(paragraph, 76) {
					fmt.Fprintf(&page, "BT /F1 11 Tf 72 %d Td (%s) Tj ET\n", y, line)
					y -= 15
				}
				y -= 9
			}
			fmt.Fprintf(&page, "BT /F1 9 Tf 300 40 Td (%d) Tj ET", n)
			content = page.String()
		}
		body := object(stream("", content))
		kids = append(kids, strconv.Itoa(object(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources %s >>", body, resources)))+" 0 R")
	}
	offsets[tree-1] = out.Len()
	fmt.Fprintf(&out, "%d 0 obj\n<< /Type /Pages /Kids [ %s ] /Count %d >>\nendobj\n", tree, strings.Join(kids, " "), pages)

	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, at := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", at)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return out.Bytes()
}

// LogbookEntry is what page n of a Logbook reads: its heading and its 2
// paragraphs. The page's number at its foot is n.
func LogbookEntry(n int) (heading string, paragraphs []string) {
	return fmt.Sprintf("Entry %d of the harbor log", n), []string{
		fmt.Sprintf("The gauge at the north mole was read at six in the morning and again at noon. Reading %d stands in the first column of the log, and the officer of the watch signed for it before the next tide came in.", n),
		fmt.Sprintf("No correction was applied to entry %d. The pier master keeps the paper copy in the harbor office, and this page is the copy that the office files with the survey.", n),
	}
}

// wrapped breaks a paragraph into lines of at most width characters, at
// its spaces.
func wrapped(paragraph string, width int) []string {
	var lines []string
	line := ""
	for word := range strings.FieldsSeq(paragraph) {
		if line != "" && len(line)+1+len(word) > width {
			lines, line = append(lines, line), ""
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	return append(lines, line)
}

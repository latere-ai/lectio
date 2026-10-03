// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"context"
	"encoding/xml"
	"strconv"
	"strings"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
)

// The namespaces a word-processing document's markup is read by. An element
// is known by its namespace and its local name, never by its prefix, which
// a producer is free to choose.
const (
	wordSpace       = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	wordStrictSpace = "http://purl.oclc.org/ooxml/wordprocessingml/main"
	mathSpace       = "http://schemas.openxmlformats.org/officeDocument/2006/math"
	compatSpace     = "http://schemas.openxmlformats.org/markup-compatibility/2006"
)

// word returns the local name of an element of the word-processing
// vocabulary, and the empty string for an element of any other.
func word(n xml.Name) string {
	if n.Space == wordSpace || n.Space == wordStrictSpace {
		return n.Local
	}
	return ""
}

// dropped are the elements whose content is not the document's current
// text: what a tracked change deleted or moved away, the code of a field as
// opposed to its result, the phonetic guide over a run, the earlier state a
// tracked change of formatting keeps, and properties. Everything else that
// the reader does not know is read through, so a wrapper it has never seen
// loses no text.
var dropped = map[string]bool{
	"del": true, "moveFrom": true, "delText": true, "instrText": true, "delInstrText": true,
	"rt": true, "pPrChange": true, "rPrChange": true, "sectPr": true, "sdtPr": true,
	"sdtEndPr": true, "tblPr": true, "tblPrEx": true, "customXmlPr": true,
}

// pictured are the elements that say a drawing shows something that is not
// text: a picture, a chart, a diagram, an embedded object, ink.
var pictured = map[string]bool{
	"blip": true, "imagedata": true, "chart": true, "relIds": true, "OLEObject": true, "contentPart": true,
}

// props are the properties of a paragraph, or of a paragraph style, that
// decide what kind of block a paragraph is.
type props struct {
	style   string // the id of the paragraph's style, or of the style a style is based on
	name    string // a style's name, in lower case and without spaces
	outline int    // the outline level, from 0; -1 when none is set
	list    int    // 1 in a list, -1 taken out of one, 0 when nothing is said
}

// docx reads one word-processing document.
type docx struct {
	p      *pkg
	styles map[string]props
	// cited are the notes the body refers to, in the order of their first
	// reference, as "footnote:<id>" and "endnote:<id>".
	cited []string
	seen  map[string]bool
	// inCell counts the table cells the reader is inside of.
	inCell int
}

// readDOCX reads a word-processing document into one page of blocks in
// document order. Tracked changes are read as accepted: what was inserted
// is text and what was deleted is not.
func readDOCX(ctx context.Context, data []byte, b bounds) ([]document.Page, error) {
	p, err := open(ctx, data, b)
	if err != nil {
		return nil, err
	}
	rels, err := p.relations("")
	if err != nil {
		return nil, err
	}
	main := first(rels, "officeDocument")
	if !p.has(main) {
		main = "word/document.xml"
	}
	parts, err := p.relations(main)
	if err != nil {
		return nil, err
	}

	d := &docx{p: p, styles: map[string]props{}, seen: map[string]bool{}}
	if name := first(parts, "styles"); p.has(name) {
		if err := d.readStyles(name); err != nil {
			return nil, err
		}
	}
	blocks, err := d.readBody(main)
	if err != nil {
		return nil, err
	}
	for _, kind := range []string{"footnote", "endnote"} {
		notes, err := d.readNotes(first(parts, kind+"s"), kind)
		if err != nil {
			return nil, err
		}
		for _, key := range d.cited {
			if text := notes[key]; text != "" {
				blocks = append(blocks, document.Block{Kind: document.KindFootnote, Text: text})
			}
		}
	}
	if len(blocks) > b.blocks {
		return nil, tooManyBlocks(b)
	}
	return []document.Page{{
		Number: 1, State: document.PageSucceeded, Source: document.SourceNative,
		Blocks: document.Number(1, blocks),
		Usage:  &document.Usage{Pages: 1},
	}}, nil
}

func tooManyBlocks(b bounds) error {
	return fault.New(fault.FileTooLarge, "the document holds over %d blocks", b.blocks)
}

// readBody reads the blocks of the main part.
func (d *docx) readBody(name string) ([]document.Block, error) {
	m, err := d.p.read(name, "main document part")
	if err != nil {
		return nil, err
	}
	defer m.close()
	root, err := m.root()
	if err != nil {
		return nil, err
	}
	if word(root.Name) != "document" {
		return nil, fault.New(fault.DocumentCorrupt, "the main document part is not a word-processing document")
	}
	var blocks []document.Block
	for m.open > 0 {
		tok, err := m.next()
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if word(se.Name) != "body" {
			// The page background, and whatever else is beside the body.
			if err := m.skip(); err != nil {
				return nil, err
			}
			continue
		}
		body, err := d.flow(m, nil)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, body...)
	}
	return blocks, m.end()
}

// readStyles reads the paragraph styles: the name of each, the style it is
// based on, and the outline level and list membership it sets.
func (d *docx) readStyles(name string) error {
	m, err := d.p.read(name, "styles part")
	if err != nil {
		return err
	}
	defer m.close()
	if _, err := m.root(); err != nil {
		return err
	}
	for m.open > 0 {
		tok, err := m.next()
		if err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || word(se.Name) != "style" {
			continue
		}
		if attr(se, "type") != "paragraph" {
			if err := m.skip(); err != nil {
				return err
			}
			continue
		}
		st := props{outline: -1}
		for in := m.open; m.open >= in; {
			tok, err := m.next()
			if err != nil {
				return err
			}
			child, ok := tok.(xml.StartElement)
			if !ok {
				continue
			}
			switch word(child.Name) {
			case "name":
				st.name = styleName(attr(child, "val"))
			case "basedOn":
				st.style = attr(child, "val")
			case "pPr":
				err = d.paragraphProps(m, &st)
			case "rPr", "tblPr", "trPr", "tcPr", "tblStylePr":
				err = m.skip()
			}
			if err != nil {
				return err
			}
		}
		if d.styles[attr(se, "styleId")] = st; len(d.styles) > d.p.b.formats {
			return fault.New(fault.FileTooLarge, "the document declares over %d styles", d.p.b.formats)
		}
	}
	return m.end()
}

// styleName folds a style's name for comparison: "Heading 1", "heading 1"
// and the id "Heading1" are one name.
func styleName(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, " ", ""))
}

// paragraphProps reads the properties of a paragraph or of a paragraph
// style, to the end of the element that holds them.
func (d *docx) paragraphProps(m *markup, into *props) error {
	for in := m.open; m.open >= in; {
		tok, err := m.next()
		if err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch word(se.Name) {
		case "pStyle":
			into.style = attr(se, "val")
		case "outlineLvl":
			if n, err := strconv.Atoi(attr(se, "val")); err == nil && n >= 0 {
				into.outline = n
			}
		case "numId":
			// The list a paragraph belongs to. The id 0 takes a paragraph
			// out of the list its style would put it in.
			if into.list = 1; attr(se, "val") == "0" {
				into.list = -1
			}
		case "rPr", "pPrChange", "sectPr":
			// The mark's own formatting, the properties before a tracked
			// change, and a section's: none says what the paragraph is.
			if err := m.skip(); err != nil {
				return err
			}
		}
	}
	return nil
}

// kind decides what a paragraph is from its properties and its style's.
// What the paragraph sets itself wins over its style, and a style over the
// one it is based on. A heading that is numbered is a heading.
func (d *docx) kind(pr props) (document.Kind, int) {
	outline, list, name := pr.outline, pr.list, ""
	// A style that is not declared is known by its id, which for the
	// built-in styles is the name without its spaces.
	id := pr.style
	for hops := 0; id != "" && hops < 16; hops++ {
		st, declared := d.styles[id]
		if !declared {
			st = props{name: styleName(id), outline: -1}
		}
		if name == "" && named(st.name) {
			name = st.name
		}
		if outline < 0 {
			outline = st.outline
		}
		if list == 0 {
			list = st.list
		}
		id = st.style
	}

	switch level, heading := strings.CutPrefix(name, "heading"); {
	case name == "title":
		return document.KindTitle, 1
	case name == "subtitle":
		return document.KindHeading, 2
	case name == "caption":
		return document.KindCaption, 0
	case heading:
		// named has checked that what follows is a level.
		n, _ := strconv.Atoi(level)
		return document.KindHeading, min(n, 6)
	}
	// Outline levels count from 0, and level 9 is body text.
	if outline >= 0 && outline < 9 {
		return document.KindHeading, min(outline+1, 6)
	}
	if list > 0 {
		return document.KindListItem, 0
	}
	return document.KindText, 0
}

// named reports whether a folded style name is one that says what a
// paragraph is.
func named(name string) bool {
	if level, ok := strings.CutPrefix(name, "heading"); ok {
		n, err := strconv.Atoi(level)
		return err == nil && n >= 1 && n <= 9
	}
	return name == "title" || name == "subtitle" || name == "caption"
}

// flow reads block-level content to the end of the element the reader is
// in: the paragraphs and tables, and what is inside the wrappers around
// them. before, when it is not nil, is shown each start tag first and
// reports whether it read the element.
func (d *docx) flow(m *markup, before func(xml.StartElement) (bool, error)) ([]document.Block, error) {
	var out []document.Block
	for in := m.open; m.open >= in; {
		tok, err := m.next()
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if before != nil {
			done, err := before(se)
			if err != nil {
				return nil, err
			}
			if done {
				continue
			}
		}
		var blocks []document.Block
		switch name := word(se.Name); {
		case name == "p":
			blocks, err = d.paragraph(m)
		case name == "tbl":
			blocks, err = d.table(m)
		case dropped[name] || alternate(se.Name):
			err = m.skip()
		}
		if err != nil {
			return nil, err
		}
		if out = append(out, blocks...); len(out) > d.p.b.blocks {
			return nil, tooManyBlocks(d.p.b)
		}
	}
	return out, nil
}

// alternate reports whether an element is the fallback of an alternate
// content block. A producer writes a drawing twice, once in the current
// markup and once in the older one for a reader that lacks it; reading both
// would say everything in it twice.
func alternate(n xml.Name) bool {
	return n.Space == compatSpace && n.Local == "Fallback"
}

// paragraph reads one paragraph: its text as a block of the kind its style
// says, then a figure for each picture in it, then the paragraphs of any
// text box anchored in it.
func (d *docx) paragraph(m *markup) ([]document.Block, error) {
	var (
		pr      = props{outline: -1}
		text    strings.Builder
		figures int
		inside  []document.Block
		// mark is what the next text of the run being read opens with: a
		// caret for a raised run, an underscore for a lowered one.
		mark string
	)
	for in := m.open; m.open >= in; {
		tok, err := m.next()
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			if end, ok := tok.(xml.EndElement); ok && word(end.Name) == "r" {
				mark = ""
			}
			continue
		}
		switch name := word(se.Name); {
		case name == "pPr":
			err = d.paragraphProps(m, &pr)
		case name == "rPr":
			mark, err = d.runMark(m)
		case name == "t" || (se.Name.Space == mathSpace && se.Name.Local == "t"):
			var s string
			if s, err = m.text(); s != "" {
				text.WriteString(mark + s)
				mark = ""
			}
		case name == "tab":
			text.WriteByte('\t')
		case name == "cr", name == "br" && (attr(se, "type") == "" || attr(se, "type") == "textWrapping"):
			// A break to the next page or column is layout, not text.
			text.WriteByte('\n')
		case name == "noBreakHyphen":
			text.WriteByte('-')
		case name == "footnoteReference", name == "endnoteReference":
			err = d.cite(strings.TrimSuffix(name, "Reference") + ":" + attr(se, "id"))
		case name == "drawing", name == "pict", name == "object":
			var figure bool
			var blocks []document.Block
			if figure, blocks, err = d.graphic(m); figure {
				figures++
			}
			inside = append(inside, blocks...)
		case dropped[name] || alternate(se.Name):
			err = m.skip()
		}
		if err != nil {
			return nil, err
		}
	}

	var out []document.Block
	if s := strings.TrimSpace(text.String()); s != "" {
		kind, level := d.kind(pr)
		out = append(out, document.Block{Kind: kind, Level: level, Text: s})
	}
	for range figures {
		out = append(out, document.Block{Kind: document.KindFigure})
	}
	return append(out, inside...), nil
}

// runMark reads a run's properties for the one that changes its text: a
// raised or lowered run inside a table cell is marked, so that n squared
// does not become n2 (specs/002-object-model.md).
func (d *docx) runMark(m *markup) (string, error) {
	mark := ""
	for in := m.open; m.open >= in; {
		tok, err := m.next()
		if err != nil {
			return "", err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || word(se.Name) != "vertAlign" || d.inCell == 0 {
			continue
		}
		switch attr(se, "val") {
		case "superscript":
			mark = "^"
		case "subscript":
			mark = "_"
		}
	}
	return mark, nil
}

// cite records a reference to a note, once.
func (d *docx) cite(key string) error {
	if d.seen[key] {
		return nil
	}
	d.seen[key] = true
	if d.cited = append(d.cited, key); len(d.cited) > d.p.b.blocks {
		return tooManyBlocks(d.p.b)
	}
	return nil
}

// graphic reads a drawing, a picture in the older vector markup, or an
// embedded object. figure reports whether it shows something that is not
// text; inside are the paragraphs of the text boxes in it.
func (d *docx) graphic(m *markup) (figure bool, inside []document.Block, err error) {
	for in := m.open; m.open >= in; {
		tok, err := m.next()
		if err != nil {
			return false, nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch {
		case word(se.Name) == "txbxContent":
			blocks, err := d.flow(m, nil)
			if err != nil {
				return false, nil, err
			}
			inside = append(inside, blocks...)
		case alternate(se.Name):
			if err := m.skip(); err != nil {
				return false, nil, err
			}
		case pictured[se.Name.Local]:
			figure = true
		}
	}
	return figure, inside, nil
}

// tableRow is one row of a table as it is read.
type tableRow struct {
	header  bool // the row repeats as a header
	deleted bool // a tracked change deleted the row
	before  int  // grid columns the row leaves empty before its first cell
	cells   []tableCell
}

// tableCell is one cell of a table as it is read.
type tableCell struct {
	span    int    // grid columns the cell covers
	merge   string // "restart" opens a vertical merge, "continue" continues one
	text    string
	figures int
}

// table reads one table into a table block, followed by a figure for each
// picture in its cells. A table inside a cell is read into the cell's text.
func (d *docx) table(m *markup) ([]document.Block, error) {
	var (
		cells      []document.Cell
		figures    int
		rows, cols int
		// down maps a grid column to the cell a vertical merge in that
		// column continues, as an index into cells.
		down = map[int]int{}
	)
	for in := m.open; m.open >= in; {
		tok, err := m.next()
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch name := word(se.Name); {
		case name == "gridCol":
			// The grid the table declares, counted one column at a time.
			if in == m.open-2 {
				cols = min(cols+1, maxSpan)
			}
		case dropped[name]:
			if err := m.skip(); err != nil {
				return nil, err
			}
		case name == "tr":
			row, err := d.row(m)
			if err != nil {
				return nil, err
			}
			if row.deleted {
				continue
			}
			col := row.before
			for _, c := range row.cells {
				figures += c.figures
				if at, continues := down[col]; continues && c.merge == "continue" {
					cells[at].RowSpan = max(cells[at].RowSpan, 1) + 1
					if c.text != "" {
						cells[at].Text = strings.TrimPrefix(cells[at].Text+"\n"+c.text, "\n")
					}
					col += c.span
					continue
				}
				// A continuation with nothing above it to continue is a
				// cell of its own.
				cell := document.Cell{Row: rows, Col: col, Header: row.header, Text: c.text}
				if c.span > 1 {
					cell.ColSpan = c.span
				}
				cells = append(cells, cell)
				if delete(down, col); c.merge == "restart" {
					down[col] = len(cells) - 1
				}
				col += c.span
			}
			cols, rows = max(cols, col), rows+1
		}
	}

	var out []document.Block
	if len(cells) > 0 {
		if err := d.p.charge(int64(rows)*int64(cols), 0); err != nil {
			return nil, err
		}
		out = append(out, tableOf(rows, cols, cells))
	}
	for range figures {
		out = append(out, document.Block{Kind: document.KindFigure})
	}
	return out, nil
}

// row reads one row of a table.
func (d *docx) row(m *markup) (tableRow, error) {
	var row tableRow
	for in := m.open; m.open >= in; {
		tok, err := m.next()
		if err != nil {
			return row, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch name := word(se.Name); {
		case name == "tblHeader":
			row.header = on(attr(se, "val"))
		case name == "gridBefore":
			if n, err := strconv.Atoi(attr(se, "val")); err == nil {
				row.before = min(max(n, 0), maxSpan)
			}
		case name == "del" && in == m.open-2:
			// The mark of a deleted row is a child of the row's
			// properties. Deeper, the same element wraps deleted text.
			row.deleted = true
		case name == "trPrChange" || dropped[name]:
			err = m.skip()
		case name == "tc":
			var cell tableCell
			if cell, err = d.cell(m); err == nil {
				row.cells = append(row.cells, cell)
			}
		}
		if err != nil {
			return row, err
		}
	}
	return row, nil
}

// cell reads one cell of a table: its properties, and its content as text.
func (d *docx) cell(m *markup) (tableCell, error) {
	cell := tableCell{span: 1}
	d.inCell++
	blocks, err := d.flow(m, func(se xml.StartElement) (bool, error) {
		if word(se.Name) != "tcPr" {
			return false, nil
		}
		for in := m.open; m.open >= in; {
			tok, err := m.next()
			if err != nil {
				return true, err
			}
			prop, ok := tok.(xml.StartElement)
			if !ok {
				continue
			}
			switch word(prop.Name) {
			case "gridSpan":
				if n, err := strconv.Atoi(attr(prop, "val")); err == nil {
					cell.span = min(max(n, 1), maxSpan)
				}
			case "vMerge":
				// With no value the cell continues the merge above it.
				if cell.merge = attr(prop, "val"); cell.merge != "restart" {
					cell.merge = "continue"
				}
			case "tcPrChange":
				if err := m.skip(); err != nil {
					return true, err
				}
			}
		}
		return true, nil
	})
	d.inCell--
	if err != nil {
		return cell, err
	}
	var lines []string
	for _, b := range blocks {
		switch {
		case b.Kind == document.KindFigure:
			cell.figures++
		case b.Text != "":
			lines = append(lines, b.Text)
		}
	}
	cell.text = strings.Join(lines, "\n")
	return cell, nil
}

// readNotes reads the footnotes or the endnotes the body refers to: the
// text of each, by the key the body cites it with. The separators a note
// part also holds are not notes.
func (d *docx) readNotes(name, kind string) (map[string]string, error) {
	notes := map[string]string{}
	if !d.p.has(name) || len(d.cited) == 0 {
		return notes, nil
	}
	m, err := d.p.read(name, kind+"s part")
	if err != nil {
		return nil, err
	}
	defer m.close()
	if _, err := m.root(); err != nil {
		return nil, err
	}
	for m.open > 0 {
		tok, err := m.next()
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || word(se.Name) != kind {
			continue
		}
		key := kind + ":" + attr(se, "id")
		if what := attr(se, "type"); !d.seen[key] || (what != "" && what != "normal") {
			if err := m.skip(); err != nil {
				return nil, err
			}
			continue
		}
		blocks, err := d.flow(m, nil)
		if err != nil {
			return nil, err
		}
		var lines []string
		for _, b := range blocks {
			if b.Text != "" {
				lines = append(lines, b.Text)
			}
		}
		notes[key] = strings.Join(lines, "\n")
	}
	return notes, m.end()
}

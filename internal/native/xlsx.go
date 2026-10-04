// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"cmp"
	"context"
	"encoding/xml"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
)

// The namespaces a workbook's markup is read by.
const (
	sheetSpace       = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	sheetStrictSpace = "http://purl.oclc.org/ooxml/spreadsheetml/main"
)

// The most rows and columns a sheet can have. A reference past either is
// not one a workbook can hold.
const (
	maxSheetRows = 1 << 20
	maxSheetCols = 1 << 14
)

// sheet returns the local name of an element of the spreadsheet vocabulary,
// and the empty string for an element of any other.
func sheet(n xml.Name) string {
	if n.Space == sheetSpace || n.Space == sheetStrictSpace {
		return n.Local
	}
	return ""
}

// xlsx reads one workbook.
type xlsx struct {
	p *pkg
	// shared are the strings cells name by their index.
	shared []string
	// formats are the cell formats, by the index a cell names.
	formats []format
	// date1904 says the workbook counts days from 1904 and not from 1900.
	date1904 bool
}

// listed is a sheet as the workbook lists it.
type listed struct {
	name string // the name on the sheet's tab
	id   string // the relationship that names the sheet's part
}

// readXLSX reads a workbook into one page per sheet, in the workbook's
// order, hidden sheets included: a title block with the sheet's name, and
// the sheet's cells as one table. A formula is never evaluated; a cell
// holds the value the workbook stored for it.
func readXLSX(ctx context.Context, data []byte, maxPages int, b bounds) ([]document.Page, error) {
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
		main = "xl/workbook.xml"
	}
	x := &xlsx{p: p}
	// The sheets are counted before any is opened, so a workbook over the
	// limit on pages is refused for what it lists.
	sheets, err := x.readWorkbook(main, maxPages)
	if err != nil {
		return nil, err
	}
	parts, err := p.relations(main)
	if err != nil {
		return nil, err
	}
	if name := first(parts, "sharedStrings"); p.has(name) {
		if err := x.readStrings(name); err != nil {
			return nil, err
		}
	}
	if name := first(parts, "styles"); p.has(name) {
		if err := x.readFormats(name); err != nil {
			return nil, err
		}
	}

	pages := make([]document.Page, 0, len(sheets))
	for i, s := range sheets {
		var blocks []document.Block
		if s.name != "" {
			blocks = append(blocks, document.Block{Kind: document.KindTitle, Level: 1, Text: s.name})
		}
		at := slices.IndexFunc(parts, func(r relation) bool { return r.id == s.id })
		if at < 0 {
			return nil, fault.New(fault.DocumentCorrupt, "the workbook lists a sheet that names no part of the package")
		}
		switch part := parts[at]; part.kind {
		case "worksheet":
			table, err := x.readSheet(part.target)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, table...)
		case "chartsheet":
			// A sheet that is one chart: a figure, with nothing read of it.
			blocks = append(blocks, document.Block{Kind: document.KindFigure})
		}
		pages = append(pages, document.Page{
			Number: i + 1, State: document.PageSucceeded, Source: document.SourceNative,
			Blocks: document.Number(i+1, blocks),
			Usage:  &document.Usage{Pages: 1},
		})
	}
	return pages, nil
}

// readWorkbook reads the list of sheets and the date system.
func (x *xlsx) readWorkbook(name string, maxPages int) ([]listed, error) {
	m, err := x.p.read(name, "workbook part")
	if err != nil {
		return nil, err
	}
	defer m.close()
	root, err := m.root()
	if err != nil {
		return nil, err
	}
	if sheet(root.Name) != "workbook" {
		return nil, fault.New(fault.DocumentCorrupt, "the workbook part is not a workbook")
	}
	// A sheet is a part, so a workbook holds no more sheets than a package
	// may hold entries, whatever the limit on pages.
	limit := x.p.b.entries
	if maxPages > 0 {
		limit = min(limit, maxPages)
	}
	var sheets []listed
	for m.open > 0 {
		tok, err := m.next()
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch sheet(se.Name) {
		case "workbookPr":
			x.date1904 = attr(se, "date1904") != "" && on(attr(se, "date1904"))
		case "sheet":
			if sheets = append(sheets, listed{name: attr(se, "name"), id: attr(se, "id")}); len(sheets) > limit {
				return nil, fault.New(fault.TooManyPages, "the workbook lists more sheets than the limit of %d pages", limit)
			}
		}
	}
	if len(sheets) == 0 {
		return nil, fault.New(fault.DocumentCorrupt, "the workbook lists no sheet")
	}
	return sheets, m.end()
}

// readStrings reads the shared strings. The part says how many it holds,
// and that number is never allocated for: it is checked against what the
// part's bytes could hold, and the strings are counted as they are read.
func (x *xlsx) readStrings(name string) error {
	m, err := x.p.read(name, "shared strings part")
	if err != nil {
		return err
	}
	defer m.close()
	root, err := m.root()
	if err != nil {
		return err
	}
	// The shortest string item is an empty element of 5 bytes.
	const shortest = int64(len("<si/>"))
	if n, err := strconv.ParseUint(attr(root, "uniqueCount"), 10, 64); err == nil && n > uint64(m.size/shortest) {
		return fault.New(fault.DocumentCorrupt, "the shared strings part declares %d strings, more than its %d bytes can hold", n, m.size)
	}
	for m.open > 0 {
		tok, err := m.next()
		if err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || sheet(se.Name) != "si" {
			continue
		}
		text, err := x.richText(m)
		if err != nil {
			return err
		}
		if x.shared = append(x.shared, text); int64(len(x.shared)) > x.p.b.cells {
			return fault.New(fault.FileTooLarge, "the workbook holds over %d shared strings", x.p.b.cells)
		}
	}
	return m.end()
}

// richText reads a string item, shared or inline, to its end: the text of
// its runs, without the phonetic guide some locales store beside it.
func (x *xlsx) richText(m *markup) (string, error) {
	var out strings.Builder
	for in := m.open; m.open >= in; {
		tok, err := m.next()
		if err != nil {
			return "", err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch sheet(se.Name) {
		case "t":
			s, err := m.text()
			if err != nil {
				return "", err
			}
			out.WriteString(s)
		case "rPh", "phoneticPr":
			if err := m.skip(); err != nil {
				return "", err
			}
		}
	}
	return out.String(), nil
}

// readFormats reads what each cell format says about how its number reads:
// whether it is a date, a time or a percentage.
func (x *xlsx) readFormats(name string) error {
	m, err := x.p.read(name, "styles part")
	if err != nil {
		return err
	}
	defer m.close()
	if _, err := m.root(); err != nil {
		return err
	}
	declared := map[int]format{}
	var ids []int
	for m.open > 0 {
		tok, err := m.next()
		if err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch sheet(se.Name) {
		case "numFmt":
			if id, err := strconv.Atoi(attr(se, "numFmtId")); err == nil {
				declared[id] = formatOf(attr(se, "formatCode"))
			}
		case "cellXfs":
			// The formats cells name. The styles part holds a second list
			// of the same element, for named styles, that no cell names.
			for in := m.open; m.open >= in; {
				tok, err := m.next()
				if err != nil {
					return err
				}
				if xf, ok := tok.(xml.StartElement); ok && sheet(xf.Name) == "xf" && m.open == in+1 {
					// A format that names no number format has the
					// general one, whose id is 0.
					id, err := strconv.Atoi(attr(xf, "numFmtId"))
					if err != nil {
						id = 0
					}
					ids = append(ids, id)
				}
				if len(ids) > x.p.b.formats {
					return tooManyFormats(x.p.b)
				}
			}
		}
		if len(declared) > x.p.b.formats {
			return tooManyFormats(x.p.b)
		}
	}
	x.formats = make([]format, len(ids))
	for i, id := range ids {
		if f, ok := declared[id]; ok {
			x.formats[i] = f
		} else {
			x.formats[i] = builtin[id]
		}
	}
	return m.end()
}

func tooManyFormats(b bounds) error {
	return fault.New(fault.FileTooLarge, "the workbook declares over %d cell formats", b.formats)
}

// found is one cell of a sheet that holds a value.
type found struct {
	row, col int
	text     string
}

// area is a rectangle of cells, its corners included.
type area struct {
	r0, c0, r1, c1 int
}

func (a area) cells() int64 { return int64(a.r1-a.r0+1) * int64(a.c1-a.c0+1) }

// readSheet reads one sheet into a table block, or into none when the sheet
// holds no value.
//
// The table is the rectangle around the cells that hold a value, with every
// position in it present, so that a row reads in columns whatever the sheet
// left empty. The size a sheet declares for itself is checked against the
// bound on cells and used for nothing: the cells are what the sheet's rows
// hold, and nothing is allocated before they are read.
func (x *xlsx) readSheet(name string) ([]document.Block, error) {
	m, err := x.p.read(name, "sheet part")
	if err != nil {
		return nil, err
	}
	defer m.close()
	if _, err := m.root(); err != nil {
		return nil, err
	}
	var (
		cells    []found
		merges   []area
		row, col = -1, -1
		sorted   = true
	)
	for m.open > 0 {
		tok, err := m.next()
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch sheet(se.Name) {
		case "dimension":
			if a, ok := rangeRef(attr(se, "ref")); ok && x.p.cells+a.cells() > x.p.b.cells {
				return nil, fault.New(fault.FileTooLarge, "a sheet declares %d cells, and the tables of a file may hold %d", a.cells(), x.p.b.cells)
			}
		case "row":
			// A row with no number follows the one before it.
			row, col = row+1, -1
			if ref := attr(se, "r"); ref != "" {
				n, err := strconv.Atoi(ref)
				if err != nil || n < 1 || n > maxSheetRows {
					return nil, badRef()
				}
				row = n - 1
			}
		case "c":
			// A cell with no reference follows the one before it.
			col++
			if ref := attr(se, "r"); ref != "" {
				if row, col, ok = cellRef(ref); !ok {
					return nil, badRef()
				}
			}
			if row < 0 || col >= maxSheetCols {
				return nil, badRef()
			}
			text, err := x.cellText(m, se)
			if err != nil {
				return nil, err
			}
			if text == "" {
				continue
			}
			if err := x.p.charge(0, int64(len(text))); err != nil {
				return nil, err
			}
			if n := len(cells); n > 0 && (cells[n-1].row > row || (cells[n-1].row == row && cells[n-1].col >= col)) {
				sorted = false
			}
			cells = append(cells, found{row: row, col: col, text: text})
		case "mergeCell":
			if a, ok := rangeRef(attr(se, "ref")); ok {
				merges = append(merges, a)
			}
		}
		if int64(len(cells)+len(merges)) > x.p.b.cells {
			return nil, fault.New(fault.FileTooLarge, "a sheet holds over %d cells and merged ranges", x.p.b.cells)
		}
	}
	if err := m.end(); err != nil || len(cells) == 0 {
		return nil, err
	}
	if !sorted {
		slices.SortStableFunc(cells, func(a, b found) int {
			return cmp.Or(cmp.Compare(a.row, b.row), cmp.Compare(a.col, b.col))
		})
	}

	// The rectangle around the values.
	box := area{r0: cells[0].row, r1: cells[len(cells)-1].row, c0: cells[0].col, c1: cells[0].col}
	for _, c := range cells {
		box.c0, box.c1 = min(box.c0, c.col), max(box.c1, c.col)
	}
	if err := x.p.charge(box.cells(), 0); err != nil {
		return nil, err
	}
	rows, cols := box.r1-box.r0+1, box.c1-box.c0+1

	// A merged range, cut to the rectangle, is one cell at its first
	// position and no cell at the others. Ranges that overlap are not a
	// sheet a workbook can hold, and stopping at the first overlap keeps
	// the marking within one pass over the rectangle.
	const covered, anchor = 1, 2
	state := make([]byte, rows*cols)
	spans := map[int][2]int{}
	for _, a := range merges {
		a = area{r0: max(a.r0, box.r0), c0: max(a.c0, box.c0), r1: min(a.r1, box.r1), c1: min(a.c1, box.c1)}
		if a.r0 > a.r1 || a.c0 > a.c1 || a.cells() == 1 {
			continue
		}
		for r := a.r0; r <= a.r1; r++ {
			for c := a.c0; c <= a.c1; c++ {
				at := (r-box.r0)*cols + c - box.c0
				if state[at] != 0 {
					return nil, fault.New(fault.DocumentCorrupt, "the merged ranges of a sheet overlap")
				}
				state[at] = covered
			}
		}
		first := (a.r0-box.r0)*cols + a.c0 - box.c0
		state[first], spans[first] = anchor, [2]int{a.r1 - a.r0 + 1, a.c1 - a.c0 + 1}
	}

	out := make([]document.Cell, 0, rows*cols)
	next := 0
	for r := range rows {
		for c := range cols {
			text := ""
			// The values are in order, so the ones for this position are
			// next. The first of several for one position is the value.
			for first := true; next < len(cells) && cells[next].row == r+box.r0 && cells[next].col == c+box.c0; next++ {
				if first {
					text, first = cells[next].text, false
				}
			}
			at := r*cols + c
			if state[at] == covered {
				continue
			}
			cell := document.Cell{Row: r, Col: c, Text: text}
			if span, ok := spans[at]; ok {
				if span[0] > 1 {
					cell.RowSpan = span[0]
				}
				if span[1] > 1 {
					cell.ColSpan = span[1]
				}
			}
			out = append(out, cell)
		}
	}
	return []document.Block{tableOf(rows, cols, out)}, nil
}

func badRef() error {
	return fault.New(fault.DocumentCorrupt, "a sheet names a cell outside the rows and columns a sheet has")
}

// cellText reads one cell to its end and returns what it shows. kind is the
// cell's type: a shared string, an inline string, a boolean, an error, a
// formula's string result, a date as text, or, with no type, a number read
// through the cell's format. A formula's own text is passed over: the value
// the workbook stored beside it is the cell's value, and a cell with a
// formula and no stored value is empty.
func (x *xlsx) cellText(m *markup, cell xml.StartElement) (string, error) {
	var value, inline string
	for in := m.open; m.open >= in; {
		tok, err := m.next()
		if err != nil {
			return "", err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch sheet(se.Name) {
		case "v":
			value, err = m.text()
		case "is":
			inline, err = x.richText(m)
		case "f":
			err = m.skip()
		}
		if err != nil {
			return "", err
		}
	}

	kind := attr(cell, "t")
	switch kind {
	case "inlineStr":
		return inline, nil
	case "str", "e", "d":
		return value, nil
	}
	if value = strings.TrimSpace(value); value == "" {
		return "", nil
	}
	switch kind {
	case "s":
		i, err := strconv.Atoi(value)
		if err != nil || i < 0 || i >= len(x.shared) {
			return "", fault.New(fault.DocumentCorrupt, "a cell names a shared string the workbook does not hold")
		}
		return x.shared[i], nil
	case "b":
		if value == "1" {
			return "TRUE", nil
		}
		return "FALSE", nil
	}
	var f format
	if i, err := strconv.Atoi(attr(cell, "s")); err == nil && i >= 0 && i < len(x.formats) {
		f = x.formats[i]
	}
	return f.render(value, x.date1904), nil
}

// cellRef reads a cell reference such as "B7" into a row and a column
// counted from 0. A dollar sign, which fixes a reference in a formula, is
// passed over.
func cellRef(ref string) (row, col int, ok bool) {
	i := 0
	for ; i < len(ref); i++ {
		c := ref[i] | 0x20
		if ref[i] == '$' {
			continue
		}
		if c < 'a' || c > 'z' {
			break
		}
		if col = col*26 + int(c-'a') + 1; col > maxSheetCols {
			return 0, 0, false
		}
	}
	for ; i < len(ref); i++ {
		if ref[i] == '$' {
			continue
		}
		if ref[i] < '0' || ref[i] > '9' {
			return 0, 0, false
		}
		if row = row*10 + int(ref[i]-'0'); row > maxSheetRows {
			return 0, 0, false
		}
	}
	if row < 1 || col < 1 {
		return 0, 0, false
	}
	return row - 1, col - 1, true
}

// rangeRef reads a range such as "A1:C3", or one cell, into an area with
// its corners in order.
func rangeRef(ref string) (area, bool) {
	from, to, ranged := strings.Cut(ref, ":")
	if !ranged {
		to = from
	}
	r0, c0, ok0 := cellRef(from)
	r1, c1, ok1 := cellRef(to)
	if !ok0 || !ok1 {
		return area{}, false
	}
	return area{r0: min(r0, r1), c0: min(c0, c1), r1: max(r0, r1), c1: max(c0, c1)}, true
}

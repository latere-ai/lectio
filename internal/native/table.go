// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"html"
	"strconv"
	"strings"

	"latere.ai/x/lectio/document"
)

// tableOf builds the block of a table from its cells, which are in row
// order and, within a row, in column order. The markup is written from the
// cells, so it holds a table and nothing else, and it is the one form that
// carries a span without loss. The text is the cells joined row by row.
func tableOf(rows, cols int, cells []document.Cell) document.Block {
	var markup, text strings.Builder
	markup.WriteString("<table>")
	at, lines := 0, 0
	for row := range rows {
		markup.WriteString("<tr>")
		for first := true; at < len(cells) && cells[at].Row == row; at++ {
			c := cells[at]
			tag := "td"
			if c.Header {
				tag = "th"
			}
			markup.WriteString("<" + tag)
			if c.RowSpan > 1 {
				markup.WriteString(` rowspan="` + strconv.Itoa(c.RowSpan) + `"`)
			}
			if c.ColSpan > 1 {
				markup.WriteString(` colspan="` + strconv.Itoa(c.ColSpan) + `"`)
			}
			markup.WriteString(">" + html.EscapeString(c.Text) + "</" + tag + ">")

			switch {
			case !first:
				text.WriteString(" | ")
			case lines > 0:
				text.WriteByte('\n')
			}
			if first {
				lines++
			}
			text.WriteString(c.Text)
			first = false
		}
		markup.WriteString("</tr>")
	}
	markup.WriteString("</table>")
	return document.Block{
		Kind:  document.KindTable,
		Table: &document.Table{Rows: rows, Cols: cols, Cells: cells, HTML: markup.String()},
		Text:  text.String(),
	}
}

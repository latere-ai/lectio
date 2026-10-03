// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// format is what a cell's number format says about how its number reads.
// A workbook stores a date as a count of days and a percentage as a
// fraction, and only the format tells either from a plain number. Nothing
// else of a format is applied: a stored number is written as the shortest
// decimal that is the same number, with no separator, currency sign or
// rounding the format would add.
type format struct {
	date    bool // the format shows a year, a month or a day
	clock   bool // the format shows hours, minutes or seconds
	seconds bool // the format shows seconds
	elapsed bool // hours count past 24: a duration, not a time of day
	percent bool // the format shows the number times 100, with a percent sign
}

// builtin are the number formats a workbook may use by id without declaring
// them, as far as they are a date, a time or a percentage. The ids 27 to 36
// and 50 to 58 stand for a different date or time format in each East Asian
// locale; they are read as a date, and 32 to 35 as a time.
var builtin = map[int]format{
	9: {percent: true}, 10: {percent: true},
	14: {date: true}, 15: {date: true}, 16: {date: true}, 17: {date: true},
	18: {clock: true}, 19: {clock: true, seconds: true}, 20: {clock: true}, 21: {clock: true, seconds: true},
	22: {date: true, clock: true},
	27: {date: true}, 28: {date: true}, 29: {date: true}, 30: {date: true}, 31: {date: true},
	32: {clock: true}, 33: {clock: true, seconds: true}, 34: {clock: true}, 35: {clock: true, seconds: true},
	36: {date: true},
	45: {clock: true, seconds: true}, 46: {clock: true, seconds: true, elapsed: true}, 47: {clock: true, seconds: true},
	50: {date: true}, 51: {date: true}, 52: {date: true}, 53: {date: true}, 54: {date: true},
	55: {date: true}, 56: {date: true}, 57: {date: true}, 58: {date: true},
}

// formatOf reads a format code for what it shows. Only the first section,
// the one for positive numbers, is read. Text in quotes, an escaped
// character, a padding character and a bracketed color, condition or
// locale are not part of what the code shows.
func formatOf(code string) format {
	var f format
	// letters are the date and time letters of the code in order, each run
	// of one letter once. An m is a month or a minute by its neighbors.
	var letters []byte
	note := func(c byte) {
		if len(letters) == 0 || letters[len(letters)-1] != c {
			letters = append(letters, c)
		}
	}
	for i := 0; i < len(code); i++ {
		switch c := code[i]; c {
		case ';':
			i = len(code)
		case '"':
			if end := strings.IndexByte(code[i+1:], '"'); end >= 0 {
				i += end + 1
			} else {
				i = len(code)
			}
		case '\\', '_', '*':
			i++
		case '[':
			end := strings.IndexByte(code[i:], ']')
			if end < 0 {
				i = len(code)
				break
			}
			// An elapsed count is the one bracket that shows something.
			switch strings.ToLower(code[i+1 : i+end]) {
			case "h", "hh":
				f.elapsed = true
				note('h')
			case "m", "mm":
				f.elapsed = true
				note('n')
			case "s", "ss":
				f.elapsed = true
				note('s')
			}
			i += end
		case '%':
			f.percent = true
		case 'y', 'Y', 'd', 'D', 'h', 'H', 's', 'S', 'm', 'M':
			note(c | 0x20)
		case 'a', 'A':
			// AM/PM and A/P mark a 12-hour clock; their letters are not
			// a month.
			rest := strings.ToLower(code[i:])
			for _, marker := range []string{"am/pm", "a/p"} {
				if strings.HasPrefix(rest, marker) {
					f.clock = true
					i += len(marker) - 1
					break
				}
			}
		}
	}
	for i, c := range letters {
		switch c {
		case 'y', 'd':
			f.date = true
		case 'h', 'n':
			f.clock = true
		case 's':
			f.clock, f.seconds = true, true
		case 'm':
			// Minutes follow hours or precede seconds; a month does neither.
			if (i > 0 && letters[i-1] == 'h') || (i+1 < len(letters) && letters[i+1] == 's') {
				f.clock = true
			} else {
				f.date = true
			}
		}
	}
	return f
}

// lastDay is the serial of 9999-12-31, the last day a workbook can show.
const lastDay = 2958465

// render writes a stored number the way its format reads. date1904 says the
// workbook counts days from 1904. A value that is not a number, or not one
// the format can show, is returned as it is stored.
func (f format) render(raw string, date1904 bool) string {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return raw
	}
	switch {
	case (f.date || f.clock) && v >= 0 && v < lastDay+1:
		return f.moment(v, date1904)
	case f.percent:
		// The product is rounded to the digits a stored number can carry,
		// so that 0.07 reads 7% and not 7.000000000000001%.
		scaled, _ := strconv.ParseFloat(strconv.FormatFloat(v*100, 'g', 15, 64), 64)
		return plain(scaled) + "%"
	}
	return plain(v)
}

// plain writes a number as the shortest decimal that reads back as the same
// number. A workbook stores 2.3 as 2.2999999999999998; both are one number
// and the short form is the one its author typed. Only a magnitude that
// would take dozens of zeros is written with an exponent.
func plain(v float64) string {
	if a := math.Abs(v); a != 0 && (a < 1e-7 || a >= 1e21) {
		return strconv.FormatFloat(v, 'e', -1, 64)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// moment writes a serial day count as an ISO 8601 date, time, or both, with
// as much as the format shows and no more: a format with no seconds gets
// none, and a format with no time gets no time.
func (f format) moment(v float64, date1904 bool) string {
	days := math.Floor(v)
	secs := int(math.Round((v - days) * 86400))
	if secs >= 86400 {
		days, secs = days+1, 0
	}

	clock := ""
	if f.clock {
		hours := secs / 3600
		if f.elapsed {
			// A duration counts its whole days as hours: 1.5 reads 36:00.
			hours += int(days) * 24
		}
		clock = pad(hours) + ":" + pad(secs/60%60)
		if f.seconds {
			clock += ":" + pad(secs%60)
		}
	}
	if !f.date || f.elapsed {
		return clock
	}

	// The 1900 system counts a February 29 that year did not have, as
	// serial 60. Days after it are one further from the epoch than their
	// serial says.
	var day string
	switch n := int(days); {
	case date1904:
		day = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n).Format(time.DateOnly)
	case n == 60:
		day = "1900-02-29"
	case n < 60:
		day = time.Date(1899, 12, 31, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n).Format(time.DateOnly)
	default:
		day = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n).Format(time.DateOnly)
	}
	if clock == "" {
		return day
	}
	return day + "T" + clock
}

// pad writes a count with at least two digits.
func pad(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

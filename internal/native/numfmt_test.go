// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import "testing"

func TestFormatOf(t *testing.T) {
	date, clock := format{date: true}, format{clock: true}
	timed := format{clock: true, seconds: true}
	for code, want := range map[string]format{
		"General":                          {},
		"0.00":                             {},
		"#,##0.00_);[Red]\\(#,##0.00\\)":   {},
		"0.00E+00":                         {},
		"@":                                {},
		`0.0" days"`:                       {},
		`0 \d`:                             {},
		"_-* #,##0 _d_-":                   {},
		`[$-409]#,##0;"month"`:             {},
		"0%":                               {percent: true},
		"0.00%":                            {percent: true},
		"m/d/yyyy":                         date,
		"yyyy-mm-dd":                       date,
		"YYYY\\-MM\\-DD":                   date,
		"d-mmm-yy":                         date,
		"mmmm":                             date,
		`[$-F800]dddd\,\ mmmm\ dd\,\ yyyy`: date,
		`yyyy"年"m"月"d"日"`:                  date,
		"h:mm":                             clock,
		"hh:mm AM/PM":                      clock,
		"h:mm a/p":                         clock,
		"h:mm:ss":                          timed,
		"mm:ss":                            timed,
		"mm:ss.0":                          timed,
		"[h]:mm:ss":                        {clock: true, seconds: true, elapsed: true},
		"[mm]:ss":                          {clock: true, seconds: true, elapsed: true},
		"[ss]":                             {clock: true, seconds: true, elapsed: true},
		"yyyy-mm-dd hh:mm":                 {date: true, clock: true},
		"m/d/yy h:mm:ss":                   {date: true, clock: true, seconds: true},
		// A code that does not close what it opens is read as far as it goes.
		`yyyy "unclosed`: date,
		"0.0[Red":        {},
		"about":          {},
	} {
		if got := formatOf(code); got != want {
			t.Errorf("formatOf(%q) = %+v, want %+v", code, got, want)
		}
	}
}

func TestRender(t *testing.T) {
	for _, tc := range []struct {
		f        format
		raw      string
		date1904 bool
		want     string
	}{
		// A stored number is the shortest decimal that is the same number.
		{format{}, "42", false, "42"},
		{format{}, "2.2999999999999998", false, "2.3"},
		{format{}, "1234.5599999999999", false, "1234.56"},
		{format{}, "-0.5", false, "-0.5"},
		{format{}, "0", false, "0"},
		{format{}, "1E+3", false, "1000"},
		{format{}, " 7 ", false, "7"},
		{format{}, "123456789012345680000", false, "123456789012345680000"},
		{format{}, "1.5E+300", false, "1.5e+300"},
		{format{}, "2.5E-9", false, "2.5e-09"},
		{format{}, "not a number", false, "not a number"},
		{format{}, "NaN", false, "NaN"},
		{format{}, "Inf", false, "Inf"},

		{format{percent: true}, "0.125", false, "12.5%"},
		{format{percent: true}, "0.07", false, "7%"},
		{format{percent: true}, "1", false, "100%"},

		// A date shows no time, and a time no seconds, that its format does not.
		{format{date: true}, "46027", false, "2026-01-05"},
		{format{date: true}, "46027.75", false, "2026-01-05"},
		{format{date: true, clock: true}, "46027.75", false, "2026-01-05T18:00"},
		{format{date: true, clock: true, seconds: true}, "46027.7505787037", false, "2026-01-05T18:00:50"},
		{format{clock: true}, "0.5", false, "12:00"},
		{format{clock: true}, "3.25", false, "06:00"},
		{format{clock: true, seconds: true}, "0.0034027777777778", false, "00:04:54"},
		{format{clock: true, seconds: true, elapsed: true}, "1.5", false, "36:00:00"},
		{format{clock: true, seconds: true, elapsed: true, date: true}, "1.5", false, "36:00:00"},
		// A time a rounding takes to midnight is the next day.
		{format{date: true, clock: true, seconds: true}, "46027.999999", false, "2026-01-06T00:00:00"},

		// The 1900 system counts a February 29 that did not exist.
		{format{date: true}, "1", false, "1900-01-01"},
		{format{date: true}, "59", false, "1900-02-28"},
		{format{date: true}, "60", false, "1900-02-29"},
		{format{date: true}, "61", false, "1900-03-01"},
		{format{date: true}, "2958465", false, "9999-12-31"},
		{format{date: true}, "0", true, "1904-01-01"},
		{format{date: true}, "44565", true, "2026-01-05"},

		// A number no date can show is written as a number.
		{format{date: true}, "-1", false, "-1"},
		{format{date: true}, "2958466", false, "2958466"},
	} {
		if got := tc.f.render(tc.raw, tc.date1904); got != tc.want {
			t.Errorf("%+v.render(%q, %v) = %q, want %q", tc.f, tc.raw, tc.date1904, got, tc.want)
		}
	}
}

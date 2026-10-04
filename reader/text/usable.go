// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package text

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"latere.ai/x/lectio/reader"
)

// The bounds a page's characters are judged by. A file can hold text that
// is not the text its page shows: a font with no table from its glyphs to
// Unicode, a table that is wrong, a text decoded with the wrong encoding
// before the file was made. None of it can be seen in the positions, so
// each is measured in the characters.
const (
	// minLetters is the least share of a page's characters that are
	// letters or digits. Prose in any script, and a page of figures, is
	// well above it; a font whose glyphs map to symbols and punctuation is
	// below. Raised, it declines pages of dotted leaders and of notation;
	// lowered, it reads pages whose text is not text.
	minLetters = 0.5

	// mojibakePairs is how many times a page may hold the 2 characters
	// that one character becomes when UTF-8 is decoded as a single-byte
	// encoding, before the page is declined. No language writes such a
	// pair, so 2 on one page are a text that was decoded twice. At 1, a
	// page that quotes one such pair would be declined.
	mojibakePairs = 2

	// maxVowelless is the largest share of a page's plain words, of 4
	// letters or more in the basic Latin alphabet and not all capitals,
	// that hold no vowel. Words of the languages written in that alphabet
	// nearly all hold one; a font whose glyphs map to the wrong letters
	// gives words that do not. Lowered, it declines pages of identifiers
	// and abbreviations; raised, it reads more pages of wrong letters.
	maxVowelless = 0.15

	// vowelSample is the fewest plain words a page holds for the share of
	// words without a vowel to be judged. Under it the share says little.
	vowelSample = 20
)

// usable returns the words of a page that are read: the ones the page
// draws. It declines a page that draws none, one that draws a word at an
// angle, and one whose characters are not usable as text.
func usable(all []reader.Word) ([]reader.Word, error) {
	words := make([]reader.Word, 0, len(all))
	var runes, letters, pairs, plain, vowelless int
	for _, w := range all {
		// A word the file holds and does not draw is not on the page.
		if w.Hidden || w.Text == "" {
			continue
		}
		if w.Turned {
			return nil, decline("a word of the page is drawn at an angle")
		}
		if w.Unmapped > 0 {
			return nil, decline("a character of the page has no Unicode mapping")
		}
		// One character of the private range is what a symbol font gives
		// a bullet: it is judged where the line it stands in is known.
		lone := utf8.RuneCountInString(w.Text) == 1
		var before rune
		for _, r := range w.Text {
			runes++
			switch {
			case unicode.IsLetter(r) || unicode.IsNumber(r):
				letters++
			case unmapped(r) && (!lone || !private(r)):
				return nil, decline("a character of the page has no Unicode mapping")
			}
			if lead(before) && trail(r) {
				pairs++
			}
			before = r
		}
		if bare := strings.TrimFunc(w.Text, func(r rune) bool { return !unicode.IsLetter(r) }); isPlain(bare) {
			plain++
			if !strings.ContainsAny(bare, "aeiouyAEIOUY") {
				vowelless++
			}
		}
		words = append(words, w)
	}
	switch {
	case len(words) == 0:
		return nil, decline("the file carries no text that is drawn on the page")
	case pairs >= mojibakePairs:
		return nil, decline("the page's text reads as text that was decoded twice")
	case float64(letters) < minLetters*float64(runes):
		return nil, decline("the page's text is not mostly letters and digits")
	case plain >= vowelSample && float64(vowelless) > maxVowelless*float64(plain):
		return nil, decline("the page's words do not read as words")
	}
	return words, nil
}

// private reports whether a character is one of the ranges Unicode leaves
// to private agreement, where a font with an encoding of its own puts its
// glyphs.
func private(r rune) bool { return unicode.Is(unicode.Co, r) }

// unmapped reports whether a character is one no text holds: the
// replacement character, a control character, a private one, or a code
// Unicode assigns to nothing.
func unmapped(r rune) bool {
	return r == unicode.ReplacementChar || unicode.IsControl(r) || private(r) ||
		(r >= 0xFDD0 && r <= 0xFDEF) || r&0xFFFE == 0xFFFE
}

// isPlain reports whether a word is one the vowel measure counts: 4 letters
// or more, all of the basic Latin alphabet, and not all capitals, which an
// abbreviation is.
func isPlain(word string) bool {
	if len(word) < 4 {
		return false
	}
	lower := false
	for _, r := range word {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r < 'A' || r > 'Z':
			return false
		}
	}
	return lower
}

// lead reports whether a character is what the first byte of a UTF-8
// sequence of 2 to 4 bytes becomes when it is read as one byte.
func lead(r rune) bool { return r >= 0x00C2 && r <= 0x00F4 }

// trail reports whether a character is what a later byte of a UTF-8
// sequence, 0x80 to 0xBF, becomes when it is read as one byte: the
// character of the same number, or the one Windows-1252 puts there.
func trail(r rune) bool {
	return (r >= 0x0080 && r <= 0x00BF) || strings.ContainsRune(windows1252, r)
}

// windows1252 is the 27 characters Windows-1252 puts at 0x80 to 0x9F,
// where Latin-1 has control codes: the euro sign, the curved quotation
// marks, the dashes, and the letters of a few languages.
const windows1252 = "\u20ac\u201a\u0192\u201e\u2026\u2020\u2021\u02c6\u2030\u0160\u2039\u0152\u017d\u2018\u2019\u201c\u201d\u2022\u2013\u2014\u02dc\u2122\u0161\u203a\u0153\u017e\u0178"

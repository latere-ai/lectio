// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package id

import (
	"regexp"
	"slices"
	"testing"
	"time"

	"latere.ai/x/pkg/ulid"
)

var shape = regexp.MustCompile(`^prs_[0-9A-HJKMNP-TV-Z]{26}$`)

// TestAnIDIsAPrefixAndAnUppercaseULID: stored identifiers are in uppercase,
// so none is minted in lowercase, and the pkg validator reads it back.
func TestAnIDIsAPrefixAndAnUppercaseULID(t *testing.T) {
	got := NewGenerator().New(Parse)
	if !shape.MatchString(got) {
		t.Fatalf("%q is not prs_ and 26 uppercase characters of Crockford base 32", got)
	}
	if !ulid.Valid("prs_", got) {
		t.Fatalf("%q is not valid", got)
	}
	// The time is the first ten characters: the same instant, the same ten.
	at := time.UnixMilli(1_700_000_000_000)
	a, b := NewGenerator(), NewGenerator()
	a.Now, b.Now = func() time.Time { return at }, func() time.Time { return at }
	if x, y := a.New(File), b.New(File); x[:14] != y[:14] || x[:14] != "fil_01HF7YAT00" {
		t.Fatalf("%q and %q do not start with the instant", x, y)
	}
}

// TestEveryKindHasItsPrefix keeps each prefix one word and an underscore, as
// the stored identifiers have them.
func TestEveryKindHasItsPrefix(t *testing.T) {
	g := NewGenerator()
	for prefix, want := range map[string]string{File: "fil_", Parse: "prs_", Set: "set_", Worker: "wrk_"} {
		if prefix != want || !ulid.Valid(want, g.New(prefix)) {
			t.Errorf("prefix %q, want %q", prefix, want)
		}
	}
}

// TestIDsSortInTheOrderTheyWereMade: within one millisecond, and across a
// clock that steps back.
func TestIDsSortInTheOrderTheyWereMade(t *testing.T) {
	times := []time.Time{time.UnixMilli(5000), time.UnixMilli(5000), time.UnixMilli(4000), time.UnixMilli(6000)}
	i := 0
	g := NewGenerator()
	g.Now = func() time.Time { i++; return times[i-1] }
	var made []string
	for range times {
		made = append(made, g.New(Parse))
	}
	if !slices.IsSorted(made) || len(slices.Compact(slices.Clone(made))) != len(made) {
		t.Fatalf("not in order, or not distinct: %v", made)
	}
}

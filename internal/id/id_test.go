// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package id

import (
	"regexp"
	"slices"
	"sync"
	"testing"
	"time"
)

var shape = regexp.MustCompile(`^prs_[0-9A-HJKMNP-TV-Z]{26}$`)

func TestAnIDIsAPrefixAndAULID(t *testing.T) {
	var g Generator
	got := g.New(Parse)
	if !shape.MatchString(got) {
		t.Fatalf("%q is not prs_ and 26 characters of Crockford base 32", got)
	}
	// The time is the first ten characters: the same instant, the same ten.
	at := time.UnixMilli(1_700_000_000_000)
	a, b := Generator{Now: func() time.Time { return at }}, Generator{Now: func() time.Time { return at }}
	if x, y := a.New(File), b.New(File); x[:14] != y[:14] || x[:14] != "fil_01HF7YAT00" {
		t.Fatalf("%q and %q do not start with the instant", x, y)
	}
}

func TestIDsSortInTheOrderTheyWereMade(t *testing.T) {
	// One instant, a clock that then steps back, and a later instant.
	times := []time.Time{time.UnixMilli(5000), time.UnixMilli(5000), time.UnixMilli(4000), time.UnixMilli(6000)}
	i := 0
	g := Generator{Now: func() time.Time { i++; return times[i-1] }}
	var made []string
	for range times {
		made = append(made, g.New(Parse))
	}
	if !slices.IsSorted(made) || len(slices.Compact(slices.Clone(made))) != len(made) {
		t.Fatalf("not in order, or not distinct: %v", made)
	}

	// A random part that is all ones carries into the next byte.
	g = Generator{Now: func() time.Time { return time.UnixMilli(7000) }}
	first := g.New(Parse)
	g.tail = [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff}
	if a, b := g.New(Parse), g.New(Parse); a <= first[:14]+"0" || a >= b {
		t.Fatalf("a carry keeps the order: %q then %q", a, b)
	}
}

func TestIDsMadeAtOnceAreDistinct(t *testing.T) {
	var g Generator
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 500 {
				v := g.New(Parse)
				mu.Lock()
				seen[v] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(seen) != 4000 {
		t.Fatalf("4000 ids, %d distinct", len(seen))
	}
}

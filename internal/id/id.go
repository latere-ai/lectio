// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package id makes the identifiers of files and parses: a prefix that says
// what the thing is, and a ULID, which is 48 bits of time followed by 80
// random bits in 26 characters of Crockford's base 32. An identifier sorts
// by when it was made, so a list ordered by id is a list ordered by time,
// and a page of that list is addressed by the last id on it.
package id

import (
	"crypto/rand"
	"sync"
	"time"
)

// The prefixes of the identifiers the API returns.
const (
	File  = "fil"
	Parse = "prs"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Generator makes identifiers. Two made in the same millisecond still sort
// in the order they were made: the second takes the first's random part
// plus one. The zero value is ready to use.
type Generator struct {
	// Now is the clock. Nil takes time.Now.
	Now func() time.Time

	mu   sync.Mutex
	ms   uint64
	tail [10]byte
}

// New returns a new identifier with the prefix: "<prefix>_<ulid>".
func (g *Generator) New(prefix string) string {
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	ms := uint64(now().UnixMilli())

	g.mu.Lock()
	if ms > g.ms {
		g.ms = ms
		// crypto/rand.Read does not fail: it aborts the process when the
		// system's source is unavailable.
		_, _ = rand.Read(g.tail[:])
	} else {
		// The same millisecond, or a clock that stepped back: stay on the
		// last time and count up, so order is kept.
		for i := len(g.tail) - 1; i >= 0; i-- {
			if g.tail[i]++; g.tail[i] != 0 {
				break
			}
		}
	}
	var raw [16]byte
	for i := range 6 {
		raw[i] = byte(g.ms >> (40 - 8*i))
	}
	copy(raw[6:], g.tail[:])
	g.mu.Unlock()

	// 128 bits in 26 characters of 5 bits: the first character holds the
	// top 3 bits, and every one after it the next 5.
	out := make([]byte, 0, len(prefix)+27)
	out = append(out, prefix...)
	out = append(out, '_', alphabet[raw[0]>>5])
	acc, bits := uint(raw[0]&0x1f), 5
	for _, b := range raw[1:] {
		acc, bits = acc<<8|uint(b), bits+8
		for bits >= 5 {
			bits -= 5
			out = append(out, alphabet[acc>>bits&0x1f])
		}
	}
	return string(out)
}

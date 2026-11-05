// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package id makes the identifiers of files, parses, sets and workers: a
// prefix that says what the thing is, an underscore, and a ULID, which is 48
// bits of time followed by 80 random bits in 26 uppercase characters of
// Crockford's base 32. An identifier sorts by when it was made, so a list
// ordered by id is a list ordered by time, and a page of that list is
// addressed by the last id on it.
package id

import "latere.ai/x/pkg/ulid"

// The prefixes of the identifiers, each with the underscore that separates
// it from the ULID.
const (
	File   = "fil_"
	Parse  = "prs_"
	Set    = "set_"
	Worker = "wrk_"
)

// NewGenerator returns a generator of identifiers in uppercase, the case
// every stored identifier is in. Two made in the same millisecond still
// sort in the order they were made.
func NewGenerator() *ulid.Generator { return &ulid.Generator{Upper: true} }

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package migrations embeds the schema of the control plane as golang-migrate
// files: the tables of specs 004, 006 and 007 and the functions that write
// them, the exchange among them. They are numbered from 000001 and applied
// in order on the direct connection; a released migration is never edited,
// because the databases that ran it already have.
package migrations

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// FS holds the migration files.
//
//go:embed *.sql
var FS embed.FS

// Highest is the newest migration this binary carries. The store refuses to
// open against a schema at any other version: below it a function this code
// calls may be missing, and above it a newer binary changed what the
// functions mean.
func Highest() (uint, error) { return highestOf(FS) }

// highestOf reads the number every migration file starts with.
func highestOf(files fs.FS) (uint, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return 0, fmt.Errorf("reading the embedded migrations: %w", err)
	}
	var highest uint
	for _, e := range entries {
		number, _, found := strings.Cut(e.Name(), "_")
		if !found {
			continue
		}
		value, err := strconv.ParseUint(number, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("the migration %q does not start with a number: %w", e.Name(), err)
		}
		highest = max(highest, uint(value))
	}
	if highest == 0 {
		return 0, errors.New("no migration is embedded")
	}
	return highest, nil
}

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

	"latere.ai/x/pkg/pgxmigrate"
)

// FS holds the migration files.
//
//go:embed *.sql
var FS embed.FS

// Highest is the newest migration this binary carries. The store refuses to
// open against a schema at any other version: below it a function this code
// calls may be missing, and above it a newer binary changed what the
// functions mean.
func Highest() (uint, error) { return pgxmigrate.HighestOf(FS, ".") }

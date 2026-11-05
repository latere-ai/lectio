// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"io/fs"
	"strings"
	"testing"
)

// TestHighestIsWhatIsEmbedded: the version the store checks a database
// against is read from the files this binary carries, not written down
// beside them.
func TestHighestIsWhatIsEmbedded(t *testing.T) {
	highest, err := Highest()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	ups := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			ups++
		}
	}
	if ups == 0 || highest != uint(ups) {
		t.Fatalf("the highest migration is %d over %d up file(s)", highest, ups)
	}
}

// TestEveryMigrationIsReversible: golang-migrate reads a pair, and a release
// that carried only one half would fail on the first down.
func TestEveryMigrationIsReversible(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		up, found := strings.CutSuffix(e.Name(), ".up.sql")
		if !found {
			continue
		}
		if _, err := fs.Stat(FS, up+".down.sql"); err != nil {
			t.Errorf("%s has no down migration: %v", e.Name(), err)
		}
	}
}

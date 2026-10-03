// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package version

import "testing"

func TestStringNamesTheBinaryAndItsBuild(t *testing.T) {
	Version, Commit, Date = "v1.2.3", "abc1234", "2026-10-03T00:00:00Z"
	t.Cleanup(func() { Version, Commit, Date = "dev", "none", "unknown" })
	if got, want := String(), "lectiod v1.2.3 (abc1234, 2026-10-03T00:00:00Z)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

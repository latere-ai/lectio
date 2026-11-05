// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package smoke holds the test for release.sh, the live check the release
// pipeline runs against lectiod after a rollout and an operator runs by hand
// after an upgrade.
//
// The script is shell so that it can be read and run without a Go toolchain;
// this package is what keeps it honest, because a smoke that passes against
// a version other than the one just deployed is worse than none.
package smoke

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package blob

// locked runs do with no lock where the system has no advisory lock on a
// directory. A Put that loses its directory to a prune there writes again, a
// bounded number of times.
func (d *Dir) locked(_ bool, do func() error) error {
	return do()
}

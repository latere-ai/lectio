// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package blob

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// locked runs do while it holds the store's lock, shared or exclusive. The
// lock is an advisory lock on the root directory, taken through a descriptor
// of this call's own, so it holds between the goroutines of one process as
// it does between processes. A Put holds it shared: any number of them write
// at once. A Delete holds it exclusive, because the directory it prunes is
// one a Put is about to write into, and a prune that lands between a Put's
// making of the directory and its write takes the directory away again.
func (d *Dir) locked(exclusive bool, do func() error) (err error) {
	root, err := os.Open(d.root)
	if err != nil {
		return fmt.Errorf("blob: opening the store's directory: %w", err)
	}
	defer func() {
		// Closing the descriptor gives the lock back.
		if cerr := root.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("blob: closing the store's directory: %w", cerr)
		}
	}()
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	for {
		err := syscall.Flock(int(root.Fd()), how)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EINTR) {
			return fmt.Errorf("blob: locking the store's directory: %w", err)
		}
	}
	return do()
}

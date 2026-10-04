// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package convert

import (
	"errors"
	"os/exec"
)

// ownGroup does nothing on a system without process groups: the end of the
// suite's context kills the program that was started, and what it started
// is left to the system.
func ownGroup(*exec.Cmd) {}

func killGroup(*exec.Cmd) error { return nil }

// Limit fails on a system that has no limit to set: a sidecar there runs
// with no memory limit or does not run.
func Limit([]string) error {
	return errors.New("a memory limit is not supported on this system")
}

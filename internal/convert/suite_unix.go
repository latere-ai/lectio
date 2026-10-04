// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package convert

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// ownGroup starts the suite in a process group of its own, and has the end
// of its context kill that whole group. The suite starts other programs,
// and killing only the one that was started would leave them running.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
}

// killGroup kills every process of the group the suite was started in. A
// group that has no process left is not an error.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
		return err
	}
	return nil
}

// Limit is what the sidecar's program does under LimitArg: it bounds its
// own address space and then becomes the program named after the bound,
// with the arguments that follow it. A limit set this way holds for the
// program and for every program it starts, and is enforced by the system:
// past it an allocation fails, and the suite ends.
//
// args are the bound in bytes, the program, and the program's arguments.
// Limit returns only when it failed.
func Limit(args []string) error {
	if len(args) < 2 {
		return errors.New("limit takes a number of bytes and a program")
	}
	bytes, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil {
		return errors.New("limit takes a number of bytes and a program")
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: bytes, Max: bytes}); err != nil {
		return fmt.Errorf("the memory limit was not set: %w", err)
	}
	return fmt.Errorf("the suite was not started: %w", syscall.Exec(args[1], args[1:], os.Environ()))
}

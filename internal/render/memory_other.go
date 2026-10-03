// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package render

import "github.com/tetratelabs/wazero/experimental"

// arena is empty on a system without mapped memory: the runtime keeps an
// instance's memory on the Go heap, still under the instance's bound, and
// the collector returns it.
type arena struct{}

func newArena() *arena { return &arena{} }

func (a *arena) allocator() experimental.MemoryAllocator { return nil }

func (a *arena) release() {}

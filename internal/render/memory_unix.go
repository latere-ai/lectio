// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package render

import (
	"sync"
	"syscall"

	"github.com/tetratelabs/wazero/experimental"
)

// arena is the memory of one engine's instances, taken from the operating
// system and not from the Go heap. An instance's whole bound is reserved
// as address space when it starts and made usable as the engine grows into
// it, so growing copies nothing and an instance can never hold more than
// its bound. On the Go heap a page written to exhaust the engine left
// several times the bound behind as garbage, for as long as the collector
// took to get to it.
//
// The runtime frees an instance's memory when the instance closes, which
// can be while a call on it is still returning. Unmapping then would take
// the memory from under running code, so freeing only marks: the arena's
// owner calls release once no call on the engine can be running, and that
// is when the memory goes back to the operating system.
type arena struct {
	mu       sync.Mutex
	mappings [][]byte
}

// mapped is the memory of one instance.
type mapped struct{ reserved []byte }

func newArena() *arena { return &arena{} }

// Allocate reserves limit bytes of address space. It returns nil when the
// reservation fails, which the runtime reports as a failure to start the
// instance.
func (a *arena) Allocate(_, limit uint64) experimental.LinearMemory {
	reserved, err := syscall.Mmap(-1, 0, int(limit), syscall.PROT_NONE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil
	}
	a.mu.Lock()
	a.mappings = append(a.mappings, reserved)
	a.mu.Unlock()
	return &mapped{reserved: reserved}
}

// allocator is the arena as the runtime takes it.
func (a *arena) allocator() experimental.MemoryAllocator { return a }

// release returns the memory of every instance the arena allocated to the
// operating system. The caller knows no call on the engine is running.
func (a *arena) release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, reserved := range a.mappings {
		_ = syscall.Munmap(reserved)
	}
	a.mappings = nil
}

// Reallocate makes the first size bytes usable and returns them. The
// address never changes. Past the reservation it returns nil, which the
// engine sees as a failed allocation.
func (m *mapped) Reallocate(size uint64) []byte {
	if size > uint64(len(m.reserved)) {
		return nil
	}
	if size > 0 {
		if err := syscall.Mprotect(m.reserved[:size], syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
			return nil
		}
	}
	return m.reserved[:size]
}

// Free does nothing: see arena.
func (m *mapped) Free() {}

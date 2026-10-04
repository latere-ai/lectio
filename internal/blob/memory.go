// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package blob

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
)

// Memory is a Store that keeps every object in the process, so nothing
// survives a restart: it is for a development server and for tests.
type Memory struct {
	mu      sync.RWMutex
	objects map[string]object
}

// object is one stored object.
type object struct {
	data        []byte
	contentType string
}

// NewMemory returns an empty store.
func NewMemory() *Memory {
	return &Memory{objects: map[string]object{}}
}

// Put writes a copy of data under key.
func (m *Memory) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = object{data: bytes.Clone(data), contentType: cmp.Or(contentType, DefaultContentType)}
	return nil
}

// Get returns a copy of the object under key.
func (m *Memory) Get(ctx context.Context, key string) ([]byte, string, error) {
	if err := ValidKey(key); err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.objects[key]
	if !ok {
		return nil, "", ErrNotFound
	}
	return bytes.Clone(o.data), o.contentType, nil
}

// Delete removes the object under key.
func (m *Memory) Delete(ctx context.Context, key string) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// List returns the keys that start with prefix, in byte order.
func (m *Memory) List(ctx context.Context, prefix string) ([]string, error) {
	if err := validPrefix(prefix); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	var keys []string
	for key := range m.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	m.mu.RUnlock()
	slices.Sort(keys)
	return keys, nil
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"sync"

	"latere.ai/x/lectio/internal/blob"
)

// cache holds the working copies the pages of running parses are read
// from, so the pages of one file fetch it once and not once each. It is
// bounded in bytes and gives up the copy used longest ago first. A copy in
// use by a page stays whole in that page's hands after the cache let it go.
//
// The design keeps this cache on the worker's disk
// (specs/005-parse-graph.md); here it is in memory.
type cache struct {
	max int64

	mu      sync.Mutex
	size    int64
	tick    int64
	entries map[string]*entry
}

// entry is one working copy, or the fetch of one that is under way.
type entry struct {
	ready chan struct{}
	data  []byte
	err   error
	used  int64
}

func newCache(maxBytes int64) *cache {
	return &cache{max: maxBytes, entries: map[string]*entry{}}
}

// get returns the object under key, from the cache or from the store. Two
// pages that ask for one key at once make one fetch.
func (c *cache) get(ctx context.Context, store blob.Store, key string) ([]byte, error) {
	c.mu.Lock()
	c.tick++
	e, ok := c.entries[key]
	if ok {
		e.used = c.tick
		c.mu.Unlock()
		select {
		case <-e.ready:
			return e.data, e.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	e = &entry{ready: make(chan struct{}), used: c.tick}
	c.entries[key] = e
	c.mu.Unlock()

	e.data, _, e.err = store.Get(ctx, key)
	c.mu.Lock()
	if e.err != nil {
		// A fetch that failed is not kept: the next page asks again.
		delete(c.entries, key)
	} else {
		c.size += int64(len(e.data))
		c.evict(key)
	}
	c.mu.Unlock()
	close(e.ready)
	return e.data, e.err
}

// evict lets copies go, the one used longest ago first, until the cache is
// within its bound or holds only the copy that was just fetched.
func (c *cache) evict(keep string) {
	for c.size > c.max {
		oldest := ""
		for key, e := range c.entries {
			select {
			case <-e.ready:
			default:
				if key != keep {
					continue // still being fetched: it counts for nothing yet
				}
			}
			if key != keep && (oldest == "" || e.used < c.entries[oldest].used) {
				oldest = key
			}
		}
		if oldest == "" {
			return
		}
		c.size -= int64(len(c.entries[oldest].data))
		delete(c.entries, oldest)
	}
}

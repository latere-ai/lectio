// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package blob_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/s3/s3test"

	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/testservers"
)

// One suite holds every implementation to the same behavior: the store in
// memory, the store over a directory, and the store over S3, which runs
// against an S3 endpoint in this process and against a real server in a
// container. Where no container runtime answers, the run against the real
// server skips and says so; the endpoint in this process carries the suite
// on every machine.

// TestMain removes the container this binary started, whatever the suite
// did.
func TestMain(m *testing.M) { testservers.Main(m) }

// TestEveryStoreBehavesTheSame runs the conformance suite against each
// implementation. The S3 store runs twice against the endpoint in this
// process, at the root of a bucket and under a prefix, because the prefix is
// added to every key that is sent and taken off every key that is listed.
func TestEveryStoreBehavesTheSame(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		conform(t, func(*testing.T) blob.Store { return blob.NewMemory() })
	})
	t.Run("dir", func(t *testing.T) {
		conform(t, func(t *testing.T) blob.Store { return openDir(t, t.TempDir()) })
	})
	t.Run("s3", func(t *testing.T) {
		conform(t, func(t *testing.T) blob.Store {
			store, _ := openFake(t, "")
			return store
		})
	})
	t.Run("s3 under a prefix", func(t *testing.T) {
		conform(t, func(t *testing.T) blob.Store {
			store, _ := openFake(t, "installations/one")
			return store
		})
	})
	t.Run("minio", func(t *testing.T) {
		srv, err := testservers.StartObjects()
		if err != nil {
			t.Skipf("no container runtime answered, so the suite did not run against a real S3 server: %v", err)
		}
		conform(t, func(t *testing.T) blob.Store {
			// The bucket is shared by every case, so each takes a prefix of
			// its own and lists nothing another wrote.
			store, err := blob.NewS3(blob.S3Config{
				Endpoint: srv.Endpoint, Region: srv.Region, Bucket: srv.Bucket,
				Prefix:    "conformance/" + strings.ToLower(rand.Text()[:8]),
				AccessKey: srv.AccessKey, SecretKey: srv.SecretKey, PathStyle: true,
			})
			if err != nil {
				t.Fatalf("opening the store on the real server: %v", err)
			}
			return store
		})
	})
}

// openDir opens the store over a directory.
func openDir(t *testing.T, root string) *blob.Dir {
	t.Helper()
	store, err := blob.NewDir(root)
	if err != nil {
		t.Fatalf("opening the store over %s: %v", root, err)
	}
	return store
}

// openFake starts an S3 endpoint in this process and opens the store on it.
// The endpoint verifies every signature against its own credential and
// region, so the store is given those.
func openFake(t *testing.T, prefix string) (*blob.S3, *s3test.Server) {
	t.Helper()
	fake := s3test.New(t, "bucket")
	store, err := blob.NewS3(blob.S3Config{
		Endpoint: fake.URL(), Region: s3test.Region, Bucket: "bucket", Prefix: prefix,
		AccessKey: s3test.Key, SecretKey: s3test.Secret, PathStyle: true, HTTPClient: fake.HTTPClient(),
	})
	if err != nil {
		t.Fatalf("opening the store on the endpoint: %v", err)
	}
	return store, fake
}

// many is how many keys the pagination case writes: more than the 1000 one
// page of an S3 listing holds, and few enough to keep the run against a real
// server short.
const many = 1100

// conform is the conformance suite. open returns an empty store, and every
// case opens its own.
func conform(t *testing.T, open func(t *testing.T) blob.Store) {
	t.Run("an object reads back as it was written", func(t *testing.T) {
		s := open(t)
		put(t, s, "parses/prs_a/pages/1.1.json", []byte(`{"number":1}`), "application/json")
		data, contentType := get(t, s, "parses/prs_a/pages/1.1.json")
		if string(data) != `{"number":1}` || contentType != "application/json" {
			t.Fatalf("the object read back as %q, %q", data, contentType)
		}
	})

	t.Run("an object put with no content type is an octet stream", func(t *testing.T) {
		s := open(t)
		put(t, s, "parses/prs_a/work/source.1.bin", []byte{0, 1, 2}, "")
		if _, contentType := get(t, s, "parses/prs_a/work/source.1.bin"); contentType != "application/octet-stream" {
			t.Fatalf("the content type of an object put with none is %q", contentType)
		}
	})

	t.Run("a second put replaces the bytes and the content type", func(t *testing.T) {
		s := open(t)
		put(t, s, "sources/o/sha/fil_1", []byte("a first and longer body"), "text/plain")
		put(t, s, "sources/o/sha/fil_1", []byte("second"), "text/markdown")
		data, contentType := get(t, s, "sources/o/sha/fil_1")
		if string(data) != "second" || contentType != "text/markdown" {
			t.Fatalf("after a second put the object is %q, %q", data, contentType)
		}
		if keys := list(t, s, ""); !slices.Equal(keys, []string{"sources/o/sha/fil_1"}) {
			t.Fatalf("two puts of one key left %q", keys)
		}
	})

	t.Run("a key that holds no object is not found", func(t *testing.T) {
		s := open(t)
		put(t, s, "parses/prs_a/pages/1.1.json", []byte("{}"), "application/json")
		for _, key := range []string{"parses/prs_a/pages/1.2.json", "parses/prs_b/pages/1.1.json", "never"} {
			data, contentType, err := s.Get(t.Context(), key)
			if !errors.Is(err, blob.ErrNotFound) || data != nil || contentType != "" {
				t.Errorf("Get(%q) = %q, %q, %v", key, data, contentType, err)
			}
		}
	})

	t.Run("a deleted object is not found, and deleting what is not there is not an error", func(t *testing.T) {
		s := open(t)
		put(t, s, "parses/prs_a/pages/1.1.json", []byte("{}"), "application/json")
		put(t, s, "parses/prs_a/pages/2.1.json", []byte("{}"), "application/json")
		for range 2 {
			if err := s.Delete(t.Context(), "parses/prs_a/pages/1.1.json"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, _, err := s.Get(t.Context(), "parses/prs_a/pages/1.1.json"); !errors.Is(err, blob.ErrNotFound) {
				t.Fatalf("a deleted object read as %v", err)
			}
		}
		if err := s.Delete(t.Context(), "parses/prs_never/pages/1.1.json"); err != nil {
			t.Fatalf("deleting a key that never held an object: %v", err)
		}
		if keys := list(t, s, ""); !slices.Equal(keys, []string{"parses/prs_a/pages/2.1.json"}) {
			t.Fatalf("after the delete the store lists %q", keys)
		}
	})

	t.Run("an empty object is an object", func(t *testing.T) {
		s := open(t)
		put(t, s, "parses/prs_a/pages/1.1.json", nil, "application/json")
		put(t, s, "parses/prs_a/pages/2.1.json", []byte{}, "application/json")
		for _, key := range []string{"parses/prs_a/pages/1.1.json", "parses/prs_a/pages/2.1.json"} {
			if data, contentType := get(t, s, key); len(data) != 0 || contentType != "application/json" {
				t.Errorf("the empty object %s read back as %q, %q", key, data, contentType)
			}
		}
		if keys := list(t, s, "parses/prs_a/"); len(keys) != 2 {
			t.Fatalf("two empty objects are listed as %q", keys)
		}
	})

	t.Run("an object of 1 MiB reads back whole", func(t *testing.T) {
		s := open(t)
		want := make([]byte, 1<<20)
		if _, err := rand.Read(want); err != nil {
			t.Fatal(err)
		}
		put(t, s, "parses/prs_a/pages/1.1.png", want, "image/png")
		data, contentType := get(t, s, "parses/prs_a/pages/1.1.png")
		if !bytes.Equal(data, want) || contentType != "image/png" {
			t.Fatalf("1 MiB read back as %d bytes of %q, equal: %v", len(data), contentType, bytes.Equal(data, want))
		}
	})

	t.Run("a key may hold any byte the rules allow", func(t *testing.T) {
		s := open(t)
		key := "sources/o/a b+c%d&e=f?g#h/übersicht 1.pdf"
		put(t, s, key, []byte("pdf"), "application/pdf")
		if data, contentType := get(t, s, key); string(data) != "pdf" || contentType != "application/pdf" {
			t.Fatalf("the object read back as %q, %q", data, contentType)
		}
		if keys := list(t, s, "sources/o/a b+"); !slices.Equal(keys, []string{key}) {
			t.Fatalf("the key is listed as %q", keys)
		}
		if err := s.Delete(t.Context(), key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if keys := list(t, s, ""); len(keys) != 0 {
			t.Fatalf("after the delete the store lists %q", keys)
		}
	})

	t.Run("a listing holds exactly the keys that start with its prefix, in byte order", func(t *testing.T) {
		s := open(t)
		// The keys are written out of order. parses/a.x sorts before every
		// key under parses/a/, because a dot is the smaller byte, while a
		// walk of directories meets it after them. parses/ab/x shares a
		// string prefix with parses/a and no path segment.
		for _, key := range []string{
			"sources/o/sha/fil_1", "parses/ab/x", "parses/a/pages/2.1.json", "parses/a/document.1.json",
			"parses/a.x", "parses/a/pages/10.1.json", "parses/a/pages/1.1.json",
		} {
			put(t, s, key, []byte(key), "text/plain")
		}
		under := []string{"parses/a/document.1.json", "parses/a/pages/1.1.json", "parses/a/pages/10.1.json", "parses/a/pages/2.1.json"}
		for _, c := range []struct {
			prefix string
			want   []string
		}{
			{"parses/a/", under},
			{"parses/a", slices.Concat([]string{"parses/a.x"}, under, []string{"parses/ab/x"})},
			{"parses/a/pages/1", []string{"parses/a/pages/1.1.json", "parses/a/pages/10.1.json"}},
			{"parses/a/pages/1.1.json", []string{"parses/a/pages/1.1.json"}},
			{"parses/ab", []string{"parses/ab/x"}},
			{"", slices.Concat([]string{"parses/a.x"}, under, []string{"parses/ab/x", "sources/o/sha/fil_1"})},
		} {
			if got := list(t, s, c.prefix); !slices.Equal(got, c.want) {
				t.Errorf("List(%q) = %q, want %q", c.prefix, got, c.want)
			}
		}
	})

	t.Run("a listing that matches nothing is empty and not an error", func(t *testing.T) {
		s := open(t)
		if keys := list(t, s, ""); len(keys) != 0 {
			t.Fatalf("an empty store lists %q", keys)
		}
		put(t, s, "parses/a/pages/1.1.json", []byte("{}"), "application/json")
		for _, prefix := range []string{"parses/b/", "parses/a/pages/1.1.json/", "parses/a/pages/2", "sources", "z/y/x/"} {
			if keys := list(t, s, prefix); len(keys) != 0 {
				t.Errorf("List(%q) = %q, want nothing", prefix, keys)
			}
		}
	})

	t.Run("a listing longer than one page holds every key", func(t *testing.T) {
		s := open(t)
		want := make([]string, many)
		for i := range want {
			want[i] = fmt.Sprintf("parses/long/pages/%04d.1.json", i)
		}
		each(t, many, func(i int) error { return s.Put(t.Context(), want[i], []byte("{}"), "application/json") })
		put(t, s, "parses/longer/pages/1.1.json", []byte("{}"), "application/json")
		if got := list(t, s, "parses/long/"); !slices.Equal(got, want) {
			t.Fatalf("a listing of %d keys returned %d, in order: %v", many, len(got), slices.IsSorted(got))
		}
		if got := list(t, s, ""); len(got) != many+1 {
			t.Fatalf("a listing of everything returned %d keys, want %d", len(got), many+1)
		}
	})

	t.Run("what a caller holds is never the store's copy", func(t *testing.T) {
		s := open(t)
		data := []byte("the bytes that were put")
		put(t, s, "parses/prs_a/pages/1.1.json", data, "application/json")
		clear(data)
		first, _ := get(t, s, "parses/prs_a/pages/1.1.json")
		if string(first) != "the bytes that were put" {
			t.Fatalf("changing the slice that was put changed the object to %q", first)
		}
		clear(first)
		if second, _ := get(t, s, "parses/prs_a/pages/1.1.json"); string(second) != "the bytes that were put" {
			t.Fatalf("changing what Get returned changed the object to %q", second)
		}
	})

	t.Run("a key that breaks the rules is refused by every method", func(t *testing.T) {
		s := open(t)
		put(t, s, "parses/a/pages/1.1.json", []byte("{}"), "application/json")
		for _, key := range []string{
			"", "/", "/parses/a", "parses/a/", "parses//a", "parses/./a", "parses/../a", ".", "..", "../parses/a/pages/1.1.json",
			`parses\a`, "parses/a\x00", strings.Repeat("k", blob.MaxKeyBytes+1),
		} {
			if err := s.Put(t.Context(), key, []byte("x"), ""); err == nil {
				t.Errorf("Put(%q) was accepted", key)
			}
			if _, _, err := s.Get(t.Context(), key); err == nil || errors.Is(err, blob.ErrNotFound) {
				t.Errorf("Get(%q) = %v, want a refusal", key, err)
			}
			if err := s.Delete(t.Context(), key); err == nil {
				t.Errorf("Delete(%q) was accepted", key)
			}
		}
		// A prefix is the start of a key: it may be empty and may end in one
		// slash, and is held to the rules of a key otherwise.
		for _, prefix := range []string{
			"/", "/parses", "parses//", "parses//a", "parses/./", "parses/..", "../", `parses\`, "parses/\x00",
			strings.Repeat("k", blob.MaxKeyBytes+1),
		} {
			if keys, err := s.List(t.Context(), prefix); err == nil {
				t.Errorf("List(%q) = %q, want a refusal", prefix, keys)
			}
		}
		// Nothing a refused call asked for was done.
		if keys := list(t, s, ""); !slices.Equal(keys, []string{"parses/a/pages/1.1.json"}) {
			t.Fatalf("after the refusals the store lists %q", keys)
		}
	})

	t.Run("a call under a canceled context does nothing", func(t *testing.T) {
		s := open(t)
		put(t, s, "parses/a/pages/1.1.json", []byte("{}"), "application/json")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := s.Put(ctx, "parses/a/pages/2.1.json", []byte("{}"), ""); !errors.Is(err, context.Canceled) {
			t.Errorf("Put under a canceled context = %v", err)
		}
		if _, _, err := s.Get(ctx, "parses/a/pages/1.1.json"); !errors.Is(err, context.Canceled) {
			t.Errorf("Get under a canceled context = %v", err)
		}
		if err := s.Delete(ctx, "parses/a/pages/1.1.json"); !errors.Is(err, context.Canceled) {
			t.Errorf("Delete under a canceled context = %v", err)
		}
		if _, err := s.List(ctx, ""); !errors.Is(err, context.Canceled) {
			t.Errorf("List under a canceled context = %v", err)
		}
		if keys := list(t, s, ""); !slices.Equal(keys, []string{"parses/a/pages/1.1.json"}) {
			t.Fatalf("after the canceled calls the store lists %q", keys)
		}
	})

	// The criterion of specs/002-object-model.md: two workers that wrote one
	// page leave two objects. Each wrote under the token of its own lease,
	// so neither wrote over the other, and the page a caller reads is the
	// one whose key the accepted settle recorded.
	t.Run("two workers that wrote one page leave two objects", func(t *testing.T) {
		s := open(t)
		stale, current := blob.PageKey("prs_a", 3, 1), blob.PageKey("prs_a", 3, 2)
		put(t, s, stale, []byte(`{"reply":"of the worker that lost its lease"}`), "application/json")
		put(t, s, current, []byte(`{"reply":"of the worker that holds it"}`), "application/json")
		if keys := list(t, s, blob.ParsePrefix("prs_a")); !slices.Equal(keys, []string{stale, current}) {
			t.Fatalf("the parse lists %q, want both pages", keys)
		}
		if data, _ := get(t, s, stale); string(data) != `{"reply":"of the worker that lost its lease"}` {
			t.Errorf("the first worker's page reads %q", data)
		}
		if data, _ := get(t, s, current); string(data) != `{"reply":"of the worker that holds it"}` {
			t.Errorf("the second worker's page reads %q", data)
		}
	})
}

// put writes an object and fails the case when the store refuses it.
func put(t *testing.T, s blob.Store, key string, data []byte, contentType string) {
	t.Helper()
	if err := s.Put(t.Context(), key, data, contentType); err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
}

// get reads an object that must be there.
func get(t *testing.T, s blob.Store, key string) ([]byte, string) {
	t.Helper()
	data, contentType, err := s.Get(t.Context(), key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	return data, contentType
}

// list returns the keys under a prefix the store must accept.
func list(t *testing.T, s blob.Store, prefix string) []string {
	t.Helper()
	keys, err := s.List(t.Context(), prefix)
	if err != nil {
		t.Fatalf("List(%q): %v", prefix, err)
	}
	return keys
}

// each calls do for every number below n, at most 16 calls at a time, and
// fails the case when any of them returned an error.
func each(t *testing.T, n int, do func(i int) error) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make([]error, n)
	slots := make(chan struct{}, 16)
	for i := range n {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			errs[i] = do(i)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
}

// TestAKeyIsAPathOfSegments: the rules of a key, one case per rule, and the
// two bounds of its length.
func TestAKeyIsAPathOfSegments(t *testing.T) {
	for _, key := range []string{
		"a", "sources/0123456789abcdef/sha/fil_1", "parses/prs_a/pages/12.3.json", "a/.hidden", "a/...", "a b/c+d",
		strings.Repeat("k", blob.MaxKeyBytes),
	} {
		if err := blob.ValidKey(key); err != nil {
			t.Errorf("ValidKey(%q) = %v", key, err)
		}
	}
	for key, rule := range map[string]string{
		"":                                      "empty",
		"/a":                                    "empty segment",
		"a/":                                    "empty segment",
		"a//b":                                  "empty segment",
		"a/./b":                                 `"." or ".."`,
		"a/..":                                  `"." or ".."`,
		`a\b`:                                   "backslash",
		"a\x00b":                                "NUL",
		strings.Repeat("k", blob.MaxKeyBytes+1): "longer than 1024 bytes",
	} {
		err := blob.ValidKey(key)
		if err == nil || !strings.Contains(err.Error(), rule) {
			t.Errorf("ValidKey(%q) = %v, want an error that names the rule %q", key, err, rule)
		}
	}
	// The error is about a key that may be anything, so it does not repeat it.
	if err := blob.ValidKey("a/secret-part/../b"); err == nil || strings.Contains(err.Error(), "secret-part") {
		t.Fatalf("the error of a refused key quotes it: %v", err)
	}
}

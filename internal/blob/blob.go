// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package blob is the object store of a parse: bytes under keys, each with a
// content type. It holds what the database does not: the source snapshots,
// the working copies, the page images, the page results and the document
// indexes of specs/002-object-model.md, under the keys of keys.go.
//
// Three implementations stand behind one interface. Memory keeps the objects
// in the process, for a development server and for tests. Dir keeps them as
// files under a directory that several processes on one machine may share.
// S3 keeps them in a bucket, which is what a deployment runs on.
//
// The store knows nothing of parses or pages, and it offers no conditional
// write: two writers of one key leave the bytes of the one that wrote last.
// That is why a key carries the lease token of the task that wrote it, so two
// workers that ran one task never write the same key.
package blob

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is what Get returns for a key that holds no object.
var ErrNotFound = errors.New("blob: no object under the key")

// DefaultContentType is the content type of an object that was put with none.
const DefaultContentType = "application/octet-stream"

// MaxKeyBytes is the longest key a store takes. It is the bound S3 puts on a
// key, so a key every store took is a key a bucket takes.
const MaxKeyBytes = 1024

// Store holds objects. Every implementation behaves the same; one conformance
// suite holds them to it. A Store is safe for concurrent use.
//
// Every method refuses a key that ValidKey refuses. One shape of key is valid
// and still not portable: a key that is also the directory of another, as a
// and a/b are. Memory keeps both, and a file system and some S3 servers
// refuse the second, so the key layout never makes such a pair.
type Store interface {
	// Put writes data under key, replacing what was there. An empty
	// contentType takes DefaultContentType. The store keeps a copy: the
	// caller may change data after Put returns.
	Put(ctx context.Context, key string, data []byte, contentType string) error

	// Get returns the object under key and its content type, or ErrNotFound.
	// The data is the caller's own: changing it does not change the object.
	Get(ctx context.Context, key string) (data []byte, contentType string, err error)

	// Delete removes the object under key. A key that holds none is not an
	// error.
	Delete(ctx context.Context, key string) error

	// List returns every key that starts with prefix, in ascending byte
	// order. The prefix is matched as a string and not as a directory, so
	// parses/a matches parses/ab/x and parses/a/ does not. An empty prefix
	// lists everything, and a prefix that matches nothing lists nothing.
	List(ctx context.Context, prefix string) ([]string, error)
}

// ValidKey reports why key cannot name an object, or nil. A key is a path of
// segments separated by slashes. No segment is empty, "." or "..", so a key
// has no leading, trailing or doubled slash and cannot climb out of a
// directory. It holds no backslash and no NUL, which a file system may read
// as a separator or an end. It is at most MaxKeyBytes long. The errors do not
// quote the key.
func ValidKey(key string) error {
	if key == "" {
		return errors.New("blob: the key is empty")
	}
	return validPath("key", key)
}

// validPrefix is ValidKey for what List takes, the start of a key. The empty
// prefix is allowed and so is one trailing slash; what is left follows the
// rules of a key.
func validPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return validPath("prefix", strings.TrimSuffix(prefix, "/"))
}

// validPath checks the rules a key and a prefix share. what names the one
// that is checked, for the error.
func validPath(what, path string) error {
	if len(path) > MaxKeyBytes {
		return fmt.Errorf("blob: the %s is longer than %d bytes", what, MaxKeyBytes)
	}
	if strings.ContainsAny(path, "\\\x00") {
		return fmt.Errorf("blob: the %s holds a backslash or a NUL", what)
	}
	for segment := range strings.SplitSeq(path, "/") {
		switch segment {
		case "":
			return fmt.Errorf("blob: the %s has an empty segment", what)
		case ".", "..":
			return fmt.Errorf(`blob: the %s has a segment "." or ".."`, what)
		}
	}
	return nil
}

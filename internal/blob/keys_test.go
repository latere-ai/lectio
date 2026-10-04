// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package blob_test

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/lectio/internal/blob"
)

// TestTheKeyLayout: the key of every kind of object, as the object model
// writes it. A key is part of what is stored: changing one orphans every
// object written under the old one.
func TestTheKeyLayout(t *testing.T) {
	owner := blob.OwnerKey("org_zulu")
	const digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	for _, c := range []struct{ got, want string }{
		{blob.SourceKey("org_zulu", digest, "fil_01"), "sources/" + owner + "/" + digest + "/fil_01"},
		{blob.ParsePrefix("prs_01"), "parses/prs_01/"},
		{blob.WorkKey("prs_01", 2, "application/pdf"), "parses/prs_01/work/source.2.pdf"},
		{blob.PageKey("prs_01", 12, 3), "parses/prs_01/pages/12.3.json"},
		{blob.ImageKey("prs_01", 12, 3, "image/png"), "parses/prs_01/pages/12.3.png"},
		{blob.ImageKey("prs_01", 12, 3, "image/jpeg"), "parses/prs_01/pages/12.3.jpg"},
		{blob.ImageKey("prs_01", 12, 3, "image/tiff"), "parses/prs_01/pages/12.3.bin"},
		{blob.ImageKey("prs_01", 12, 3, ""), "parses/prs_01/pages/12.3.bin"},
		{blob.AssembledPageKey("prs_01", 12, 1), "parses/prs_01/pages/12.a1.json"},
		{blob.IndexKey("prs_01", 1), "parses/prs_01/document.1.json"},
	} {
		if c.got != c.want {
			t.Errorf("the key is %q, want %q", c.got, c.want)
		}
	}
}

// TestAWorkingCopyTakesTheExtensionOfItsMediaType: the extension comes from
// a fixed table, the same on every machine, and a type the table does not
// hold takes "bin".
func TestAWorkingCopyTakesTheExtensionOfItsMediaType(t *testing.T) {
	for mediaType, ext := range map[string]string{
		"application/pdf": "pdf",
		"image/png":       "png",
		"image/jpeg":      "jpg",
		"image/tiff":      "tif",
		"text/plain":      "txt",
		"text/markdown":   "md",
		"text/csv":        "csv",
		"text/html":       "html",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   "docx",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         "xlsx",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": "pptx",
		// Parameters and letter case are not part of the type.
		"text/html; charset=utf-8": "html",
		"Image/PNG":                "png",
		// Anything else.
		"application/zip":    "bin",
		"application/msword": "bin",
		"":                   "bin",
		"../../etc":          "bin",
	} {
		if got, want := blob.WorkKey("prs_01", 7, mediaType), "parses/prs_01/work/source.7."+ext; got != want {
			t.Errorf("WorkKey for %q = %q, want %q", mediaType, got, want)
		}
	}
}

// TestAnOwnerKeyRevealsNoOwner: the key of an owner is 32 hex characters of
// a digest of the owner. Two owners have two keys, one owner always the same
// one, and the key holds no character of the owner's name.
func TestAnOwnerKeyRevealsNoOwner(t *testing.T) {
	// No letter of either name is a hex digit, so a key that held any part
	// of a name would show it.
	const one, other = "org_zulu", "org_kilo"
	sum := sha256.Sum256([]byte(one))
	key := blob.OwnerKey(one)
	if key != hex.EncodeToString(sum[:])[:32] || len(key) != 32 {
		t.Fatalf("OwnerKey(%q) = %q, want the first 32 hex characters of its SHA-256", one, key)
	}
	if blob.OwnerKey(one) != key || blob.OwnerKey(other) == key {
		t.Fatalf("the keys of %q and %q are %q and %q", one, other, key, blob.OwnerKey(other))
	}
	if strings.ContainsAny(key, one) || strings.Contains(blob.SourceKey(one, "sha", "fil_01"), one) {
		t.Fatalf("the key %q holds a part of the owner %q", key, one)
	}
	// An owner may be any string: its key is a valid segment all the same.
	for _, owner := range []string{"", "../other", "a/b", "über", "with\x00nul", strings.Repeat("o", 4096)} {
		if err := blob.ValidKey(blob.SourceKey(owner, "sha", "fil_01")); err != nil {
			t.Errorf("the source key of the owner %q is refused: %v", owner, err)
		}
	}
}

// TestEveryKeyOfTheLayoutIsAKeyAStoreTakes: what the helpers build passes
// the rules of a key, the prefix of a parse passes the rules of a prefix,
// and a listing of that prefix finds every key of the parse and none of
// another whose id it starts.
func TestEveryKeyOfTheLayoutIsAKeyAStoreTakes(t *testing.T) {
	keys := []string{
		blob.WorkKey("prs_01", 1, "application/pdf"),
		blob.WorkKey("prs_01", 1, "application/x-unknown"),
		blob.PageKey("prs_01", 1, 1),
		blob.PageKey("prs_01", 3000, 9_000_000_000),
		blob.ImageKey("prs_01", 1, 1, "image/png"),
		blob.ImageKey("prs_01", 1, 1, "image/jpeg"),
		blob.ImageKey("prs_01", 1, 1, "image/webp"),
		blob.AssembledPageKey("prs_01", 1, 1),
		blob.IndexKey("prs_01", 1),
	}
	others := []string{
		blob.SourceKey("org_zulu", "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "fil_01"),
		blob.PageKey("prs_011", 1, 1),
		blob.IndexKey("prs_0", 1),
	}
	store := blob.NewMemory()
	for _, key := range slices.Concat(keys, others) {
		if err := blob.ValidKey(key); err != nil {
			t.Errorf("ValidKey(%q) = %v", key, err)
		}
		put(t, store, key, []byte("{}"), "")
	}
	if len(slices.Compact(slices.Sorted(slices.Values(keys)))) != len(keys) {
		t.Fatalf("two kinds of object share a key: %q", keys)
	}
	// The prefix ends in a slash, so it is the start of a key and not one.
	prefix := blob.ParsePrefix("prs_01")
	if err := blob.ValidKey(prefix); err == nil {
		t.Fatalf("the prefix %q passed for a key", prefix)
	}
	if err := blob.ValidKey(strings.TrimSuffix(prefix, "/")); err != nil {
		t.Fatalf("the prefix %q without its slash is refused: %v", prefix, err)
	}
	if got := list(t, store, prefix); !slices.Equal(got, slices.Sorted(slices.Values(keys))) {
		t.Fatalf("List(%q) = %q, want the keys of the parse %q", prefix, got, keys)
	}
}

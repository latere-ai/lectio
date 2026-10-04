// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// The key layout of specs/002-object-model.md. Every key under a parse
// carries a token: the lease token of the task that wrote the object, a
// number the task store raises on every claim. Two workers that ran one task
// hold two tokens and so write two keys, the settle that is accepted records
// its own, and a stale worker's late write lands on a key nothing points at.

// OwnerKey is the name an owner's sources are stored under: the first 32 hex
// characters of the SHA-256 of the owner, 128 bits of it. A key therefore
// carries no owner's name, and a listing of the bucket shows none. It is a
// digest and not a secret: whoever knows an owner can compute the owner's key.
func OwnerKey(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:16])
}

// SourceKey is where a source snapshot is stored, the bytes of a file as they
// were uploaded or fetched: "sources/<owner key>/<sha256>/<file id>". The
// middle segment is the hex SHA-256 of the bytes, and the last the id of the
// file that holds them.
func SourceKey(owner, sha256Hex, fileID string) string {
	return "sources/" + OwnerKey(owner) + "/" + sha256Hex + "/" + fileID
}

// ParsePrefix is the prefix of everything one parse wrote: "parses/<parse>/".
// A listing of it finds every object of the parse, and removing those removes
// the parse from the store. It ends in a slash, so the prefix of one parse
// never matches the keys of another whose id it starts.
func ParsePrefix(parseID string) string {
	return "parses/" + parseID + "/"
}

// WorkKey is where the working copy of a parse is stored, the file the pages
// are rendered from after any conversion:
// "parses/<parse>/work/source.<token>.<ext>". The extension follows the media
// type of the copy and is "bin" for a type this package has no extension for.
func WorkKey(parseID string, token int64, mediaType string) string {
	return fmt.Sprintf("%swork/source.%d.%s", ParsePrefix(parseID), token, extension(mediaType))
}

// PageKey is where the result of reading page n is stored, as a reader gave
// it: "parses/<parse>/pages/<n>.<token>.json".
func PageKey(parseID string, n int, token int64) string {
	return fmt.Sprintf("%spages/%d.%d.json", ParsePrefix(parseID), n, token)
}

// ImageKey is where the image of page n that a reader saw is stored, as raw
// image bytes: "parses/<parse>/pages/<n>.<token>.png", with ".jpg" for a JPEG
// and ".bin" for any other media type.
func ImageKey(parseID string, n int, token int64, mediaType string) string {
	ext := extension(mediaType)
	if ext != "png" && ext != "jpg" {
		ext = "bin"
	}
	return fmt.Sprintf("%spages/%d.%d.%s", ParsePrefix(parseID), n, token, ext)
}

// AssembledPageKey is where the result of page n is stored as assembly
// rewrote it, a page whose running headers and footers it marked:
// "parses/<parse>/pages/<n>.a<token>.json". The token is the assemble task's,
// and the "a" keeps the key apart from the page task's own result, whose
// token may be the same number.
func AssembledPageKey(parseID string, n int, token int64) string {
	return fmt.Sprintf("%spages/%d.a%d.json", ParsePrefix(parseID), n, token)
}

// IndexKey is where the document index of a parse is stored, the page list
// with the winning key of every page, the outline and the usage, without
// blocks: "parses/<parse>/document.<token>.json".
func IndexKey(parseID string, token int64) string {
	return fmt.Sprintf("%sdocument.%d.json", ParsePrefix(parseID), token)
}

// extensions is the file extension of each media type a working copy or a
// page image has. The table is fixed and is not the machine's: the standard
// library's lookup reads files that differ from one machine to the next, and
// one object must have one key on all of them.
var extensions = map[string]string{
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
}

// extension is the file extension of a media type, without its dot, and "bin"
// for a type the table does not hold. Parameters and letter case are ignored,
// so "text/html; charset=utf-8" is "html".
func extension(mediaType string) string {
	base, _, _ := strings.Cut(mediaType, ";")
	if ext, ok := extensions[strings.ToLower(strings.TrimSpace(base))]; ok {
		return ext
	}
	return "bin"
}

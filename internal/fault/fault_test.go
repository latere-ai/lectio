// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package fault

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestErrorCarriesCodeDetailAndCause(t *testing.T) {
	plain := New(InvalidPages, "range %q is empty", "9-3")
	if got, want := plain.Error(), `invalid_pages: range "9-3" is empty`; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if plain.Unwrap() != nil {
		t.Fatalf("New keeps no cause, got %v", plain.Unwrap())
	}

	wrapped := Wrap(DocumentCorrupt, io.ErrUnexpectedEOF, "page %d", 3)
	if got, want := wrapped.Error(), "document_corrupt: page 3: unexpected EOF"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(wrapped, io.ErrUnexpectedEOF) {
		t.Fatal("Wrap must keep the cause reachable through errors.Is")
	}
}

func TestCodeOfAndDetailOfReadTheChain(t *testing.T) {
	inner := New(TooManyPages, "3001 pages, limit 3000")
	outer := fmt.Errorf("prepare: %w", inner)

	if got := CodeOf(outer); got != TooManyPages {
		t.Fatalf("CodeOf = %q, want %q", got, TooManyPages)
	}
	if got, want := DetailOf(outer), "3001 pages, limit 3000"; got != want {
		t.Fatalf("DetailOf = %q, want %q", got, want)
	}
}

func TestAnUnclassifiedErrorIsInternalAndSaysNothing(t *testing.T) {
	err := errors.New("dial tcp 10.0.0.7:5432: secret-in-here")
	if got := CodeOf(err); got != Internal {
		t.Fatalf("CodeOf = %q, want %q", got, Internal)
	}
	if got := DetailOf(err); got != "" {
		t.Fatalf("DetailOf must not repeat an unclassified error's text, got %q", got)
	}
	if got := CodeOf(nil); got != Internal {
		t.Fatalf("CodeOf(nil) = %q, want %q", got, Internal)
	}
}

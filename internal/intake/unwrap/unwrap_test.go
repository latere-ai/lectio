// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package unwrap

import (
	"bytes"
	"encoding/asn1"
	"encoding/pem"
	"testing"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/testfixtures"
)

func TestUnwrapWrappedPDF(t *testing.T) {
	p7m := testfixtures.Read(t, testfixtures.WrappedPDF)
	inner, err := Unwrap(p7m)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if !bytes.HasPrefix(inner, []byte("%PDF-")) {
		t.Fatalf("inner does not start with %%PDF-: %q", inner[:min(len(inner), 16)])
	}
	// The recovered document must equal the PDF fixture byte for byte.
	want := testfixtures.Read(t, testfixtures.MinimalPDF)
	if !bytes.Equal(inner, want) {
		t.Errorf("recovered document (%d bytes) != original PDF (%d bytes)", len(inner), len(want))
	}
}

func TestUnwrapWrappedXML(t *testing.T) {
	p7m := testfixtures.Read(t, testfixtures.WrappedXML)
	inner, err := Unwrap(p7m)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if !bytes.Contains(inner, []byte("FatturaElettronica")) {
		t.Errorf("inner XML missing expected content: %q", inner)
	}
}

func TestUnwrapPEMArmored(t *testing.T) {
	der := testfixtures.Read(t, testfixtures.WrappedXML)
	armored := pem.EncodeToMemory(&pem.Block{Type: "PKCS7", Bytes: der})
	inner, err := Unwrap(armored)
	if err != nil {
		t.Fatalf("Unwrap PEM: %v", err)
	}
	if !bytes.Contains(inner, []byte("FatturaElettronica")) {
		t.Errorf("inner XML missing expected content: %q", inner)
	}
}

// oidData is 1.2.840.113549.1.7.1, the content type of plain data.
var oidData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}

// explicit0 wraps content in a context-specific, constructed [0] tag.
func explicit0(content []byte) asn1.RawValue {
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: content}
}

// container builds a DER ContentInfo of the given type around a SignedData
// whose encapsulated content is eContent. A nil eContent leaves the content
// out, which is the shape of a detached signature.
func container(t *testing.T, contentType asn1.ObjectIdentifier, eContent []byte) []byte {
	t.Helper()
	type encap struct {
		EContentType asn1.ObjectIdentifier
		EContent     asn1.RawValue `asn1:"optional"`
	}
	type signed struct {
		Version          int
		DigestAlgorithms asn1.RawValue
		EncapContentInfo encap
		SignerInfos      asn1.RawValue
	}
	emptySet := asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true}
	sd := signed{Version: 1, DigestAlgorithms: emptySet, SignerInfos: emptySet}
	sd.EncapContentInfo.EContentType = oidData
	if eContent != nil {
		sd.EncapContentInfo.EContent = explicit0(eContent)
	}
	inner, err := asn1.Marshal(sd)
	if err != nil {
		t.Fatalf("marshal SignedData: %v", err)
	}
	out, err := asn1.Marshal(struct {
		ContentType asn1.ObjectIdentifier
		Content     asn1.RawValue
	}{contentType, explicit0(inner)})
	if err != nil {
		t.Fatalf("marshal ContentInfo: %v", err)
	}
	return out
}

// octetString encodes b as a DER OCTET STRING.
func octetString(t *testing.T, b []byte) []byte {
	t.Helper()
	out, err := asn1.Marshal(b)
	if err != nil {
		t.Fatalf("marshal OCTET STRING: %v", err)
	}
	return out
}

// TestUnwrapContentShapes covers the two shapes the encapsulated content
// takes: an OCTET STRING around the document, and the document placed
// directly under the tag.
func TestUnwrapContentShapes(t *testing.T) {
	doc := []byte("the document")
	tests := []struct {
		name     string
		eContent []byte
	}{
		{"octet string", octetString(t, doc)},
		{"document placed directly", doc},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Unwrap(container(t, oidSignedData, tt.eContent))
			if err != nil {
				t.Fatalf("Unwrap: %v", err)
			}
			if !bytes.Equal(got, doc) {
				t.Errorf("document = %q, want %q", got, doc)
			}
		})
	}
}

func TestUnwrapBadInput(t *testing.T) {
	doc := []byte("the document")
	// A ContentInfo that says signedData and holds something else.
	notSignedData, err := asn1.Marshal(struct {
		ContentType asn1.ObjectIdentifier
		Content     asn1.RawValue
	}{oidSignedData, explicit0(octetString(t, doc))})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cases := map[string][]byte{
		"empty":                {},
		"random":               []byte("not a pkcs7 container at all"),
		"truncated der":        {0x30, 0x05, 0x06, 0x03, 0x2a, 0x86},
		"another content type": container(t, oidData, octetString(t, doc)),
		"not signed data":      notSignedData,
		"detached signature":   container(t, oidSignedData, nil),
		"pem around garbage":   pem.EncodeToMemory(&pem.Block{Type: "PKCS7", Bytes: []byte("garbage")}),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Unwrap(in)
			if err == nil {
				t.Fatalf("Unwrap = %d bytes, want an error", len(got))
			}
			if code := fault.CodeOf(err); code != fault.DocumentCorrupt {
				t.Errorf("code = %q, want %q", code, fault.DocumentCorrupt)
			}
		})
	}
}

// TestUnwrapNestedContainers opens a container nested MaxDepth deep one
// level per call, the way the loop that MaxDepth bounds does.
func TestUnwrapNestedContainers(t *testing.T) {
	doc := []byte("the document")
	cur := doc
	for range MaxDepth {
		cur = container(t, oidSignedData, octetString(t, cur))
	}
	for level := range MaxDepth {
		inner, err := Unwrap(cur)
		if err != nil {
			t.Fatalf("level %d: %v", level+1, err)
		}
		cur = inner
	}
	if !bytes.Equal(cur, doc) {
		t.Errorf("document = %q, want %q", cur, doc)
	}
}

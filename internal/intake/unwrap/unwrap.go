// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package unwrap opens a signed .p7m container, a CMS SignedData structure
// (RFC 5652) that carries the signed document inside it, and returns that
// document. It reads the ASN.1 structure and nothing more: no signature and
// no certificate is verified, so the result says what the container holds and
// nothing about who signed it.
package unwrap

import (
	"encoding/asn1"
	"encoding/pem"

	"latere.ai/x/lectio/internal/fault"
)

// MaxDepth is how many containers, one inside the other, a caller opens
// before it refuses the file. Unwrap opens one container per call; the bound
// is for the loop around it, which would otherwise follow nesting for as long
// as the file supplies it.
const MaxDepth = 3

// oidSignedData is 1.2.840.113549.1.7.2.
var oidSignedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}

// contentInfo is the outer CMS wrapper.
type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"explicit,optional,tag:0"`
}

// signedData is the CMS SignedData structure. The fields after the
// encapsulated content are raw and optional, so a container with or without
// certificates and revocation lists reads the same way.
type signedData struct {
	Version          int
	DigestAlgorithms asn1.RawValue
	EncapContentInfo encapContentInfo
	Certificates     asn1.RawValue `asn1:"optional,tag:0"`
	CRLs             asn1.RawValue `asn1:"optional,tag:1"`
	SignerInfos      asn1.RawValue `asn1:"optional,set"`
}

// encapContentInfo holds the signed document.
type encapContentInfo struct {
	EContentType asn1.ObjectIdentifier
	EContent     asn1.RawValue `asn1:"explicit,optional,tag:0"`
}

// Unwrap returns the document inside one signed container. It reads p7m as
// DER, then as PEM around DER. Input that is neither, or that carries no
// document, fails with fault.DocumentCorrupt.
func Unwrap(p7m []byte) ([]byte, error) {
	if inner, ok := tryDER(p7m); ok {
		return inner, nil
	}
	// PEM: decode the first block and read it as DER.
	if block, _ := pem.Decode(p7m); block != nil {
		if inner, ok := tryDER(block.Bytes); ok {
			return inner, nil
		}
	}
	return nil, fault.New(fault.DocumentCorrupt,
		"the signed container is not a readable CMS SignedData structure with a document inside")
}

// tryDER reads DER-encoded CMS SignedData and returns its encapsulated
// content.
func tryDER(der []byte) ([]byte, bool) {
	var ci contentInfo
	if _, err := asn1.Unmarshal(der, &ci); err != nil {
		return nil, false
	}
	if !ci.ContentType.Equal(oidSignedData) {
		return nil, false
	}
	var sd signedData
	if _, err := asn1.Unmarshal(ci.Content.Bytes, &sd); err != nil {
		return nil, false
	}
	content := sd.EncapContentInfo.EContent
	if len(content.Bytes) == 0 {
		return nil, false
	}
	// Inside the explicit [0] tag the content is an OCTET STRING holding the
	// document, which is peeled here. Some producers put the document there
	// directly; then the peel fails and the bytes already are the document.
	var payload []byte
	if _, err := asn1.Unmarshal(content.Bytes, &payload); err == nil && len(payload) > 0 {
		return payload, true
	}
	return content.Bytes, true
}

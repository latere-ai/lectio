// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package detect identifies the media type of a file and names the route
// intake takes for it. The content decides first: the leading bytes, the part
// names of a zipped office package, and the signature of a signed container.
// The file name's extension is the first fallback and the declared type the
// second, so a file that is misnamed or mislabeled is still handled as what
// it is.
package detect

import (
	"bytes"
	"net/http"
	"path/filepath"
	"strings"

	"latere.ai/x/lectio/internal/fault"
)

// DeclaredType is what the submitter said about a file: the media type from
// upload metadata or an HTTP Content-Type, and the file name. Detect uses
// both only as fallbacks behind the content.
type DeclaredType struct {
	MIME     string
	FileName string
}

// RouteClass is the route intake takes for a detected type.
type RouteClass string

// The route classes.
const (
	// ClassReader is a file whose pages are images, or are rendered to
	// images, and read through a reader.
	ClassReader RouteClass = "reader"
	// ClassNativeDOCX is a word-processing package read from its own
	// structure.
	ClassNativeDOCX RouteClass = "native-docx"
	// ClassConvertDOCToDOCX is a legacy word-processing file, converted to
	// the current format and then read as ClassNativeDOCX.
	ClassConvertDOCToDOCX RouteClass = "convert-doc-to-docx"
	// ClassNativeSpreadsheet is a workbook or a delimited table read from its
	// own structure.
	ClassNativeSpreadsheet RouteClass = "native-spreadsheet"
	// ClassNativeText is a text format read from its own structure.
	ClassNativeText RouteClass = "native-text"
	// ClassConvertToPDF is a presentation or a rich text file, converted to
	// PDF and then read as ClassReader.
	ClassConvertToPDF RouteClass = "convert-to-pdf"
	// ClassUnwrapP7M is a signed container, opened to the document inside,
	// which is then detected again.
	ClassUnwrapP7M RouteClass = "unwrap-p7m"
)

// The canonical media types intake knows.
const (
	MIMEPDF        = "application/pdf"
	MIMEJPEG       = "image/jpeg"
	MIMEPNG        = "image/png"
	MIMETIFF       = "image/tiff"
	MIMEDOCX       = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	MIMEDOC        = "application/msword"
	MIMEXLSX       = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	MIMEXLS        = "application/vnd.ms-excel"
	MIMEXLSM       = "application/vnd.ms-excel.sheet.macroenabled.12"
	MIMECSV        = "text/csv"
	MIMETXT        = "text/plain"
	MIMEHTML       = "text/html"
	MIMEXMLText    = "text/xml"
	MIMEMarkdown   = "text/markdown"
	MIMEPPTX       = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	MIMEPPT        = "application/vnd.ms-powerpoint"
	MIMERTF        = "application/rtf"
	MIMEKeynote    = "application/vnd.apple.keynote"
	MIMEPKCS7MIME  = "application/pkcs7-mime"
	MIMEPKCS7XMIME = "application/x-pkcs7-mime"
	MIMEPKCS7Sig   = "application/pkcs7-signature"
	MIMEOctet      = "application/octet-stream"
)

// classByMIME maps a canonical media type to its route class. A type that is
// not in this table is unsupported.
var classByMIME = map[string]RouteClass{
	MIMEPDF:        ClassReader,
	MIMEJPEG:       ClassReader,
	MIMEPNG:        ClassReader,
	MIMETIFF:       ClassReader,
	MIMEDOCX:       ClassNativeDOCX,
	MIMEDOC:        ClassConvertDOCToDOCX,
	MIMEXLSX:       ClassNativeSpreadsheet,
	MIMEXLS:        ClassNativeSpreadsheet,
	MIMEXLSM:       ClassNativeSpreadsheet,
	MIMECSV:        ClassNativeSpreadsheet,
	MIMETXT:        ClassNativeText,
	MIMEHTML:       ClassNativeText,
	MIMEXMLText:    ClassNativeText,
	MIMEMarkdown:   ClassNativeText,
	MIMEPPTX:       ClassConvertToPDF,
	MIMEPPT:        ClassConvertToPDF,
	MIMERTF:        ClassConvertToPDF,
	MIMEKeynote:    ClassConvertToPDF,
	MIMEPKCS7MIME:  ClassUnwrapP7M,
	MIMEPKCS7XMIME: ClassUnwrapP7M,
	MIMEPKCS7Sig:   ClassUnwrapP7M,
}

// mimeByExt maps a lowercase file name extension, without the dot, to a
// canonical media type.
var mimeByExt = map[string]string{
	"pdf":     MIMEPDF,
	"jpg":     MIMEJPEG,
	"jpeg":    MIMEJPEG,
	"png":     MIMEPNG,
	"tif":     MIMETIFF,
	"tiff":    MIMETIFF,
	"docx":    MIMEDOCX,
	"doc":     MIMEDOC,
	"xlsx":    MIMEXLSX,
	"xls":     MIMEXLS,
	"xlsm":    MIMEXLSM,
	"csv":     MIMECSV,
	"txt":     MIMETXT,
	"html":    MIMEHTML,
	"htm":     MIMEHTML,
	"xml":     MIMEXMLText,
	"md":      MIMEMarkdown,
	"pptx":    MIMEPPTX,
	"ppt":     MIMEPPT,
	"rtf":     MIMERTF,
	"key":     MIMEKeynote,
	"keynote": MIMEKeynote,
	"p7m":     MIMEPKCS7MIME,
}

// ClassOf returns the route class of a media type, and whether the type is
// supported. The type is made canonical first.
func ClassOf(mime string) (RouteClass, bool) {
	c, ok := classByMIME[Canonical(mime)]
	return c, ok
}

// Canonical normalizes a media type: it drops the parameters, lowercases the
// rest, and maps a known alias to the one spelling the tables use.
func Canonical(mime string) string {
	if mime == "" {
		return ""
	}
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	switch mime {
	case "application/xml":
		return MIMEXMLText
	case "text/rtf":
		return MIMERTF
	case "image/jpg", "image/pjpeg":
		return MIMEJPEG
	case "image/x-png":
		return MIMEPNG
	case "image/tif", "image/x-tiff":
		return MIMETIFF
	case "application/vnd.ms-word", "application/word":
		return MIMEDOC
	}
	return mime
}

// Detect returns the canonical media type of a file. peek is the head of the
// file, 8 KiB or more for a reliable answer on zipped packages; declared
// carries the file name and the declared type. A file that no step identifies
// as a supported type fails with fault.UnsupportedMediaType.
func Detect(peek []byte, declared DeclaredType) (string, error) {
	// The content comes first. It cannot tell the text formats apart, since
	// CSV, Markdown and plain text are all just text, so a plain text result
	// is refined by the extension when the extension names a text format.
	if mime := sniff(peek); mime != "" && mime != MIMEOctet {
		if mime == MIMETXT {
			if ext := mimeFromExt(declared.FileName); isTextFormat(ext) {
				mime = ext
			}
		}
		if _, ok := classByMIME[mime]; ok {
			return mime, nil
		}
	}

	// The extension is the first fallback.
	if mime := mimeFromExt(declared.FileName); mime != "" {
		if _, ok := classByMIME[mime]; ok {
			return mime, nil
		}
	}

	// The declared type is the second. A generic binary type says nothing.
	if mime := Canonical(declared.MIME); mime != "" && mime != MIMEOctet {
		if _, ok := classByMIME[mime]; ok {
			return mime, nil
		}
	}

	return "", fault.New(fault.UnsupportedMediaType,
		"the content, the file name extension and the declared type name no supported format")
}

// mimeFromExt returns the media type the extension of name stands for, or the
// empty string.
func mimeFromExt(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	return mimeByExt[ext]
}

// sniff identifies a type from its leading bytes, and for a ZIP or a signed
// container from what the bytes hold. It returns the empty string when
// nothing matches.
func sniff(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	switch {
	case bytes.HasPrefix(b, []byte("%PDF-")):
		return MIMEPDF
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return MIMEPNG
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return MIMEJPEG
	case bytes.HasPrefix(b, []byte("II*\x00")) || bytes.HasPrefix(b, []byte("MM\x00*")):
		return MIMETIFF
	case bytes.HasPrefix(b, []byte("{\\rtf")):
		return MIMERTF
	case bytes.HasPrefix(b, []byte("PK\x03\x04")):
		return classifyZip(b) // a ZIP that is no supported package is generic binary
	case looksLikePKCS7(b):
		return MIMEPKCS7MIME
	}
	// The text formats, through the standard library's sniffer. It reports
	// XML only when the declaration is the very first bytes, so leading
	// white space is handled here.
	if startsWithXML(b) {
		return MIMEXMLText
	}
	switch Canonical(http.DetectContentType(b)) {
	case MIMEHTML:
		return MIMEHTML
	case MIMEXMLText:
		return MIMEXMLText
	case MIMETXT:
		return MIMETXT
	}
	return ""
}

// isTextFormat reports whether mime is one of the formats whose content is
// plain text, which only the file name extension tells apart.
func isTextFormat(mime string) bool {
	switch mime {
	case MIMECSV, MIMETXT, MIMEHTML, MIMEXMLText, MIMEMarkdown:
		return true
	default:
		return false
	}
}

func startsWithXML(b []byte) bool {
	t := strings.TrimSpace(string(b[:min(len(b), 64)]))
	return strings.HasPrefix(t, "<?xml")
}

// classifyZip identifies a zipped office package by the part names each
// format always carries. It searches the head of the file for the names as
// they appear in the ZIP local file headers. Reading only the head keeps the
// answer independent of the central directory at the end of the file, and
// works on a ZIP written as a stream, whose local headers carry no sizes. A
// ZIP that matches no supported layout is generic binary.
func classifyZip(b []byte) string {
	has := func(s string) bool { return bytes.Contains(b, []byte(s)) }
	switch {
	case has("word/document.xml"):
		return MIMEDOCX
	case has("ppt/presentation.xml") || has("ppt/slides/"):
		return MIMEPPTX
	case has("xl/workbook.xml") || has("xl/worksheets/"):
		if has("xl/vbaProject.bin") {
			return MIMEXLSM
		}
		return MIMEXLSX
	case has(".apxl") || has("Index/Document.iwa"):
		return MIMEKeynote
	}
	return MIMEOctet
}

// looksLikePKCS7 recognizes a DER-encoded CMS SignedData container: an ASN.1
// SEQUENCE whose first bytes hold the signedData object identifier.
func looksLikePKCS7(b []byte) bool {
	if len(b) < 2 || b[0] != 0x30 {
		return false
	}
	// signedData is 1.2.840.113549.1.7.2, encoded 2a 86 48 86 f7 0d 01 07 02.
	oid := []byte{0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x07, 0x02}
	return bytes.Contains(b[:min(len(b), 64)], oid)
}

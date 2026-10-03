# Test fixtures

The files under `files/` are real documents the intake tests read. They are
committed so the tests run offline and give the same result everywhere. The
whole set stays under 2 MB.

| File | Bytes | Origin | What it is |
|---|---|---|---|
| `minimal.pdf` | 16978 | github.com/py-pdf/sample-files `001-trivial/minimal-document.pdf` | one-page PDF |
| `multipage.pdf` | 105779 | github.com/mozilla/pdf.js `test/pdfs/basicapi.pdf` | three-page PDF |
| `sample.docx` | 10692 | github.com/jgm/pandoc `test/docx/golden/block_quotes.docx` | OOXML word-processing package |
| `sample.xlsx` | 6840 | github.com/tealeg/xlsx `testdocs/testfile.xlsx` | OOXML spreadsheet package |
| `sample.pptx` | 29783 | github.com/jgm/pandoc `test/pptx/code/output.pptx` | OOXML presentation package |
| `sample.jpg` | 21459 | github.com/golang/go `src/image/testdata/video-001.jpeg` | JPEG |
| `sample.png` | 29228 | github.com/golang/go `src/image/testdata/video-001.png` | PNG |
| `multipage.tiff` | 816 | github.com/python-pillow/Pillow `Tests/images/multipage.tiff` | three-frame TIFF |
| `sample.csv` | 62 | generated locally | CSV |
| `sample.txt` | 37 | generated locally | plain text |
| `sample.html` | 113 | generated locally | HTML |
| `sample.xml` | 61 | generated locally | XML |
| `sample.md` | 56 | generated locally | Markdown |
| `wrapped-pdf.p7m` | 18434 | `openssl smime -sign -nodetach` over `minimal.pdf`, DER output | CMS SignedData with the PDF attached |
| `wrapped-xml.p7m` | 1549 | `openssl smime -sign -nodetach` over a small XML invoice, DER output | CMS SignedData with the XML attached |
| `sample.doc` | 19456 | `sample.docx` through macOS `textutil -convert doc` | Word 97 binary (OLE2) |
| `sample.rtf` | 846 | `sample.docx` through macOS `textutil -convert rtf` | RTF |
| `sample.ppt` | 505344 | `sample.pptx` through LibreOffice 25.2 `--convert-to ppt` | PowerPoint 97 binary (OLE2) |
| `sample.key` | 124499 | one slide made in Keynote 15.3.1, theme images and previews removed with `zip -d` | Keynote package in the IWA layout (ZIP) |

## Signed containers

The two `.p7m` files are CMS SignedData structures with the content attached,
signed with a self-signed certificate made for this purpose and used for
nothing else. The signature is not meant to verify against any trust store.

## Files produced locally

The text files hold content that no test depends on beyond its format. The
`.doc` and `.rtf` files come from Apple's text system and the `.ppt` file from
LibreOffice, each converted from the OOXML fixture of the same name. The
Keynote package keeps only its `Index/*.iwa` parts, which carry the slide.

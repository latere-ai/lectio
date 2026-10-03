// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"path"
	"strings"

	"latere.ai/x/lectio/internal/fault"
)

// bounds are what a zipped office package is held to. A package is a small
// file that says how large it becomes: its directory says how many parts it
// has and how far each inflates, and its markup says how deep it nests and
// how many cells a sheet has. Every such statement is checked against a
// bound before anything is allocated for it, and the reading itself is held
// to the same bounds, because a statement can be false.
type bounds struct {
	// directoryBytes is how much of the file may be read to list its
	// entries. The directory is read whole before any entry can be
	// counted, and each entry costs several times its bytes in memory.
	directoryBytes int64
	// entries is how many entries a package may list.
	entries int
	// partBytes is how far one part that is read may inflate, and
	// inflatedBytes how far all of them together may.
	partBytes, inflatedBytes int64
	// depth is how many elements may be open at once, and tokens how many
	// tokens the parts that are read may hold together.
	depth  int
	tokens int64
	// runBytes is the most bytes between two opening angle brackets: the
	// size of one tag or one run of text. A start tag is decoded whole,
	// and its attributes cost many times their bytes.
	runBytes int
	// cells is how many cells the tables of a file may hold together,
	// counted as rows times columns, and textBytes how many bytes of text
	// their cells may hold. A cell that names a shared string costs a few
	// bytes in the file and the whole string in the result.
	cells     int64
	textBytes int64
	// blocks is how many blocks one page may hold.
	blocks int
	// formats is how many cell formats a workbook, or paragraph styles a
	// word-processing document, may declare.
	formats int
}

// limits are the bounds every package is read under.
var limits = bounds{
	directoryBytes: 4 << 20,
	entries:        10_000,
	partBytes:      64 << 20,
	inflatedBytes:  128 << 20,
	depth:          256,
	tokens:         32 << 20,
	runBytes:       1 << 20,
	cells:          1 << 20,
	textBytes:      32 << 20,
	blocks:         500_000,
	formats:        1 << 16,
}

// maxSpan bounds a span a word-processing table declares. A span beyond it
// is the bound, so a hostile span cannot make a table arbitrarily wide.
const maxSpan = 1000

// errBounds are the sentinels the readers under a part return, so that the
// error a decoder passes up can be told from a malformed part.
var (
	errDirectory = errors.New("the directory is over its bound")
	errRun       = errors.New("a tag or a run of text is over its bound")
)

// budgeted is the file as the directory is read from it. It fails once
// more than left bytes were read, which bounds how many entries the
// directory can make the reader hold.
type budgeted struct {
	r    *bytes.Reader
	left int64
	// open ends the budget: the directory was read, and what follows are
	// the parts, each under its own bound.
	open bool
}

func (b *budgeted) ReadAt(p []byte, off int64) (int, error) {
	if !b.open {
		if b.left -= int64(len(p)); b.left < 0 {
			return 0, errDirectory
		}
	}
	return b.r.ReadAt(p, off)
}

// pkg is an open package and what reading it has cost so far.
type pkg struct {
	ctx   context.Context
	b     bounds
	parts map[string]*zip.File

	inflated int64 // bytes the parts read so far declared
	tokens   int64 // tokens read so far
	cells    int64 // cells charged so far
	text     int64 // bytes of cell text charged so far
}

// open reads the directory of a package. Nothing is inflated here.
func open(ctx context.Context, data []byte, b bounds) (*pkg, error) {
	src := &budgeted{r: bytes.NewReader(data), left: b.directoryBytes}
	zr, err := zip.NewReader(src, int64(len(data)))
	switch {
	case errors.Is(err, errDirectory):
		return nil, fault.New(fault.FileTooLarge, "the package's directory is over %d bytes", b.directoryBytes)
	case err != nil && !errors.Is(err, zip.ErrInsecurePath):
		// An entry with a name that leaves the package is not an error
		// here: no entry is ever written to a file system, and a part is
		// found by its name among the entries or not at all.
		return nil, fault.Wrap(fault.DocumentCorrupt, err, "the file is not a readable package")
	}
	src.open = true
	if len(zr.File) > b.entries {
		return nil, fault.New(fault.FileTooLarge, "the package lists %d entries, over the limit of %d", len(zr.File), b.entries)
	}
	p := &pkg{ctx: ctx, b: b, parts: make(map[string]*zip.File, len(zr.File))}
	for _, f := range zr.File {
		// Part names compare without case. The first entry of a name is
		// the part; a second one with the same name is never read.
		name := strings.ToLower(f.Name)
		if _, taken := p.parts[name]; !taken {
			p.parts[name] = f
		}
	}
	return p, nil
}

// has reports whether the package holds a part.
func (p *pkg) has(name string) bool {
	return p.parts[strings.ToLower(name)] != nil
}

// read opens a part for reading as markup. what names the part's role for
// an error's detail, which never holds a name the file supplied.
//
// The size the directory declares is checked before a byte is inflated, so
// a part that would inflate past a bound is refused for what it declares.
// A part that inflates past what it declares is refused by the archive
// reader as it is read, so the declared size also bounds the reading.
func (p *pkg) read(name, what string) (*markup, error) {
	f := p.parts[strings.ToLower(name)]
	if f == nil {
		return nil, fault.New(fault.DocumentCorrupt, "the package has no %s", what)
	}
	if f.UncompressedSize64 > uint64(p.b.partBytes) {
		return nil, fault.New(fault.FileTooLarge, "the %s declares %d bytes, over the limit of %d for one part", what, f.UncompressedSize64, p.b.partBytes)
	}
	if p.inflated += int64(f.UncompressedSize64); p.inflated > p.b.inflatedBytes {
		return nil, fault.New(fault.FileTooLarge, "the parts of the package declare over %d bytes together", p.b.inflatedBytes)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fault.Wrap(fault.DocumentCorrupt, err, "the %s could not be opened", what)
	}
	return &markup{p: p, what: what, size: int64(f.UncompressedSize64), rc: rc, dec: xml.NewDecoder(&runs{r: rc, max: p.b.runBytes})}, nil
}

// charge counts cells and bytes of cell text against the file's bounds.
func (p *pkg) charge(cells, text int64) error {
	if p.cells += cells; p.cells > p.b.cells {
		return fault.New(fault.FileTooLarge, "the tables of the file hold over %d cells", p.b.cells)
	}
	if p.text += text; p.text > p.b.textBytes {
		return fault.New(fault.FileTooLarge, "the cells of the file hold over %d bytes of text", p.b.textBytes)
	}
	return nil
}

// runs is a part's bytes on their way to the decoder. It fails when more
// than max bytes pass without an opening angle bracket. The decoder returns
// a start tag whole, with one value per attribute, so a tag of many
// megabytes would cost many times its size; a bracket cannot occur inside a
// tag, so the distance between two brackets bounds the tag.
type runs struct {
	r   io.Reader
	max int
	run int
}

func (r *runs) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	for chunk := p[:n]; len(chunk) > 0; {
		i := bytes.IndexByte(chunk, '<')
		if i < 0 {
			r.run += len(chunk)
			break
		}
		if r.run+i > r.max {
			return 0, errRun
		}
		r.run, chunk = 0, chunk[i+1:]
	}
	if r.run > r.max {
		return 0, errRun
	}
	return n, err
}

// markup is one part being read as a stream of tokens. The part is never
// held whole: what a reader keeps is what it builds from the tokens.
type markup struct {
	p    *pkg
	what string
	size int64
	rc   io.Closer
	dec  *xml.Decoder
	open int // elements open
}

// close releases the part. An error closing a part that was read to its
// end, or abandoned on an earlier error, says nothing the reading did not.
func (m *markup) close() { _ = m.rc.Close() }

// token returns the next token, or io.EOF at the end of the part. It is
// where the bounds on markup are enforced, so every reader is under them.
func (m *markup) token() (xml.Token, error) {
	tok, err := m.dec.Token()
	switch {
	case errors.Is(err, io.EOF):
		return nil, io.EOF
	case errors.Is(err, errRun):
		return nil, fault.New(fault.DocumentCorrupt, "the %s holds a tag or a run of text of over %d bytes", m.what, m.p.b.runBytes)
	case err != nil:
		return nil, fault.Wrap(fault.DocumentCorrupt, err, "the %s could not be read as markup", m.what)
	}
	if m.p.tokens++; m.p.tokens > m.p.b.tokens {
		return nil, fault.New(fault.FileTooLarge, "the parts of the package hold over %d tokens of markup", m.p.b.tokens)
	}
	if m.p.tokens%4096 == 0 {
		if err := m.p.ctx.Err(); err != nil {
			return nil, err
		}
	}
	switch tok.(type) {
	case xml.StartElement:
		if m.open++; m.open > m.p.b.depth {
			return nil, fault.New(fault.DocumentCorrupt, "the %s nests over %d elements deep", m.what, m.p.b.depth)
		}
	case xml.EndElement:
		m.open--
	case xml.Directive:
		// The format forbids a document type declaration. The decoder
		// would not act on one: it defines no entity from it and fetches
		// nothing it names. Refusing the part says so outright.
		return nil, fault.New(fault.DocumentCorrupt, "the %s declares a document type, which the format forbids", m.what)
	}
	return tok, nil
}

// next returns the next token of the element a reader is in. The end of
// the part is an error here: a reader reads to the end of its element, and
// the decoder reports the end of the part only once every element is
// closed, so a reader that meets it was given a part with no element.
func (m *markup) next() (xml.Token, error) {
	tok, err := m.token()
	if errors.Is(err, io.EOF) {
		return nil, fault.New(fault.DocumentCorrupt, "the %s holds no markup", m.what)
	}
	return tok, err
}

// end reads from the end of the root element to the end of the part. The
// archive checks a part's bytes against its checksum only at the part's
// end, so a reader that stopped where the markup ends would take a damaged
// part for a sound one.
func (m *markup) end() error {
	for {
		if _, err := m.token(); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
	}
}

// root reads to the part's root element and returns its start tag.
func (m *markup) root() (xml.StartElement, error) {
	for {
		tok, err := m.next()
		if err != nil {
			return xml.StartElement{}, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se, nil
		}
	}
}

// skip reads past the element whose start tag was just returned.
func (m *markup) skip() error {
	for in := m.open; m.open >= in; {
		if _, err := m.next(); err != nil {
			return err
		}
	}
	return nil
}

// text returns the character data of the element whose start tag was just
// returned, without the markup inside it.
func (m *markup) text() (string, error) {
	var out strings.Builder
	for in := m.open; m.open >= in; {
		tok, err := m.next()
		if err != nil {
			return "", err
		}
		if chars, ok := tok.(xml.CharData); ok {
			out.Write(chars)
		}
	}
	return out.String(), nil
}

// attr returns the value of the attribute with a local name, whatever its
// prefix, and the empty string when the element has none.
func attr(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// on reports whether an attribute that switches something on is set: it is
// present and its value is not one of the spellings of off. An element such
// as a header-row mark is on by being there.
func on(value string) bool {
	switch value {
	case "0", "false", "off":
		return false
	}
	return true
}

// relation is one relationship of a part: its id, the last segment of its
// type, and the part it names.
type relation struct {
	id, kind, target string
}

// relations reads the relationships of a part, in order. A package's own
// relationships are those of the part with the empty name. A relationship
// whose target is outside the package is left out: one that says so, and
// one whose path climbs out of the package's root. Nothing is ever read
// through a relationship but a part of the same package.
func (p *pkg) relations(of string) ([]relation, error) {
	dir, base := path.Split(of)
	name := dir + "_rels/" + base + ".rels"
	if !p.has(name) {
		return nil, nil
	}
	m, err := p.read(name, "relationships part")
	if err != nil {
		return nil, err
	}
	defer m.close()
	if _, err := m.root(); err != nil {
		return nil, err
	}
	var out []relation
	for m.open > 0 {
		tok, err := m.next()
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Relationship" {
			continue
		}
		if strings.EqualFold(attr(se, "TargetMode"), "External") {
			continue
		}
		target, inside := resolve(dir, attr(se, "Target"))
		if !inside {
			continue
		}
		kind := attr(se, "Type")
		out = append(out, relation{id: attr(se, "Id"), kind: kind[strings.LastIndexByte(kind, '/')+1:], target: target})
	}
	return out, m.end()
}

// resolve returns the part a relationship's target names, from the
// directory of the part that holds the relationship. inside is false for a
// target that is empty or that leaves the package.
func resolve(dir, target string) (name string, inside bool) {
	if target == "" {
		return "", false
	}
	if rooted, ok := strings.CutPrefix(target, "/"); ok {
		name = path.Clean(rooted)
	} else {
		name = path.Clean(dir + target)
	}
	if name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
		return "", false
	}
	return name, true
}

// first returns the target of the first relationship of a kind, or the
// empty string.
func first(rels []relation, kind string) string {
	for _, r := range rels {
		if r.kind == kind {
			return r.target
		}
	}
	return ""
}

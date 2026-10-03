// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package prompts holds every instruction Lectio sends to a model, and is
// the only place one is written. Each is a template file beside this one.
// The files are compiled into the binary and parsed once, when the process
// starts; a prompt is rendered when a call is made, from that call's data:
// the page's languages, the caller's schema, what an earlier reply got
// wrong. No adapter builds an instruction out of strings.
//
// The data a template is rendered with is a struct, one per prompt, and
// that struct is the list of what the template may refer to. A template
// that names anything else does not parse into a working prompt, and a
// test renders every one, so a broken template fails the build's tests and
// not a page.
//
// What a caller supplies, its instructions, its schema, the document, is
// data and is written into the prompt as it is. It is never parsed as a
// template, so template syntax in a caller's text is plain text.
//
// The reply schemas that go with a prompt are not here. They are the wire
// structure an adapter decodes, and they live with the code that decodes
// them.
package prompts

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"strconv"
	"strings"
	"text/template"
)

// The names of the prompts. A prompt's template is the file <name>.tmpl.
const (
	PageName    = "page"
	ExtractName = "extract"
)

//go:embed *.tmpl
var files embed.FS

// templates is every prompt, parsed. A template that refers to a member
// its data does not have fails when it is rendered, and not silently.
var templates = template.Must(template.New("").
	Option("missingkey=error").
	Funcs(template.FuncMap{"join": strings.Join}).
	ParseFS(files, "*.tmpl"))

// PageData is what the page prompt is rendered with.
type PageData struct {
	// Kinds is the closed set of block kinds, by name.
	Kinds []string

	// Grid is the size of the grid the model places boxes on.
	Grid int

	// Languages are the languages the page is most likely written in.
	Languages []string
}

// ExtractData is what the extraction prompt is rendered with.
type ExtractData struct {
	// Citations says whether each value is to cite the lines it came from.
	Citations bool

	// Instructions are the caller's own. Surrounding space is dropped.
	Instructions string

	// Schema is the caller's JSON Schema, as text.
	Schema string

	// Problems are what an earlier reply got wrong.
	Problems []string

	// Text is the document, one block per line, each led by its ref.
	Text string
}

// Page renders the instruction for reading one page.
func Page(d PageData) (string, error) { return render(PageName, d) }

// Extract renders the instruction for filling a schema from a document.
func Extract(d ExtractData) (string, error) {
	d.Instructions = strings.TrimSpace(d.Instructions)
	return render(ExtractName, d)
}

func render(name string, data any) (string, error) {
	var out strings.Builder
	if err := templates.ExecuteTemplate(&out, name+".tmpl", data); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Names lists the prompts.
func Names() []string { return []string{PageName, ExtractName} }

// Source returns a prompt's template as it is written, comments included.
func Source(name string) (string, error) {
	raw, err := files.ReadFile(name + ".tmpl")
	return string(raw), err
}

// PageVersion names the page prompt as it is asked: the template and the
// two inputs that are the same for every page, the kinds and the grid. Two
// pages read under the same version were asked the same thing, apart from
// their languages. It is a digest of the source, so any edit to the
// template changes it and no number has to be remembered and raised.
func PageVersion(kinds []string, grid int) string {
	return version(PageName, strings.Join(kinds, ","), strconv.Itoa(grid))
}

// ExtractVersion names the extraction prompt as it is asked. Everything
// else that goes into it comes with the request.
func ExtractVersion() string { return version(ExtractName) }

// version is a short digest of a template's source and its fixed inputs.
func version(name string, fixed ...string) string {
	// A name from the list above is always a file in the binary.
	source, _ := Source(name)
	sum := sha256.Sum256([]byte(strings.Join(append([]string{source}, fixed...), "\x00")))
	return hex.EncodeToString(sum[:6])
}

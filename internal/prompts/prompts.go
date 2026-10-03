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
	"strings"
	"text/template"
)

// The names of the prompts. A prompt's template is the file <name>.tmpl.
const (
	PageName    = "page"
	ExtractName = "extract"
	FigureName  = "figure"
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

	// BoxOrder names a box's four numbers in the order this model is asked
	// for them: x0, y0, x1, y1, or y0, x0, y1, x1.
	BoxOrder []string

	// Pixels says boxes are in pixels of the image. When it is false they
	// are on a grid of Grid by Grid.
	Pixels bool
	Grid   int

	// Languages are the languages the page is most likely written in.
	Languages []string
}

// ExtractData is what the extraction prompt is rendered with.
type ExtractData struct {
	// Text is the document, each block led by its ref.
	Text string

	// Schema is the caller's JSON Schema, as text.
	Schema string

	// Citations says whether each value is to cite the blocks it came from.
	Citations bool

	// Instructions are the caller's own. Surrounding space is dropped.
	Instructions string

	// Previous is the reply an earlier attempt gave, and Problems is what
	// it got wrong. Both are empty on a first attempt.
	Previous string
	Problems []string
}

// FigureData is what the figure prompt is rendered with.
type FigureData struct {
	// Types is the closed set of figure types, by name.
	Types []string

	// Caption is the caption the figure has on its page. Surrounding space
	// is dropped. May be empty.
	Caption string

	// Languages are the languages the figure is most likely written in.
	Languages []string
}

// Page renders the instruction for reading one page.
func Page(d PageData) (string, error) { return render(PageName, d) }

// Extract renders the instruction for filling a schema from a document.
func Extract(d ExtractData) (string, error) {
	d.Instructions = strings.TrimSpace(d.Instructions)
	return render(ExtractName, d)
}

// Figure renders the instruction for describing one figure.
func Figure(d FigureData) (string, error) {
	d.Caption = strings.TrimSpace(d.Caption)
	return render(FigureName, d)
}

func render(name string, data any) (string, error) {
	var out strings.Builder
	if err := templates.ExecuteTemplate(&out, name+".tmpl", data); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Names lists the prompts.
func Names() []string { return []string{PageName, ExtractName, FigureName} }

// Source returns a prompt's template as it is written, comments included.
func Source(name string) (string, error) {
	raw, err := files.ReadFile(name + ".tmpl")
	return string(raw), err
}

// PageVersion names the page prompt as it is asked of one reader: the
// template and everything it is rendered with that is the same for every
// page, which is all of d but its languages. Two pages read under the same
// version were asked the same thing. It is a digest, so any edit to the
// template or to how a reader asks for boxes changes it, and no number has
// to be remembered and raised.
func PageVersion(d PageData) string {
	d.Languages = nil
	// The data is fixed and the template is tested, so this render does
	// not fail; if it did, the version would still follow the source.
	asked, _ := Page(d)
	return version(PageName, asked)
}

// ExtractVersion names the extraction prompt as it is asked. Everything
// else that goes into it comes with the request.
func ExtractVersion() string { return version(ExtractName) }

// FigureVersion names the figure prompt as it is asked: the template and
// the set of types. The caption and the languages come with the figure.
func FigureVersion(types []string) string {
	return version(FigureName, strings.Join(types, ","))
}

// version is a short digest of a template's source and its fixed inputs.
func version(name string, fixed ...string) string {
	// A name from the list above is always a file in the binary.
	source, _ := Source(name)
	sum := sha256.Sum256([]byte(strings.Join(append([]string{source}, fixed...), "\x00")))
	return hex.EncodeToString(sum[:6])
}

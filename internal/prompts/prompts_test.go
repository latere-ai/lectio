// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package prompts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var kinds = []string{"title", "text", "table"}

// What a model is asked for a page, in full. A change to the template is a
// change to this test, on purpose: the instruction is behavior.
const pageGolden = `You are reading one page of a document. Return every region of the page as a block, in the order a person would read them.

For each block give:
- kind: one of title, text, table.
- text: the region's content, transcribed exactly. Do not summarize, translate, correct, repeat, or invent text. Leave out what cannot be read. Write a table as HTML with rowspan and colspan, a formula as LaTeX, and a figure as one sentence saying what it shows.
- box: the region as [x0, y0, x1, y1] on a grid where the page is 1000 wide and 1000 high and the origin is the top left corner.
- level: the depth of a title or a heading, from 1 to 6, and null for every other kind.

Mark running headers, running footers and page numbers with their own kinds. A page with nothing on it has no blocks.

Reply with one JSON object and nothing else: {"blocks": [{"kind": "...", "text": "...", "box": [x0, y0, x1, y1], "level": null}]}`

func TestThePagePrompt(t *testing.T) {
	got, err := Page(PageData{Kinds: kinds, Grid: 1000})
	if err != nil || got != pageGolden {
		t.Fatalf("no languages: %v\n%s", err, got)
	}
	got, err = Page(PageData{Kinds: kinds, Grid: 1000, Languages: []string{"de", "en"}})
	if want := pageGolden + "\n\nThe page is most likely written in: de, en."; err != nil || got != want {
		t.Fatalf("with languages: %v\n%s", err, got)
	}
}

const extractGolden = `Fill the schema below from the document that follows it. Use only what the document says. Where the document does not say, use null.
Every line of the document begins with a ref in square brackets. For each value you fill, list the refs of the lines it was read from.
Reply with one JSON object and nothing else: {"data": <the object the schema describes>, "citations": [{"pointer": "<JSON pointer into data>", "refs": ["<ref>"]}]}.

Instructions:
Amounts are in euros.

Schema:
{"type":"object"}

An earlier reply did not satisfy the schema. Correct these:
- /total: expected number
- /date: missing

Document:
[2.2] Total 1,280.50
[2.7] Sum 1,280.50`

const extractPlainGolden = `Fill the schema below from the document that follows it. Use only what the document says. Where the document does not say, use null.
Reply with one JSON object and nothing else: {"data": <the object the schema describes>, "citations": [{"pointer": "<JSON pointer into data>", "refs": ["<ref>"]}]} with an empty list of citations.

Schema:
{"type":"object"}

Document:
[1.1] Total 5`

func TestTheExtractionPrompt(t *testing.T) {
	got, err := Extract(ExtractData{
		Citations: true, Instructions: "  Amounts are in euros.\n", Schema: `{"type":"object"}`,
		Problems: []string{"/total: expected number", "/date: missing"},
		Text:     "[2.2] Total 1,280.50\n[2.7] Sum 1,280.50",
	})
	if err != nil || got != extractGolden {
		t.Fatalf("every part: %v\n%s", err, got)
	}
	got, err = Extract(ExtractData{Schema: `{"type":"object"}`, Text: "[1.1] Total 5"})
	if err != nil || got != extractPlainGolden {
		t.Fatalf("no optional part: %v\n%s", err, got)
	}
}

// What a caller supplies is written into a prompt as it is, and is never
// read as a template.
func TestACallersTextIsDataAndNotATemplate(t *testing.T) {
	hostile := `{{.Schema}} {{template "page.tmpl" .}} {{end}} <b>&amp;</b>`
	got, err := Extract(ExtractData{Instructions: hostile, Schema: hostile, Problems: []string{hostile}, Text: hostile + "\n\n"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, hostile) != 4 || !strings.HasSuffix(got, hostile+"\n\n") {
		t.Fatalf("the caller's text was changed:\n%s", got)
	}
	page, err := Page(PageData{Kinds: kinds, Grid: 10, Languages: []string{hostile}})
	if err != nil || !strings.HasSuffix(page, "written in: "+hostile+".") {
		t.Fatalf("a language is data too: %v\n%s", err, page)
	}
}

func TestEveryPromptIsAFileAndEveryFileIsAPrompt(t *testing.T) {
	found, err := filepath.Glob("*.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != len(Names()) {
		t.Fatalf("%d template files, %d prompts named: %v, %v", len(found), len(Names()), found, Names())
	}
	// A template comment is not sent: no rendering holds one.
	rendered := map[string]string{}
	rendered[PageName], _ = Page(PageData{Kinds: kinds, Grid: 1000, Languages: []string{"de"}})
	rendered[ExtractName], _ = Extract(ExtractData{Citations: true, Instructions: "x", Schema: "{}", Problems: []string{"p"}, Text: "t"})
	for _, name := range Names() {
		source, err := Source(name)
		onDisk, readErr := os.ReadFile(name + ".tmpl")
		if err != nil || readErr != nil || source != string(onDisk) || !strings.Contains(source, "SPDX-License-Identifier") {
			t.Errorf("%s: %v, %v", name, err, readErr)
		}
		out := rendered[name]
		if out == "" || strings.Contains(out, "SPDX") || strings.Contains(out, "{{") || strings.HasPrefix(out, "\n") || strings.HasSuffix(out, "\n") {
			t.Errorf("%s renders as:\n%s", name, out)
		}
	}
	if _, err := Source("nothing"); err == nil {
		t.Error("a prompt that is not there has no source")
	}
	if _, err := render("nothing", nil); err == nil {
		t.Error("a prompt that is not there does not render")
	}
}

func TestAVersionNamesWhatIsAsked(t *testing.T) {
	short := regexp.MustCompile(`^[0-9a-f]{12}$`)
	page, extract := PageVersion(kinds, 1000), ExtractVersion()
	if !short.MatchString(page) || !short.MatchString(extract) || page == extract {
		t.Fatalf("versions %q and %q", page, extract)
	}
	if PageVersion(kinds, 1000) != page {
		t.Fatal("the same template and inputs are the same version")
	}
	// Another grid or another set of kinds asks another thing.
	if PageVersion(kinds, 500) == page || PageVersion(append([]string{"code"}, kinds...), 1000) == page {
		t.Fatal("a version does not follow its fixed inputs")
	}
	// And so does another template: the version is a digest of the source.
	if version(PageName) == version(ExtractName) || version(PageName, "x") == version(PageName) {
		t.Fatal("a version does not follow its template")
	}
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/authorizer"
)

// specRow is one row of the vocabulary table of spec 012.
type specRow struct {
	kind, action string
	fields       []string
}

var ticked = regexp.MustCompile("`([^`]+)`")

// specTable reads the vocabulary table out of the spec: the rows between
// its header and the first line that is not a row.
func specTable(t *testing.T) []specRow {
	t.Helper()
	raw, err := os.ReadFile("../specs/012-identity-and-authorization.md")
	if err != nil {
		t.Fatalf("the spec the vocabulary is written in: %v", err)
	}
	_, after, found := strings.Cut(string(raw), "| Kind | Action | Resource fields sent |\n|---|---|---|\n")
	if !found {
		t.Fatal("spec 012 has no vocabulary table with the columns Kind, Action and Resource fields sent")
	}
	var rows []specRow
	for line := range strings.SplitSeq(after, "\n") {
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 3 {
			t.Fatalf("the row %q has %d cells, want 3", line, len(cells))
		}
		row := specRow{
			kind:   strings.Trim(strings.TrimSpace(cells[0]), "`"),
			action: strings.Trim(strings.TrimSpace(cells[1]), "`"),
		}
		for _, m := range ticked.FindAllStringSubmatch(cells[2], -1) {
			row.fields = append(row.fields, m[1])
		}
		rows = append(rows, row)
	}
	return rows
}

// The constants are the spec's table: the same actions in the same order,
// each on the same kind with the same fields. A row added to one and not
// to the other fails here.
func TestTheVocabularyIsTheSpecs(t *testing.T) {
	rows := specTable(t)
	var actions []string
	for _, r := range rows {
		actions = append(actions, r.action)
		if got := authorizer.Kind(r.action); got != r.kind {
			t.Errorf("Kind(%s) = %q, the spec says %q", r.action, got, r.kind)
		}
		if got := authorizer.Fields(r.action); !slices.Equal(got, r.fields) {
			t.Errorf("Fields(%s) = %v, the spec says %v", r.action, got, r.fields)
		}
	}
	if got := authorizer.Actions(); !slices.Equal(got, actions) {
		t.Errorf("Actions() = %v, the spec's table is %v", got, actions)
	}
}

func TestVocabulary(t *testing.T) {
	v := authorizer.Vocabulary()
	if v.Core != authorizer.Core {
		t.Errorf("the vocabulary belongs to %q, want %q", v.Core, authorizer.Core)
	}
	// The shared contract refuses a table it cannot read as a map from
	// action to kind: an empty name, a name used twice.
	if _, err := authz.NewVocabulary(v.Core, v.Actions...); err != nil {
		t.Fatalf("the shared contract refuses the table: %v", err)
	}
	for _, action := range authorizer.Actions() {
		kind, known := v.Kind(action)
		if !known || kind != authorizer.Kind(action) || !authorizer.Known(action) {
			t.Errorf("%s: the vocabulary says %q, %v, and Kind says %q", action, kind, known, authorizer.Kind(action))
		}
	}
	want := []string{authorizer.KindParse, authorizer.KindFile, authorizer.KindReader, authorizer.KindUsage, authorizer.KindQueue}
	if got := v.Kinds(); !slices.Equal(got, want) {
		t.Errorf("Kinds() = %v, want %v", got, want)
	}
	if got := v.Label(authorizer.KindParse); got != "Parses" {
		t.Errorf("the label of Parse is %q, want Parses", got)
	}

	// A caller that changes its copy changes nothing here.
	v.Actions[0].Name = "changed"
	if got := authorizer.Vocabulary().Actions[0].Name; got != authorizer.ActionParseCreate {
		t.Errorf("the first action is %q after a caller changed its copy", got)
	}
}

func TestAnActionOutsideTheVocabulary(t *testing.T) {
	for _, action := range []string{"", "parse.update", "PARSE.READ", "field.create"} {
		if authorizer.Known(action) || authorizer.Kind(action) != "" || authorizer.Fields(action) != nil {
			t.Errorf("%q is read as an action of the vocabulary", action)
		}
	}
}

func TestFieldsAreACopy(t *testing.T) {
	fields := authorizer.Fields(authorizer.ActionParseRead)
	fields[0] = "changed"
	if got := authorizer.Fields(authorizer.ActionParseRead)[0]; got != authorizer.FieldID {
		t.Errorf("the first field is %q after a caller changed its copy", got)
	}
	// A create of a file is asked before the file exists, so it is the one
	// row of a stored kind that carries no id.
	if slices.Contains(authorizer.Fields(authorizer.ActionFileCreate), authorizer.FieldID) {
		t.Error("file.create sends an id")
	}
}

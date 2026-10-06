// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"fmt"
	"regexp/syntax"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"latere.ai/x/lectio/internal/fault"
)

// chain is the definitions of a schema that chain: each applies the next
// one 2 times to the value it is applied to, so holding any value to the
// first applies the last 2^links times.
func chain(links int) string {
	var defs []string
	for i := range links {
		defs = append(defs, fmt.Sprintf(`"a%d":{"allOf":[{"$ref":"#/$defs/a%d"},{"$ref":"#/$defs/a%d"}]}`, i, i+1, i+1))
	}
	defs = append(defs, fmt.Sprintf(`"a%d":{"type":"object"}`, links))
	return `"$defs":{` + strings.Join(defs, ",") + `}`
}

// doubling is a schema that applies the first definition of a chain to the
// object it describes.
func doubling(links int) string {
	return `{"type":"object","$ref":"#/$defs/a0",` + chain(links) + `}`
}

// within fails the test when what ran since began took longer than a check
// of a schema may: the bound is what keeps a worker's slot from being held.
func within(t *testing.T, began time.Time, what string) {
	t.Helper()
	if took := time.Since(began); took > time.Second {
		t.Errorf("%s took %s", what, took)
	}
}

// TestASchemaThatDoublesItsWorkIsRefusedWhenItArrives: a schema of 1,726
// bytes whose definitions each apply the next one 2 times compiles at once
// and would hold a validator for as long as 2^40 applications take. It is
// refused with invalid_schema before anything is held to it, and so is
// every schema that applies more than MaxApplied schemas to one value. The
// longest chain below the bound is taken, and validated.
func TestASchemaThatDoublesItsWorkIsRefusedWhenItArrives(t *testing.T) {
	began := time.Now()
	_, err := Compile([]byte(doubling(40)))
	within(t, began, "refusing a chain of 40")
	if fault.CodeOf(err) != fault.InvalidSchema || !strings.Contains(fault.DetailOf(err), "applies more than 256 subschemas to one value at #") {
		t.Fatalf("a chain of 40: %v", err)
	}
	if detail := fault.DetailOf(err); strings.Contains(detail, resource) || strings.Contains(detail, "file:") {
		t.Fatalf("the reason names a place of the server's own: %s", detail)
	}

	// A link applies itself, 2 references and 2 times what the next link
	// applies: 1, 5, 13, 29, 61, 125, 253, 509. The root adds itself.
	if _, err := Compile([]byte(doubling(7))); fault.CodeOf(err) != fault.InvalidSchema {
		t.Fatalf("a chain of 7 applies 510 at its root: %v", err)
	}
	s := compiled(t, doubling(6))
	if n := s.applied[s.compiled]; n != 254 {
		t.Fatalf("a chain of 6 applies %d schemas at its root, want 254", n)
	}
	if findings := s.Check([]byte(`{}`), false); findings != nil {
		t.Fatalf("a chain of 6 holds {} to: %+v", findings)
	}
	if findings := s.Check([]byte(`[]`), false); len(findings) == 0 {
		t.Fatalf("a chain of 6 takes a list")
	}

	// The same bound for every keyword that applies a schema in place.
	wide := func(keyword string) string {
		return `{"type":"object","` + keyword + `":[` + strings.TrimSuffix(strings.Repeat(`{"$ref":"#/$defs/a"},`, 128), ",") + `],"$defs":{"a":{"type":"object"}}}`
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		if _, err := Compile([]byte(wide(keyword))); fault.CodeOf(err) != fault.InvalidSchema {
			t.Errorf("128 references under %s apply 257: %v", keyword, err)
		}
	}
	for name, schema := range map[string]string{
		"not":              `{"type":"object","not":{"$ref":"#/$defs/a0"},`,
		"if":               `{"type":"object","if":{"$ref":"#/$defs/a0"},`,
		"then":             `{"type":"object","if":true,"then":{"$ref":"#/$defs/a0"},`,
		"else":             `{"type":"object","if":false,"else":{"$ref":"#/$defs/a0"},`,
		"dependentSchemas": `{"type":"object","dependentSchemas":{"a":{"$ref":"#/$defs/a0"}},`,
		"a member":         `{"type":"object","properties":{"a":{"$ref":"#/$defs/a0"}},`,
		"an item":          `{"type":"object","properties":{"a":{"type":"array","items":{"$ref":"#/$defs/a0"}}},`,
	} {
		if _, err := Compile([]byte(schema + chain(8) + `}`)); fault.CodeOf(err) != fault.InvalidSchema || !strings.Contains(fault.DetailOf(err), "applies more than") {
			t.Errorf("a chain of 8 under %s: %v", name, err)
		}
	}
}

// TestASchemaThatAppliesItselfWithoutEndIsRefused: a schema that reaches
// itself with no member and no item in between would be applied to one
// value without end. It is refused whatever keyword closes the circle.
func TestASchemaThatAppliesItselfWithoutEndIsRefused(t *testing.T) {
	for name, schema := range map[string]string{
		"the root refers to itself":   `{"type":"object","$ref":"#"}`,
		"2 definitions refer in turn": `{"type":"object","$ref":"#/$defs/a","$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}}}`,
		"allOf":                       `{"type":"object","properties":{"a":{"$ref":"#/$defs/a"}},"$defs":{"a":{"allOf":[{"$ref":"#/$defs/a"}]}}}`,
		"anyOf":                       `{"type":"object","anyOf":[{"type":"object"},{"$ref":"#"}]}`,
		"oneOf":                       `{"type":"object","oneOf":[{"$ref":"#"}]}`,
		"not":                         `{"type":"object","not":{"$ref":"#"}}`,
		"if":                          `{"type":"object","if":{"$ref":"#"}}`,
		"then":                        `{"type":"object","if":true,"then":{"$ref":"#"}}`,
		"else":                        `{"type":"object","if":false,"else":{"$ref":"#"}}`,
		"dependentSchemas":            `{"type":"object","dependentSchemas":{"a":{"$ref":"#"}}}`,
	} {
		began := time.Now()
		_, err := Compile([]byte(schema))
		within(t, began, "refusing "+name)
		if fault.CodeOf(err) != fault.InvalidSchema || !strings.Contains(fault.DetailOf(err), "without end") {
			t.Errorf("%s: %v, want invalid_schema for a schema applied without end", name, err)
		}
	}
}

// tree is a schema that recurs through its members and its items: a
// section holds sections.
const tree = `{"type":"object","required":["title"],"additionalProperties":false,"properties":{
  "title":{"type":"string"},
  "first":{"$ref":"#"},
  "sections":{"type":"array","items":{"$ref":"#"}}}}`

// TestASchemaThatRecursThroughItsMembersIsTakenAndHeld: a schema that
// reaches itself through a member or an item applies itself to another
// value each time, as often as the object nests. It is taken, and an object
// 3 levels deep is held to it at every level.
func TestASchemaThatRecursThroughItsMembersIsTakenAndHeld(t *testing.T) {
	s := compiled(t, tree)
	good := `{"title":"1","first":{"title":"1.1"},"sections":[{"title":"1.1","sections":[{"title":"1.1.1"},{"title":"1.1.2"}]},{"title":"1.2"}]}`
	if findings := s.Check([]byte(good), false); findings != nil {
		t.Fatalf("a tree that satisfies the schema: %+v", findings)
	}
	bad := `{"title":"1","sections":[{"title":"1.1","sections":[{"title":7},{"name":"1.1.2"}]}]}`
	findings := s.Check([]byte(bad), false)
	var rules []string
	for _, f := range findings {
		rules = append(rules, f.Pointer+" "+f.Rule)
	}
	want := []string{
		"/sections/0/sections/0/title #/properties/title/type",
		"/sections/0/sections/1 #/required",
		"/sections/0/sections/1 #/additionalProperties",
	}
	for _, w := range want {
		found := false
		for _, r := range rules {
			found = found || r == w
		}
		if !found {
			t.Errorf("the findings %v lack %q", rules, w)
		}
	}
}

// TestASchemaThatSharesItsDefinitionsIsTaken: a schema as one is written
// for a document, with definitions used in several places, choices between
// shapes, and a condition, is far below the bound and is held as before.
func TestASchemaThatSharesItsDefinitionsIsTaken(t *testing.T) {
	s := compiled(t, `{"type":"object","required":["seller","buyer"],
	  "properties":{
	    "seller":{"$ref":"#/$defs/party"},"buyer":{"$ref":"#/$defs/party"},"ship_to":{"$ref":"#/$defs/address"},
	    "lines":{"type":"array","items":{"oneOf":[{"$ref":"#/$defs/product"},{"$ref":"#/$defs/service"}]}},
	    "payment":{"anyOf":[{"$ref":"#/$defs/money"},{"type":"null"}]}},
	  "if":{"properties":{"kind":{"const":"credit"}},"required":["kind"]},"then":{"required":["refers_to"]},
	  "$defs":{
	    "address":{"type":"object","properties":{"street":{"type":"string"},"city":{"type":"string"}}},
	    "party":{"type":"object","required":["name"],"properties":{"name":{"type":"string"},"address":{"$ref":"#/$defs/address"}}},
	    "money":{"type":"object","properties":{"amount":{"type":"number"},"currency":{"type":"string"}}},
	    "product":{"type":"object","required":["sku"],"properties":{"sku":{"type":"string"},"price":{"$ref":"#/$defs/money"}}},
	    "service":{"type":"object","required":["hours"],"properties":{"hours":{"type":"number"},"price":{"$ref":"#/$defs/money"}}}}}`)
	if n := s.applied[s.compiled]; n != 3 {
		t.Fatalf("the root applies %d schemas to the object, want itself, if and then", n)
	}
	good := `{"seller":{"name":"A","address":{"city":"X"}},"buyer":{"name":"B"},"ship_to":{"street":"1"},
	  "lines":[{"sku":"s","price":{"amount":1}},{"hours":2}],"payment":null}`
	if findings := s.Check([]byte(good), false); findings != nil {
		t.Fatalf("an object that satisfies it: %+v", findings)
	}
	if findings := s.Check([]byte(`{"seller":{"name":"A"},"buyer":{"address":{"city":1}}}`), false); len(findings) != 2 {
		t.Fatalf("a buyer with no name and a city that is a number: %+v", findings)
	}
}

// TestAReferencePointsAtASchemaOfTheSchema: a reference is "#" or "#" and
// a JSON pointer, and it points at a place the schema holds a schema in. A
// pointer into an example, a default or a listed value would have the
// validator apply what was never read as a schema, with keywords nobody
// checked, so it is refused, and so is a pointer written in a second way,
// a name, and an address.
func TestAReferencePointsAtASchemaOfTheSchema(t *testing.T) {
	hidden := `{"dependencies":{"a":{"$ref":"#"}}}`
	for name, schema := range map[string]string{
		"into an example":             `{"type":"object","examples":[` + hidden + `],"properties":{"a":{"$ref":"#/examples/0"}}}`,
		"into a default":              `{"type":"object","default":` + hidden + `,"properties":{"a":{"$ref":"#/default"}}}`,
		"into a listed value":         `{"type":"object","properties":{"a":{"enum":[` + hidden + `]},"b":{"$ref":"#/properties/a/enum/0"}}}`,
		"into a fixed value":          `{"type":"object","properties":{"a":{"const":` + hidden + `},"b":{"$ref":"#/properties/a/const"}}}`,
		"at the definitions":          `{"type":"object","properties":{"a":{"$ref":"#/$defs"}},"$defs":{"a":{"type":"string"}}}`,
		"at the members":              `{"type":"object","properties":{"a":{"$ref":"#/properties"}}}`,
		"at nothing":                  `{"type":"object","properties":{"a":{"$ref":"#/$defs/gone"}}}`,
		"written with a percent":      `{"type":"object","properties":{"a":{"$ref":"#/%24defs/a"}},"$defs":{"a":{"type":"string"}}}`,
		"with an escape that is none": `{"type":"object","properties":{"a":{"$ref":"#/$defs/a~2"}},"$defs":{"a~2":{"type":"string"}}}`,
		"a name":                      `{"type":"object","properties":{"a":{"$ref":"#a"}},"$defs":{"a":{"type":"string"}}}`,
		"an address":                  `{"type":"object","properties":{"a":{"$ref":"https://example.com/other.json"}}}`,
		"a sibling":                   `{"type":"object","properties":{"a":{"$ref":"other.json#/a"}}}`,
	} {
		_, err := Compile([]byte(schema))
		if fault.CodeOf(err) != fault.InvalidSchema || !strings.Contains(fault.DetailOf(err), "does not point at a schema of this schema") {
			t.Errorf("a reference %s: %v", name, err)
		}
	}
	for name, schema := range map[string]string{
		"a reference that is no text": `{"type":"object","properties":{"a":{"$ref":7}}}`,
		"schemas that are no list":    `{"type":"object","allOf":{"type":"object"}}`,
		"members that are no object":  `{"type":"object","properties":[{"type":"string"}]}`,
		"a schema that is a number":   `{"type":"object","properties":{"a":7}}`,
		"a schema that is a list":     `{"type":"object","not":[{"type":"string"}]}`,
		"a dialect in a member":       `{"type":"object","properties":{"a":{"$schema":"` + draft + `","type":"string"}}}`,
	} {
		if _, err := Compile([]byte(schema)); fault.CodeOf(err) != fault.InvalidSchema {
			t.Errorf("%s: %v", name, err)
		}
	}

	// A member's name is no keyword, whatever it is, and a name with a
	// slash or a tilde is referred to as a pointer escapes it.
	s := compiled(t, `{"type":"object","properties":{"$id":{"type":"string"},"dependencies":{"$ref":"#/$defs/a~1b~0c"},
	  "x":{"$ref":"#/properties/dependencies"},"y":{"$ref":"#/properties/z/prefixItems/1"},"z":{"prefixItems":[true,{"type":"number"}]},
	  "w":{"$ref":"#"},"v":{"$ref":"#/$defs/yes"}},
	  "$defs":{"a/b~c":{"type":"integer"},"yes":true,"definitions":false},
	  "format":"x","title":"t","description":"d","default":{"$ref":7},"examples":[{"x-own":1}],"$comment":"c",
	  "deprecated":false,"readOnly":false,"writeOnly":false}`)
	if findings := s.Check([]byte(`{"$id":"a","dependencies":3,"x":4,"y":5,"v":[]}`), false); findings != nil {
		t.Fatalf("an object that satisfies it: %+v", findings)
	}
	if findings := s.Check([]byte(`{"dependencies":3.5,"y":"five","w":[]}`), false); len(findings) != 3 {
		t.Fatalf("3 members that break it: %+v", findings)
	}
}

// TestACompiledSchemaHoldsNothingTheCountHasNoPriceFor: the keywords are
// held from the other side too, on what the validator will read. A compiled
// schema with a field set that the count does not know is refused by the
// field's name, so a keyword that a later version of the validator comes to
// apply is refused until it has a price. The same holds for a subschema
// compiled in another dialect.
func TestACompiledSchemaHoldsNothingTheCountHasNoPriceFor(t *testing.T) {
	sub := &jsonschema.Schema{DraftVersion: dialect}
	for field, s := range map[string]*jsonschema.Schema{
		"Dependencies":          {DraftVersion: dialect, Dependencies: map[string]any{"a": sub}},
		"RecursiveRef":          {DraftVersion: dialect, RecursiveRef: sub},
		"RecursiveAnchor":       {DraftVersion: dialect, RecursiveAnchor: true},
		"DynamicRef":            {DraftVersion: dialect, DynamicRef: &jsonschema.DynamicRef{Ref: sub}},
		"DynamicAnchor":         {DraftVersion: dialect, DynamicAnchor: "node"},
		"Anchor":                {DraftVersion: dialect, Anchor: "a"},
		"ID":                    {DraftVersion: dialect, ID: "https://example.com/a.json"},
		"UnevaluatedProperties": {DraftVersion: dialect, UnevaluatedProperties: sub},
		"UnevaluatedItems":      {DraftVersion: dialect, UnevaluatedItems: sub},
		"Items":                 {DraftVersion: dialect, Items: sub},
		"AdditionalItems":       {DraftVersion: dialect, AdditionalItems: false},
		"ContentSchema":         {DraftVersion: dialect, ContentSchema: sub},
		"Format":                {DraftVersion: dialect, Format: &jsonschema.Format{Name: "date"}},
	} {
		if got := unmodeled(s); got != field {
			t.Errorf("a schema with %s set names %q", field, got)
		}
		_, err := measure(&jsonschema.Schema{DraftVersion: dialect, Properties: map[string]*jsonschema.Schema{"a": s}})
		if fault.CodeOf(err) != fault.InvalidSchema || !strings.Contains(fault.DetailOf(err), field) {
			t.Errorf("a member with %s set: %v", field, err)
		}
	}
	if _, err := measure(&jsonschema.Schema{DraftVersion: 7}); fault.CodeOf(err) != fault.InvalidSchema || !strings.Contains(fault.DetailOf(err), "another dialect") {
		t.Errorf("a schema of draft 7: %v", err)
	}
	// Every keyword a schema may use compiles to fields that are known.
	s := compiled(t, `{"type":"object","title":"t","description":"d","default":1,"examples":[1],"$comment":"c","deprecated":true,
	  "readOnly":true,"writeOnly":true,"format":"date",
	  "properties":{"a":{"type":["string","null"],"enum":["a",null],"const":"a","minLength":1,"maxLength":2,"pattern":"a","format":"email"},
	    "n":{"multipleOf":1,"maximum":9,"exclusiveMaximum":10,"minimum":0,"exclusiveMinimum":-1},
	    "l":{"items":{"$ref":"#/$defs/d"},"prefixItems":[true],"contains":{"type":"number"},"minContains":0,"maxContains":9,
	         "minItems":0,"maxItems":9,"uniqueItems":true}},
	  "patternProperties":{"^x":false},"additionalProperties":true,"propertyNames":{"maxLength":9},
	  "minProperties":0,"maxProperties":9,"required":["a"],"dependentRequired":{"a":["n"]},"dependentSchemas":{"a":{"type":"object"}},
	  "allOf":[true],"anyOf":[true],"oneOf":[true],"not":false,"if":true,"then":true,"else":true,"$defs":{"d":{"type":"number"}}}`)
	if findings := s.Check([]byte(`{"a":"a","n":3,"l":[1,2]}`), false); findings != nil {
		t.Fatalf("an object that satisfies a schema of every keyword: %+v", findings)
	}
	used := map[string]bool{}
	for _, keyword := range []string{"type", "title", "description", "default", "examples", "$comment", "deprecated", "readOnly", "writeOnly", "format",
		"properties", "enum", "const", "minLength", "maxLength", "pattern", "multipleOf", "maximum", "exclusiveMaximum", "minimum", "exclusiveMinimum",
		"items", "prefixItems", "contains", "minContains", "maxContains", "minItems", "maxItems", "uniqueItems",
		"patternProperties", "additionalProperties", "propertyNames", "minProperties", "maxProperties", "required", "dependentRequired",
		"dependentSchemas", "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "$defs", "$ref", "$schema"} {
		used[keyword] = true
	}
	for _, keyword := range Keywords() {
		if !used[keyword] {
			t.Errorf("the keyword %s is listed and no case of this test uses it", keyword)
		}
	}
	if len(Keywords()) != len(used) {
		t.Errorf("%d keywords are listed and %d are used here", len(Keywords()), len(used))
	}
}

// deep is an object that nests the member a, levels deep.
func deep(levels int, innermost string) string {
	return strings.Repeat(`{"a":`, levels) + innermost + strings.Repeat(`}`, levels)
}

// TestAnObjectThatWouldCostTooMuchToCheckIsNotHeldToTheSchema: a schema can
// apply 2 schemas to one member with nothing that doubles at any one value,
// so what it applies to one value is small and what it applies to an object
// doubles with every level the object nests. Such a schema is taken, an
// object that nests a few levels is held to it, and an object 40 levels
// deep, 240 bytes that would hold the validator for 2^40 applications, is
// not checked: the one finding says so, no repair is asked for, and the
// extraction is not satisfied.
func TestAnObjectThatWouldCostTooMuchToCheckIsNotHeldToTheSchema(t *testing.T) {
	const said = "the object was not held to the schema, which costs too much to check it against: more than 1048576 units of work"
	for name, schema := range map[string]string{
		"2 patterns that take one member": `{"type":"object","patternProperties":{"a":{"$ref":"#"},"^a":{"$ref":"#"}}}`,
		"2 schemas that describe one member": `{"type":"object","$ref":"#/$defs/n",
		  "$defs":{"n":{"allOf":[{"properties":{"a":{"$ref":"#/$defs/n"}}},{"properties":{"a":{"$ref":"#/$defs/n"}}}]}}}`,
		"a member, by its name and by nothing": `{"type":"object","$ref":"#/$defs/n",
		  "$defs":{"n":{"allOf":[{"properties":{"a":{"$ref":"#/$defs/n"}}},{"additionalProperties":{"$ref":"#/$defs/n"}}]}}}`,
		"an item and one of the items": `{"type":"object","properties":{"a":{"$ref":"#/$defs/l"}},
		  "$defs":{"l":{"type":["array","object"],"items":{"$ref":"#/$defs/l"},"contains":{"$ref":"#/$defs/l"},"minContains":0,"properties":{"a":{"$ref":"#/$defs/l"}}}}}`,
	} {
		s := compiled(t, schema)
		shallow, costly := deep(4, `{}`), deep(40, `{}`)
		if strings.Contains(name, "item") {
			shallow, costly = `{"a":[[[]]]}`, `{"a":`+strings.Repeat(`[`, 40)+strings.Repeat(`]`, 40)+`}`
		}
		if findings := s.Check([]byte(shallow), false); findings != nil {
			t.Errorf("%s: an object 4 levels deep: %+v", name, findings)
		}
		began := time.Now()
		findings := s.Check([]byte(costly), false)
		within(t, began, name+": an object 40 levels deep")
		if !unchecked(findings) || findings[0].Rule != "#" || findings[0].Message != said {
			t.Errorf("%s: an object 40 levels deep: %+v", name, findings)
			continue
		}
		if got := Broken(findings); got != said {
			t.Errorf("%s: the field would say %q", name, got)
		}
	}
}

// applications is the work a check of data would count.
func applications(t *testing.T, s *Schema, data string) int {
	t.Helper()
	value, flaws, err := decode([]byte(data), MaxValueDepth)
	if err != nil || len(flaws) > 0 {
		t.Fatalf("%s: %v, %v", data, flaws, err)
	}
	return s.work(value)
}

// TestTheWorkOfACheckIsCountedKeywordByKeyword: every listed keyword has a
// price, and the count of a check is the sum of them. An application is 2
// by itself. A member is half a unit to pass over and what its schemas
// cost, each pattern its name is held to costs before it is matched and
// never nothing, a number held to a bound or asked to be an integer is a
// rational, each value a schema lists is compared, each name it requires is
// looked up, a text held to a length or a pattern costs by its bytes, and a
// list that must hold no value 2 times costs what it weighs. A value that
// is reached again is counted again and looked at once, and a count that
// has looked at MaxCheckWork applications stops.
func TestTheWorkOfACheckIsCountedKeywordByKeyword(t *testing.T) {
	names := make([]string, 32)
	for i := range names {
		names[i] = fmt.Sprintf(`"n%d"`, i)
	}
	text := strings.Repeat("a", 640)
	numbers := func(n int) string { return join(n, strconv.Itoa) }
	for name, tc := range map[string]struct {
		schema, data string
		want         int
	}{
		"an object with nothing in it": {`{"type":"object"}`, `{}`, 2},
		"a member":                     {`{"type":"object","properties":{"a":{"type":"string"}}}`, `{"a":"x","b":1}`, 2 + 1 + 2},
		"a member through a reference": {`{"type":"object","properties":{"a":{"$ref":"#/$defs/s"}},"$defs":{"s":{"type":"string"}}}`, `{"a":"x"}`, 2 + 1 + 4},
		// 3 members, each held to 2 patterns at 1 a match, and 2 of them to
		// the schema of the pattern they match.
		"patterns of names":         {`{"type":"object","patternProperties":{"^a":{"type":"string"},"^b":{"type":"string"}}}`, `{"a1":"x","a2":"y","c":1}`, 2 + 2 + 6 + 4},
		"members nothing describes": {`{"type":"object","properties":{"a":true},"additionalProperties":{"type":"number"}}`, `{"a":1,"b":2,"c":3}`, 2 + 2 + 6},
		"members nobody may add":    {`{"type":"object","properties":{"a":true},"additionalProperties":false}`, `{"a":1,"b":2}`, 2 + 1 + 2},
		// A name is a text: 3 schemas, the one that fixes it compares 1
		// value, and the one that bounds its length counts its bytes.
		"the names of members":          {`{"type":"object","propertyNames":{"anyOf":[{"maxLength":3},{"const":"total"}]}}`, `{"a":1,"total":2}`, 2 + 1 + 2*(2+2+2+1+1)},
		"a prefix and the rest":         {`{"type":"object","properties":{"l":{"prefixItems":[{"type":"string"}],"items":{"type":"number"}}}}`, `{"l":["x",1,2]}`, 2 + 1 + 2 + 6},
		"a prefix and nothing after":    {`{"type":"object","properties":{"l":{"prefixItems":[{"type":"string"}]}}}`, `{"l":["x",1,2]}`, 2 + 1 + 2 + 2},
		"what a list must contain":      {`{"type":"object","properties":{"l":{"contains":{"type":"number"},"minContains":1,"maxContains":2}}}`, `{"l":[1,2]}`, 2 + 1 + 2 + 4},
		"a condition and both its arms": {`{"type":"object","if":{"required":["a"]},"then":{"required":["b"]},"else":{"not":{"required":["c"]}}}`, `{}`, 2 + 3 + 3 + 2 + 3},
		"a schema a member brings":      {`{"type":"object","dependentSchemas":{"a":{"required":["b"]}}}`, `{"a":1}`, 2 + 1 + 3 + 1},
		"an object in a list, 2 times": {`{"type":"object","properties":{"l":{"items":{"$ref":"#/$defs/o"},"contains":{"$ref":"#/$defs/o"}}},"$defs":{"o":{"properties":{"n":{"type":"number"}}}}}`,
			`{"l":[{"n":1}]}`, 2 + 1 + 2 + 2*(2+1+2+1+2)},
		"32 names required":           {`{"type":"object","required":[` + strings.Join(names, ",") + `]}`, `{}`, 2 + 16},
		"16 texts listed":             {`{"type":"object","properties":{"a":{"enum":[` + strings.Join(names[:16], ",") + `]}}}`, `{"a":"x"}`, 2 + 1 + 2 + 16},
		"16 numbers listed":           {`{"type":"object","properties":{"a":{"enum":[` + numbers(16) + `]}}}`, `{"a":"x"}`, 2 + 1 + 2 + 16*4},
		"16 names a member requires":  {`{"type":"object","dependentRequired":{"a":[` + strings.Join(names[:16], ",") + `]}}`, `{}`, 2 + 8},
		"a fixed object":              {`{"type":"object","properties":{"a":{"const":{"k":[1,"x"]}}}}`, `{"a":1}`, 2 + 1 + 2 + 1 + 1 + 1 + 4 + 1},
		"a number within bounds":      {`{"type":"object","properties":{"a":{"minimum":0,"maximum":9,"multipleOf":3}}}`, `{"a":3}`, 2 + 1 + 2 + 4},
		"an integer within bounds":    {`{"type":"object","properties":{"a":{"type":"integer","exclusiveMinimum":0,"exclusiveMaximum":9}}}`, `{"a":3}`, 2 + 1 + 2 + 4 + 4},
		"a text of a length":          {`{"type":"object","properties":{"a":{"minLength":1,"maxLength":700}}}`, `{"a":"` + text + `"}`, 2 + 1 + 2 + 1 + 640*2/64},
		"a pattern of 2,003 steps":    {`{"type":"object","properties":{"a":{"pattern":"^[a-z]{1,1000}$"}}}`, `{"a":"` + text + `"}`, 2 + 1 + 2 + 1 + 640*2003/64},
		"a name held to a pattern":    {`{"type":"object","patternProperties":{"^[a-z]{1,1000}$":{"type":"number"}}}`, `{"` + text + `":1}`, 2 + 1 + 1 + 640*2003/64 + 2},
		"a name held to its schema":   {`{"type":"object","propertyNames":{"pattern":"^[a-z]{1,1000}$"}}`, `{"` + text + `":1}`, 2 + 1 + 2 + 1 + 640*2003/64},
		"a pattern through a choice":  {`{"type":"object","properties":{"a":{"anyOf":[{"pattern":"^[a-z]{1,1000}$"},{"pattern":"^[a-z]{1,1000}$"}]}}}`, `{"a":"` + text + `"}`, 2 + 1 + 6 + 1 + 640*4006/64},
		"a pattern held to no text":   {`{"type":"object","properties":{"a":{"pattern":"^[a-z]{1,1000}$"}}}`, `{"a":640}`, 2 + 1 + 2},
		"3 numbers that must differ":  {`{"type":"object","properties":{"l":{"uniqueItems":true}}}`, `{"l":[1,2,3]}`, 2 + 1 + 2 + 3*(1+3*4)},
		"21 numbers that must differ": {`{"type":"object","properties":{"l":{"uniqueItems":true}}}`, `{"l":[` + numbers(21) + `]}`, 2 + 1 + 2 + 2*(1+21*4)},
		"a list of lists that differ": {`{"type":"object","properties":{"l":{"uniqueItems":true}}}`, `{"l":[[1],{"k":"` + text + `"},"` + text + `",null,[]]}`, 2 + 1 + 2 + 5*(1+5+23+21+1+1)},
	} {
		if got := applications(t, compiled(t, tc.schema), tc.data); got != tc.want {
			t.Errorf("%s: counts %d, want %d", name, got, tc.want)
		}
	}

	// A list that is held to 2 schemas that ask it to hold no value 2
	// times is weighed once.
	twice := compiled(t, `{"type":"object","properties":{"l":{"allOf":[{"uniqueItems":true},{"uniqueItems":true}]}}}`)
	if got := applications(t, twice, `{"l":[1,2,3]}`); got != 2+1+2+2*(2+39) {
		t.Errorf("a list held to 2 such schemas counts %d", got)
	}

	s := compiled(t, `{"type":"object","properties":{"l":{"type":"array","items":{"type":"number"}}}}`)
	value, _, err := decode([]byte(`{"l":[1,2,3]}`), MaxValueDepth)
	if err != nil {
		t.Fatal(err)
	}
	m := &meter{measured: s.measured, counted: map[node]int{}, weights: map[uintptr]int{}, looked: MaxCheckWork - 2}
	if got := m.cost(s.compiled, value); got <= MaxCheckWork {
		t.Fatalf("a count that looked at more than MaxCheckWork applications is %d", got)
	}

	// A reply as long as a model writes one, held to a choice of shapes for
	// each of its values, is checked.
	var items []string
	for i := range 4000 {
		items = append(items, fmt.Sprintf(`{"sku":"s%d","price":{"amount":%d,"currency":"EUR"},"tags":["a","b"]}`, i, i))
	}
	long := compiled(t, `{"type":"object","properties":{"lines":{"type":"array","items":{"anyOf":[
	  {"$ref":"#/$defs/line"},{"$ref":"#/$defs/line"},{"$ref":"#/$defs/line"},{"$ref":"#/$defs/line"},
	  {"$ref":"#/$defs/line"},{"$ref":"#/$defs/line"},{"$ref":"#/$defs/line"},{"$ref":"#/$defs/line"}]}}},
	  "$defs":{"line":{"type":"object","required":["sku"],"properties":{"sku":{"type":"string"},
	    "price":{"type":"object","properties":{"amount":{"type":"number"},"currency":{"type":"string"}}},
	    "tags":{"type":"array","items":{"type":"string"}}}}}}`)
	data := `{"lines":[` + strings.Join(items, ",") + `]}`
	if n := applications(t, long, data); n > MaxCheckWork*3/4 {
		t.Fatalf("a reply of %d bytes with 4,000 lines counts %d of the %d a check may take", len(data), n, MaxCheckWork)
	}
	if findings := long.Check([]byte(data), false); findings != nil {
		t.Fatalf("a long reply that satisfies its schema: %d findings, first %+v", len(findings), findings[0])
	}
}

// TestTheLongestCheckIsWithinWhatAModelCallTakes: an object that counts
// just under what a check may take is checked, in a time that is small
// beside the call that produced it. The object here is the costliest kind
// the prices were set by, a chain of 200 references held to each number of
// a list.
func TestTheLongestCheckIsWithinWhatAModelCallTakes(t *testing.T) {
	schema, data, work := sized(t, families[0], MaxCheckWork)
	if work < MaxCheckWork/2 || work > MaxCheckWork {
		t.Fatalf("the object counts %d of %d", work, MaxCheckWork)
	}
	findings, took, refused := held(schema, data)
	if refused != nil || findings != nil || took > 3*time.Second*slowdown {
		t.Fatalf("%d units took %s with %d findings, refused %v", work, took, len(findings), refused)
	}
}

// TestThePatternsOfASchemaAreBoundedByWhatTheyCompileTo: a pattern is
// matched in time that is the text's length times the steps the pattern
// compiled to, and a repetition with a count compiles to as many copies of
// what it repeats. A pattern of 579 bytes that compiles to 128,005 steps is
// refused before it is compiled, and so are patterns that pass the bound
// together, whether they are held to a string or to the names of members. A
// pattern a schema uses in many places counts once, and the patterns a
// schema for a document holds are taken and held.
func TestThePatternsOfASchemaAreBoundedByWhatTheyCompileTo(t *testing.T) {
	heavy := strings.Repeat(`.{0,1000}`, 64)
	var distinct, same []string
	for i := range 9 {
		distinct = append(distinct, fmt.Sprintf(`"p%d":{"type":"string","pattern":"^%d[a-z]{1,1000}$"}`, i, i))
		same = append(same, fmt.Sprintf(`"p%d":{"type":"string","pattern":"^[a-z]{1,1000}$"}`, i))
	}
	for name, schema := range map[string]string{
		"one pattern of 128,005 steps":    `{"type":"object","properties":{"a":{"type":"string","pattern":"^` + heavy + `b$"}}}`,
		"the same for the names":          `{"type":"object","patternProperties":{"^` + heavy + `b$":{"type":"string"}}}`,
		"9 patterns of 2,000 steps each":  `{"type":"object","properties":{` + strings.Join(distinct, ",") + `}}`,
		"a pattern in a pattern's schema": `{"type":"object","patternProperties":{"^[a-z]{1,1000}$":{"pattern":"^` + heavy + `$"}}}`,
	} {
		began := time.Now()
		_, err := Compile([]byte(schema))
		within(t, began, "refusing "+name)
		if fault.CodeOf(err) != fault.InvalidSchema || !strings.Contains(fault.DetailOf(err), "the patterns of the schema compile to more than 16384 steps") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Compile([]byte(`{"type":"object","properties":{"a":{"pattern":"(a{1000}){1000}"}}}`)); fault.CodeOf(err) != fault.InvalidSchema {
		t.Errorf("a repetition of a repetition: %v", err)
	}

	// 8 of them fit, and one pattern used 9 times is one pattern.
	compiled(t, `{"type":"object","properties":{`+strings.Join(distinct[:8], ",")+`}}`)
	compiled(t, `{"type":"object","properties":{`+strings.Join(same, ",")+`}}`)
	s := compiled(t, `{"type":"object","properties":{
	  "date":{"type":"string","pattern":"^\\d{4}-\\d{2}-\\d{2}$"},
	  "iban":{"type":"string","pattern":"^[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}$"},
	  "mail":{"type":"string","pattern":"^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,255}\\.[A-Za-z]{2,24}$"}},
	  "patternProperties":{"^x-[a-z]+$":{"type":"string"}}}`)
	if findings := s.Check([]byte(`{"date":"2031-03-01","iban":"DE89370400440532013000","mail":"a.b@example.com","x-note":"n"}`), false); findings != nil {
		t.Fatalf("an object that matches its patterns: %+v", findings)
	}
	if findings := s.Check([]byte(`{"date":"1 March","x-note":7}`), false); len(findings) != 2 {
		t.Fatalf("a date and a note that do not match: %+v", findings)
	}

	// The size read from a pattern's parse is what the pattern compiles
	// to, to within a factor of 2 and a few steps.
	for _, source := range []string{`^\d{4}-\d{2}-\d{2}$`, `.{0,1000}`, `[a-z]{1,1000}`, `(ab|cd)*e+f?`, `x{7,}`, `^[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}$`, `abcdef`, ``} {
		parsed, err := syntax.Parse(source, syntax.Perl)
		if err != nil {
			t.Fatal(err)
		}
		prog, err := syntax.Compile(parsed.Simplify())
		if err != nil {
			t.Fatal(err)
		}
		if got, real := weigh(parsed), len(prog.Inst); 2*got+4 < real || got > 2*real+4 {
			t.Errorf("%q weighs %d and compiles to %d steps", source, got, real)
		}
	}
}

// TestWorkPastTheBoundIsNotChecked: a long text held to a pattern of many
// steps, a long name, a long list of numbers held to a long list of values,
// and a long list held to a long list of names each count past what a check
// may take, and none is held to its schema.
func TestWorkPastTheBoundIsNotChecked(t *testing.T) {
	names := join(3200, func(i int) string { return fmt.Sprintf(`"n%d"`, i) })
	long := strings.Repeat("a", 200000)
	began := time.Now()
	for name, tc := range map[string]struct{ schema, data string }{
		"a long text":           {`{"type":"object","properties":{"a":{"pattern":"^[a-z]{1,1000}$"}}}`, `{"a":"` + long + `"}`},
		"a long name":           {`{"type":"object","patternProperties":{"^[a-z]{1,1000}$":{"type":"number"}}}`, `{"` + long + `":1}`},
		"a long name, by rule":  {`{"type":"object","propertyNames":{"pattern":"^[a-z]{1,1000}$"}}`, `{"` + long + `":1}`},
		"a long list of names":  {`{"type":"object","properties":{"l":{"items":{"required":[` + names + `]}}}}`, `{"l":[` + join(1200, func(int) string { return `{}` }) + `]}`},
		"a long list in a list": {`{"type":"object","properties":{"l":{"prefixItems":[{"required":[` + names + `]}],"items":{"required":[` + names + `]},"contains":{"type":"object"}}}}`, `{"l":[` + join(1200, func(int) string { return `{}` }) + `]}`},
		"numbers that differ":   {`{"type":"object","properties":{"l":{"uniqueItems":true}}}`, `{"l":[` + join(140000, strconv.Itoa) + `]}`},
		"many members":          {`{"type":"object","allOf":[` + join(200, func(int) string { return `{"type":"object"}` }) + `]}`, `{` + join(12000, func(i int) string { return fmt.Sprintf(`"m%d":1`, i) }) + `}`},
	} {
		s := compiled(t, tc.schema)
		if findings := s.Check([]byte(tc.data), false); !unchecked(findings) {
			t.Errorf("%s: %d findings, want the object not to be checked", name, len(findings))
		}
	}
	if took := time.Since(began); took > 2*time.Second*slowdown {
		t.Errorf("not checking 7 objects took %s", took)
	}
}

// TestItemsThatHashAlikeAreOneGroupAndPricedPastTheBound: the validator
// frames a text in the hash of a list's items with no length, so items
// whose texts are cut at other places hash alike and are compared in full.
// The meter hashes as the validator does, puts such items in one group and
// prices their pairs past the bound. The case fails closed when the meter's
// hash stops following the validator's: items that meet there are then
// priced as distinct.
func TestItemsThatHashAlikeAreOneGroupAndPricedPastTheBound(t *testing.T) {
	big := join(60, func(j int) string { return strconv.Itoa(j) + ".5" })
	s := compiled(t, `{"type":"object","properties":{"l":{"uniqueItems":true}}}`)
	data := `{"l":[` + join(len(cuts), func(i int) string { return `[[` + big + `],` + cuts[i] + `]` }) + `]}`
	value, _, err := decode([]byte(data), MaxValueDepth)
	if err != nil {
		t.Fatal(err)
	}
	list := value.(map[string]any)["l"].([]any)
	first := hashed(list[0])
	for i, item := range list {
		if hashed(item) != first {
			t.Fatalf("item %d of the 462 does not hash as the first does", i)
		}
	}
	m := &meter{measured: s.measured, counted: map[node]int{}, weights: map[uintptr]int{}}
	if got := m.unique(list); got <= MaxCheckWork {
		t.Fatalf("462 items in 1 group are priced %d, want past %d", got, MaxCheckWork)
	}
	if got := s.work(value); got <= MaxCheckWork {
		t.Fatalf("the check of them counts %d, want past %d", got, MaxCheckWork)
	}
	// Distinct texts hash apart, and so do numbers that differ in size.
	if hashed("ab") == hashed("a\x04b") || hashed([]any{"a", "b"}) != hashed([]any{"a", "b"}) || hashed(1.5) == hashed(2.5) {
		t.Fatal("the hash does not tell what the validator's tells apart")
	}
}

// TestAListOfDistinctSmallObjectsIsChecked: 2,000 objects of 2 members that
// differ hash apart, so the check hashes them and compares none, and it is
// made, in well under a second.
func TestAListOfDistinctSmallObjectsIsChecked(t *testing.T) {
	s := compiled(t, `{"type":"object","properties":{"l":{"uniqueItems":true}}}`)
	data := `{"l":[` + join(2000, func(i int) string { return fmt.Sprintf(`{"id":%d,"name":"item%d"}`, i, i) }) + `]}`
	began := time.Now()
	findings := s.Check([]byte(data), false)
	if unchecked(findings) || len(findings) != 0 {
		t.Fatalf("2,000 distinct objects: %v", findings)
	}
	if took := time.Since(began); took > 500*time.Millisecond*slowdown {
		t.Fatalf("the check took %s", took)
	}
	twice := `{"l":[` + join(2000, func(i int) string { return fmt.Sprintf(`{"id":%d,"name":"item%d"}`, i%1999, i%1999) }) + `]}`
	if findings := s.Check([]byte(twice), false); unchecked(findings) || len(findings) == 0 {
		t.Fatalf("a repeated object is not found: %v", findings)
	}
}

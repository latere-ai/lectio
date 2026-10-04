// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"bytes"
	"fmt"
	"regexp/syntax"
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
		"a dynamic reference":         `{"type":"object","$dynamicAnchor":"node","allOf":[{"$dynamicRef":"#node"}]}`,
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

	// One dynamic anchor names 1 subschema: a reference to it is the
	// reference it reads as, and the tree it describes is held the same.
	dynamic := compiled(t, `{"type":"object","$dynamicAnchor":"node","required":["title"],"properties":{
	  "title":{"type":"string"},"sections":{"type":"array","items":{"$dynamicRef":"#node"}}}}`)
	if findings := dynamic.Check([]byte(good), false); findings != nil {
		t.Fatalf("a tree held through a dynamic reference: %+v", findings)
	}
	if findings := dynamic.Check([]byte(`{"title":"1","sections":[{"sections":[]}]}`), false); len(findings) != 1 || findings[0].Pointer != "/sections/0" {
		t.Fatalf("a section with no title, through a dynamic reference: %+v", findings)
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

// TestADynamicAnchorNamesOneSubschema: with 2 subschemas under one dynamic
// anchor the object would decide which a $dynamicRef applies, and what the
// schema applies could not be counted from the schema. It is refused. A
// member that is named like the keyword is a member.
func TestADynamicAnchorNamesOneSubschema(t *testing.T) {
	shared := `{"type":"object","properties":{"a":{"$dynamicRef":"#node"}},
	  "$defs":{"x":{"$dynamicAnchor":"node","type":"string"},
	           "y":{"$id":"other.json","$dynamicAnchor":"node","type":"number","examples":[["node"]]}}}`
	_, err := Compile([]byte(shared))
	if fault.CodeOf(err) != fault.InvalidSchema || !strings.Contains(fault.DetailOf(err), `gives the dynamic anchor "node" to 2 subschemas`) {
		t.Fatalf("2 subschemas under one dynamic anchor: %v", err)
	}
	compiled(t, `{"type":"object","properties":{"$dynamicAnchor":{"type":"string"},"b":{"properties":{"$dynamicAnchor":{"type":"string"}}}}}`)
}

// TestASubschemaInAnotherDialectIsRefused: the dialect is checked for every
// schema of the document and not for its root alone, so no keyword of an
// earlier draft is applied uncounted.
func TestASubschemaInAnotherDialectIsRefused(t *testing.T) {
	_, err := Compile([]byte(`{"type":"object","properties":{"a":{"$id":"inner.json","$schema":"http://json-schema.org/draft-07/schema#","type":"string"}}}`))
	if fault.CodeOf(err) != fault.InvalidSchema || !strings.Contains(fault.DetailOf(err), "another dialect") {
		t.Fatalf("a member in draft 7: %v", err)
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
	for name, schema := range map[string]string{
		"2 patterns that take one member": `{"type":"object","patternProperties":{"a":{"$ref":"#"},"^a":{"$ref":"#"}}}`,
		"2 schemas that describe one member": `{"type":"object","$ref":"#/$defs/n",
		  "$defs":{"n":{"allOf":[{"properties":{"a":{"$ref":"#/$defs/n"}}},{"properties":{"a":{"$ref":"#/$defs/n"}}}]}}}`,
		"a member and every member": `{"type":"object","properties":{"a":{"$ref":"#"}},"unevaluatedProperties":{"$ref":"#"}}`,
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
		if !unchecked(findings) || findings[0].Rule != "#" || !strings.Contains(findings[0].Message, "more than 2097152 applications") {
			t.Errorf("%s: an object 40 levels deep: %+v", name, findings)
			continue
		}
		if got := Broken(findings); got != "the object was not held to the schema: that would take more than 2097152 applications of the schema" {
			t.Errorf("%s: the field would say %q", name, got)
		}

		in := Input{Extractor: "text", Windows: []Window{{Text: "[1.1] a", Refs: []string{"1.1"}}}}
		var p Progress
		step, _, got := Take(s, in, &p, answer(costly, nil))
		if step != Unsatisfied || !unchecked(got) || p.Repairs != 0 || p.Summary(in).Attempts != 1 {
			t.Errorf("%s: step %d after %d repairs, %+v", name, step, p.Repairs, got)
		}
	}
}

// applications is the count a check of data would start from.
func applications(t *testing.T, s *Schema, data string) int {
	t.Helper()
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(data)))
	if err != nil {
		t.Fatal(err)
	}
	m := &meter{measured: s.measured, counted: map[node]int{}}
	return m.cost(s.compiled, value)
}

// TestTheCountOfACheckIsWhatTheValidatorAppliesAtMost: the count takes a
// schema for each value it is applied to: for a member its own schema,
// every pattern its name matches, the schema for members nothing else
// describes when nothing else does, the one for members nothing evaluated,
// and the schema of its name; for an item its place in the prefix or the
// schema of the rest, and the one a list must contain. A value that is
// reached again is counted again and looked at once, and a count that has
// looked at MaxCheckWork applications stops.
func TestTheCountOfACheckIsWhatTheValidatorAppliesAtMost(t *testing.T) {
	for name, tc := range map[string]struct {
		schema, data string
		want         int
	}{
		"an object with nothing in it":  {`{"type":"object"}`, `{}`, 1},
		"a member":                      {`{"type":"object","properties":{"a":{"type":"string"}}}`, `{"a":"x","b":1}`, 2},
		"a member through a reference":  {`{"type":"object","properties":{"a":{"$ref":"#/$defs/s"}},"$defs":{"s":{"type":"string"}}}`, `{"a":"x"}`, 3},
		"a pattern that matches":        {`{"type":"object","patternProperties":{"^a":{"type":"string"},"^b":{"type":"string"}}}`, `{"a1":"x","a2":"y","c":1}`, 3},
		"members nothing describes":     {`{"type":"object","properties":{"a":true},"additionalProperties":{"type":"number"}}`, `{"a":1,"b":2,"c":3}`, 4},
		"members nobody may add":        {`{"type":"object","properties":{"a":true},"additionalProperties":false}`, `{"a":1,"b":2}`, 2},
		"members nothing evaluated":     {`{"type":"object","properties":{"a":true},"unevaluatedProperties":{"type":"number"}}`, `{"a":1,"b":2}`, 4},
		"the names of members":          {`{"type":"object","propertyNames":{"anyOf":[{"maxLength":3},{"const":"total"}]}}`, `{"a":1,"total":2}`, 7},
		"a prefix and the rest":         {`{"type":"object","properties":{"l":{"prefixItems":[{"type":"string"}],"items":{"type":"number"}}}}`, `{"l":["x",1,2]}`, 5},
		"a prefix and nothing after":    {`{"type":"object","properties":{"l":{"prefixItems":[{"type":"string"}]}}}`, `{"l":["x",1,2]}`, 3},
		"what a list must contain":      {`{"type":"object","properties":{"l":{"contains":{"type":"number"},"unevaluatedItems":false}}}`, `{"l":[1,2]}`, 6},
		"a condition and both its arms": {`{"type":"object","if":{"required":["a"]},"then":{"required":["b"]},"else":{"not":{"required":["c"]}}}`, `{}`, 5},
		"a dependent schema":            {`{"type":"object","dependentSchemas":{"a":{"required":["b"]}}}`, `{"a":1}`, 2},
		"an object in a list, 2 times":  {`{"type":"object","properties":{"l":{"items":{"$ref":"#/$defs/o"},"contains":{"$ref":"#/$defs/o"}}},"$defs":{"o":{"properties":{"n":{"type":"number"}}}}}`, `{"l":[{"n":1}]}`, 8},
	} {
		if got := applications(t, compiled(t, tc.schema), tc.data); got != tc.want {
			t.Errorf("%s: %s applied to %s counts %d, want %d", name, tc.schema, tc.data, got, tc.want)
		}
	}

	s := compiled(t, `{"type":"object","properties":{"l":{"type":"array","items":{"type":"number"}}}}`)
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(`{"l":[1,2,3]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := &meter{measured: s.measured, counted: map[node]int{}, looked: MaxCheckWork - 2}
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
	if n := applications(t, long, data); n > MaxCheckWork/4 {
		t.Fatalf("a reply of %d bytes with 4,000 lines counts %d of the %d a check may take", len(data), n, MaxCheckWork)
	}
	if findings := long.Check([]byte(data), false); findings != nil {
		t.Fatalf("a long reply that satisfies its schema: %d findings, first %+v", len(findings), findings[0])
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

// TestTheWorkOfACheckCountsWhatAnApplicationLooksUp: an application is not
// one unit of work whatever its schema holds. A pattern costs its steps for
// every byte of the string or the name it is matched against, and the names
// a schema requires and the values it lists are each looked up in the
// value. Both are counted, so a long string held to a pattern of many
// steps, and a long list of objects held to a long list of names, are not
// checked.
func TestTheWorkOfACheckCountsWhatAnApplicationLooksUp(t *testing.T) {
	names := make([]string, 3200)
	for i := range names {
		names[i] = fmt.Sprintf(`"n%d"`, i)
	}
	for name, tc := range map[string]struct {
		schema, data string
		want         int
	}{
		"32 names required":           {`{"type":"object","required":[` + strings.Join(names[:32], ",") + `]}`, `{}`, 3},
		"16 values listed":            {`{"type":"object","properties":{"a":{"enum":[` + strings.Join(names[:16], ",") + `]}}}`, `{"a":"x"}`, 3},
		"16 names a member requires":  {`{"type":"object","dependentRequired":{"a":[` + strings.Join(names[:16], ",") + `]}}`, `{}`, 2},
		"a pattern of 2,003 steps":    {`{"type":"object","properties":{"a":{"pattern":"^[a-z]{1,1000}$"}}}`, `{"a":"` + strings.Repeat("a", 640) + `"}`, 2 + 640*2003/128},
		"a name held to a pattern":    {`{"type":"object","patternProperties":{"^[a-z]{1,1000}$":{"type":"number"}}}`, `{"` + strings.Repeat("a", 640) + `":1}`, 2 + 640*2003/128},
		"a name held to its schema":   {`{"type":"object","propertyNames":{"pattern":"^[a-z]{1,1000}$"}}`, `{"` + strings.Repeat("a", 640) + `":1}`, 2 + 640*2003/128},
		"a pattern through a choice":  {`{"type":"object","properties":{"a":{"anyOf":[{"pattern":"^[a-z]{1,1000}$"},{"pattern":"^[a-z]{1,1000}$"}]}}}`, `{"a":"` + strings.Repeat("a", 640) + `"}`, 4 + 640*(2*2003)/128},
		"a pattern held to no string": {`{"type":"object","properties":{"a":{"pattern":"^[a-z]{1,1000}$"}}}`, `{"a":640}`, 2},
	} {
		if got := applications(t, compiled(t, tc.schema), tc.data); got != tc.want {
			t.Errorf("%s: counts %d, want %d", name, got, tc.want)
		}
	}

	began := time.Now()
	long := strings.Repeat("a", 200000)
	for name, tc := range map[string]struct{ schema, data string }{
		"a long string":         {`{"type":"object","properties":{"a":{"pattern":"^[a-z]{1,1000}$"}}}`, `{"a":"` + long + `"}`},
		"a long name":           {`{"type":"object","patternProperties":{"^[a-z]{1,1000}$":{"type":"number"}}}`, `{"` + long + `":1}`},
		"a long name, by rule":  {`{"type":"object","propertyNames":{"pattern":"^[a-z]{1,1000}$"}}`, `{"` + long + `":1}`},
		"a long list of names":  {`{"type":"object","properties":{"l":{"items":{"required":[` + strings.Join(names, ",") + `]}}}}`, `{"l":[` + strings.TrimSuffix(strings.Repeat(`{},`, 12000), ",") + `]}`},
		"a long list in a list": {`{"type":"object","properties":{"l":{"prefixItems":[{"required":[` + strings.Join(names, ",") + `]}],"items":{"required":[` + strings.Join(names, ",") + `]},"contains":{"type":"object"}}}}`, `{"l":[` + strings.TrimSuffix(strings.Repeat(`{},`, 12000), ",") + `]}`},
	} {
		s := compiled(t, tc.schema)
		if findings := s.Check([]byte(tc.data), false); !unchecked(findings) {
			t.Errorf("%s: %d findings, want the object not to be checked", name, len(findings))
		}
	}
	within(t, began, "not checking 5 objects")
}

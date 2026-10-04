// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"latere.ai/x/lectio/internal/fault"
)

// held holds a schema and an object together as a request and its reply
// would be, and answers what came of it and how long it took: the schema
// refused, the object not checked, or the findings of a check.
func held(schema, data string) (refused error, findings []Finding, took time.Duration) {
	began := time.Now()
	s, err := Compile([]byte(schema))
	if err != nil {
		return err, nil, time.Since(began)
	}
	findings = s.Check([]byte(data), false)
	return nil, findings, time.Since(began)
}

// TestAKeywordTheCountDoesNotModelIsRefused: the keywords of earlier drafts
// that a validator applies in place, and the ones whose target the object
// decides, are not counted, so a schema that uses one is refused by its
// name and never held to anything. A chain of 22 definitions that each
// apply the one before 2 times through dependencies compiled at once and
// took 2.29 seconds to hold {"a":1,"b":1} to, doubling with each link.
func TestAKeywordTheCountDoesNotModelIsRefused(t *testing.T) {
	defs := []string{`"d0":{"type":"object"}`}
	for k := 1; k <= 22; k++ {
		defs = append(defs, fmt.Sprintf(`"d%d":{"dependencies":{"a":{"$ref":"#/$defs/d%d"},"b":{"$ref":"#/$defs/d%d"}}}`, k, k-1, k-1))
	}
	doubling := `{"type":"object","$ref":"#/$defs/d22","$defs":{` + strings.Join(defs, ",") + `}}`
	for name, tc := range map[string]struct{ schema, keyword string }{
		"dependencies that double":  {doubling, "dependencies"},
		"a recursive reference":     {`{"type":"object","properties":{"a":{"$recursiveRef":"#"}}}`, "$recursiveRef"},
		"a recursive anchor":        {`{"type":"object","$recursiveAnchor":true}`, "$recursiveAnchor"},
		"a dynamic reference":       {`{"type":"object","properties":{"a":{"$dynamicRef":"#/$defs/a"}},"$defs":{"a":{"type":"string"}}}`, "$dynamicRef"},
		"a dynamic anchor":          {`{"type":"object","$defs":{"a":{"$dynamicAnchor":"node"}}}`, "$dynamicAnchor"},
		"definitions":               {`{"type":"object","definitions":{"a":{"type":"string"}}}`, "definitions"},
		"a vocabulary":              {`{"type":"object","$vocabulary":{"https://json-schema.org/draft/2020-12/vocab/core":true}}`, "$vocabulary"},
		"an anchor":                 {`{"type":"object","$defs":{"a":{"$anchor":"a"}}}`, "$anchor"},
		"an identifier":             {`{"type":"object","$id":"https://example.com/invoice.json"}`, "$id"},
		"items after a prefix":      {`{"type":"object","properties":{"a":{"additionalItems":false}}}`, "additionalItems"},
		"a schema of content":       {`{"type":"object","properties":{"a":{"contentSchema":{"type":"object"}}}}`, "contentSchema"},
		"members nothing evaluated": {`{"type":"object","unevaluatedProperties":false}`, "unevaluatedProperties"},
		"items nothing evaluated":   {`{"type":"object","properties":{"a":{"unevaluatedItems":false}}}`, "unevaluatedItems"},
		"a keyword nobody knows":    {`{"type":"object","properties":{"a":{"type":"string","x-order":3}}}`, "x-order"},
		"a keyword in a definition": {`{"type":"object","$defs":{"a":{"nullable":true}}}`, "nullable"},
	} {
		refused, _, took := held(tc.schema, `{"a":1,"b":1}`)
		if fault.CodeOf(refused) != fault.InvalidSchema || !strings.Contains(fault.DetailOf(refused), `"`+tc.keyword+`"`) {
			t.Errorf("%s: %v after %s, want invalid_schema naming %q", name, refused, took, tc.keyword)
		}
		if took > time.Second {
			t.Errorf("%s took %s", name, took)
		}
	}
}

// TestANumberCostsNoMoreThanAMachineNumber: a validator that reads a number
// as a rational of any size spends on one number what its digits ask for:
// 4,000,000 digits held to a multiple and a minimum took 19.7 seconds. A
// number of a reply is at most 32 characters and within what a machine
// number holds, or the reply does not satisfy the schema, and such a number
// in a schema is refused.
func TestANumberCostsNoMoreThanAMachineNumber(t *testing.T) {
	schema := `{"type":"object","properties":{"n":{"type":"integer","minimum":0,"multipleOf":2}}}`
	for name, number := range map[string]string{
		"4,000,000 digits":    strings.Repeat("7", 4000000),
		"33 digits":           strings.Repeat("7", 33),
		"an exponent of 400":  "1e400",
		"an exponent of -400": "1e-400",
		"a long exponent":     "1e-99999999999999999999",
	} {
		refused, findings, took := held(schema, `{"n":`+number+`}`)
		if refused != nil || len(findings) != 1 || findings[0].Pointer != "/n" || findings[0].Unchecked || took > time.Second {
			t.Errorf("%s: refused %v, findings %+v, after %s", name, refused, findings, took)
		}
	}
	for name, data := range map[string]string{
		"32 digits":       `{"n":` + strings.Repeat("2", 32) + `}`,
		"a decimal":       `{"n":4.0}`,
		"a large number":  `{"n":1e300}`,
		"a small integer": `{"n":0}`,
	} {
		if refused, findings, took := held(schema, data); refused != nil || findings != nil || took > time.Second {
			t.Errorf("%s: refused %v, findings %+v, after %s", name, refused, findings, took)
		}
	}
	// A multiple is held as a decimal is written, not as its nearest
	// machine number: 0.3 is 3 times 0.1.
	cents := `{"type":"object","properties":{"n":{"type":"number","multipleOf":0.1}}}`
	if refused, findings, _ := held(cents, `{"n":0.3}`); refused != nil || findings != nil {
		t.Errorf("0.3 as a multiple of 0.1: %v, %+v", refused, findings)
	}
	if refused, findings, _ := held(cents, `{"n":0.35}`); refused != nil || len(findings) != 1 {
		t.Errorf("0.35 as a multiple of 0.1: %v, %+v", refused, findings)
	}

	for name, schema := range map[string]string{
		"a bound of 33 digits":       `{"type":"object","properties":{"n":{"maximum":` + strings.Repeat("9", 33) + `}}}`,
		"a multiple out of range":    `{"type":"object","properties":{"n":{"multipleOf":1e-400}}}`,
		"a value listed, too long":   `{"type":"object","properties":{"n":{"enum":[1,` + strings.Repeat("9", 4000) + `]}}}`,
		"an example, too long":       `{"type":"object","examples":[` + strings.Repeat("9", 4000) + `]}`,
		"a count that is no integer": `{"type":"object","properties":{"n":{"type":"string","maxLength":1e999}}}`,
	} {
		refused, _, took := held(schema, `{}`)
		if fault.CodeOf(refused) != fault.InvalidSchema || !strings.Contains(fault.DetailOf(refused), "number") || took > time.Second {
			t.Errorf("%s: %v after %s", name, refused, took)
		}
	}
}

// TestAListOfValuesIsPricedByWhatAComparisonCosts: a value is compared with
// every value its schema lists, and a comparison of 2 numbers builds 2
// rationals. 2,000 numbers listed and 6,000 numbers that are none of them
// took 4.18 seconds and counted as checked. The count now prices each
// comparison, and such a reply is not checked.
func TestAListOfValuesIsPricedByWhatAComparisonCosts(t *testing.T) {
	listed := make([]string, 2000)
	for i := range listed {
		listed[i] = fmt.Sprint(2 * i)
	}
	values := make([]string, 6000)
	for i := range values {
		values[i] = fmt.Sprint(2*i + 1)
	}
	schema := `{"type":"object","properties":{"l":{"type":"array","items":{"enum":[` + strings.Join(listed, ",") + `]}}}}`
	refused, findings, took := held(schema, `{"l":[`+strings.Join(values, ",")+`]}`)
	if refused != nil || !unchecked(findings) || took > time.Second {
		t.Fatalf("2,000 values listed and 6,000 numbers: refused %v, %d findings, after %s", refused, len(findings), took)
	}
	// 60 numbers held to the same list are checked, and each one that is
	// not listed is a finding.
	refused, findings, took = held(schema, `{"l":[`+strings.Join(values[:60], ",")+`,4]}`)
	if refused != nil || len(findings) != 60 || took > time.Second {
		t.Fatalf("60 numbers: refused %v, %d findings, after %s", refused, len(findings), took)
	}
}

// TestWhatAFindingSaysIsBounded: a finding names where a value is and what
// rule it broke in a length that does not grow with the reply or with the
// schema. A check answers 64 findings at most, a place is named in at most
// 512 bytes, the names a message lists are the first 5, and the values a
// schema lists are shown only when they are few and short. A model that
// repairs a reply has the schema beside these.
func TestWhatAFindingSaysIsBounded(t *testing.T) {
	long := strings.Repeat("n", 700)
	s := compiled(t, `{"type":"object","additionalProperties":false,"propertyNames":{"maxLength":800},
	  "required":["r1","r2","r3","r4","r5","r6","r7"],"dependentRequired":{"a":["x","y"]},
	  "properties":{"a":{"enum":[1,2,3,4,5,6]},"b":{"enum":["open",2,true,null]},"c":{"const":{"k":1}},"d":{"pattern":"^[0-9]+$"},
	    "e":{"contains":{"type":"number"},"minContains":2},"f":{"contains":{"type":"number"},"maxContains":1},
	    "g":{"enum":[{"k":1}]},"h":{"enum":["`+long+`"]},"i":{"type":"array","items":false},
	    "j":{"properties":{"`+long+`":{"properties":{"`+long+`":{"type":"number"}}}}}}}`)
	said := map[string]string{}
	for _, f := range s.Check([]byte(`{"a":7,"b":7,"c":7,"d":"x","e":[1],"f":[1,2,3],"g":7,"h":7,"i":[1],
	  "j":{"`+long+`":{"`+long+`":"x"}},"u1":1,"u2":1,"u3":1,"u4":1,"u5":1,"u6":1,"`+strings.Repeat("U", 90)+`":1}`), false) {
		said[f.Rule] = f.Message
		if len(f.Message) > maxMessage+3 || len(f.Pointer) > maxPointer+4 {
			t.Errorf("the finding at %s is %d bytes at a place of %d", f.Rule, len(f.Message), len(f.Pointer))
		}
	}
	for rule, want := range map[string]string{
		"#/properties/a/enum":        "at /a: the value is none of the 6 the schema lists",
		"#/properties/b/enum":        "at /b: value must be one of 'open', 2, true, null",
		"#/properties/c/const":       "at /c: the value is not the one the schema fixes",
		"#/properties/d/pattern":     "at /d: the text does not match the pattern ^[0-9]+$",
		"#/properties/e/minContains": "at /e: 1 items are as the schema asks some to be, and at least 2 must be",
		"#/properties/f/maxContains": "at /f: 3 items are as the schema asks some to be, and at most 1 may be",
		"#/properties/g/enum":        "at /g: the value is none of the 1 the schema lists",
		"#/properties/h/enum":        "at /h: the value is none of the 1 the schema lists",
		"#/properties/i/items":       "at /i/0: false schema",
		"#/required":                 "at the root: missing properties 'r1', 'r2', 'r3', 'r4', 'r5' and 2 more",
		"#/dependentRequired/a":      "at the root: with 'a', missing 'x', 'y'",
	} {
		if said[rule] != want {
			t.Errorf("the finding at %s says %q, want %q", rule, said[rule], want)
		}
	}
	if got := said["#/additionalProperties"]; got != "at the root: members the schema does not take: '"+strings.Repeat("U", 40)+"...', 'u1', 'u2', 'u3', 'u4' and 2 more" {
		t.Errorf("the members nobody may add are said as %q", got)
	}
	if got := said["#/properties/j/properties/"+long+"/properties/"+long+"/type"]; got != "at /j/...: got string, want number" {
		t.Errorf("a value at a long place is said as %q", got)
	}

	// A rule this package has no words of its own for is named.
	if got := say(&kind.InvalidJsonValue{}); got != "the value breaks a rule of the schema" {
		t.Errorf("a rule with no keyword says %q", got)
	}
	if got := say(&kind.AllOf{}); got != "the value breaks the rule allOf" {
		t.Errorf("a rule with a keyword says %q", got)
	}
	// A name its schema does not let be is said by the rule the name broke.
	names := compiled(t, `{"type":"object","propertyNames":{"maxLength":2},"properties":{"l":{"contains":{"type":"number"}}}}`)
	if findings := names.Check([]byte(`{"abc":1}`), false); len(findings) != 1 || findings[0].Rule != "#/propertyNames/maxLength" || findings[0].Message != "at the root: maxLength: got 3, want 2" {
		t.Errorf("a name that is too long: %+v", findings)
	}
	if findings := names.Check([]byte(`{"l":[]}`), false); len(findings) != 1 || findings[0].Message != "at /l: no items match contains schema" {
		t.Errorf("a list that holds nothing: %+v", findings)
	}

	// 100 values that break a schema are 64 findings, and the field says
	// there may be more.
	many := compiled(t, `{"type":"object","properties":{"l":{"items":{"type":"string"}}}}`)
	findings := many.Check([]byte(`{"l":[`+strings.TrimSuffix(strings.Repeat("1,", 100), ",")+`]}`), false)
	if len(findings) != MaxFindings || Broken(findings) != "the object does not satisfy the schema in 64 places or more, at #/properties/l/items/type" {
		t.Fatalf("%d findings, and the field would say %q", len(findings), Broken(findings))
	}

	// A reply nests 64 levels at most, objects and lists alike.
	for name, data := range map[string]string{
		"objects": strings.Repeat(`{"a":`, 65) + `1` + strings.Repeat(`}`, 65),
		"lists":   `{"l":` + strings.Repeat(`[`, 64) + strings.Repeat(`]`, 64) + `}`,
	} {
		findings := many.Check([]byte(data), false)
		if len(findings) != 1 || !strings.Contains(findings[0].Message, "nests deeper than 64 levels") || findings[0].Unchecked {
			t.Errorf("%s nested 65 levels deep: %+v", name, findings)
		}
	}
	if findings := many.Check([]byte(strings.Repeat(`{"a":`, 63)+`{}`+strings.Repeat(`}`, 63)), false); findings != nil {
		t.Errorf("an object nested 64 levels deep: %+v", findings)
	}
	// A schema reads nothing from outside itself, were a reference ever to
	// reach its loader.
	if _, err := (isolated{}).Load("https://example.com/other.json"); err == nil {
		t.Error("the loader of a schema loaded an address")
	}
}

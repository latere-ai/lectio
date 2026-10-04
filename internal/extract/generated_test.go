// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
)

// The prices of cost.go are held to what the validator takes by the cases
// of this file. A family is a schema and an object built to make one
// keyword, or one way of combining keywords, as expensive as it can be for
// its size, and the random cases combine the listed keywords with no plan.
// For every pair the work is counted and the check is timed.

// family builds a schema and an object of a size.
type family struct {
	name  string
	build func(n int) (schema, data string)
}

// join writes n values, each made by its index, as the items of a list.
func join(n int, item func(i int) string) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(item(i))
	}
	return b.String()
}

// listOf is a schema that holds a list under l to a schema for its items,
// with definitions, and an object whose list has the items given.
func listOf(items, defs, values string) (schema, data string) {
	if defs != "" {
		defs = `,"$defs":{` + defs + `}`
	}
	return `{"type":"object","properties":{"l":{"type":"array","items":` + items + `}}` + defs + `}`, `{"l":[` + values + `]}`
}

// odd is the i-th odd number, written.
func odd(i int) string { return strconv.Itoa(2*i + 1) }

// families are the cases built to be expensive.
var families = []family{
	{"a chain of 200 references, on numbers", func(n int) (string, string) {
		defs := join(200, func(i int) string {
			if i == 199 {
				return `"d199":{"type":"number"}`
			}
			return fmt.Sprintf(`"d%d":{"$ref":"#/$defs/d%d"}`, i, i+1)
		})
		return listOf(`{"$ref":"#/$defs/d0"}`, defs, join(n, odd))
	}},
	{"a choice of 250 that all fail", func(n int) (string, string) {
		return listOf(`{"anyOf":[`+join(250, func(int) string { return `{"type":"string"}` })+`]}`, "", join(n, odd))
	}},
	{"250 schemas that all hold", func(n int) (string, string) {
		return listOf(`{"allOf":[`+join(250, func(int) string { return `{"type":"number"}` })+`]}`, "", join(n, odd))
	}},
	{"one of 250 where all match", func(n int) (string, string) {
		return listOf(`{"oneOf":[`+join(250, func(int) string { return `{"type":"number"}` })+`]}`, "", join(n, odd))
	}},
	{"50 negations and conditions", func(n int) (string, string) {
		return listOf(`{"allOf":[`+join(50, func(int) string {
			return `{"not":{"type":"string"},"if":{"type":"number"},"then":{"minimum":0},"else":{"type":"null"}}`
		})+`]}`, "", join(n, odd))
	}},
	{"2,000 numbers listed, none met", func(n int) (string, string) {
		return listOf(`{"enum":[`+join(2000, func(i int) string { return strconv.Itoa(2 * i) })+`]}`, "", join(n, odd))
	}},
	{"1,000 texts listed, none met", func(n int) (string, string) {
		return listOf(`{"enum":[`+join(1000, func(i int) string { return fmt.Sprintf(`"value-%014d"`, i) })+`]}`, "",
			join(n, func(i int) string { return fmt.Sprintf(`"other-%014d"`, i) }))
	}},
	{"40 long texts listed, alike to the end", func(n int) (string, string) {
		long := strings.Repeat("x", 1200)
		return listOf(`{"enum":[`+join(40, func(i int) string { return `"` + long + strconv.Itoa(1000+i) + `"` })+`]}`, "",
			join(n, func(i int) string { return `"` + long + strconv.Itoa(5000+i%1000) + `"` }))
	}},
	{"a fixed object of 300 members", func(n int) (string, string) {
		object := func(last string) string {
			return `{` + join(300, func(i int) string { return fmt.Sprintf(`"m%03d":%d`, i, i) }) + `,"z":` + last + `}`
		}
		return listOf(`{"const":`+object("0")+`}`, "", join(n, func(int) string { return object("1") }))
	}},
	{"3,000 names required, none there", func(n int) (string, string) {
		return listOf(`{"required":[`+join(3000, func(i int) string { return fmt.Sprintf(`"name%04d"`, i) })+`]}`, "",
			join(n, func(int) string { return `{}` }))
	}},
	{"1,500 names a member requires", func(n int) (string, string) {
		return listOf(`{"dependentRequired":{"a":[`+join(1500, func(i int) string { return fmt.Sprintf(`"name%04d"`, i) })+`]}}`, "",
			join(n, func(int) string { return `{"a":1}` }))
	}},
	{"every bound of a number", func(n int) (string, string) {
		return listOf(`{"type":"integer","minimum":-5,"maximum":1e300,"exclusiveMinimum":-6,"exclusiveMaximum":1e301,"multipleOf":0.5}`, "",
			join(n, func(i int) string { return strconv.Itoa(i) + ".25" }))
	}},
	{"long texts held to a length", func(n int) (string, string) {
		return listOf(`{"minLength":1,"maxLength":3}`, "", join(8, func(int) string { return `"` + strings.Repeat("é", n) + `"` }))
	}},
	{"long texts held to a pattern", func(n int) (string, string) {
		return listOf(`{"pattern":"^[a-z]{1,1000}[0-9]{1,1000}$"}`, "", join(4, func(int) string { return `"` + strings.Repeat("a", n) + `"` }))
	}},
	{"short texts held to a pattern", func(n int) (string, string) {
		return listOf(`{"pattern":"^[a-z]+-[0-9]+$"}`, "", join(n, func(i int) string { return `"ab-` + strconv.Itoa(i) + `x"` }))
	}},
	{"8 patterns that take every member", func(n int) (string, string) {
		patterns := join(8, func(i int) string { return fmt.Sprintf(`"^m|%d":{"type":"number"}`, i) })
		return `{"type":"object","patternProperties":{` + patterns + `},"additionalProperties":false}`,
			`{` + join(n, func(i int) string { return fmt.Sprintf(`"m%d":"x"`, i) }) + `}`
	}},
	{"long names held to a pattern", func(n int) (string, string) {
		return `{"type":"object","patternProperties":{"^[a-z]{1,1000}$":{"type":"number"}}}`,
			`{` + join(6, func(i int) string { return `"` + strings.Repeat("a", n) + strconv.Itoa(i) + `":1` }) + `}`
	}},
	{"names held to a schema of their own", func(n int) (string, string) {
		return `{"type":"object","propertyNames":{"maxLength":4,"pattern":"^[a-z]+$","enum":["a","b","c"]}}`,
			`{` + join(n, func(i int) string { return fmt.Sprintf(`"member%d":1`, i) }) + `}`
	}},
	{"members nobody may add", func(n int) (string, string) {
		return `{"type":"object","properties":{"a":true},"additionalProperties":false}`,
			`{` + join(n, func(i int) string { return fmt.Sprintf(`"m%d":1`, i) }) + `}`
	}},
	{"a wide object under 250 schemas", func(n int) (string, string) {
		return `{"type":"object","allOf":[` + join(250, func(int) string { return `{"type":"object","properties":{"zz":true}}` }) + `]}`,
			`{` + join(n, func(i int) string { return fmt.Sprintf(`"m%d":1`, i) }) + `}`
	}},
	{"numbers that must differ", func(n int) (string, string) {
		return `{"type":"object","properties":{"l":{"uniqueItems":true}}}`, `{"l":[` + join(n, odd) + `]}`
	}},
	{"texts that must differ", func(n int) (string, string) {
		return `{"type":"object","properties":{"l":{"uniqueItems":true}}}`,
			`{"l":[` + join(n, func(i int) string { return fmt.Sprintf(`"%s%d"`, strings.Repeat("y", 100), i) }) + `]}`
	}},
	{"objects that must differ", func(n int) (string, string) {
		return `{"type":"object","properties":{"l":{"uniqueItems":true}}}`,
			`{"l":[` + join(n, func(i int) string { return fmt.Sprintf(`{"a":[1,2,3],"b":"text","c":%d}`, i) }) + `]}`
	}},
	{"20 long lists that must differ", func(n int) (string, string) {
		return `{"type":"object","properties":{"l":{"uniqueItems":true}}}`,
			`{"l":[` + join(20, func(i int) string {
				return `[` + join(n, func(j int) string { return strconv.Itoa(j) }) + `,` + strconv.Itoa(-i-1) + `]`
			}) + `]}`
	}},
	{"20 long texts that must differ", func(n int) (string, string) {
		return `{"type":"object","properties":{"l":{"uniqueItems":true}}}`,
			`{"l":[` + join(20, func(i int) string { return `"` + strings.Repeat("z", n) + strconv.Itoa(i) + `"` }) + `]}`
	}},
	{"many short lists that must differ", func(n int) (string, string) {
		return listOf(`{"uniqueItems":true}`, "", join(n, func(i int) string {
			return `[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,` + strconv.Itoa(20+i) + `]`
		}))
	}},
	{"a list that must contain what it does not", func(n int) (string, string) {
		return `{"type":"object","properties":{"l":{"contains":{"type":"string"},"minContains":3,"maxContains":4}}}`, `{"l":[` + join(n, odd) + `]}`
	}},
	{"a list that contains too many", func(n int) (string, string) {
		return `{"type":"object","properties":{"l":{"contains":{"type":"number"},"maxContains":2}}}`, `{"l":[` + join(n, odd) + `]}`
	}},
	{"a prefix of 300 items", func(n int) (string, string) {
		return listOf(`{"prefixItems":[`+join(300, func(int) string { return `{"type":"string"}` })+`],"items":{"type":"null"}}`, "",
			join(n, func(int) string { return `[` + join(310, func(j int) string { return strconv.Itoa(j) }) + `]` }))
	}},
	{"objects 60 levels deep", func(n int) (string, string) {
		deep := strings.Repeat(`{"a":`, 60) + `1` + strings.Repeat(`}`, 60)
		return listOf(`{"$ref":"#/$defs/n"}`, `"n":{"type":"object","properties":{"a":{"$ref":"#/$defs/n"}}}`, join(n, func(int) string { return deep }))
	}},
	{"2 patterns that take one member, nested", func(n int) (string, string) {
		return `{"type":"object","patternProperties":{"a":{"$ref":"#"},"^a":{"$ref":"#"}}}`,
			strings.Repeat(`{"a":`, n) + `{}` + strings.Repeat(`}`, n)
	}},
	{"schemas that depend on a member", func(n int) (string, string) {
		deps := join(100, func(i int) string {
			return fmt.Sprintf(`"k%d":{"required":["zz"],"properties":{"k0":{"type":"string"}}}`, i)
		})
		object := `{` + join(100, func(i int) string { return fmt.Sprintf(`"k%d":1`, i) }) + `}`
		return listOf(`{"dependentSchemas":{`+deps+`}}`, "", join(n, func(int) string { return object }))
	}},
	{"objects of many members and a schema for each", func(n int) (string, string) {
		props := join(200, func(i int) string { return fmt.Sprintf(`"p%d":{"type":"string","minLength":1}`, i) })
		object := `{` + join(200, func(i int) string { return fmt.Sprintf(`"p%d":%d`, i, i) }) + `}`
		return listOf(`{"properties":{`+props+`},"required":["p0","p1","q"]}`, "", join(n, func(int) string { return object }))
	}},
}

// counted builds a family at a size and counts the work of its check.
func counted(t testing.TB, f family, n int) (s *Schema, data string, work int) {
	t.Helper()
	schema, data := f.build(n)
	s, err := Compile([]byte(schema))
	if err != nil {
		t.Fatalf("%s: %v", f.name, err)
	}
	value, flaws, err := decode([]byte(data), MaxValueDepth)
	if err != nil || len(flaws) > 0 {
		t.Fatalf("%s of %d: %v, %v", f.name, n, flaws, err)
	}
	return s, data, s.work(value)
}

// sized returns a family at the size whose check counts between half a
// target of work and the target, or the nearest size below it.
func sized(t testing.TB, f family, target int) (s *Schema, data string, work int) {
	t.Helper()
	n := 4
	for range 16 {
		s, data, work = counted(t, f, n)
		switch {
		case work > target && n > 1:
			n = max(1, min(n-1, n*target/work))
		case work < target/2:
			// Most families grow with their size, and one doubles with it.
			n = max(n+1, min(2*n, n*(target/max(work, 1))*3/4))
		default:
			return s, data, work
		}
	}
	for work > target && n > 1 {
		n--
		s, data, work = counted(t, f, n)
	}
	return s, data, work
}

// perUnit is what a test allows the validator to take for each unit of
// counted work: 25 times the 100 nanoseconds the slowest family took where
// the prices were set, since a machine that runs the suite may be several
// times slower, and more under the race detector.
const perUnit = 2500 * time.Nanosecond * slowdown

// allowed is how long a check that counts work may take in a test.
func allowed(work int) time.Duration {
	return 50*time.Millisecond*slowdown + time.Duration(min(work, MaxCheckWork))*perUnit
}

// TestCountedWorkBoundsTheValidatorsTime: for each family of expensive
// cases, an object sized to count a sixteenth of what a check may take is
// checked within the time that much work is allowed, so the count is an
// upper bound of what the validator does. An object sized past what a check
// may take is not held to the schema, and is answered without validating.
// Under the race detector the objects are smaller by what it slows a check
// down by, and what is held is the same: time for each unit of work.
func TestCountedWorkBoundsTheValidatorsTime(t *testing.T) {
	target := MaxCheckWork / 16 / slowdown
	for _, f := range families {
		s, data, work := sized(t, f, target)
		// The smallest object of a family may count more than the target.
		if work > MaxCheckWork || work == 0 {
			t.Errorf("%s: the smallest object counts %d of a target of %d", f.name, work, target)
			continue
		}
		began := time.Now()
		findings := s.Check([]byte(data), false)
		took := time.Since(began)
		if unchecked(findings) || took > allowed(work) {
			t.Errorf("%s: %d units in %d bytes took %s, allowed %s, unchecked %t", f.name, work, len(data), took, allowed(work), unchecked(findings))
		}

		// The same family, sized past the bound, when that is an object a
		// test can afford to build.
		schema, larger := f.build(1)
		if size := len(data) * (MaxCheckWork/max(work, 1) + 2); size > (2<<20)/slowdown {
			continue
		}
		n := 2
		for range 24 {
			if _, larger, work = counted(t, f, n); work > MaxCheckWork {
				break
			}
			n = max(n+1, min(2*n, n*(MaxCheckWork/max(work, 1)+1)))
		}
		if work <= MaxCheckWork {
			t.Errorf("%s: no object of up to %d counts past the bound", f.name, n)
			continue
		}
		schema, _ = f.build(n)
		began = time.Now()
		refused, findings, took := held(schema, larger)
		if refused != nil || !unchecked(findings) || took > 400*time.Millisecond*slowdown {
			t.Errorf("%s: an object of %d bytes past the bound: refused %v, %d findings, after %s", f.name, len(larger), refused, len(findings), time.Since(began))
		}
	}
}

// The words a random schema and a random object are made of: few, so that
// an object often holds what its schema names.
var (
	someNames    = []string{"a", "b", "c", "ab", "total", "items"}
	someTypes    = []string{"object", "array", "string", "number", "integer", "boolean", "null"}
	somePatterns = []string{"a", "^a", "^[a-c]+$", ".*", "b$", "^(ab)*$", "[0-9]{1,40}", "^.{0,200}$"}
	someScalars  = []string{`"a"`, `"ab"`, `"total"`, `0`, `1`, `2.5`, `-3`, `1e3`, `true`, `false`, `null`, `""`, `"abababab"`, `"0123456789"`}
)

// generator makes schemas and objects from a seed.
type generator struct {
	r    *rand.Rand
	defs int
}

func (g *generator) pick(words []string) string { return words[g.r.IntN(len(words))] }

func (g *generator) scalar() string { return g.pick(someScalars) }

// value is a JSON value, with objects and lists down to a depth.
func (g *generator) value(depth int) string {
	switch k := g.r.IntN(10); {
	case depth > 0 && k < 3:
		return `{` + join(g.r.IntN(6), func(i int) string { return `"` + someNames[i] + `":` + g.value(depth-1) }) + `}`
	case depth > 0 && k < 5:
		return `[` + join(g.r.IntN(8), func(int) string { return g.value(depth - 1) }) + `]`
	}
	return g.scalar()
}

// schema is a schema of the listed keywords, with schemas inside it down to
// a depth. Some of what it writes is no schema, which is a case too.
func (g *generator) schema(depth int) string {
	if depth == 0 || g.r.IntN(12) == 0 {
		switch g.r.IntN(6) {
		case 0:
			return `true`
		case 1:
			return `false`
		case 2:
			return fmt.Sprintf(`{"$ref":"#/$defs/d%d"}`, g.r.IntN(g.defs))
		}
		return `{"type":"` + g.pick(someTypes) + `"}`
	}
	sub := func() string { return g.schema(depth - 1) }
	several := func() string { return `[` + join(1+g.r.IntN(3), func(int) string { return sub() }) + `]` }
	named := func(names []string) string {
		return `{` + join(1+g.r.IntN(3), func(i int) string {
			return `"` + names[(i*2+g.r.IntN(2))%len(names)] + `x` + strconv.Itoa(i) + `":` + sub()
		}) + `}`
	}
	members := func() string {
		return `{` + join(1+g.r.IntN(4), func(i int) string { return `"` + someNames[i] + `":` + sub() }) + `}`
	}
	choices := []func() string{
		func() string { return `"type":"` + g.pick(someTypes) + `"` },
		func() string { return `"type":["` + g.pick(someTypes) + `","null"]` },
		func() string { return `"enum":[` + join(1+g.r.IntN(5), func(int) string { return g.value(1) }) + `]` },
		func() string { return `"const":` + g.value(2) },
		func() string { return `"minimum":` + strconv.Itoa(g.r.IntN(5)-2) },
		func() string { return `"exclusiveMaximum":` + strconv.Itoa(g.r.IntN(2000)) },
		func() string { return `"multipleOf":` + g.pick([]string{"1", "2", "0.5", "0.1"}) },
		func() string { return `"minLength":` + strconv.Itoa(g.r.IntN(4)) },
		func() string { return `"maxLength":` + strconv.Itoa(g.r.IntN(12)) },
		func() string { return `"pattern":"` + g.pick(somePatterns) + `"` },
		func() string { return `"minItems":` + strconv.Itoa(g.r.IntN(3)) },
		func() string { return `"maxItems":` + strconv.Itoa(g.r.IntN(9)) },
		func() string { return `"uniqueItems":true` },
		func() string { return `"minProperties":` + strconv.Itoa(g.r.IntN(3)) },
		func() string { return `"maxProperties":` + strconv.Itoa(g.r.IntN(7)) },
		func() string { return `"required":["` + g.pick(someNames) + `"]` },
		func() string { return `"dependentRequired":{"a":["b","` + g.pick(someNames) + `"]}` },
		func() string { return `"properties":` + members() },
		func() string {
			return `"patternProperties":{` + join(1+g.r.IntN(3), func(i int) string { return `"` + somePatterns[(i+g.r.IntN(3))%4] + `":` + sub() }) + `}`
		},
		func() string { return `"additionalProperties":` + sub() },
		func() string { return `"propertyNames":` + sub() },
		func() string { return `"items":` + sub() },
		func() string { return `"prefixItems":` + several() },
		func() string { return `"contains":` + sub() + `,"minContains":` + strconv.Itoa(g.r.IntN(3)) },
		func() string { return `"allOf":` + several() },
		func() string { return `"anyOf":` + several() },
		func() string { return `"oneOf":` + several() },
		func() string { return `"not":` + sub() },
		func() string { return `"if":` + sub() + `,"then":` + sub() + `,"else":` + sub() },
		func() string { return `"dependentSchemas":` + named(someNames) },
		func() string { return `"$ref":"#/$defs/d` + strconv.Itoa(g.r.IntN(g.defs)) + `"` },
		func() string { return `"description":"a note","examples":[` + g.value(1) + `],"default":` + g.value(1) },
	}
	used := map[int]bool{}
	var parts []string
	for range 1 + g.r.IntN(5) {
		if k := g.r.IntN(len(choices)); !used[k] {
			used[k] = true
			parts = append(parts, choices[k]())
		}
	}
	return `{` + strings.Join(parts, ",") + `}`
}

// document is a whole schema: an object with members and definitions that
// refer to each other and to it.
func (g *generator) document() string {
	g.defs = 1 + g.r.IntN(4)
	defs := join(g.defs, func(i int) string { return fmt.Sprintf(`"d%d":%s`, i, g.schema(2)) })
	body := strings.TrimSuffix(strings.TrimPrefix(g.schema(3), `{`), `}`)
	if body == "true" || body == "false" || strings.Contains(body, `"type":`) {
		body = `"properties":{"a":` + g.schema(2) + `,"items":` + g.schema(3) + `}`
	}
	return `{"type":"object",` + body + `,"$defs":{` + defs + `}}`
}

// TestGeneratedSchemasAreHeldWithinTheirCount: 300 schemas written from the
// listed keywords with no plan, each held to 4 objects made of the same few
// words. A schema is taken or refused with invalid_schema, never anything
// else, and each object is checked within the time its counted work is
// allowed, or not checked when it counts past the bound. The cases are the
// same on every run.
func TestGeneratedSchemasAreHeldWithinTheirCount(t *testing.T) {
	g := &generator{r: rand.New(rand.NewPCG(2026, 11))}
	taken, refusals, findingsSeen, clean := 0, 0, 0, 0
	for i := range 300 {
		schema := g.document()
		began := time.Now()
		s, err := Compile([]byte(schema))
		if took := time.Since(began); took > 200*time.Millisecond*slowdown {
			t.Errorf("case %d: reading the schema took %s: %s", i, took, schema)
		}
		if err != nil {
			if fault.CodeOf(err) != fault.InvalidSchema {
				t.Errorf("case %d: %v for %s", i, err, schema)
			}
			refusals++
			continue
		}
		taken++
		for range 4 {
			data := `{` + join(g.r.IntN(7), func(i int) string { return `"` + someNames[i] + `":` + g.value(4) }) + `}`
			value, flaws, err := decode([]byte(data), MaxValueDepth)
			if err != nil || len(flaws) > 0 {
				t.Fatalf("case %d: the object %s: %v, %v", i, data, flaws, err)
			}
			work := s.work(value)
			began := time.Now()
			findings := s.Check([]byte(data), false)
			took := time.Since(began)
			if unchecked(findings) != (work > MaxCheckWork) || took > allowed(work) {
				t.Errorf("case %d: %d units took %s, allowed %s, unchecked %t: %s held to %s", i, work, took, allowed(work), unchecked(findings), data, schema)
			}
			if len(findings) > 0 {
				findingsSeen++
			} else {
				clean++
			}
		}
	}
	// The generator is of use only while it makes schemas that are taken
	// and objects on both sides of them.
	if taken < 100 || refusals == 0 || findingsSeen < 100 || clean < 100 {
		t.Fatalf("of 300 schemas %d were taken and %d refused, and of their objects %d broke the schema and %d satisfied it", taken, refusals, findingsSeen, clean)
	}
}

// TestCalibration prints what the validator takes for each unit of counted
// work, family by family, when LECTIO_CALIBRATE is set. It is how the
// prices of cost.go were set, and it asserts nothing.
func TestCalibration(t *testing.T) {
	if os.Getenv("LECTIO_CALIBRATE") == "" {
		t.Skip("set LECTIO_CALIBRATE to print the validator's time for each unit of work")
	}
	target := MaxCheckWork / 4
	type row struct {
		name  string
		work  int
		took  time.Duration
		bytes int
	}
	var rows []row
	for _, f := range families {
		s, data, work := sized(t, f, target)
		value, _, err := decode([]byte(data), MaxValueDepth)
		if err != nil {
			t.Fatal(err)
		}
		best := time.Duration(1 << 62)
		for range 3 {
			began := time.Now()
			err := s.compiled.Validate(value)
			_ = err
			best = min(best, time.Since(began))
		}
		rows = append(rows, row{f.name, work, best, len(data)})
	}
	sort.Slice(rows, func(i, j int) bool {
		return float64(rows[i].took)/float64(rows[i].work) > float64(rows[j].took)/float64(rows[j].work)
	})
	for _, r := range rows {
		t.Logf("%7.0f ns/unit  %8d units  %10s  %9d bytes  %s", float64(r.took)/float64(r.work), r.work, r.took.Round(time.Microsecond), r.bytes, r.name)
	}
}

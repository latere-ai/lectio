// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"latere.ai/x/lectio/internal/fault"
)

// What holding an object to a schema may cost.
//
// A validator applies a schema to a value by applying to the same value
// every schema it refers to and combines, and to each member and item of
// the value the schema that describes it. It keeps nothing of what it has
// applied, so a schema that reaches one subschema by 2 ways applies it 2
// times, and a chain of such schemas doubles the work at every link. That
// work has no bound in the size of the schema, and a validation that began
// cannot be stopped. So the work is bounded before it begins, in 2 places.
//
// A schema is bounded when it arrives: no subschema may apply more than
// MaxApplied schemas to one value, none may reach itself with no value in
// between, and its patterns may compile to no more than MaxPatternSize
// steps together. That refuses a schema whose cost is in the schema alone.
//
// A check is bounded when a reply arrives: the work the validator would do
// to hold this object to this schema is counted first, and an object that
// would take more than MaxCheckWork is not held to the schema. That covers
// a schema whose cost grows with how deep the object nests, which no
// reading of the schema alone can bound.
const (
	// MaxApplied is how many schemas a schema may apply to one value: itself
	// and every schema it reaches through $ref, $dynamicRef, allOf, anyOf,
	// oneOf, not, if, then, else and dependentSchemas, each counted as often
	// as it is reached. A choice between 64 definitions applies 129, itself
	// and for each a reference and what it refers to, so 256 is above what a
	// schema written to describe a document needs.
	MaxApplied = 256

	// MaxPatternSize is how many steps the patterns of a schema may compile
	// to together, each pattern counted once. A pattern is matched by an
	// automaton that never backtracks, in time that is the length of the
	// text times the steps the pattern compiled to, and a repetition with a
	// count compiles to as many copies of what it repeats: 64 times
	// ".{0,1000}" is 579 bytes, compiled to 128,005 steps in 49 MiB, and
	// took 12.9 seconds to match 30,000 bytes once where it was measured.
	// A class repeated 1,000 times is 2,000 steps, so 16,384 leaves room
	// for the patterns of a schema that describes a document.
	MaxPatternSize = 16384

	// MaxCheckWork is how much work one check of an object may take, counted
	// in applications of a schema to a value: 2,097,152. The validator made
	// that many in 0.3 seconds where it was measured, and a reply of 4,000
	// objects held to a choice between 8 shapes each takes less than a
	// quarter of it.
	MaxCheckWork = 2 << 20
)

// What an application costs beyond itself, in the unit of MaxCheckWork.
const (
	// operands is how many names a schema requires, or values it lists, cost
	// what 1 application costs: each is looked up in the value.
	operands = 16
	// steps is how many steps of a pattern's automaton cost what 1
	// application costs.
	steps = 128
)

// dialect is draft 2020-12 as the validator numbers the dialect a schema
// was compiled in.
const dialect = 2020

// pattern is a compiled pattern and the steps it compiled to.
type pattern struct {
	*regexp.Regexp
	size int
}

// patterns compiles the patterns of one schema, each once however often the
// schema uses it, and refuses the one that takes the schema's patterns past
// MaxPatternSize before it is compiled: the size is read from the pattern's
// parse, in which a repetition is one node, and the compiled form is what
// takes the memory.
func patterns() jsonschema.RegexpEngine {
	compiled, total := map[string]pattern{}, 0
	return func(source string) (jsonschema.Regexp, error) {
		if p, ok := compiled[source]; ok {
			return p, nil
		}
		parsed, err := syntax.Parse(source, syntax.Perl)
		if err != nil {
			return nil, err
		}
		size := weigh(parsed)
		if total = min(total+size, MaxPatternSize+1); total > MaxPatternSize {
			return nil, fmt.Errorf("the patterns of the schema compile to more than %d steps", MaxPatternSize)
		}
		re, err := regexp.Compile(source)
		if err != nil {
			return nil, err
		}
		compiled[source] = pattern{Regexp: re, size: size}
		return compiled[source], nil
	}
}

// weigh is how many steps a parsed pattern compiles to, to within a small
// factor and never above MaxPatternSize + 1: a repetition with a count is
// as many copies of what it repeats.
func weigh(re *syntax.Regexp) int {
	n := 1
	switch re.Op {
	case syntax.OpLiteral:
		n = max(1, len(re.Rune))
	case syntax.OpRepeat:
		times := re.Max
		if times < 0 {
			// An open repetition is its least count and a loop.
			times = re.Min + 1
		}
		return min((weigh(re.Sub[0])+1)*max(times, 1), MaxPatternSize+1)
	}
	for _, sub := range re.Sub {
		n = min(n+weigh(sub), MaxPatternSize+1)
	}
	return n
}

// sizeOf is the steps a schema's own pattern compiled to, 0 for a schema
// with none.
func sizeOf(re jsonschema.Regexp) int {
	p, _ := re.(pattern)
	return p.size
}

// inPlace are the schemas a schema applies to the value it is itself
// applied to: the one it refers to, and the ones it combines, negates,
// chooses between and makes depend on a member.
func inPlace(s *jsonschema.Schema) []*jsonschema.Schema {
	var out []*jsonschema.Schema
	if s.Ref != nil {
		out = append(out, s.Ref)
	}
	if s.DynamicRef != nil {
		out = append(out, s.DynamicRef.Ref)
	}
	out = append(out, s.AllOf...)
	out = append(out, s.AnyOf...)
	out = append(out, s.OneOf...)
	for _, t := range []*jsonschema.Schema{s.Not, s.If, s.Then, s.Else} {
		if t != nil {
			out = append(out, t)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.DependentSchemas)) {
		out = append(out, s.DependentSchemas[name])
	}
	return out
}

// below are the schemas a schema applies to the members, the member names
// and the items of the value it is applied to.
func below(s *jsonschema.Schema) []*jsonschema.Schema {
	var out []*jsonschema.Schema
	for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
		out = append(out, s.Properties[name])
	}
	for _, re := range slices.SortedFunc(maps.Keys(s.PatternProperties), func(a, b jsonschema.Regexp) int {
		return strings.Compare(a.String(), b.String())
	}) {
		out = append(out, s.PatternProperties[re])
	}
	if t, ok := s.AdditionalProperties.(*jsonschema.Schema); ok {
		out = append(out, t)
	}
	out = append(out, s.PrefixItems...)
	for _, t := range []*jsonschema.Schema{s.PropertyNames, s.UnevaluatedProperties, s.Contains, s.Items2020, s.UnevaluatedItems} {
		if t != nil {
			out = append(out, t)
		}
	}
	return out
}

// at is where in the caller's schema a subschema is, as a JSON pointer
// behind a "#".
func at(s *jsonschema.Schema) string {
	_, pointer, _ := strings.Cut(s.Location, "#")
	return "#" + pointer
}

// own is what applying a schema to a value costs by itself: 1, and what the
// names it requires and the values it lists take to look up.
func own(s *jsonschema.Schema) int {
	listed := len(s.Required)
	if s.Enum != nil {
		listed += len(s.Enum.Values)
	}
	for _, names := range s.DependentRequired {
		listed += len(names)
	}
	return 1 + listed/operands
}

// measured is what each schema of a compiled schema costs to apply to one
// value that holds nothing: a scalar.
type measured struct {
	// applied is how many schemas a schema applies to one value, never
	// above MaxApplied + 1: a count past the bound is not counted further.
	applied map[*jsonschema.Schema]int
	// work is what applying a schema to a scalar costs, with every schema
	// it applies in place, in the unit of MaxCheckWork.
	work map[*jsonschema.Schema]int
	// text is the steps the patterns of a schema, and of every schema it
	// applies in place, take for each byte of a string it is applied to.
	text map[*jsonschema.Schema]int
}

// gauge takes the measures of a compiled schema.
type gauge struct {
	measured
	// open are the schemas whose count is being taken. One that is reached
	// again from itself applies itself to the value it is applied to.
	open map[*jsonschema.Schema]bool
	seen map[*jsonschema.Schema]bool
}

// measure checks every schema a compiled schema can apply, and answers what
// each costs to apply to one value. A schema in another dialect than draft
// 2020-12, one that reaches itself with no value in between, and one that
// applies more than MaxApplied schemas to one value are refused with
// invalid_schema.
func measure(root *jsonschema.Schema) (measured, error) {
	g := &gauge{
		applied: map[*jsonschema.Schema]int{},
		work:    map[*jsonschema.Schema]int{},
		text:    map[*jsonschema.Schema]int{},
		open:    map[*jsonschema.Schema]bool{},
		seen:    map[*jsonschema.Schema]bool{},
	}
	if err := g.reach(root); err != nil {
		return measured{}, err
	}
	return g.measured, nil
}

// reach checks a schema and every schema it applies, to its own value and
// to the values inside it.
func (g *gauge) reach(s *jsonschema.Schema) error {
	if g.seen[s] {
		return nil
	}
	g.seen[s] = true
	if s.DraftVersion != dialect {
		return fault.New(fault.InvalidSchema, "the schema names another dialect than %s at %s", draft, at(s))
	}
	n, err := g.count(s)
	if err != nil {
		return err
	}
	if n > MaxApplied {
		return fault.New(fault.InvalidSchema,
			"the schema applies more than %d subschemas to one value at %s, and a schema applies at most %d", MaxApplied, at(s), MaxApplied)
	}
	for _, t := range slices.Concat(inPlace(s), below(s)) {
		if err := g.reach(t); err != nil {
			return err
		}
	}
	return nil
}

// count is how many schemas a schema applies to one value: itself, and what
// each schema it applies in place applies. Each count is taken once, and
// what the schema costs to apply is summed with it. A count past MaxApplied
// is refused by reach, so the sums of a schema that was taken are exact.
func (g *gauge) count(s *jsonschema.Schema) (int, error) {
	if n, ok := g.applied[s]; ok {
		return n, nil
	}
	if g.open[s] {
		return 0, fault.New(fault.InvalidSchema, "the schema applies %s to the value it is itself applied to, without end", at(s))
	}
	g.open[s] = true
	n, work, text := 1, own(s), sizeOf(s.Pattern)
	for _, t := range inPlace(s) {
		m, err := g.count(t)
		if err != nil {
			return 0, err
		}
		n = min(n+m, MaxApplied+1)
		work = min(work+g.work[t], MaxCheckWork+1)
		text = min(text+g.text[t], MaxCheckWork+1)
	}
	delete(g.open, s)
	g.applied[s], g.work[s], g.text[s] = n, work, text
	return n, nil
}

// anchors counts, for each name, the places of a schema document that give
// it as a dynamic anchor.
func anchors(doc any, counts map[string]int) {
	switch v := doc.(type) {
	case map[string]any:
		if name, ok := v["$dynamicAnchor"].(string); ok {
			counts[name]++
		}
		for _, member := range v {
			anchors(member, counts)
		}
	case []any:
		for _, member := range v {
			anchors(member, counts)
		}
	}
}

// node names one application the validator makes at an object or an array:
// a schema, and the value by where it is in memory. A scalar needs no name:
// what a schema costs to apply to it is known from the schema.
type node struct {
	schema *jsonschema.Schema
	value  uintptr
}

// meter counts the work a check of one object would take.
type meter struct {
	measured
	// counted is what each application counted so far comes to, with what
	// it makes inside its value. The validator makes it again each time it
	// reaches it, and the count is taken once.
	counted map[node]int
	// looked is how many applications were looked at, each a part of the
	// count: more of them than MaxCheckWork is a count above it.
	looked int
}

// costly reports whether holding a value to the schema would take more work
// than MaxCheckWork. The count is what the validator does at most: every
// schema of a choice and both arms of a condition are counted, and a schema
// for the members nothing else describes is counted for every member.
func (s *Schema) costly(value any) bool {
	m := &meter{measured: s.measured, counted: map[node]int{}}
	return m.cost(s.compiled, value) > MaxCheckWork
}

// matching is what matching patterns of size steps against a text costs.
func matching(text string, size int) int {
	return min(len(text)*size/steps, MaxCheckWork+1)
}

// cost is what applying a schema to a value comes to, never above
// MaxCheckWork + 1. A count that has passed the bound is not taken further.
func (m *meter) cost(s *jsonschema.Schema, value any) int {
	if m.looked++; m.looked > MaxCheckWork {
		return MaxCheckWork + 1
	}
	var where node
	switch v := value.(type) {
	case map[string]any:
		where = node{s, reflect.ValueOf(v).Pointer()}
	case []any:
		where = node{s, reflect.ValueOf(v).Pointer()}
	case string:
		return min(m.work[s]+matching(v, m.text[s]), MaxCheckWork+1)
	default:
		return m.work[s]
	}
	if n, ok := m.counted[where]; ok {
		return n
	}
	n := own(s)
	// add counts an application, and reports whether the count is still
	// within the bound.
	add := func(t *jsonschema.Schema, v any) bool {
		n = min(n+m.cost(t, v), MaxCheckWork+1)
		return n <= MaxCheckWork
	}
	for _, t := range inPlace(s) {
		if !add(t, value) {
			return n
		}
	}
	switch v := value.(type) {
	case map[string]any:
		for name, member := range v {
			described := false
			if t, ok := s.Properties[name]; ok {
				described = true
				if !add(t, member) {
					return n
				}
			}
			for re, t := range s.PatternProperties {
				// The match is counted before it is made.
				if n = min(n+matching(name, sizeOf(re)), MaxCheckWork+1); n > MaxCheckWork {
					return n
				}
				if re.MatchString(name) {
					described = true
					if !add(t, member) {
						return n
					}
				}
			}
			if t, ok := s.AdditionalProperties.(*jsonschema.Schema); ok && !described && !add(t, member) {
				return n
			}
			if s.UnevaluatedProperties != nil && !add(s.UnevaluatedProperties, member) {
				return n
			}
			if s.PropertyNames != nil && !add(s.PropertyNames, name) {
				return n
			}
		}
	case []any:
		for i, item := range v {
			if i < len(s.PrefixItems) {
				if !add(s.PrefixItems[i], item) {
					return n
				}
			} else if s.Items2020 != nil && !add(s.Items2020, item) {
				return n
			}
			for _, t := range []*jsonschema.Schema{s.Contains, s.UnevaluatedItems} {
				if t != nil && !add(t, item) {
					return n
				}
			}
		}
	}
	m.counted[where] = n
	return n
}

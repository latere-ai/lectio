// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"math/big"
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
// A schema is bounded when it arrives: it uses the listed keywords alone
// (keywords.go), no subschema may apply more than MaxApplied schemas to one
// value, none may reach itself with no value in between, and its patterns
// may compile to no more than MaxPatternSize steps together. That refuses a
// schema whose cost is in the schema alone.
//
// A check is bounded when a reply arrives: the work the validator would do
// to hold this object to this schema is counted first, and an object that
// would take more than MaxCheckWork is not held to the schema. That covers
// a schema whose cost grows with the object, which no reading of the schema
// alone can bound. Every listed keyword has a price below, and the prices
// are held above what the validator was measured to take by a test that
// generates schemas and objects (generated_test.go).
const (
	// MaxApplied is how many schemas a schema may apply to one value: itself
	// and every schema it reaches through $ref, allOf, anyOf, oneOf, not,
	// if, then, else and dependentSchemas, each counted as often as it is
	// reached. A choice between 64 definitions applies 129, itself and for
	// each a reference and what it refers to, so 256 is above what a schema
	// written to describe a document needs.
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

	// MaxCheckWork is how much work one check of an object may take:
	// 1,048,576 units, where applying a schema that asks nothing to a value
	// is 2. The costliest of the cases the prices were set by took the
	// validator 100 nanoseconds a unit where it was measured, which is 0.1
	// seconds for all of it, and a reply of 4,000 objects held to a choice
	// between 8 shapes each takes less than three quarters of it.
	MaxCheckWork = 1 << 20
)

// What the validator does for each keyword, in the unit of MaxCheckWork.
const (
	// applying is one application of a schema to a value by itself: a
	// scope, the path to the value, and an error when the value fails.
	applying = 2
	// comparing is a number held against a number: the validator builds a
	// rational for each side, for a listed value, a bound, a multiple, and
	// for the question whether a number is an integer.
	comparing = 4
	// bytesPerUnit is how many bytes of text are compared, counted or
	// hashed for 1.
	bytesPerUnit = 32
	// steps is how many steps of a pattern's automaton cost 1.
	steps = 64
	// lookups is how many names are looked up in an object for 1.
	lookups = 2
	// members is how many members of an object an application passes over
	// for 1, whatever its schema asks of them.
	members = 2
	// fewItems is the length up to which the validator compares the items
	// of a list that must be unique each with each. A longer list is
	// hashed.
	fewItems = 20
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
	for _, t := range []*jsonschema.Schema{s.PropertyNames, s.Contains, s.Items2020} {
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

// plus adds 2 amounts of work and stops at MaxCheckWork + 1: an amount
// past the bound is not counted further.
func plus(a, b int) int { return min(a+b, MaxCheckWork+1) }

// weight is what a value costs to compare with another or to hash: a
// number is 2 rationals, a text its bytes, and an object or a list what it
// holds.
func weight(v any) int {
	switch v := v.(type) {
	case float64:
		return comparing
	case string:
		return 1 + len(v)/bytesPerUnit
	case []any:
		n := 1
		for _, item := range v {
			n = plus(n, weight(item))
		}
		return n
	case map[string]any:
		n := 1
		for name, member := range v {
			n = plus(n, plus(1+len(name)/bytesPerUnit, weight(member)))
		}
		return n
	}
	return 1
}

// own is what applying a schema to a value costs by itself, whatever the
// value: the application, a comparison with each value the schema lists and
// with the one it fixes, a rational when it bounds a number or asks for an
// integer, and a lookup of each name it requires.
func own(s *jsonschema.Schema) int {
	n := applying
	if s.Enum != nil {
		for _, v := range s.Enum.Values {
			n = plus(n, weight(v))
		}
	}
	if s.Const != nil {
		n = plus(n, weight(*s.Const))
	}
	if s.Minimum != nil || s.Maximum != nil || s.ExclusiveMinimum != nil || s.ExclusiveMaximum != nil || s.MultipleOf != nil {
		n += comparing
	}
	if s.Types != nil && slices.Contains(s.Types.ToStrings(), "integer") {
		n += comparing
	}
	names := len(s.Required)
	for _, required := range s.DependentRequired {
		names += len(required)
	}
	return plus(n, (names+lookups-1)/lookups)
}

// perByte is the steps applying a schema to a text costs for each byte of
// it: its pattern's, and a count of the text's characters when the schema
// bounds its length.
func perByte(s *jsonschema.Schema) int {
	n := sizeOf(s.Pattern)
	if s.MinLength != nil || s.MaxLength != nil {
		n += steps / bytesPerUnit
	}
	return n
}

// measured is what each schema of a compiled schema costs to apply to one
// value that holds nothing: a scalar.
type measured struct {
	// applied is how many schemas a schema applies to one value, never
	// above MaxApplied + 1: a count past the bound is not counted further.
	applied map[*jsonschema.Schema]int
	// flat is what applying a schema to a scalar costs, with every schema
	// it applies in place, in the unit of MaxCheckWork.
	flat map[*jsonschema.Schema]int
	// text is the steps a schema, and every schema it applies in place,
	// takes for each byte of a string it is applied to.
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
// 2020-12, one that holds what the count has no price for, one that
// reaches itself with no value in between, and one that applies more than
// MaxApplied schemas to one value are refused with invalid_schema.
func measure(root *jsonschema.Schema) (measured, error) {
	g := &gauge{
		applied: map[*jsonschema.Schema]int{},
		flat:    map[*jsonschema.Schema]int{},
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
	if field := unmodeled(s); field != "" {
		return fault.New(fault.InvalidSchema, "the schema holds at %s what this server has no bound for: %s", at(s), field)
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
// what the schema costs to apply is summed with it.
func (g *gauge) count(s *jsonschema.Schema) (int, error) {
	if n, ok := g.applied[s]; ok {
		return n, nil
	}
	if g.open[s] {
		return 0, fault.New(fault.InvalidSchema, "the schema applies %s to the value it is itself applied to, without end", at(s))
	}
	g.open[s] = true
	n, flat, text := 1, own(s), perByte(s)
	for _, t := range inPlace(s) {
		m, err := g.count(t)
		if err != nil {
			return 0, err
		}
		n = min(n+m, MaxApplied+1)
		flat = plus(flat, g.flat[t])
		text = plus(text, g.text[t])
	}
	delete(g.open, s)
	g.applied[s], g.flat[s], g.text[s] = n, flat, text
	return n, nil
}

// node names one application the validator makes at an object or a list: a
// schema, and the value by where it is in memory. A scalar needs no name:
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
	// weights is what each list that must hold no value 2 times weighs.
	weights map[uintptr]int
	// looked is how many applications were looked at, each a part of the
	// count: more of them than MaxCheckWork is a count above it.
	looked int
}

// work is what holding a value to the schema would take, never above
// MaxCheckWork + 1. It is what the validator does at most: every schema of
// a choice and both arms of a condition are counted.
func (s *Schema) work(value any) int {
	m := &meter{measured: s.measured, counted: map[node]int{}, weights: map[uintptr]int{}}
	return m.cost(s.compiled, value)
}

// matching is what holding a text of n bytes to patterns and bounds of size
// steps for each byte costs: nothing for no steps, and never less than 1
// for some.
func matching(n, size int) int {
	if size == 0 {
		return 0
	}
	return min(1+n*size/steps, MaxCheckWork+1)
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
		return plus(m.flat[s], matching(len(v), m.text[s]))
	default:
		return m.flat[s]
	}
	if n, ok := m.counted[where]; ok {
		return n
	}
	n := own(s)
	// add counts an amount, and reports whether the count is still within
	// the bound.
	add := func(amount int) bool {
		n = plus(n, amount)
		return n <= MaxCheckWork
	}
	for _, t := range inPlace(s) {
		if !add(m.cost(t, value)) {
			return n
		}
	}
	switch v := value.(type) {
	case map[string]any:
		if !add((len(v) + members - 1) / members) {
			return n
		}
		for name, member := range v {
			described := false
			if t, ok := s.Properties[name]; ok {
				described = true
				if !add(m.cost(t, member)) {
					return n
				}
			}
			for re, t := range s.PatternProperties {
				// The match is counted before it is made.
				if !add(matching(len(name), sizeOf(re))) {
					return n
				}
				if re.MatchString(name) {
					described = true
					if !add(m.cost(t, member)) {
						return n
					}
				}
			}
			if t, ok := s.AdditionalProperties.(*jsonschema.Schema); ok && !described && !add(m.cost(t, member)) {
				return n
			}
			if s.PropertyNames != nil && !add(m.cost(s.PropertyNames, name)) {
				return n
			}
		}
	case []any:
		if s.UniqueItems && !add(m.unique(v)) {
			return n
		}
		for i, item := range v {
			if i < len(s.PrefixItems) {
				if !add(m.cost(s.PrefixItems[i], item)) {
					return n
				}
			} else if s.Items2020 != nil && !add(m.cost(s.Items2020, item)) {
				return n
			}
			if s.Contains != nil && !add(m.cost(s.Contains, item)) {
				return n
			}
		}
	}
	m.counted[where] = n
	return n
}

// unique is what finding whether a list holds a value 2 times costs. A
// list of up to fewItems is compared each item with each. A longer one is
// hashed, each item once, and compared in full with each item before it
// that has the same hash. So the list's items are grouped by the validator's
// own hash, and the price is hashing every item, twice the list's weight,
// and for each group of k items its k*(k-1)/2 pairs each at the weight of
// the group's heaviest item. Items that differ in their hash are never
// compared, so a list of distinct objects costs its hashing and nothing
// more, while items whose texts are cut at other places and hash alike,
// which the hash allows since it frames a text with no length, are priced
// as the full comparisons they make.
func (m *meter) unique(list []any) int {
	at := reflect.ValueOf(list).Pointer()
	w, ok := m.weights[at]
	if !ok {
		w = weight(list)
		m.weights[at] = w
	}
	if len(list) <= fewItems {
		return min(w*len(list), MaxCheckWork+1)
	}
	// Hashing every item costs the list's weight twice, which is also what
	// the meter spends to group them: a list whose hashing alone is past the
	// bound is answered so before any item is hashed.
	n := plus(w, w)
	if n > MaxCheckWork {
		return n
	}
	type group struct{ n, weight int }
	groups := map[uint64]*group{}
	for _, item := range list {
		key := hashed(item)
		g := groups[key]
		if g == nil {
			g = &group{}
			groups[key] = g
		}
		g.n++
		g.weight = max(g.weight, weight(item))
	}
	for _, g := range groups {
		pairs := g.n * (g.n - 1) / 2
		if pairs == 0 {
			continue
		}
		if g.weight > (MaxCheckWork+1)/pairs {
			return MaxCheckWork + 1
		}
		if n = plus(n, pairs*g.weight); n > MaxCheckWork {
			return n
		}
	}
	return n
}

// hashed is the hash the validator gives an item of a list that must be
// unique, as far as it tells items apart: 2 items have the same value here
// exactly when they have the same input to the validator's hash. The input
// is written as writeHash of util.go writes it, in version v6.0.3 of
// github.com/santhosh-tekuri/jsonschema/v6, which duplicates uses for a
// list of more than 20 items: a tag byte for the kind of value, the members
// of an object in the order of their names, a text with no length, and for a
// number the bytes of the numerator and of the denominator of its rational,
// whose sign is not written. The validator's hash is seeded at random and
// this one is not, which only ever tells fewer items apart than the
// validator does. A change of the validator's hash is not followed here
// unseen: TestItemsThatHashAlikeAreOneGroupAndPricedPastTheBound builds
// items that meet in this hash and fails when the price is not past the
// bound.
func hashed(v any) uint64 {
	h := fnv.New64a()
	writeHash(v, h)
	return h.Sum64()
}

// writeHash writes the hash input of a value, as writeHash of the
// validator's util.go does.
func writeHash(v any, h io.Writer) {
	switch v := v.(type) {
	case map[string]any:
		_, _ = h.Write([]byte{0})
		for _, name := range slices.Sorted(maps.Keys(v)) {
			writeHash(name, h)
			writeHash(v[name], h)
		}
	case []any:
		_, _ = h.Write([]byte{1})
		for _, item := range v {
			writeHash(item, h)
		}
	case nil:
		_, _ = h.Write([]byte{2})
	case bool:
		if v {
			_, _ = h.Write([]byte{3, 1})
		} else {
			_, _ = h.Write([]byte{3, 0})
		}
	case string:
		_, _ = h.Write([]byte{4})
		_, _ = io.WriteString(h, v)
	case float64:
		_, _ = h.Write([]byte{5})
		num, _ := new(big.Rat).SetString(fmt.Sprint(v))
		_, _ = h.Write(num.Num().Bytes())
		_, _ = h.Write(num.Denom().Bytes())
	}
}

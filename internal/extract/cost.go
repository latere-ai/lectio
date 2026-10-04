// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"maps"
	"reflect"
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
// MaxApplied schemas to one value, and none may reach itself with no value
// in between. That refuses a schema whose cost is in the schema alone.
//
// A check is bounded when a reply arrives: the applications the validator
// would make of this schema to this object are counted first, and an object
// that would take more than MaxCheckWork is not held to the schema. That
// covers a schema whose cost grows with how deep the object nests, which no
// reading of the schema alone can bound.
const (
	// MaxApplied is how many schemas a schema may apply to one value: itself
	// and every schema it reaches through $ref, $dynamicRef, allOf, anyOf,
	// oneOf, not, if, then, else and dependentSchemas, each counted as often
	// as it is reached. A choice between 64 definitions applies 129, itself
	// and for each a reference and what it refers to, so 256 is above what a
	// schema written to describe a document needs.
	MaxApplied = 256

	// MaxCheckWork is how many applications of a schema one check of an
	// object may make: 2,097,152. The validator made that many in 0.3
	// seconds where it was measured, and a reply of 4,000 objects held to a
	// choice between 8 shapes each takes less than a quarter of it.
	MaxCheckWork = 2 << 20
)

// dialect is draft 2020-12 as the validator numbers the dialect a schema
// was compiled in.
const dialect = 2020

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
	for _, t := range s.PatternProperties {
		out = append(out, t)
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

// gauge counts what each schema of a compiled schema applies to one value.
type gauge struct {
	// applied is the count of each schema that was counted, never above
	// MaxApplied + 1: a count past the bound is not counted further.
	applied map[*jsonschema.Schema]int
	// open are the schemas whose count is being taken. One that is reached
	// again from itself applies itself to the value it is applied to.
	open map[*jsonschema.Schema]bool
	seen map[*jsonschema.Schema]bool
}

// measure checks every schema a compiled schema can apply, and answers what
// each applies to one value. A schema in another dialect than draft
// 2020-12, one that reaches itself with no value in between, and one that
// applies more than MaxApplied schemas to one value are refused with
// invalid_schema.
func measure(root *jsonschema.Schema) (map[*jsonschema.Schema]int, error) {
	g := &gauge{
		applied: map[*jsonschema.Schema]int{},
		open:    map[*jsonschema.Schema]bool{},
		seen:    map[*jsonschema.Schema]bool{},
	}
	if err := g.reach(root); err != nil {
		return nil, err
	}
	return g.applied, nil
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
// each schema it applies in place applies. Each count is taken once.
func (g *gauge) count(s *jsonschema.Schema) (int, error) {
	if n, ok := g.applied[s]; ok {
		return n, nil
	}
	if g.open[s] {
		return 0, fault.New(fault.InvalidSchema, "the schema applies %s to the value it is itself applied to, without end", at(s))
	}
	g.open[s] = true
	n := 1
	for _, t := range inPlace(s) {
		m, err := g.count(t)
		if err != nil {
			return 0, err
		}
		n = min(n+m, MaxApplied+1)
	}
	delete(g.open, s)
	g.applied[s] = n
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
// what a schema applies to it is what it applies to any one value.
type node struct {
	schema *jsonschema.Schema
	value  uintptr
}

// meter counts the applications a check of one object would make.
type meter struct {
	applied map[*jsonschema.Schema]int
	// counted is what each application counted so far comes to, with what
	// it makes inside its value. The validator makes it again each time it
	// reaches it, and the count is taken once.
	counted map[node]int
	// steps is how many applications were looked at, each a part of the
	// count: more of them than MaxCheckWork is a count above it.
	steps int
}

// costly reports whether holding a value to the schema would take more than
// MaxCheckWork applications. The count is what the validator makes at most:
// every schema of a choice and both arms of a condition are counted, and a
// schema for the members nothing else describes is counted for every member.
func (s *Schema) costly(value any) bool {
	m := &meter{applied: s.applied, counted: map[node]int{}}
	return m.cost(s.compiled, value) > MaxCheckWork
}

// cost is how many applications applying a schema to a value comes to,
// never above MaxCheckWork + 1.
func (m *meter) cost(s *jsonschema.Schema, value any) int {
	if m.steps++; m.steps > MaxCheckWork {
		return MaxCheckWork + 1
	}
	var where node
	switch v := value.(type) {
	case map[string]any:
		where = node{s, reflect.ValueOf(v).Pointer()}
	case []any:
		where = node{s, reflect.ValueOf(v).Pointer()}
	default:
		return m.applied[s]
	}
	if n, ok := m.counted[where]; ok {
		return n
	}
	n := 1
	add := func(t *jsonschema.Schema, v any) {
		n = min(n+m.cost(t, v), MaxCheckWork+1)
	}
	for _, t := range inPlace(s) {
		add(t, value)
	}
	switch v := value.(type) {
	case map[string]any:
		for name, member := range v {
			described := false
			if t, ok := s.Properties[name]; ok {
				described = true
				add(t, member)
			}
			for pattern, t := range s.PatternProperties {
				if pattern.MatchString(name) {
					described = true
					add(t, member)
				}
			}
			if t, ok := s.AdditionalProperties.(*jsonschema.Schema); ok && !described {
				add(t, member)
			}
			if s.UnevaluatedProperties != nil {
				add(s.UnevaluatedProperties, member)
			}
			if s.PropertyNames != nil {
				add(s.PropertyNames, name)
			}
		}
	case []any:
		for i, item := range v {
			if i < len(s.PrefixItems) {
				add(s.PrefixItems[i], item)
			} else if s.Items2020 != nil {
				add(s.Items2020, item)
			}
			for _, t := range []*jsonschema.Schema{s.Contains, s.UnevaluatedItems} {
				if t != nil {
					add(t, item)
				}
			}
		}
	}
	m.counted[where] = n
	return n
}

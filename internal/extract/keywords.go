// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"latere.ai/x/lectio/internal/fault"
)

// The keywords a schema may use.
//
// What holding an object to a schema costs is counted keyword by keyword
// (cost.go), and a count has a hole wherever a keyword is applied that it
// does not know. So a schema is read before it is compiled, and a keyword
// that is not listed here is refused by its name. The list is the keywords
// of draft 2020-12 the count has a price for, and the annotations a
// validator does nothing with. It leaves out what an earlier draft applied
// (dependencies, definitions, additionalItems, $recursiveRef), what lets
// the object decide which subschema is applied ($dynamicRef,
// $dynamicAnchor), what changes how a reference resolves ($id, $anchor,
// $vocabulary), the 2 keywords whose cost follows every schema applied
// beside them (unevaluatedProperties, unevaluatedItems), and every keyword
// nobody defined: a caller's own keyword is one this server does not know
// the meaning of.

// shape is what a keyword's value is, which says where the schemas inside
// it are.
type shape int

const (
	// data is a value the walk does not enter: a keyword that asserts with
	// it, or an annotation. Nothing in it is a schema.
	data shape = iota
	// one is a schema.
	one
	// list is an array of schemas.
	list
	// named is an object whose members are schemas under names that are
	// the caller's and no keywords.
	named
	// reference is a pointer to a schema of the document.
	reference
	// atRoot is a keyword the root alone may carry.
	atRoot
)

// keywords are the keywords a schema may use, each with the shape of its
// value.
var keywords = map[string]shape{
	"$schema": atRoot, "$ref": reference, "$defs": named, "$comment": data,

	"allOf": list, "anyOf": list, "oneOf": list, "not": one,
	"if": one, "then": one, "else": one, "dependentSchemas": named,

	"properties": named, "patternProperties": named, "additionalProperties": one, "propertyNames": one,
	"items": one, "prefixItems": list, "contains": one,

	"type": data, "enum": data, "const": data,
	"multipleOf": data, "maximum": data, "exclusiveMaximum": data, "minimum": data, "exclusiveMinimum": data,
	"maxLength": data, "minLength": data, "pattern": data,
	"maxItems": data, "minItems": data, "uniqueItems": data, "maxContains": data, "minContains": data,
	"maxProperties": data, "minProperties": data, "required": data, "dependentRequired": data,

	"title": data, "description": data, "default": data, "examples": data,
	"deprecated": data, "readOnly": data, "writeOnly": data, "format": data,
}

// Keywords lists the keywords a schema may use, sorted.
func Keywords() []string {
	out := make([]string, 0, len(keywords))
	for name := range keywords {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// reading is a walk over a schema document.
type reading struct {
	// schemas are the places of the document that hold a schema, each as a
	// JSON pointer.
	schemas map[string]bool
	// refs are the references the document makes, each with where it is.
	refs []referral
}

type referral struct{ at, to string }

// read walks a schema document and refuses, with invalid_schema, a keyword
// that is not listed, a value that is not of its keyword's shape, and a
// reference that does not point at a schema of the document. A reference
// is "#" or "#" and a JSON pointer, and it points at a place the walk found
// a schema in: a pointer into an example, a default or a listed value
// would have a validator apply what was never read as a schema.
func read(doc any) error {
	r := &reading{schemas: map[string]bool{}}
	if err := r.schema(doc, nil); err != nil {
		return err
	}
	for _, ref := range r.refs {
		target, ok := fragment(ref.to)
		if !ok || !r.schemas[target] {
			return fault.New(fault.InvalidSchema,
				"the reference at #%s does not point at a schema of this schema: a reference is \"#\", or \"#\" and a JSON pointer to a schema", ref.at)
		}
	}
	return nil
}

// fragment reads a reference as the JSON pointer it carries, written the
// one way a pointer is written. ok is false for a reference to anything
// but a place in the document.
func fragment(ref string) (pointer string, ok bool) {
	rest, found := strings.CutPrefix(ref, "#")
	if !found || strings.Contains(rest, "%") || (rest != "" && !strings.HasPrefix(rest, "/")) {
		return "", false
	}
	// "~" begins an escape, and there are 2: "~0" and "~1".
	if strings.Contains(strings.NewReplacer("~0", "", "~1", "").Replace(rest), "~") {
		return "", false
	}
	return rest, true
}

// schema walks one schema: true, false, or an object of keywords.
func (r *reading) schema(node any, path []string) error {
	at := pointer(path)
	r.schemas[exact(path)] = true
	members, ok := node.(map[string]any)
	if !ok {
		if _, boolean := node.(bool); boolean {
			return nil
		}
		return fault.New(fault.InvalidSchema, "the schema holds a value that is no schema at #%s", at)
	}
	for _, keyword := range slices.Sorted(maps.Keys(members)) {
		value := members[keyword]
		form, listed := keywords[keyword]
		if !listed || (form == atRoot && len(path) > 0) {
			return fault.New(fault.InvalidSchema,
				"the schema uses the keyword %q at #%s, which is not one of the keywords a schema may use here", clipTo(keyword, 80), at)
		}
		here := append(slices.Clone(path), keyword)
		switch form {
		case one:
			if err := r.schema(value, here); err != nil {
				return err
			}
		case list:
			items, ok := value.([]any)
			if !ok {
				return misshapen(keyword, at, "a list of schemas")
			}
			for i, item := range items {
				if err := r.schema(item, append(slices.Clone(here), strconv.Itoa(i))); err != nil {
					return err
				}
			}
		case named:
			schemas, ok := value.(map[string]any)
			if !ok {
				return misshapen(keyword, at, "an object of schemas")
			}
			for _, name := range slices.Sorted(maps.Keys(schemas)) {
				if err := r.schema(schemas[name], append(slices.Clone(here), name)); err != nil {
					return err
				}
			}
		case reference:
			to, ok := value.(string)
			if !ok {
				return misshapen(keyword, at, "a reference")
			}
			r.refs = append(r.refs, referral{at: at, to: to})
		case atRoot:
			if value != draft {
				return fault.New(fault.InvalidSchema, "the schema names another dialect than %s", draft)
			}
		case data:
		}
	}
	return nil
}

func misshapen(keyword, at, want string) error {
	return fault.New(fault.InvalidSchema, "the keyword %q at #%s does not hold %s", keyword, at, want)
}

// exact writes a path as a JSON pointer in full.
func exact(path []string) string {
	var b strings.Builder
	for _, token := range path {
		b.WriteString("/")
		b.WriteString(escape(token))
	}
	return b.String()
}

// modeled are the fields of a compiled schema that the count has a price
// for, or that hold what a validator does nothing with.
var modeled = []string{
	"DraftVersion", "Location",
	"Bool", "Ref", "Types", "Enum", "Const", "Not", "AllOf", "AnyOf", "OneOf", "If", "Then", "Else",
	"MaxProperties", "MinProperties", "Required", "PropertyNames", "Properties", "PatternProperties",
	"AdditionalProperties", "DependentRequired", "DependentSchemas",
	"MinItems", "MaxItems", "UniqueItems", "Contains", "MinContains", "MaxContains", "PrefixItems", "Items2020",
	"MinLength", "MaxLength", "Pattern",
	"Maximum", "Minimum", "ExclusiveMaximum", "ExclusiveMinimum", "MultipleOf",
	"Title", "Description", "Default", "Comment", "ReadOnly", "WriteOnly", "Examples", "Deprecated",
}

// unmodeled names a field of a compiled schema that is set and that the
// count has no price for, or nothing. The walk above lets no keyword
// through that would set one. This is the same rule held from the other
// side, on what the validator will read: a field a later version of the
// validator adds is refused until it is listed.
func unmodeled(s *jsonschema.Schema) string {
	v := reflect.ValueOf(s).Elem()
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if field.IsExported() && !slices.Contains(modeled, field.Name) && !v.Field(i).IsZero() {
			return field.Name
		}
	}
	return ""
}

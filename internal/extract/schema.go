// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"latere.ai/x/lectio/internal/fault"
)

// The bounds of a caller's schema.
const (
	// MaxSchemaBytes is the largest schema a request may carry.
	MaxSchemaBytes = 64 << 10
	// MaxSchemaDepth is how deep a schema may nest, counting every object
	// and every array of the schema document as a level.
	MaxSchemaDepth = 16
)

// draft is the one dialect a schema is read in.
const draft = "https://json-schema.org/draft/2020-12/schema"

// resource is the name the caller's schema is compiled under. It is no
// address: nothing is fetched from it.
const resource = "schema.json"

// Schema is a caller's JSON Schema, compiled.
type Schema struct {
	compiled *jsonschema.Schema
	// measured is what each schema inside the compiled one costs to apply
	// to one value, which is where the count of a check starts from.
	measured

	// Constrainable reports whether the schema uses nothing but what a
	// decoder that enforces a schema takes, so it may be sent for
	// enforcement. A schema that uses anything else is stated in the prompt
	// alone, and the validator holds the reply to it.
	Constrainable bool
}

// isolated is the loader of every reference a schema makes outside itself.
// It loads none: a caller's schema never makes the server read a file or
// fetch an address.
type isolated struct{}

func (isolated) Load(string) (any, error) {
	return nil, errors.New("a schema refers to nothing outside itself")
}

// Compile checks a caller's schema and compiles it. A schema that is larger
// than MaxSchemaBytes, is not a JSON object, nests deeper than
// MaxSchemaDepth, names another dialect than draft 2020-12, does not
// describe an object at its root, refers to anything outside itself, does
// not compile, applies more than MaxApplied schemas to one value, holds
// patterns that compile to more than MaxPatternSize steps, or gives one
// dynamic anchor to 2 subschemas is refused with invalid_schema and the
// reason.
func Compile(raw []byte) (*Schema, error) {
	refuse := func(format string, args ...any) (*Schema, error) {
		return nil, fault.New(fault.InvalidSchema, format, args...)
	}
	if len(raw) > MaxSchemaBytes {
		return refuse("the schema is %d bytes, and a schema is at most %d", len(raw), MaxSchemaBytes)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return refuse("the schema is not JSON")
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return refuse("the schema is not a JSON object")
	}
	if d := depth(doc); d > MaxSchemaDepth {
		return refuse("the schema nests %d levels deep, and a schema nests at most %d", d, MaxSchemaDepth)
	}
	if dialect, named := root["$schema"]; named && dialect != draft {
		return refuse("the schema names another dialect than %s", draft)
	}
	if root["type"] != "object" {
		return refuse("the schema's root does not have the type object")
	}
	// A dynamic anchor that 2 subschemas carry makes the object decide which
	// of them a $dynamicRef applies. With 1 it is the one the reference
	// names, and what the schema applies can be counted from the schema.
	named := map[string]int{}
	anchors(doc, named)
	for _, name := range slices.Sorted(maps.Keys(named)) {
		if named[name] > 1 {
			return refuse("the schema gives the dynamic anchor %q to %d subschemas, and a dynamic anchor names 1", name, named[name])
		}
	}

	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(isolated{})
	c.UseRegexpEngine(patterns())
	if err := c.AddResource(resource, doc); err != nil {
		return refuse("the schema does not compile: %s", reason(err))
	}
	compiled, err := c.Compile(resource)
	if err != nil {
		return refuse("the schema does not compile: %s", reason(err))
	}
	measures, err := measure(compiled)
	if err != nil {
		return nil, err
	}
	return &Schema{compiled: compiled, measured: measures, Constrainable: constrainable(doc)}, nil
}

// reason is a compile error as a caller reads it: without the name the
// schema was compiled under, which is this package's own.
func reason(err error) string {
	text := strings.ReplaceAll(err.Error(), "file://", "")
	if i := strings.Index(text, resource); i >= 0 {
		text = "at " + strings.TrimLeft(text[i+len(resource):], "'\": ")
	}
	return strings.TrimPrefix(text, "jsonschema: ")
}

// depth is how deep a JSON value nests: 0 for a scalar, and 1 more than its
// deepest member for an object or an array.
func depth(v any) int {
	deepest := 0
	switch v := v.(type) {
	case map[string]any:
		for _, member := range v {
			deepest = max(deepest, depth(member))
		}
	case []any:
		for _, member := range v {
			deepest = max(deepest, depth(member))
		}
	default:
		return 0
	}
	return deepest + 1
}

// enforced are the keywords a decoder that enforces a schema takes
// wherever such decoding is offered: the ones that say what shape a value
// has. A keyword outside the set, a combination of schemas other than a
// choice, a condition, a pattern of property names, a bound on a number, a
// length or a count, makes the schema one that is stated in the prompt and
// checked by the validator.
var enforced = []string{
	"$schema", "$id", "$ref", "$defs", "definitions", "$comment",
	"type", "properties", "required", "items", "enum", "const", "anyOf", "additionalProperties",
	"title", "description", "default", "examples",
}

// constrainable reports whether a schema, and every schema inside it, uses
// only the keywords of enforced. additionalProperties is taken only as
// false: a decoder's closed form has no place for members nobody named.
func constrainable(schema any) bool {
	node, ok := schema.(map[string]any)
	if !ok {
		// A schema that is true or false admits everything or nothing, and
		// no decoder takes it.
		return false
	}
	for keyword, value := range node {
		if !slices.Contains(enforced, keyword) {
			return false
		}
		switch keyword {
		case "properties", "$defs", "definitions":
			members, ok := value.(map[string]any)
			if !ok {
				return false
			}
			for _, member := range members {
				if !constrainable(member) {
					return false
				}
			}
		case "items":
			if !constrainable(value) {
				return false
			}
		case "anyOf":
			choices, ok := value.([]any)
			if !ok || len(choices) == 0 {
				return false
			}
			for _, choice := range choices {
				if !constrainable(choice) {
					return false
				}
			}
		case "additionalProperties":
			if value != false {
				return false
			}
		}
	}
	return true
}

// Finding is one way a reply does not satisfy a schema.
type Finding struct {
	// Pointer is where in the reply's object the value is, as a JSON
	// pointer. The root is the empty pointer.
	Pointer string
	// Rule is where in the schema the rule it broke is, with the rule's
	// keyword last: "#/properties/total/type".
	Rule string
	// Message says what is wrong, for the model that is asked to repair
	// the reply. It may quote the reply.
	Message string
	// Unchecked says the object was not held to the schema: checking it
	// would take more work than MaxCheckWork. No repair is asked for it.
	Unchecked bool
}

// english prints the validator's messages.
var english = message.NewPrinter(language.English)

// Check holds a reply's object to the schema and returns what it does not
// satisfy, or nothing. part says the object was filled from one window of a
// longer document: a member that is missing, and a list or an object that
// holds too few, may be in another window, so those rules are held to the
// merged object and not to a part.
//
// An object that would take the validator more work than MaxCheckWork is
// not held to the schema, and the one finding says so: the validation could
// not be stopped once it began.
func (s *Schema) Check(data []byte, part bool) []Finding {
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return []Finding{{Rule: "#", Message: "the data is not a JSON value"}}
	}
	if s.costly(value) {
		return []Finding{{Rule: "#", Unchecked: true, Message: fmt.Sprintf(
			"at the root: holding the object to the schema would take more than %d applications of the schema", MaxCheckWork)}}
	}
	err = s.compiled.Validate(value)
	if err == nil {
		return nil
	}
	invalid, ok := errors.AsType[*jsonschema.ValidationError](err)
	if !ok {
		return []Finding{{Rule: "#", Message: "the data holds a value JSON does not have"}}
	}
	var out []Finding
	collect(invalid, part, &out)
	return out
}

// collect gathers the findings of a validation error: the errors with no
// error underneath, which are the rules a value broke. An error with others
// underneath is a schema that failed because a schema inside it did.
func collect(e *jsonschema.ValidationError, part bool, out *[]Finding) {
	if len(e.Causes) > 0 {
		for _, cause := range e.Causes {
			collect(cause, part, out)
		}
		return
	}
	if part && partial(e.ErrorKind) {
		return
	}
	_, where, _ := strings.Cut(e.SchemaURL, "#")
	rule := "#" + where
	if path := e.ErrorKind.KeywordPath(); len(path) > 0 {
		rule += "/" + strings.Join(path, "/")
	}
	tokens := make([]string, len(e.InstanceLocation))
	for i, token := range e.InstanceLocation {
		tokens[i] = "/" + escape(token)
	}
	pointer, at := strings.Join(tokens, ""), "at the root"
	if pointer != "" {
		at = "at " + pointer
	}
	*out = append(*out, Finding{Pointer: pointer, Rule: rule, Message: at + ": " + e.ErrorKind.LocalizedString(english)})
}

// partial reports whether a rule is one a part of a document cannot be held
// to: a member, an item or a property that another part may hold.
func partial(k jsonschema.ErrorKind) bool {
	switch k.(type) {
	case *kind.Required, *kind.MinItems, *kind.MinProperties, *kind.MinContains, *kind.Contains,
		*kind.DependentRequired, *kind.Dependency:
		return true
	}
	return false
}

// escape writes a member's name as a token of a JSON pointer.
func escape(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}

// shown is how many findings a failure names.
const shown = 5

// Broken is the detail a field that failed validation carries: the rules of
// the schema the last reply broke, each by its place in the schema. It
// names the caller's schema and never a value of the reply, which is
// content of the document.
func Broken(findings []Finding) string {
	if unchecked(findings) {
		return fmt.Sprintf("the object was not held to the schema: that would take more than %d applications of the schema", MaxCheckWork)
	}
	rules := make([]string, 0, shown)
	for _, f := range findings {
		if len(rules) == shown {
			break
		}
		if !slices.Contains(rules, f.Rule) {
			rules = append(rules, f.Rule)
		}
	}
	return fmt.Sprintf("the object does not satisfy the schema in %d places, at %s", len(findings), strings.Join(rules, ", "))
}

// unchecked reports whether the findings are the one of an object that was
// not held to the schema.
func unchecked(findings []Finding) bool {
	return len(findings) == 1 && findings[0].Unchecked
}

// Problems are findings as the model that is asked to repair its reply is
// told them.
func Problems(findings []Finding) []string {
	out := make([]string, len(findings))
	for i, f := range findings {
		out[i] = f.Message
	}
	return out
}

// encode writes a value as JSON. The values this package encodes were
// decoded from JSON, so they encode; one that did not would be written as
// null, which no schema that asks for an object takes.
func encode(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return raw
}

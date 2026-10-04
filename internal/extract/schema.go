// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
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

// Compile checks a caller's schema and compiles it. A schema is refused
// with invalid_schema and the reason when it is larger than MaxSchemaBytes,
// is not a JSON object, holds a number written in more than MaxNumberLength
// characters or outside what a machine number holds, nests deeper than
// MaxSchemaDepth, uses a keyword that is not one of Keywords, refers to
// anything but a schema inside itself, names another dialect than draft
// 2020-12, does not describe an object at its root, does not compile,
// applies more than MaxApplied schemas to one value, or holds patterns that
// compile to more than MaxPatternSize steps.
func Compile(raw []byte) (*Schema, error) {
	refuse := func(format string, args ...any) (*Schema, error) {
		return nil, fault.New(fault.InvalidSchema, format, args...)
	}
	if len(raw) > MaxSchemaBytes {
		return refuse("the schema is %d bytes, and a schema is at most %d", len(raw), MaxSchemaBytes)
	}
	// The schema's own depth is held just below, with its own reason.
	doc, flaws, err := decode(raw, len(raw))
	if err != nil {
		return refuse("the schema is not JSON")
	}
	if len(flaws) > 0 {
		return refuse("the schema holds a number that is no number of a schema at #%s: %s", flaws[0].pointer, flaws[0].what)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return refuse("the schema is not a JSON object")
	}
	if d := depth(doc); d > MaxSchemaDepth {
		return refuse("the schema nests %d levels deep, and a schema nests at most %d", d, MaxSchemaDepth)
	}
	if err := read(doc); err != nil {
		return nil, err
	}
	if root["type"] != "object" {
		return refuse("the schema's root does not have the type object")
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
	"$schema", "$ref", "$defs", "$comment",
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
		case "properties", "$defs":
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
	// pointer of at most 512 bytes. The root is the empty pointer.
	Pointer string
	// Rule is where in the schema the rule it broke is, with the rule's
	// keyword last: "#/properties/total/type".
	Rule string
	// Message says what is wrong, for the model that is asked to repair
	// the reply, in at most 300 bytes.
	Message string
	// Unchecked says the object was not held to the schema: checking it
	// would take more work than MaxCheckWork, or took longer than a check
	// may. No repair is asked for it.
	Unchecked bool
}

// MaxFindings is how many findings a check answers. An object that breaks
// its schema in more places is said to break it in these.
const MaxFindings = 64

// maxMessage is how many bytes a finding's message has at most.
const maxMessage = 300

// english prints the validator's messages.
var english = message.NewPrinter(language.English)

// costs is the finding of an object that was not held to its schema, with
// why.
func costs(why string) []Finding {
	return []Finding{{Rule: "#", Unchecked: true, Message: "the object was not held to the schema, which costs too much to check it against: " + why}}
}

// Check holds a reply's object to the schema and returns what it does not
// satisfy, or nothing, and never more than MaxFindings. part says the
// object was filled from one window of a longer document: a member that is
// missing, and a list or an object that holds too few, may be in another
// window, so those rules are held to the merged object and not to a part.
//
// A reply that holds a number written in more than MaxNumberLength
// characters or outside what a machine number holds, or that nests deeper
// than MaxValueDepth, does not satisfy the schema, and says where. An
// object that would take the validator more work than MaxCheckWork is not
// held to the schema, and the one finding says so: the validation could not
// be stopped once it began.
func (s *Schema) Check(data []byte, part bool) []Finding {
	value, flaws, err := decode(data, MaxValueDepth)
	if err != nil {
		return []Finding{{Rule: "#", Message: "the data is not a JSON value"}}
	}
	if len(flaws) > 0 {
		out := make([]Finding, len(flaws))
		for i, f := range flaws {
			out[i] = Finding{Pointer: f.pointer, Rule: "#", Message: where(f.pointer) + ": " + f.what}
		}
		return out
	}
	if s.work(value) > MaxCheckWork {
		return costs(fmt.Sprintf("more than %d units of work", MaxCheckWork))
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

// where is a value's place as a message names it.
func where(pointer string) string {
	if pointer == "" {
		return "at the root"
	}
	return "at " + pointer
}

// collect gathers the findings of a validation error, up to MaxFindings:
// the errors with no error underneath, which are the rules a value broke.
// An error with others underneath is a schema that failed because a schema
// inside it did.
func collect(e *jsonschema.ValidationError, part bool, out *[]Finding) {
	if len(*out) >= MaxFindings {
		return
	}
	if len(e.Causes) > 0 {
		for _, cause := range e.Causes {
			collect(cause, part, out)
		}
		return
	}
	if part && partial(e.ErrorKind) {
		return
	}
	_, at, _ := strings.Cut(e.SchemaURL, "#")
	rule := "#" + at
	if path := e.ErrorKind.KeywordPath(); len(path) > 0 {
		rule += "/" + strings.Join(path, "/")
	}
	place := pointer(e.InstanceLocation)
	*out = append(*out, Finding{Pointer: place, Rule: rule, Message: clipTo(where(place)+": "+say(e.ErrorKind), maxMessage)})
}

// say is what a broken rule says, in a length that does not grow with the
// reply or the schema. The validator's own words are taken where they are
// bounded by what they name: a type, a count, a number. Where they would
// print a value of the reply or every value the schema lists, the rule is
// named and the values are not.
func say(k jsonschema.ErrorKind) string {
	switch k := k.(type) {
	case *kind.Type, *kind.MinLength, *kind.MaxLength, *kind.MinItems, *kind.MaxItems,
		*kind.MinProperties, *kind.MaxProperties, *kind.Minimum, *kind.Maximum,
		*kind.ExclusiveMinimum, *kind.ExclusiveMaximum, *kind.MultipleOf,
		*kind.UniqueItems, *kind.FalseSchema, *kind.Not, *kind.OneOf, *kind.Contains:
		return k.LocalizedString(english)
	case *kind.Required:
		if len(k.Missing) == 1 {
			return "missing property " + some(k.Missing)
		}
		return "missing properties " + some(k.Missing)
	case *kind.DependentRequired:
		return "with " + some([]string{k.Prop}) + ", missing " + some(k.Missing)
	case *kind.AdditionalProperties:
		// The validator names them in the order it met them, which is no
		// order.
		return "members the schema does not take: " + some(slices.Sorted(slices.Values(k.Properties)))
	case *kind.Enum:
		if listed, ok := few(k.Want); ok {
			return "value must be one of " + listed
		}
		return fmt.Sprintf("the value is none of the %d the schema lists", len(k.Want))
	case *kind.Const:
		return "the value is not the one the schema fixes"
	case *kind.Pattern:
		return "the text does not match the pattern " + clipTo(k.Want, 80)
	case *kind.MinContains:
		return fmt.Sprintf("%d items are as the schema asks some to be, and at least %d must be", len(k.Got), k.Want)
	case *kind.MaxContains:
		return fmt.Sprintf("%d items are as the schema asks some to be, and at most %d may be", len(k.Got), k.Want)
	}
	if path := k.KeywordPath(); len(path) > 0 {
		return "the value breaks the rule " + path[len(path)-1]
	}
	return "the value breaks a rule of the schema"
}

// shownNames is how many names a message lists.
const shownNames = 5

// some lists names as a message quotes them: the first few, each in its
// first bytes, and how many more there are.
func some(names []string) string {
	quoted := make([]string, 0, shownNames)
	for _, name := range names[:min(len(names), shownNames)] {
		quoted = append(quoted, "'"+clipTo(name, 40)+"'")
	}
	out := strings.Join(quoted, ", ")
	if more := len(names) - len(quoted); more > 0 {
		out += fmt.Sprintf(" and %d more", more)
	}
	return out
}

// few lists the values a schema allows, when they are few and each is a
// short text, a number, true, false or null: a model that is asked to
// repair a reply is helped by seeing them, and a long list or a value that
// is an object says no more than the schema it was given.
func few(values []any) (listed string, ok bool) {
	if len(values) > shownNames {
		return "", false
	}
	out := make([]string, len(values))
	for i, v := range values {
		switch v := v.(type) {
		case string:
			if len(v) > 40 {
				return "", false
			}
			out[i] = "'" + v + "'"
		case float64, bool, nil:
			out[i] = string(encode(v))
		default:
			return "", false
		}
	}
	return strings.Join(out, ", "), true
}

// clipTo is the first bytes of a text, and the mark of a cut.
func clipTo(s string, most int) string {
	if len(s) > most {
		return s[:most] + "..."
	}
	return s
}

// partial reports whether a rule is one a part of a document cannot be held
// to: a member, an item or a property that another part may hold.
func partial(k jsonschema.ErrorKind) bool {
	switch k.(type) {
	case *kind.Required, *kind.MinItems, *kind.MinProperties, *kind.MinContains, *kind.Contains,
		*kind.DependentRequired:
		return true
	}
	return false
}

// escape writes a member's name as a token of a JSON pointer.
func escape(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}

// shown is how many rules a failure names.
const shown = 5

// Broken is the detail a field that failed validation carries: the rules of
// the schema the last reply broke, each by its place in the schema, or why
// the reply was not held to the schema. It names the caller's schema and
// never a value of the reply, which is content of the document.
func Broken(findings []Finding) string {
	if unchecked(findings) {
		return findings[0].Message
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
	places := strconv.Itoa(len(findings)) + " places"
	if len(findings) >= MaxFindings {
		places += " or more"
	}
	return "the object does not satisfy the schema in " + places + ", at " + strings.Join(rules, ", ")
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

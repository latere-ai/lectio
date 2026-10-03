// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package chat

import (
	"context"
	"encoding/json"
	"maps"
	"slices"

	"latere.ai/x/lectio/internal/prompts"
	"latere.ai/x/lectio/reader"
)

// Extractor fills a schema from a document's text through chat completions.
type Extractor struct{ c *client }

// NewExtractor builds an extractor. It refuses a configuration with no
// name, no model, or no usable endpoint.
func NewExtractor(cfg Config) (*Extractor, error) {
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Extractor{c: c}, nil
}

// Describe says what the extractor accepts.
func (e *Extractor) Describe() reader.ExtractorDescription {
	return reader.ExtractorDescription{Name: e.c.cfg.Name, MaxInput: e.c.cfg.MaxInput, Constrained: e.c.cfg.Constrain}
}

// citationsSchema is the shape citations are asked for in. A list of
// pointer and refs, and not an object keyed by pointer, because constrained
// decoding does not accept an object whose keys are not known in advance.
const citationsSchema = `{"type":"array","items":{"type":"object","additionalProperties":false,"required":["pointer","refs"],"properties":{"pointer":{"type":"string"},"refs":{"type":"array","items":{"type":"string"}}}}}`

// Extract asks the model for an object in the shape of the schema.
func (e *Extractor) Extract(ctx context.Context, in reader.ExtractRequest) (reader.ExtractResult, error) {
	if len(in.Text) > e.c.cfg.MaxInput {
		return reader.ExtractResult{}, reader.Errorf(reader.Permanent, "the text is %d characters, past this extractor's bound of %d", len(in.Text), e.c.cfg.MaxInput)
	}
	ask, err := prompts.Extract(prompts.ExtractData{
		Text: in.Text, Schema: string(in.Schema), Citations: in.Citations, Instructions: in.Instructions,
		Previous: in.Previous, Problems: in.Problems,
	})
	if err != nil {
		return reader.ExtractResult{}, &reader.Error{Class: reader.Misconfigured, Detail: "the extraction prompt does not render", Err: err}
	}

	var caller any
	if err := json.Unmarshal(in.Schema, &caller); err != nil {
		return reader.ExtractResult{}, reader.Errorf(reader.Permanent, "the schema is not JSON")
	}
	constrain := in.Constrain && e.c.cfg.Constrain
	req := e.c.request([]part{{Type: "text", Text: ask}})
	if constrain {
		// A decoder that enforces a schema must be allowed to write null
		// for what the document does not state. The caller's schema as it
		// stands may forbid that, and would force a value to be invented.
		wire, err := json.Marshal(strict(caller, false))
		if err != nil {
			return reader.ExtractResult{}, &reader.Error{Class: reader.Permanent, Detail: "the schema could not be encoded", Err: err}
		}
		wrapped := `{"type":"object","additionalProperties":false,"required":["data","citations"],"properties":{"data":` + string(wire) + `,"citations":` + citationsSchema + `}}`
		req.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: jsonSchema{Name: "extraction", Strict: true, Schema: json.RawMessage(wrapped)}}
	}

	done, err := e.c.complete(ctx, in.Credential, req)
	if err != nil {
		return reader.ExtractResult{}, err
	}
	if done.truncated {
		return reader.ExtractResult{}, reader.Errorf(reader.Invalid, "the reply ended at the model's output limit")
	}
	var reply struct {
		Data      json.RawMessage `json:"data"`
		Citations []struct {
			Pointer string   `json:"pointer"`
			Refs    []string `json:"refs"`
		} `json:"citations"`
	}
	if decode(done.content, &reply) != nil || len(reply.Data) == 0 || string(reply.Data) == "null" {
		return reader.ExtractResult{}, reader.Errorf(reader.Invalid, "the reply is not the JSON the instruction asks for")
	}

	// A null stands for "the document does not say". Where the caller's
	// schema has no place for a null, the member is left out, so the
	// caller's validator sees a value that is missing and never one that
	// was made up.
	var data any
	if err := json.Unmarshal(reply.Data, &data); err != nil {
		return reader.ExtractResult{}, reader.Errorf(reader.Invalid, "the reply's data is not JSON")
	}
	stripped, err := json.Marshal(dropNulls(data, caller))
	if err != nil {
		return reader.ExtractResult{}, &reader.Error{Class: reader.Invalid, Detail: "the reply's data could not be encoded", Err: err}
	}

	out := reader.ExtractResult{Data: stripped, Model: done.model, Usage: done.usage, Constrained: constrain}
	if in.Citations {
		out.Citations = make(map[string][]string, len(reply.Citations))
		for _, c := range reply.Citations {
			if c.Pointer != "" && len(c.Refs) > 0 {
				out.Citations[c.Pointer] = append(out.Citations[c.Pointer], c.Refs...)
			}
		}
	}
	return out, nil
}

// strict derives, from a caller's schema, the schema a constrained decoder
// is given. Such a decoder takes only a closed form: every object lists
// all its properties as required and admits no other. The derived schema
// is that form with every property made nullable, so the model can say
// that the document does not state a value. nullable says whether this
// node itself may be null.
func strict(schema any, nullable bool) any {
	node, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	out := make(map[string]any, len(node)+2)
	maps.Copy(out, node)

	if props, ok := node["properties"].(map[string]any); ok {
		closed := make(map[string]any, len(props))
		for name, prop := range props {
			closed[name] = strict(prop, true)
		}
		out["properties"] = closed
		out["required"] = slices.Sorted(maps.Keys(props))
		out["additionalProperties"] = false
	}
	if items, ok := node["items"]; ok {
		out["items"] = strict(items, false)
	}
	for _, key := range []string{"$defs", "definitions"} {
		if defs, ok := node[key].(map[string]any); ok {
			derived := make(map[string]any, len(defs))
			for name, def := range defs {
				derived[name] = strict(def, false)
			}
			out[key] = derived
		}
	}
	if alts, ok := node["anyOf"].([]any); ok {
		derived := make([]any, 0, len(alts)+1)
		for _, alt := range alts {
			derived = append(derived, strict(alt, false))
		}
		out["anyOf"] = derived
	}
	if !nullable || admitsNull(node) {
		return out
	}

	switch t := node["type"].(type) {
	case string:
		out["type"] = []any{t, "null"}
	case []any:
		out["type"] = append(slices.Clone(t), "null")
	default:
		// A reference or a choice has no type to widen: null becomes one
		// more alternative.
		if alts, ok := out["anyOf"].([]any); ok {
			out["anyOf"] = append(alts, map[string]any{"type": "null"})
			return out
		}
		return map[string]any{"anyOf": []any{out, map[string]any{"type": "null"}}}
	}
	if enum, ok := node["enum"].([]any); ok {
		out["enum"] = append(slices.Clone(enum), nil)
	}
	return out
}

// admitsNull reports whether a schema node already allows null.
func admitsNull(node map[string]any) bool {
	switch t := node["type"].(type) {
	case string:
		return t == "null"
	case []any:
		return slices.Contains(t, any("null"))
	}
	alts, _ := node["anyOf"].([]any)
	return slices.ContainsFunc(alts, func(alt any) bool {
		m, ok := alt.(map[string]any)
		return ok && admitsNull(m)
	})
}

// dropNulls removes from data the object members that are null where the
// caller's schema has no place for a null.
func dropNulls(data, schema any) any {
	node, _ := schema.(map[string]any)
	switch v := data.(type) {
	case map[string]any:
		props, _ := node["properties"].(map[string]any)
		for name, member := range v {
			prop, _ := props[name].(map[string]any)
			if member == nil {
				if prop == nil || !admitsNull(prop) {
					delete(v, name)
				}
				continue
			}
			v[name] = dropNulls(member, props[name])
		}
	case []any:
		for i, item := range v {
			v[i] = dropNulls(item, node["items"])
		}
	}
	return data
}

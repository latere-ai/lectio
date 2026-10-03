// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package chat

import (
	"context"
	"encoding/json"
	"strings"

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
	if !json.Valid(in.Schema) {
		return reader.ExtractResult{}, reader.Errorf(reader.Permanent, "the schema is not JSON")
	}

	var ask strings.Builder
	ask.WriteString("Fill the schema below from the document that follows it. Use only what the document says. Where the document does not say, use null.\n")
	if in.Citations {
		ask.WriteString("Every line of the document begins with a ref in square brackets. For each value you fill, list the refs of the lines it was read from.\n")
	}
	ask.WriteString(`Reply with one JSON object and nothing else: {"data": <the object the schema describes>, "citations": [{"pointer": "<JSON pointer into data>", "refs": ["<ref>"]}]}`)
	if !in.Citations {
		ask.WriteString(" with an empty list of citations")
	}
	ask.WriteString(".\n")
	if s := strings.TrimSpace(in.Instructions); s != "" {
		ask.WriteString("\nInstructions:\n" + s + "\n")
	}
	ask.WriteString("\nSchema:\n" + string(in.Schema) + "\n")
	if len(in.Problems) > 0 {
		ask.WriteString("\nAn earlier reply did not satisfy the schema. Correct these:\n")
		for _, p := range in.Problems {
			ask.WriteString("- " + p + "\n")
		}
	}
	ask.WriteString("\nDocument:\n" + in.Text)

	constrain := in.Constrain && e.c.cfg.Constrain
	req := request{
		Model:     e.c.cfg.Model,
		MaxTokens: e.c.cfg.MaxOutputTokens,
		Messages:  []message{{Role: "user", Content: []part{{Type: "text", Text: ask.String()}}}},
	}
	if constrain {
		wrapped := `{"type":"object","additionalProperties":false,"required":["data","citations"],"properties":{"data":` + string(in.Schema) + `,"citations":` + citationsSchema + `}}`
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

	out := reader.ExtractResult{Data: reply.Data, Model: done.model, Usage: done.usage, Constrained: constrain}
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

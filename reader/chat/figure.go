// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package chat

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/lectio/internal/prompts"
	"latere.ai/x/lectio/reader"
)

// figureSchema is the reply schema of a figure, in the closed form a
// constrained decoder takes.
var figureSchema = func() json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"type", "description", "text"},
		"properties": map[string]any{
			"type":        map[string]any{"type": "string", "enum": reader.FigureTypes()},
			"description": map[string]any{"type": "string"},
			"text":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	})
	if err != nil {
		panic(err) // the schema is a literal; it cannot fail to encode
	}
	return raw
}()

// Describer says what a figure shows, through chat completions. It sends
// the figure the way a reader sends a page: one user message holding the
// instruction and the image, with the parameters the configuration names.
type Describer struct{ c *client }

// NewDescriber builds a describer. It refuses a configuration with no
// name, no model, or no usable endpoint.
func NewDescriber(cfg Config) (*Describer, error) {
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Describer{c: c}, nil
}

// Describe says what the describer accepts.
func (d *Describer) Describe() reader.DescriberDescription {
	return reader.DescriberDescription{Name: d.c.cfg.Name, Accepts: slices.Clone(imageTypes), Version: d.version()}
}

// version names what this describer's configuration decides about a
// figure's description: the model, the prompt, whether the reply is
// constrained, the temperature, and the output bound.
func (d *Describer) version() string {
	cfg := d.c.cfg
	temperature := "default"
	if cfg.Temperature != nil {
		temperature = strconv.FormatFloat(*cfg.Temperature, 'g', -1, 64)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		cfg.Model, prompts.FigureVersion(reader.FigureTypes()), strconv.FormatBool(cfg.Constrain), temperature,
		strconv.Itoa(cfg.MaxOutputTokens),
	}, "\x00")))
	return hex.EncodeToString(sum[:6])
}

// DescribeFigure sends the figure to the model and returns what it says
// the figure shows.
func (d *Describer) DescribeFigure(ctx context.Context, req reader.FigureRequest) (reader.FigureResult, error) {
	if !slices.Contains(imageTypes, req.MediaType) {
		return reader.FigureResult{}, reader.Errorf(reader.Permanent, "a chat describer takes a PNG or a JPEG, and the figure is %s", req.MediaType)
	}
	instruction, err := prompts.Figure(prompts.FigureData{Types: reader.FigureTypes(), Caption: req.Caption, Languages: req.Languages})
	if err != nil {
		return reader.FigureResult{}, &reader.Error{Class: reader.Misconfigured, Detail: "the figure prompt does not render", Err: err}
	}
	ask := d.c.request([]part{
		{Type: "text", Text: instruction},
		{Type: "image_url", ImageURL: &imageURL{URL: "data:" + req.MediaType + ";base64," + base64.StdEncoding.EncodeToString(req.Data)}},
	})
	if d.c.cfg.Constrain {
		ask.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: jsonSchema{Name: "figure", Strict: true, Schema: figureSchema}}
	}

	done, err := d.c.complete(ctx, req.Credential, ask)
	if err != nil {
		return reader.FigureResult{}, err
	}
	if done.truncated {
		// A description is short. One that ran to the output limit is a
		// model that did not stop, and half a sentence is not kept.
		return reader.FigureResult{}, reader.Errorf(reader.Invalid, "the reply ended at the model's output limit")
	}
	var reply struct {
		Type        string   `json:"type"`
		Description string   `json:"description"`
		Text        []string `json:"text"`
	}
	if decode(done.content, &reply) != nil || strings.TrimSpace(reply.Description) == "" {
		return reader.FigureResult{}, reader.Errorf(reader.Invalid, "the reply is not the JSON the instruction asks for")
	}

	out := reader.FigureResult{
		Type:        reader.FigureType(strings.ToLower(strings.TrimSpace(reply.Type))),
		Description: strings.TrimSpace(reply.Description),
		Model:       done.model,
		Usage:       done.usage,
	}
	for _, label := range reply.Text {
		if label = strings.TrimSpace(label); label != "" {
			out.Labels = append(out.Labels, label)
		}
	}
	return out, nil
}

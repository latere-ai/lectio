// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package chat reaches a model through OpenAI-compatible chat completions:
// one user message holding an instruction and, for a page, the page as an
// image, with a JSON schema as the response format. That shape is served by
// model gateways, by several providers directly, and by local model servers,
// so this one adapter reaches most models worth configuring. When the
// endpoint is a gateway, the gateway owns the differences between vendors.
//
// The package has two types over one client: Reader reads a page, and
// Extractor fills a schema from text.
package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"latere.ai/x/pkg/llmjson"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/reader"
)

// Config is how a chat reader or extractor reaches its model.
type Config struct {
	// Name is the configured name of the reader or extractor.
	Name string

	// Endpoint is the base URL of the API, the part before
	// "/chat/completions": for example https://gateway.example/v1.
	Endpoint string

	// Model is passed through as the request's model. This package assigns
	// it no meaning.
	Model string

	// Image is how a page is rendered for the model. Zero fields take the
	// defaults: 160 dpi, a long edge of 2048 pixels, PNG.
	Image reader.ImageSpec

	// Constrain sends the reply schema as the response format, for an
	// endpoint that enforces one. Without it the schema is stated in the
	// instruction alone.
	Constrain bool

	// Boxes is how this model is asked for positions. Model families are
	// trained on different conventions, and a model asked in another
	// places boxes worse, or returns valid numbers in the wrong order. The
	// zero value asks for [x0, y0, x1, y1] on a grid of 1000.
	Boxes Boxes

	// Temperature is sent as the request's temperature when it is set. It
	// is not set by default: several current models refuse a request that
	// names one.
	Temperature *float64

	// MaxOutputTokens bounds the reply. Zero takes 8192.
	MaxOutputTokens int

	// OutputLimit names the request member the bound is sent as:
	// "max_completion_tokens", the default, or "max_tokens" for an
	// endpoint that knows only the older name.
	OutputLimit string

	// MaxInput bounds the text of one extraction call, in characters. Zero
	// takes 400,000.
	MaxInput int

	// Timeout bounds one call. Zero takes two minutes.
	Timeout time.Duration

	// HTTPClient sends the requests. Nil takes a client that carries the
	// caller's trace to the endpoint.
	HTTPClient *http.Client
}

// Boxes is a convention for the position of a region.
type Boxes struct {
	// Order is the order of the four numbers: "xyxy" for [x0, y0, x1, y1],
	// the default, or "yxyx" for [y0, x0, y1, x1].
	Order string

	// Space is what the numbers measure: "grid", the default, a fixed
	// range of 1000 by 1000 over the page, or "pixels", the pixels of the
	// image as it was sent.
	Space string
}

// The members of a box convention.
const (
	OrderXY     = "xyxy"
	OrderYX     = "yxyx"
	SpaceGrid   = "grid"
	SpacePixels = "pixels"
)

// The names the output bound is sent under.
const (
	LimitCompletionTokens = "max_completion_tokens"
	LimitTokens           = "max_tokens"
)

// maxReply bounds how much of a response body is read. A reply longer than
// this is not a page of blocks.
const maxReply = 16 << 20

// client is what Reader and Extractor share: the endpoint, the model, and
// the one request shape.
type client struct {
	cfg  Config
	url  string
	http *http.Client
}

func newClient(cfg Config) (*client, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, errors.New("chat: the configuration names no reader")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("chat: reader %q names no model", cfg.Name)
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("chat: reader %q has no usable endpoint", cfg.Name)
	}
	if cfg.Image.DPI <= 0 {
		cfg.Image.DPI = 160
	}
	if cfg.Image.LongEdge <= 0 {
		cfg.Image.LongEdge = 2048
	}
	if cfg.Image.Format == "" {
		cfg.Image.Format = "png"
	}
	if cfg.Image.Format != "png" && cfg.Image.Format != "jpeg" {
		return nil, fmt.Errorf("chat: reader %q asks for image format %q, which is neither png nor jpeg", cfg.Name, cfg.Image.Format)
	}
	if cfg.Boxes.Order == "" {
		cfg.Boxes.Order = OrderXY
	}
	if cfg.Boxes.Space == "" {
		cfg.Boxes.Space = SpaceGrid
	}
	if (cfg.Boxes.Order != OrderXY && cfg.Boxes.Order != OrderYX) || (cfg.Boxes.Space != SpaceGrid && cfg.Boxes.Space != SpacePixels) {
		return nil, fmt.Errorf("chat: reader %q asks for boxes as %q in %q; the order is xyxy or yxyx and the space is grid or pixels", cfg.Name, cfg.Boxes.Order, cfg.Boxes.Space)
	}
	if cfg.OutputLimit == "" {
		cfg.OutputLimit = LimitCompletionTokens
	}
	if cfg.OutputLimit != LimitCompletionTokens && cfg.OutputLimit != LimitTokens {
		return nil, fmt.Errorf("chat: reader %q sends its output bound as %q, which is neither max_completion_tokens nor max_tokens", cfg.Name, cfg.OutputLimit)
	}
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = 8192
	}
	if cfg.MaxInput <= 0 {
		cfg.MaxInput = 400_000
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = otel.HTTPClient()
	}
	return &client{cfg: cfg, url: strings.TrimRight(cfg.Endpoint, "/") + "/chat/completions", http: hc}, nil
}

// request is the body of a chat completions call.
type request struct {
	Model               string          `json:"model"`
	Temperature         *float64        `json:"temperature,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Messages            []message       `json:"messages"`
	ResponseFormat      *responseFormat `json:"response_format,omitempty"`
}

// request builds a request holding one user message. It sends what the
// configuration names and nothing else: no sampling parameter the operator
// did not ask for, and the output bound under the one name configured.
func (c *client) request(content []part) request {
	req := request{Model: c.cfg.Model, Temperature: c.cfg.Temperature, Messages: []message{{Role: "user", Content: content}}}
	if c.cfg.OutputLimit == LimitTokens {
		req.MaxTokens = c.cfg.MaxOutputTokens
	} else {
		req.MaxCompletionTokens = c.cfg.MaxOutputTokens
	}
	return req
}

type message struct {
	Role    string `json:"role"`
	Content []part `json:"content"`
}

type part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

// response is what the adapter reads of a chat completions reply.
type response struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content json.RawMessage `json:"content"`
			Refusal string          `json:"refusal"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// completion is one reply, reduced to what both callers need.
type completion struct {
	content   string
	model     string
	usage     document.Usage
	truncated bool
}

// complete sends one request and returns the model's text. Every failure is
// a *reader.Error: a transport failure and a 5xx are retryable, a rate
// limit says how long to wait, a spent budget is its own class, a request
// the endpoint rejects is the reader's misconfiguration, and content the
// model declines is refused.
func (c *client) complete(ctx context.Context, credential reader.Credential, req request) (completion, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return completion{}, &reader.Error{Class: reader.Permanent, Detail: "the request could not be encoded", Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return completion{}, &reader.Error{Class: reader.Permanent, Detail: "the request could not be built", Err: err}
	}
	hreq.Header.Set("Content-Type", "application/json")
	if !credential.IsZero() {
		hreq.Header.Set("Authorization", "Bearer "+credential.Reveal())
	}

	hres, err := c.http.Do(hreq)
	if err != nil {
		return completion{}, reader.FromTransport(err)
	}
	defer func() { _ = hres.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(hres.Body, maxReply))
	if err != nil {
		return completion{}, reader.FromTransport(err)
	}
	if hres.StatusCode < 200 || hres.StatusCode > 299 {
		return completion{}, reader.FromStatus(hres.StatusCode, hres.Header, raw)
	}

	var res response
	if err := json.Unmarshal(raw, &res); err != nil || len(res.Choices) == 0 {
		return completion{}, reader.Errorf(reader.Invalid, "the endpoint's reply is not a chat completion")
	}
	choice := res.Choices[0]
	if choice.Message.Refusal != "" || choice.FinishReason == "content_filter" {
		return completion{}, reader.Errorf(reader.Refused, "the model declined the content")
	}
	return completion{
		content:   text(choice.Message.Content),
		model:     res.Model,
		usage:     document.Usage{InputTokens: res.Usage.PromptTokens, OutputTokens: res.Usage.CompletionTokens},
		truncated: choice.FinishReason == "length",
	}, nil
}

// text reads a message's content, which an endpoint sends either as a
// string or as a list of parts.
func text(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var parts []part
	if json.Unmarshal(content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// decode reads the model's JSON into v. A reply is decoded as it arrived,
// and only when that fails is it decoded again with its code fence removed
// and its control characters escaped, so a well-formed answer is never
// rewritten.
func decode(content string, v any) error {
	if json.Unmarshal([]byte(content), v) == nil {
		return nil
	}
	if json.Unmarshal([]byte(llmjson.Repair(content)), v) == nil {
		return nil
	}
	return json.Unmarshal([]byte(llmjson.Repair(repairEscapes(content))), v)
}

// latex are the commands whose first letter is also a JSON escape. A model
// that writes a formula into a JSON string with a single backslash writes
// the command frac as a form feed followed by "rac", which is valid JSON
// and a corrupt formula. The command is told from a real escape by its
// whole word.
var latex = map[string]bool{
	"bar": true, "begin": true, "beta": true, "big": true, "bigg": true, "bigcap": true, "bigcup": true,
	"binom": true, "bm": true, "boldsymbol": true, "bot": true, "boxed": true, "bullet": true,
	"frac": true, "forall": true, "flat": true,
	"nabla": true, "neq": true, "neg": true, "notin": true, "nu": true, "nmid": true, "nolimits": true,
	"nonumber": true, "newline": true, "nleq": true, "ngeq": true,
	"rho": true, "right": true, "rightarrow": true, "rangle": true, "rceil": true, "rfloor": true,
	"rvert": true, "rbrace": true, "rbrack": true,
	"tau": true, "text": true, "textbf": true, "textit": true, "textrm": true, "texttt": true, "theta": true,
	"tilde": true, "times": true, "to": true, "top": true, "triangle": true, "tfrac": true, "tanh": true,
	"therefore": true,
}

// repairEscapes doubles the backslashes inside JSON strings that begin a
// LaTeX command and not a JSON escape: a backslash before a character JSON
// does not escape (the first of alpha, or a parenthesis), a u that four
// hex digits do not follow, and a backslash before one of the commands in
// latex. A string that was escaped correctly is not changed.
func repairEscapes(content string) string {
	var out strings.Builder
	inString := false
	for i := 0; i < len(content); i++ {
		c := content[i]
		if c == '"' {
			inString = !inString
		}
		if c != '\\' || !inString || i+1 >= len(content) {
			out.WriteByte(c)
			continue
		}
		next := content[i+1]
		word := i + 1
		for word < len(content) && (content[word] >= 'a' && content[word] <= 'z' || content[word] >= 'A' && content[word] <= 'Z') {
			word++
		}
		command := content[i+1 : word]
		switch {
		case next == '"' || next == '\\' || next == '/':
			// A real escape of two characters: both are kept as they are.
			out.WriteByte(c)
			out.WriteByte(next)
			i++
		case next == 'u' && hex4(content[min(i+2, len(content)):]):
			out.WriteByte(c)
		case strings.ContainsRune("bfnrt", rune(next)) && !latex[command]:
			out.WriteByte(c)
		default:
			out.WriteString(`\\`)
		}
	}
	return out.String()
}

// hex4 reports whether s begins with four hexadecimal digits.
func hex4(s string) bool {
	if len(s) < 4 {
		return false
	}
	for _, c := range []byte(s[:4]) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// corrupt reports whether decoded text holds a control character that no
// transcription holds: a backspace, a form feed, a tab or a carriage
// return, which is what a command such as beta, frac, theta or rho decodes
// to when its backslash was not escaped.
func corrupt(text string) bool { return strings.ContainsAny(text, "\b\f\t\r") }

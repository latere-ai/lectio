// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package layout reaches an OCR or layout engine behind a small HTTP
// contract: one page image in, the regions on it out. It is the adapter for
// an engine someone runs themselves, where chat is the adapter for a hosted
// model. Anything that can answer the contract below can read pages for
// Lectio, whatever model is behind it.
//
// The request is a POST of multipart/form-data with one file field, file,
// holding the page as a PNG or a JPEG, and an optional field, languages,
// holding comma-separated hints. A key, when one is configured for the
// call, is sent as a bearer.
//
// The reply is JSON:
//
//	{
//	  "elements": [
//	    {"category": "Table", "bbox": [x0, y0, x1, y1], "text": "...", "reading_order": 3}
//	  ],
//	  "model": "the engine's name for itself",
//	  "usage": {"input_tokens": 0, "output_tokens": 0}
//	}
//
// bbox is in pixels of the image that was sent, origin at the top left.
// category is the engine's own word for the kind of region; the names
// engines commonly use are understood, and one nobody knows becomes text.
// A table's text may be its HTML. reading_order may be left out, and then
// the order of the list is the reading order. An engine that is still
// loading answers 503, with Retry-After when it knows how long.
package layout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"time"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/reader"
)

// Config is how a layout reader reaches its engine.
type Config struct {
	// Name is the reader's configured name.
	Name string

	// Endpoint is the URL the page is posted to.
	Endpoint string

	// Image is how a page is rendered for the engine. Zero fields take the
	// defaults: 200 dpi, no bound on the long edge, PNG.
	Image reader.ImageSpec

	// Timeout bounds one call. Zero takes five minutes, since an engine
	// that scales to zero may load its model on the first call.
	Timeout time.Duration

	// HTTPClient sends the requests. Nil takes a client that carries the
	// caller's trace to the engine.
	HTTPClient *http.Client
}

// maxReply bounds how much of a response body is read.
const maxReply = 16 << 20

// imageTypes are the media types a layout reader takes.
var imageTypes = []string{"image/png", "image/jpeg"}

// Reader reads a page through a layout engine.
type Reader struct {
	cfg  Config
	http *http.Client
}

// New builds a reader. It refuses a configuration with no name or no usable
// endpoint.
func New(cfg Config) (*Reader, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, errors.New("layout: the configuration names no reader")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("layout: reader %q has no usable endpoint", cfg.Name)
	}
	if cfg.Image.DPI <= 0 {
		cfg.Image.DPI = 200
	}
	if cfg.Image.Format == "" {
		cfg.Image.Format = "png"
	}
	if cfg.Image.Format != "png" && cfg.Image.Format != "jpeg" {
		return nil, fmt.Errorf("layout: reader %q asks for image format %q, which is neither png nor jpeg", cfg.Name, cfg.Image.Format)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Minute
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = otel.HTTPClient()
	}
	return &Reader{cfg: cfg, http: hc}, nil
}

// Describe says what the reader accepts and returns.
func (r *Reader) Describe() reader.Description {
	accepts := slices.Clone(imageTypes)
	if r.cfg.Image.Format == "jpeg" {
		slices.Reverse(accepts)
	}
	return reader.Description{Name: r.cfg.Name, Accepts: accepts, Image: r.cfg.Image, Boxes: true}
}

// reply is the engine's answer.
type reply struct {
	Elements *[]struct {
		Category     string    `json:"category"`
		BBox         []float64 `json:"bbox"`
		Text         string    `json:"text"`
		ReadingOrder int       `json:"reading_order"`
	} `json:"elements"`
	Model string `json:"model"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// ReadPage posts the page to the engine and returns its blocks.
func (r *Reader) ReadPage(ctx context.Context, page reader.Page) (reader.Result, error) {
	if !slices.Contains(imageTypes, page.MediaType) {
		return reader.Result{}, reader.Errorf(reader.Permanent, "a layout reader takes a PNG or a JPEG, and the page is %s", page.MediaType)
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="page"`)
	header.Set("Content-Type", page.MediaType)
	// Writes to a bytes.Buffer do not fail, so neither do the form's.
	part, _ := form.CreatePart(header)
	_, _ = part.Write(page.Data)
	if len(page.Languages) > 0 {
		_ = form.WriteField("languages", strings.Join(page.Languages, ","))
	}
	_ = form.Close()

	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.Endpoint, &body)
	if err != nil {
		return reader.Result{}, &reader.Error{Class: reader.Permanent, Detail: "the request could not be built", Err: err}
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	if !page.Credential.IsZero() {
		req.Header.Set("Authorization", "Bearer "+page.Credential.Reveal())
	}

	res, err := r.http.Do(req)
	if err != nil {
		return reader.Result{}, reader.FromTransport(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxReply))
	if err != nil {
		return reader.Result{}, reader.FromTransport(err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return reader.Result{}, reader.FromStatus(res.StatusCode, res.Header, raw)
	}

	var out reply
	if json.Unmarshal(raw, &out) != nil || out.Elements == nil {
		return reader.Result{}, reader.Errorf(reader.Invalid, "the engine's reply holds no list of elements")
	}
	raws := make([]reader.Raw, 0, len(*out.Elements))
	for _, e := range *out.Elements {
		raws = append(raws, reader.Raw{Label: e.Category, Text: e.Text, Box: e.BBox, Order: e.ReadingOrder})
	}
	return reader.Result{
		Blocks: reader.Normalize(raws, reader.Grid{Width: float64(page.Width), Height: float64(page.Height)}),
		Model:  out.Model,
		Usage:  document.Usage{Pages: 1, InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens},
	}, nil
}

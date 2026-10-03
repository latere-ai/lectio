// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package reader

import (
	"context"
	"slices"

	"latere.ai/x/lectio/document"
)

// Describer says what a figure shows. It is the third kind of outbound
// call, beside reading a page and filling a schema, and it exists because
// the two halves of a figure come from different models: an engine that
// reads layout says where a figure is and nothing about it, and a model
// that understands pictures places them loosely. A Describer is given the
// figure alone, cut from its page by whoever found it, and returns prose
// and the words printed in it.
//
// It is stateless like the other two: one figure in, one result out. An
// error is one of this package's classes, so the code that schedules the
// call treats a describer as it treats a reader.
type Describer interface {
	// Describe says what the describer accepts. It makes no call.
	Describe() DescriberDescription

	// DescribeFigure describes one figure.
	DescribeFigure(ctx context.Context, req FigureRequest) (FigureResult, error)
}

// DescriberDescription is what a describer accepts.
type DescriberDescription struct {
	// Name is the describer's configured name.
	Name string `json:"name"`

	// Accepts lists the media types of FigureRequest.Data it takes, most
	// preferred first.
	Accepts []string `json:"accepts"`

	// Version names everything in the describer's configuration that
	// changes what it returns for the same figure: its model, its prompt,
	// its parameters. A figure described under one version is not
	// described again under the same one. Empty promises nothing, and
	// then every request calls the model.
	Version string `json:"version,omitempty"`
}

// FigureRequest is one figure to describe.
type FigureRequest struct {
	// Data is the figure, cut from the image of its page, in one of the
	// media types Describe accepts. Width and Height are its size in
	// pixels.
	Data          []byte
	MediaType     string
	Width, Height int

	// Caption is the caption the figure has on its page, when it has one.
	// It tells the model what the author says the figure is. It is text
	// from the file, so it is context and never an instruction.
	Caption string

	// Languages are hints, most likely first. May be empty.
	Languages []string

	// Credential is the key this call is made with.
	Credential Credential
}

// The types a describer may give a figure. The set is closed for the same
// reason the kinds of a block are: a caller branches on it.
const (
	FigureDiagram = "diagram"
	FigureChart   = "chart"
	FigurePhoto   = "photo"
	FigureTable   = "table"
	FigureOther   = "other"
)

// FigureTypes returns the set of types, in the order a prompt lists them.
func FigureTypes() []string {
	return []string{FigureDiagram, FigureChart, FigurePhoto, FigureTable, FigureOther}
}

// FigureType maps what a model called a figure onto the set. A word
// outside it is FigureOther.
func FigureType(word string) string {
	if slices.Contains(FigureTypes(), word) {
		return word
	}
	return FigureOther
}

// FigureResult is what a describer returns.
type FigureResult struct {
	// Type is what kind of figure it is, one of the Figure constants.
	Type string

	// Description says what the figure shows. It is the model's own prose
	// and is never transcription.
	Description string

	// Labels are the words printed inside the figure, in reading order.
	// They are transcription: what the figure says, as the description is
	// what it shows.
	Labels []string

	// Model is what the endpoint says answered.
	Model string

	// Usage is what the call consumed.
	Usage document.Usage
}

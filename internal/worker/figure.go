// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"encoding/json"
	"errors"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/figures"
	"latere.ai/x/lectio/internal/objects"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/reader"
)

// figureUnit is a figure and its describer, in the reasons a failed
// description is recorded with.
var figureUnit = unit{what: "figure", who: "describer", unreadable: fault.FigureUnreadable}

// reused is what a figure task says of a description it took from an
// earlier run.
var reused = json.RawMessage(`{"reused":true}`)

// figure describes one figure of a parse that has ended: it takes the
// description an earlier run kept for the same figure when there is one,
// and otherwise resolves the key of the figure's group, cuts the figure
// from the image of its page, has the describer it was claimed for
// describe it with its caption, and writes what came back under this
// task's token. Where the figure is, and what its caption is, is read from
// the page's stored result, so the task is told nothing of the page's
// content.
func (w *Worker) figure(ctx context.Context, c tasks.Claim, s *tasks.Settle) {
	ref, ok := tasks.FigureOf(c.Task)
	ask := c.Context.Figure
	if !ok || ask == nil {
		permanent(s, fault.Internal, "the task carries no figure")
		return
	}
	if ask.Reuse != "" && w.redescribed(ctx, c, ref, s) {
		return
	}
	d := w.Describers[c.Reader]
	if d == nil {
		// This process has no such describer: another may take the figure.
		s.Outcome = tasks.Next
		s.Error = &tasks.Error{Code: string(fault.ReaderUnavailable), Detail: "the describer the figure was claimed for is not configured"}
		return
	}
	// The key is resolved before anything is fetched or cut: a figure that
	// has none yet goes back to the queue having cost nothing.
	req := reader.FigureRequest{Languages: c.Context.Languages}
	if w.Keys != nil {
		key, err := w.Keys.Key(ctx, c.Group, c.Context.Owner, c.Parse)
		if err != nil {
			keyless(s, err)
			return
		}
		req.Credential = key
	}

	page, err := objects.GetPage(ctx, w.Objects, ask.Page)
	if err != nil {
		unstored(s, "the figure's page", err)
		return
	}
	at := -1
	for i, b := range page.Page.Blocks {
		if b.Ref == ref && b.Kind == document.KindFigure && b.Box != nil {
			at = i
		}
	}
	switch {
	case at < 0:
		permanent(s, fault.FigureUnreadable, "the page holds no figure with a place on it under the ref")
		return
	case page.Image == "":
		permanent(s, fault.FigureUnreadable, "the figure's page has no image")
		return
	}
	data, mediaType, err := w.Objects.Get(ctx, page.Image)
	if err != nil {
		unstored(s, "the image of the figure's page", err)
		return
	}
	cut, err := render.Crop(render.Image{Data: data, MediaType: mediaType}, *page.Page.Blocks[at].Box)
	if err != nil {
		// The page's image and the figure's box are what was read, and no
		// other attempt or describer changes them.
		permanent(s, fault.FigureUnreadable, "the figure could not be cut from its page's image")
		return
	}
	req.Data, req.MediaType, req.Width, req.Height = cut.Data, cut.MediaType, cut.Width, cut.Height
	req.Caption = figures.Caption(page.Page.Blocks, at)

	res, err := d.DescribeFigure(ctx, req)
	if err != nil {
		w.failed(c, s, err, figureUnit)
		return
	}
	s.Usage = tasks.Usage{Calls: 1, InputTokens: res.Usage.InputTokens, OutputTokens: res.Usage.OutputTokens}
	s.Units, s.Health = w.cost(c.Reader), tasks.Healthy
	key := blob.FigureKey(c.Parse, ref, c.Token)
	if err := objects.Put(ctx, w.Objects, key, figures.Kept(res)); err != nil {
		unstored(s, "the figure's description", err)
		return
	}
	s.Outcome, s.Output = tasks.Done, key
}

// unstored ends a figure's attempt for an error of the object store. A
// page or an image that is no longer there cannot be cut a figure from,
// whatever is tried. Any other error says nothing about the figure, and the
// task is tried again.
func unstored(s *tasks.Settle, what string, err error) {
	if errors.Is(err, blob.ErrNotFound) {
		permanent(s, fault.FigureUnreadable, "%s is not in the object store", what)
		return
	}
	retryable(s, fault.Internal, "%s could not be read or written", what)
}

// redescribed takes the description an earlier run kept for the same
// figure: it is copied under this task's own key, so the parse depends on
// no object of another, and no model is called. It reports false when the
// description is no longer there, and the figure is then described.
func (w *Worker) redescribed(ctx context.Context, c tasks.Claim, ref string, s *tasks.Settle) bool {
	var kept figures.Description
	err := objects.Get(ctx, w.Objects, c.Context.Figure.Reuse, &kept)
	if errors.Is(err, blob.ErrNotFound) {
		return false
	}
	if err != nil {
		unstored(s, "the description kept for the figure", err)
		return true
	}
	key := blob.FigureKey(c.Parse, ref, c.Token)
	if err := objects.Put(ctx, w.Objects, key, kept); err != nil {
		unstored(s, "the figure's description", err)
		return true
	}
	s.Outcome, s.Output, s.Result = tasks.Done, key, reused
	return true
}

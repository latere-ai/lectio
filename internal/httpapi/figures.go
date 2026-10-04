// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/store"
)

// getBlockImage serves a block as it looks on its page: the page's image
// cut to the block's box. The image is the one a reader saw, so the cut is
// exactly the region the block's box names.
func (s *Server) getBlockImage(w http.ResponseWriter, r *http.Request, owner string) error {
	ref, parseID := r.PathValue("ref"), r.PathValue("parse")
	n, _, err := document.ParseRef(ref)
	if err != nil {
		return invalid("ref", "a block's ref is <page>.<order>")
	}
	p, page, err := s.page(r.Context(), owner, parseID, n)
	if err != nil {
		return err
	}
	block, err := page.Find(ref)
	if err != nil {
		return fault.New(fault.BlockNotFound, "page %d holds no block %s", n, ref)
	}
	if block.Box == nil {
		return fault.New(fault.BlockNotFound, "block %s has no position on its page, so it has no image", ref)
	}
	img, ok, err := s.Backend.Image(r.Context(), p, n)
	if err != nil {
		return err
	}
	if !ok {
		return fault.New(fault.BlockNotFound, "page %d was not read from an image, so its blocks have none", n)
	}
	cut, err := render.Crop(img, *block.Box)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", cut.MediaType)
	_, _ = w.Write(cut.Data)
	return nil
}

// figuresRequest is the body of a request to describe a parse's figures.
// Every member is optional, and so is the body.
type figuresRequest struct {
	Pages     string `json:"pages"`
	Describer string `json:"describer"`
	Redo      bool   `json:"redo"`
}

// runView is a run that describes figures, as the API returns it.
type runView struct {
	State      string         `json:"state"`
	Total      int            `json:"total"`
	Done       int            `json:"done"`
	Failed     int            `json:"failed"`
	Reused     int            `json:"reused,omitempty"`
	Usage      document.Usage `json:"usage"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
}

// figureView is one figure of a parse: where it is, what is printed in
// it, and, once it was described, what it shows.
type figureView struct {
	Ref         string           `json:"ref"`
	Page        int              `json:"page"`
	Box         *document.Box    `json:"box"`
	Text        string           `json:"text"`
	Description string           `json:"description,omitempty"`
	Figure      *document.Figure `json:"figure,omitempty"`
	Error       *document.Error  `json:"error,omitempty"`
}

// figuresView is a parse's figures and the run that describes them.
type figuresView struct {
	Run     *runView     `json:"run,omitempty"`
	Figures []figureView `json:"figures"`
}

// figures builds the listing of a parse's figures. A figure whose last
// description failed says why.
func (s *Server) figures(ctx context.Context, p store.Parse) (figuresView, error) {
	out := figuresView{Figures: []figureView{}}
	current, started, err := s.Backend.FigureRun(ctx, p.ID)
	if err != nil {
		return out, err
	}
	read, err := s.Backend.Pages(ctx, p)
	if err != nil {
		return out, err
	}
	if started {
		out.Run = &runView{
			State: current.State, Total: current.Total, Done: current.Done, Failed: current.Failed, Reused: current.Reused,
			Usage: current.Usage, StartedAt: current.StartedAt, FinishedAt: current.FinishedAt,
		}
	}
	for _, page := range read {
		for _, b := range page.Blocks {
			if b.Kind != document.KindFigure {
				continue
			}
			f := figureView{Ref: b.Ref, Page: page.Number, Box: b.Box, Text: b.Text, Description: b.Description, Figure: b.Figure}
			if why, lost := current.Failures[b.Ref]; lost {
				f.Error = &why
			}
			out.Figures = append(out.Figures, f)
		}
	}
	return out, nil
}

// createFigures starts describing the figures of a parse that has ended.
// It is a request against a parse, as an extraction is: figures can be
// described after the fact, for some pages, without a page being read
// again.
func (s *Server) createFigures(w http.ResponseWriter, r *http.Request, owner string) error {
	p, err := s.Backend.Parse(r.Context(), owner, r.PathValue("parse"))
	if err != nil {
		return err
	}
	var req figuresRequest
	if _, err := decodeBody(w, r, &req, true); err != nil {
		return err
	}
	opt := run.FigureOptions{Describer: req.Describer, Redo: req.Redo}
	if selection := strings.TrimSpace(req.Pages); selection != "" {
		if err := pages.Check(selection); err != nil {
			return err
		}
		if p.Manifest == nil {
			return fault.New(fault.InvalidPages, "parse %s has no pages to select from", p.ID)
		}
		if opt.Pages, err = pages.Select(selection, p.Manifest.PagesTotal); err != nil {
			return err
		}
	}

	if err := s.Backend.Figures(r.Context(), p, opt); err != nil {
		return err
	}
	if d := wait(r); d > 0 {
		w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(int(d/time.Second)))
		s.Backend.WaitFigures(r.Context(), p.ID, d)
	}
	out, err := s.figures(r.Context(), p)
	if err != nil {
		return err
	}
	status := http.StatusAccepted
	if out.Run != nil && out.Run.State != store.RunRunning {
		status = http.StatusOK
	}
	httpjson.Write(w, status, out)
	return nil
}

// listFigures lists a parse's figures, described or not, and the run that
// describes them when one was started.
func (s *Server) listFigures(w http.ResponseWriter, r *http.Request, owner string) error {
	p, err := s.Backend.Parse(r.Context(), owner, r.PathValue("parse"))
	if err != nil {
		return err
	}
	out, err := s.figures(r.Context(), p)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, out)
	return nil
}

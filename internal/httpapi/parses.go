// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/id"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/pkg/httpjson"
)

// The bounds of a submit that the contract states.
const (
	maxBody       = 1 << 20 // bytes of JSON in one request
	maxKey        = 255     // bytes of an idempotency key
	maxLanguages  = 8
	maxLanguage   = 35
	maxLabels     = 16
	maxLabelValue = 256
	maxStore      = 64
	maxPath       = 1024
	maxVersion    = 64
	maxWait       = 60 // seconds a submit may be held
	defaultLimit  = 50
	maxLimit      = 200
)

// parseRequest is the body of a submit. A member the contract does not
// name is refused, so a caller finds a misspelled option at once and not in
// a result that ignored it.
type parseRequest struct {
	Source struct {
		File string `json:"file"`
		URL  string `json:"url"`
	} `json:"source"`
	Origin           *store.Origin     `json:"origin"`
	Pages            string            `json:"pages"`
	Reader           string            `json:"reader"`
	Languages        []string          `json:"languages"`
	Class            string            `json:"class"`
	Priority         int               `json:"priority"`
	AllowFailedPages int               `json:"allow_failed_pages"`
	Deadline         string            `json:"deadline"`
	Reuse            *bool             `json:"reuse"`
	Labels           map[string]string `json:"labels"`
}

// progress is where a parse stands.
type progress struct {
	Stage       string `json:"stage"`
	PagesTotal  int    `json:"pages_total"`
	PagesDone   int    `json:"pages_done"`
	PagesFailed int    `json:"pages_failed"`
}

// parseView is a parse as the API returns it.
type parseView struct {
	ID               string            `json:"id"`
	Owner            string            `json:"owner,omitempty"`
	State            string            `json:"state"`
	Reused           bool              `json:"reused,omitempty"`
	Class            string            `json:"class"`
	Priority         int               `json:"priority"`
	File             string            `json:"file"`
	Origin           *store.Origin     `json:"origin,omitempty"`
	Pages            string            `json:"pages,omitempty"`
	Reader           string            `json:"reader,omitempty"`
	Languages        []string          `json:"languages,omitempty"`
	AllowFailedPages int               `json:"allow_failed_pages,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	Progress         progress          `json:"progress"`
	Usage            document.Usage    `json:"usage"`
	Error            *document.Error   `json:"error,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	StartedAt        *time.Time        `json:"started_at,omitempty"`
	FinishedAt       *time.Time        `json:"finished_at,omitempty"`
	DeadlineAt       *time.Time        `json:"deadline_at,omitempty"`
}

func viewParse(p store.Parse) parseView {
	return parseView{
		ID: p.ID, Owner: p.Owner, State: p.State, Class: p.Class, Priority: p.Priority, File: p.File,
		Origin: p.Origin, Pages: p.Pages, Reader: p.Reader, Languages: p.Languages,
		AllowFailedPages: p.AllowFailedPages, Labels: p.Labels,
		Progress: progress{Stage: p.Stage, PagesTotal: p.PagesTotal, PagesDone: p.PagesDone, PagesFailed: p.PagesFailed},
		Usage:    p.Usage, Error: p.Error,
		CreatedAt: p.CreatedAt, StartedAt: p.StartedAt, FinishedAt: p.FinishedAt, DeadlineAt: p.DeadlineAt,
	}
}

// decode reads a JSON body into v and refuses what the contract does not
// describe: a member it does not name, a second value, a body over the
// limit.
func decode(w http.ResponseWriter, r *http.Request, v any) ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return nil, fault.New(fault.InvalidRequest, "the body is over %d bytes or stopped before its end", maxBody)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if name, found := strings.CutPrefix(err.Error(), "json: unknown field "); found {
			name = strings.Trim(name, `"`)
			return nil, fault.Wrap(fault.UnknownField, field(name), "the body has a member %q the contract does not describe", name)
		}
		if at, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			return nil, invalid(at.Field, "%s is a %s", at.Field, at.Type)
		}
		return nil, fault.New(fault.InvalidRequest, "the body is not a JSON object")
	}
	if dec.More() {
		return nil, fault.New(fault.InvalidRequest, "the body holds more than one JSON value")
	}
	return raw, nil
}

// validate checks a submit against the contract and returns the parse it
// asks for, without its file.
func (s *Server) validate(req parseRequest) (store.Parse, error) {
	p := store.Parse{
		Origin: req.Origin, Pages: strings.TrimSpace(req.Pages), Reader: req.Reader, Languages: req.Languages,
		Class: req.Class, Priority: req.Priority, AllowFailedPages: req.AllowFailedPages, Labels: req.Labels,
	}
	if (req.Source.File == "") == (req.Source.URL == "") {
		return p, invalid("source", "source has exactly one of file and url")
	}
	if o := req.Origin; o != nil && (len(o.Store) > maxStore || len(o.Path) > maxPath || len(o.Version) > maxVersion) {
		return p, invalid("origin", "origin's store, path and version are at most %d, %d and %d bytes", maxStore, maxPath, maxVersion)
	}
	if err := pages.Check(p.Pages); err != nil {
		return p, err
	}
	if p.Reader != "" && s.Readers[p.Reader] == nil {
		return p, fault.New(fault.ReaderNotFound, "no reader is named %q", p.Reader)
	}
	if len(p.Languages) > maxLanguages || slices.ContainsFunc(p.Languages, func(l string) bool { return l == "" || len(l) > maxLanguage }) {
		return p, invalid("languages", "languages holds at most %d tags of at most %d bytes", maxLanguages, maxLanguage)
	}
	switch p.Class {
	case "":
		p.Class = store.ClassInteractive
	case store.ClassInteractive, store.ClassBatch:
	default:
		return p, invalid("class", "class is interactive or batch")
	}
	if p.AllowFailedPages < 0 {
		return p, invalid("allow_failed_pages", "allow_failed_pages is zero or more")
	}
	if len(p.Labels) > maxLabels {
		return p, invalid("labels", "labels holds at most %d keys", maxLabels)
	}
	for k, v := range p.Labels {
		if k == "" || len(k) > maxLabelValue || len(v) > maxLabelValue {
			return p, invalid("labels", "a label's key and value are at most %d bytes, and the key is not empty", maxLabelValue)
		}
	}
	if req.Deadline != "" {
		limit := s.MaxDeadline
		if limit <= 0 {
			limit = time.Hour
		}
		d, err := time.ParseDuration(req.Deadline)
		if err != nil || d <= 0 || d > limit {
			return p, invalid("deadline", "deadline is a duration such as 10m, above zero and at most %s", limit)
		}
		at := s.now().Add(d)
		p.DeadlineAt = &at
	}
	return p, nil
}

// fingerprint is the options of a parse that change its result, as one
// value. Two parses of the same bytes with the same fingerprint do the
// same work. Class, priority, deadline and labels change when and for whom
// the work runs, and not what it produces, so they are left out.
func (s *Server) fingerprint(p store.Parse) string {
	chain := s.Chain
	if p.Reader != "" {
		chain = []string{p.Reader}
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		p.Pages, strings.Join(chain, ","), strings.Join(p.Languages, ","),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// wait reads how long a submit asked to be held from its Prefer header.
// A preference the server does not know is ignored, as preferences are.
func wait(r *http.Request) time.Duration {
	for _, value := range r.Header.Values("Prefer") {
		for pref := range strings.SplitSeq(value, ",") {
			if n, found := strings.CutPrefix(strings.TrimSpace(pref), "wait="); found {
				if secs, err := strconv.Atoi(n); err == nil && secs > 0 {
					return time.Duration(min(secs, maxWait)) * time.Second
				}
			}
		}
	}
	return 0
}

func (s *Server) createParse(w http.ResponseWriter, r *http.Request, owner string) error {
	key := r.Header.Get("Idempotency-Key")
	if len(key) > maxKey {
		return invalid("Idempotency-Key", "an idempotency key is at most %d bytes", maxKey)
	}
	var req parseRequest
	raw, err := decode(w, r, &req)
	if err != nil {
		return err
	}
	p, err := s.validate(req)
	if err != nil {
		return err
	}

	var file store.File
	if req.Source.URL != "" {
		if s.Fetcher == nil {
			return invalid("source.url", "this server fetches no url; upload the file and name it")
		}
		got, err := s.Fetcher.Fetch(r.Context(), req.Source.URL)
		if err != nil {
			return err
		}
		if file, _, err = s.putFile(owner, got.Name, got.MediaType, got.Data); err != nil {
			return err
		}
	} else if file, err = s.Store.File(owner, req.Source.File); err != nil {
		return err
	}

	p.ID, p.Owner, p.File = s.IDs.New(id.Parse), owner, file.ID
	p.State, p.Stage, p.CreatedAt = store.StateQueued, store.StageQueued, s.now()
	p.ContentSHA, p.Fingerprint = file.SHA256, s.fingerprint(p)

	if req.Reuse == nil || *req.Reuse {
		if earlier, ok := s.Store.Reusable(owner, p.ContentSHA, p.Fingerprint); ok {
			view := viewParse(earlier)
			view.Reused = true
			httpjson.Write(w, http.StatusOK, view)
			return nil
		}
	}

	body := sha256.Sum256(raw)
	stored, created, err := s.Store.CreateParse(p, key, hex.EncodeToString(body[:]))
	if err != nil {
		return err
	}
	if created {
		s.Runner.Submit(stored)
	}

	if d := wait(r); d > 0 {
		w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(int(d/time.Second)))
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-s.Runner.Done(stored.ID):
		case <-timer.C:
		case <-r.Context().Done():
		}
	}
	// The parse may have moved, or ended, since it was stored.
	if now, err := s.Store.Parse(owner, stored.ID); err == nil {
		stored = now
	}
	status := http.StatusAccepted
	if stored.Terminal() {
		status = http.StatusOK
	}
	httpjson.Write(w, status, viewParse(stored))
	return nil
}

func (s *Server) listParses(w http.ResponseWriter, r *http.Request, owner string) error {
	q := r.URL.Query()
	filter := store.Filter{State: q.Get("state"), File: q.Get("file"), OriginPath: q.Get("origin.path")}
	states := []string{"", store.StateQueued, store.StateRunning, store.StateSucceeded, store.StateFailed, store.StateCanceled}
	if !slices.Contains(states, filter.State) {
		return invalid("state", "state is one of queued, running, succeeded, failed, canceled")
	}
	for _, label := range q["label"] {
		k, v, found := strings.Cut(label, "=")
		if !found || k == "" {
			return invalid("label", "a label filter is key=value")
		}
		if filter.Labels == nil {
			filter.Labels = map[string]string{}
		}
		filter.Labels[k] = v
	}
	limit := defaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			return invalid("limit", "limit is from 1 to %d", maxLimit)
		}
		limit = n
	}

	found, more := s.Store.ListParses(owner, filter, q.Get("cursor"), limit)
	out := struct {
		Parses     []parseView `json:"parses"`
		NextCursor string      `json:"next_cursor,omitempty"`
	}{Parses: make([]parseView, 0, len(found))}
	for _, p := range found {
		out.Parses = append(out.Parses, viewParse(p))
	}
	if more {
		// Ids sort by time, so the last one on a page is where the next begins.
		out.NextCursor = found[len(found)-1].ID
	}
	httpjson.Write(w, http.StatusOK, out)
	return nil
}

func (s *Server) getParse(w http.ResponseWriter, r *http.Request, owner string) error {
	p, err := s.Store.Parse(owner, r.PathValue("parse"))
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, viewParse(p))
	return nil
}

func (s *Server) deleteParse(w http.ResponseWriter, r *http.Request, owner string) error {
	if err := s.Store.DeleteParse(owner, r.PathValue("parse")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) cancelParse(w http.ResponseWriter, r *http.Request, owner string) error {
	p, err := s.Store.Parse(owner, r.PathValue("parse"))
	if err != nil {
		return err
	}
	if p.Terminal() {
		return fault.New(fault.AlreadyTerminal, "parse %s is %s", p.ID, p.State)
	}
	if p, err = s.Store.UpdateParse(p.ID, func(p *store.Parse) { p.CancelRequested = true }); err != nil {
		return err
	}
	s.Runner.Cancel(p.ID)
	httpjson.Write(w, http.StatusOK, viewParse(p))
	return nil
}

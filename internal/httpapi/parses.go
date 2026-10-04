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
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/id"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/store"
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
	PagesReused int    `json:"pages_reused,omitempty"`
}

// parseView is a parse as the API returns it.
type parseView struct {
	ID               string            `json:"id"`
	Owner            string            `json:"owner,omitempty"`
	State            string            `json:"state"`
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
		Progress: progress{Stage: p.Stage, PagesTotal: p.PagesTotal, PagesDone: p.PagesDone, PagesFailed: p.PagesFailed, PagesReused: p.PagesReused},
		Usage:    p.Usage, Error: p.Error,
		CreatedAt: p.CreatedAt, StartedAt: p.StartedAt, FinishedAt: p.FinishedAt, DeadlineAt: p.DeadlineAt,
	}
}

// decode reads a JSON body into v and refuses what the contract does not
// describe: a member it does not name, a second value, a body over the
// limit.
func decode(w http.ResponseWriter, r *http.Request, v any) ([]byte, error) {
	return decodeBody(w, r, v, false)
}

// decodeBody is decode for a request whose body may be left out: with
// optional set, a request with no body leaves v as it is.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, optional bool) ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return nil, fault.New(fault.InvalidRequest, "the body is over %d bytes or stopped before its end", maxBody)
	}
	if optional && len(bytes.TrimSpace(raw)) == 0 {
		return raw, nil
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

// selected is how many pages a selection names, when the selection alone
// says so: every range is closed. An empty selection and one with an open
// range name as many pages as the document turns out to have, which a
// submit does not know. The selection has passed pages.Check.
func selected(expr string) (n int, known bool) {
	if expr == "" {
		return 0, false
	}
	type span struct{ lo, hi int }
	var spans []span
	for part := range strings.SplitSeq(expr, ",") {
		lo, hi, ranged := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return 0, false
		}
		b := a
		if ranged {
			if b, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				// An open range, which runs to the document's end.
				return 0, false
			}
		}
		spans = append(spans, span{a, b})
	}
	// Pages named twice are one page: the spans are merged before they are
	// measured, and never listed, so a wide range costs nothing.
	slices.SortFunc(spans, func(x, y span) int { return x.lo - y.lo })
	end := 0
	for _, sp := range spans {
		if sp.hi > end {
			n += sp.hi - max(sp.lo, end+1) + 1
			end = sp.hi
		}
	}
	return n, true
}

// within holds a submit to what its allow names of the request's own
// members: the classes the caller may use, the readers it may pin, and the
// bound on its priority. The ceilings on the file and on the pages are
// held where the file and the pages are known.
func within(p store.Parse, l authorizer.Limits) error {
	if len(l.Classes) > 0 && !slices.Contains(l.Classes, p.Class) {
		return fault.Wrap(fault.Forbidden, field("class"), "the class %s is not among the classes the caller may use", p.Class)
	}
	if p.Reader != "" && len(l.Readers) > 0 && !slices.Contains(l.Readers, p.Reader) {
		return fault.Wrap(fault.ReaderNotPermitted, field("reader"), "the reader %s is not among the readers the caller may pin", p.Reader)
	}
	if p.Priority > l.MaxPriority || p.Priority < -l.MaxPriority {
		return invalid("priority", "priority is from %d to %d for this caller", -l.MaxPriority, l.MaxPriority)
	}
	return nil
}

// admission is what a parse is admitted with, as its allow says: the
// group and the project it joins, their bounds, the pages its group may
// have read in a day, the most pages it may select, and how long it is
// kept.
func admission(l authorizer.Limits) store.Admission {
	return store.Admission{
		Group: l.Group, Project: l.Project, Weight: l.Weight, ProjectWeight: l.ProjectWeight,
		MaxRunning: l.MaxRunning, MaxQueued: l.MaxQueued, MaxPriority: l.MaxPriority,
		PagesPerDay: l.PagesPerDay, MaxPages: l.MaxPages, Retention: l.Retention,
	}
}

func (s *Server) createParse(w http.ResponseWriter, r *http.Request, c call) error {
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

	// The question carries what the request chose. It names no owner: the
	// parse is asked for in the caller's context, and the allow says whose
	// it is.
	res := access.Parse{Class: p.Class, Priority: p.Priority, Reader: p.Reader, Labels: p.Labels}
	if p.Origin != nil {
		res.Origin = access.Origin{Store: p.Origin.Store, Path: p.Origin.Path, Version: p.Origin.Version}
	}
	res.Pages, _ = selected(p.Pages)
	d, err := s.allowed(r, c, res.Resource())
	if err != nil {
		return err
	}
	if err := within(p, d.Limits); err != nil {
		return err
	}
	owner, limit := d.Limits.Owner, s.maxBytes(d.Limits)

	var file store.File
	if req.Source.URL != "" {
		if s.Fetcher == nil {
			return invalid("source.url", "this server fetches no url; upload the file and name it")
		}
		got, err := s.Fetcher.Fetch(r.Context(), req.Source.URL)
		if err != nil {
			return err
		}
		if limit > 0 && int64(len(got.Data)) > limit {
			return fault.New(fault.FileTooLarge, "the file is %d bytes, over the limit of %d bytes", len(got.Data), limit)
		}
		if file, _, err = s.putFile(r.Context(), owner, got.Name, got.MediaType, got.Data, s.fileRetention(d.Limits)); err != nil {
			return err
		}
	} else {
		// A parse reads a file of its own owner, and another owner's file
		// is not found.
		file, err = s.Backend.File(r.Context(), req.Source.File)
		if err == nil && file.Owner != owner {
			err = fault.New(fault.FileNotFound, "no file %s", req.Source.File)
		}
		if err != nil {
			return err
		}
		if limit > 0 && file.Size > limit {
			return fault.New(fault.FileTooLarge, "the file is %d bytes, over the limit of %d bytes", file.Size, limit)
		}
	}

	p.ID, p.Owner, p.File = s.IDs.New(id.Parse), owner, file.ID
	p.State, p.Stage, p.CreatedAt = store.StateQueued, store.StageQueued, s.now()
	p.ContentSHA, p.Reuse = file.SHA256, req.Reuse == nil || *req.Reuse

	body := sha256.Sum256(raw)
	stored, _, err := s.Backend.Submit(r.Context(), p, admission(d.Limits), key, hex.EncodeToString(body[:]))
	if err != nil {
		return err
	}

	if d := wait(r); d > 0 {
		w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(int(d/time.Second)))
		s.Backend.Wait(r.Context(), owner, stored.ID, d)
	}
	// The parse may have moved, or ended, since it was stored.
	if now, err := s.Backend.Parse(r.Context(), stored.ID); err == nil {
		stored = now
	}
	status := http.StatusAccepted
	if stored.Terminal() {
		status = http.StatusOK
	}
	httpjson.Write(w, status, viewParse(stored))
	return nil
}

// narrowed lays the filter of a list's allow over what the caller asked
// for: the owners the list ranges over, and labels a parse must carry
// beside the caller's own. none is set when nothing can match: the filter
// names no owner at all, or a label the caller asked for with another
// value.
func narrowed(f store.Filter, by *authz.Filter) (owners []string, out store.Filter, none bool) {
	if by == nil {
		return nil, f, false
	}
	if by.Owners != nil {
		// A list that is there and empty is nobody's, and not everybody's.
		owners = slices.Clone(by.Owners)
		if len(owners) == 0 {
			return owners, f, true
		}
	}
	if len(by.Labels) > 0 {
		merged := maps.Clone(f.Labels)
		if merged == nil {
			merged = map[string]string{}
		}
		for k, v := range by.Labels {
			if asked, ok := merged[k]; ok && asked != v {
				return owners, f, true
			}
			merged[k] = v
		}
		f.Labels = merged
	}
	return owners, f, false
}

func (s *Server) listParses(w http.ResponseWriter, r *http.Request, c call) error {
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

	d, err := s.allowed(r, c, access.Parses())
	if err != nil {
		return err
	}
	out := struct {
		Parses     []parseView `json:"parses"`
		NextCursor string      `json:"next_cursor,omitempty"`
	}{Parses: []parseView{}}
	owners, filter, none := narrowed(filter, d.Filter)
	if !none {
		found, more, err := s.Backend.Parses(r.Context(), owners, filter, q.Get("cursor"), limit)
		if err != nil {
			return err
		}
		for _, p := range found {
			out.Parses = append(out.Parses, viewParse(p))
		}
		if more {
			// Ids sort by time, so the last one on a page is where the next begins.
			out.NextCursor = found[len(found)-1].ID
		}
	}
	httpjson.Write(w, http.StatusOK, out)
	return nil
}

func (s *Server) getParse(w http.ResponseWriter, r *http.Request, c call) error {
	p, _, err := s.parse(r, c)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, viewParse(p))
	return nil
}

func (s *Server) deleteParse(w http.ResponseWriter, r *http.Request, c call) error {
	p, _, err := s.parse(r, c)
	if err != nil {
		return err
	}
	if err := s.Backend.DeleteParse(r.Context(), p.Owner, p.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) cancelParse(w http.ResponseWriter, r *http.Request, c call) error {
	p, _, err := s.parse(r, c)
	if err != nil {
		return err
	}
	if p, err = s.Backend.Cancel(r.Context(), p.Owner, p.ID); err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, viewParse(p))
	return nil
}

// retryParse reads again the pages of a parse that failed. The pages that
// were read are kept and not read again, and the answer is the parse as the
// retry left it: running, with the failed pages open.
func (s *Server) retryParse(w http.ResponseWriter, r *http.Request, c call) error {
	p, _, err := s.parse(r, c)
	if err != nil {
		return err
	}
	if p, err = s.Backend.Retry(r.Context(), p.Owner, p.ID); err != nil {
		return err
	}
	httpjson.Write(w, http.StatusAccepted, viewParse(p))
	return nil
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package store holds what a parse reads and writes: files, parses, pages,
// page images, and documents. Memory is the one implementation today. It
// keeps everything in the process, so nothing survives a restart: it is for
// a development server and for tests. The durable store, with parses and
// tasks in Postgres and bytes in an object store, is specs/004 and 002.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"sync"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/reader"
)

// File is a source snapshot.
type File struct {
	ID        string
	Owner     string
	Name      string
	MediaType string
	SHA256    string
	Size      int64
	CreatedAt time.Time
	Data      []byte

	// Retention is how long the file is kept from an upload of it, and
	// past the end of the last parse that read it, as its allow or the
	// server's setting says. Zero keeps it. ExpiresAt is when a durable
	// backend may remove the file, and nil where nothing expires.
	Retention time.Duration
	ExpiresAt *time.Time
}

// The states of a parse.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCanceled  = "canceled"
)

// The stages a parse reports while it runs.
const (
	StageQueued     = "queued"
	StagePreparing  = "preparing"
	StageReading    = "reading"
	StageAssembling = "assembling"
	StageDone       = "done"
)

// The classes of work.
const (
	ClassInteractive = "interactive"
	ClassBatch       = "batch"
)

// Origin is where a file lives for the caller. It is stored and returned,
// and never interpreted.
type Origin struct {
	Store   string `json:"store,omitempty"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
}

// Parse is one request to turn a file into a document, and where it stands.
type Parse struct {
	ID    string
	Owner string
	State string

	// What was asked.
	File             string
	Origin           *Origin
	Pages            string
	Reader           string
	Languages        []string
	Class            string
	Priority         int
	AllowFailedPages int
	Labels           map[string]string

	// MaxPages is the most pages the parse may select, as its allow says.
	// Zero leaves the server's own bound on a document alone.
	MaxPages int

	// ContentSHA is the digest of the file's bytes. With a page's number
	// and the readers that may read it, it names a page's result whoever
	// asks for it. Reuse says whether this parse may take a page an
	// earlier parse of the same owner already read.
	ContentSHA string
	Reuse      bool

	// Where it stands.
	Stage       string
	PagesTotal  int
	PagesDone   int
	PagesFailed int
	PagesReused int
	Usage       document.Usage
	Error       *document.Error

	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	DeadlineAt *time.Time

	CancelRequested bool
	Manifest        *parse.Manifest

	// IndexKey and ManifestToken are a durable backend's own: the object
	// key of the parse's document index once it has one, and the lease
	// token of the prepare task that wrote the manifest. The memory store
	// sets neither.
	IndexKey      string
	ManifestToken int64
}

// Admission is what a parse is admitted with: the group and the project it
// joins in the fair queue, their weights, and the bounds the group is held
// to (specs/006-fairness-and-priority.md), and what its allow holds the
// parse itself to (specs/013-limits-and-usage.md,
// specs/014-sources-and-retention.md). The zero value is the group of the
// owner, every weight 1, and nothing bounded. An authorizer's allow, laid
// over the server's settings, supplies the rest
// (specs/012-identity-and-authorization.md).
type Admission struct {
	Group         string
	Project       string
	Weight        int
	ProjectWeight int
	MaxRunning    int
	MaxQueued     int
	MaxPriority   int

	// PagesPerDay is how many pages the group's parses may count in one
	// day. Zero is no budget.
	PagesPerDay int
	// MaxPages is the most pages the parse may select. Zero leaves the
	// server's own bound alone.
	MaxPages int
	// Retention is how long the parse and what it wrote are kept after it
	// ended. Zero keeps them.
	Retention time.Duration
}

// PageEvent is one page of a parse that settled: how it ended, and the
// change of its parse that settled it.
type PageEvent struct {
	Seq   int64
	Page  int
	State string
	Error *document.Error
}

// Events is what a stream of a parse's events is told at one instant: the
// parse as it stands, how many times its state or its progress has changed,
// and the pages that settled after one of those changes, oldest first. Seq
// only grows, and a page's Seq is never above it.
type Events struct {
	Parse Parse
	Seq   int64
	Pages []PageEvent
}

// UsageQuery says which sums of the meters are read
// (specs/013-limits-and-usage.md): by which key, over which intervals, in
// which span of time, and for whom.
type UsageQuery struct {
	// By is the key the sums are grouped by: group, owner or reader.
	By string
	// Interval is the length of one interval: hour or day, in UTC.
	Interval string
	// From and To bound the read: an interval is in the answer when it
	// begins at or after From and before To.
	From time.Time
	To   time.Time
	// Owners and Groups narrow the read to those owners and those groups.
	// Nil is every one, and an empty list is none.
	Owners []string
	Groups []string
}

// UsageSum is what one key used in one interval: the pages read, and the
// model calls and the tokens they cost, the calls that failed included. It
// is written into an answer as it is.
type UsageSum struct {
	Key          string    `json:"key"`
	Start        time.Time `json:"start"`
	Pages        int64     `json:"pages"`
	Calls        int64     `json:"calls"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
}

// Queue is the queue as it stands at one instant
// (specs/006-fairness-and-priority.md, specs/007-model-capacity.md): the
// groups with what waits and what runs for each, and the readers' pools. It
// is written into an answer as it is.
type Queue struct {
	Groups []QueueGroup `json:"groups"`
	Pools  []QueuePool  `json:"pools"`
}

// QueueClass is the tasks of one class that wait and that run.
type QueueClass struct {
	Class   string `json:"class"`
	Queued  int    `json:"queued"`
	Running int    `json:"running"`
}

// QueueGroup is the queue of one group: its share and its bounds, its
// parses that have not ended, its tasks per class, and its projects.
type QueueGroup struct {
	Group      string         `json:"group"`
	Weight     int            `json:"weight"`
	MaxRunning int            `json:"max_running"`
	MaxQueued  int            `json:"max_queued"`
	Parses     int            `json:"parses"`
	Classes    []QueueClass   `json:"classes"`
	Projects   []QueueProject `json:"projects"`
}

// QueueProject is the queue of one project of a group. The group's own
// project has the empty name.
type QueueProject struct {
	Project string       `json:"project"`
	Weight  int          `json:"weight"`
	Parses  int          `json:"parses"`
	Classes []QueueClass `json:"classes"`
}

// QueuePool is the pool of one reader: its bound on calls in flight, the
// calls in flight, whether its breaker admits calls, and the key scopes
// that were limited.
type QueuePool struct {
	Reader      string       `json:"reader"`
	MaxInFlight int          `json:"max_in_flight"`
	InFlight    int          `json:"in_flight"`
	Breaker     string       `json:"breaker"`
	Scopes      []QueueScope `json:"scopes"`
}

// QueueScope is one key scope of a pool: the key every group shares, which
// has the empty name, or one group's key.
type QueueScope struct {
	Scope       string     `json:"scope"`
	Ceiling     int        `json:"ceiling"`
	InFlight    int        `json:"in_flight"`
	PausedUntil *time.Time `json:"paused_until,omitempty"`
}

// Terminal reports whether the parse has ended.
func (p Parse) Terminal() bool {
	return p.State == StateSucceeded || p.State == StateFailed || p.State == StateCanceled
}

type pageKey struct {
	parse string
	page  int
}

// Memory is a store that keeps everything in the process.
type Memory struct {
	mu        sync.RWMutex
	files     map[string]File
	parses    map[string]Parse
	pages     map[pageKey]document.Page
	images    map[pageKey]render.Image
	documents map[string]document.Document
	keys      map[string]idempotent
	read      map[string]readPage
	runs      map[string]FigureRun
	figures   map[string]reader.FigureResult
}

// readPage is one page's result as a reader gave it, kept under what was
// read and not under the parse that asked.
type readPage struct {
	page  document.Page
	image *render.Image
}

// idempotent is what an idempotency key remembers: the body it came with
// and the parse it made.
type idempotent struct {
	body  string
	parse string
}

// NewMemory returns an empty store.
func NewMemory() *Memory {
	return &Memory{
		files: map[string]File{}, parses: map[string]Parse{}, pages: map[pageKey]document.Page{},
		images: map[pageKey]render.Image{}, documents: map[string]document.Document{}, keys: map[string]idempotent{},
		read: map[string]readPage{}, runs: map[string]FigureRun{}, figures: map[string]reader.FigureResult{},
	}
}

// Digest is the SHA-256 of a file's bytes, in hex.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PutFile stores a file. When the owner already has a file with the same
// bytes, that file is returned and created is false: the same bytes are
// one file per owner.
func (m *Memory) PutFile(f File) (stored File, created bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, have := range m.files {
		if have.Owner == f.Owner && have.SHA256 == f.SHA256 {
			return have, false
		}
	}
	m.files[f.ID] = f
	return f, true
}

// File returns an owner's file.
func (m *Memory) File(owner, id string) (File, error) {
	f, err := m.FileByID(id)
	if err != nil || f.Owner != owner {
		return File{}, fault.New(fault.FileNotFound, "no file %s", id)
	}
	return f, nil
}

// FileByID returns a file whoever owns it. It is what a handler reads
// before it asks whether its caller may: the file's owner is part of the
// question.
func (m *Memory) FileByID(id string) (File, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.files[id]
	if !ok {
		return File{}, fault.New(fault.FileNotFound, "no file %s", id)
	}
	return f, nil
}

// DeleteFile removes an owner's file. A file that a parse which has not
// ended reads is not removed.
func (m *Memory) DeleteFile(owner, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[id]
	if !ok || f.Owner != owner {
		return fault.New(fault.FileNotFound, "no file %s", id)
	}
	for _, p := range m.parses {
		if p.File == id && !p.Terminal() {
			return fault.New(fault.NotTerminal, "parse %s still reads the file", p.ID)
		}
	}
	delete(m.files, id)
	return nil
}

// CreateParse stores a new parse. With an idempotency key it is safe to
// repeat: the same key with the same body returns the parse the first call
// made, and with another body it is refused. The key and the parse are
// written under one lock, so two calls with one key make one parse.
func (m *Memory) CreateParse(p Parse, key, body string) (stored Parse, created bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if key != "" {
		scoped := p.Owner + "\x00" + key
		if seen, ok := m.keys[scoped]; ok {
			if seen.body != body {
				return Parse{}, false, fault.New(fault.IdempotencyConflict, "the idempotency key was used with another body")
			}
			return m.parses[seen.parse], false, nil
		}
		m.keys[scoped] = idempotent{body: body, parse: p.ID}
	}
	m.parses[p.ID] = p
	return p, true, nil
}

// Parse returns an owner's parse.
func (m *Memory) Parse(owner, id string) (Parse, error) {
	p, err := m.ParseByID(id)
	if err != nil || p.Owner != owner {
		return Parse{}, fault.New(fault.ParseNotFound, "no parse %s", id)
	}
	return p, nil
}

// ParseByID returns a parse whoever owns it, for a handler to ask about.
func (m *Memory) ParseByID(id string) (Parse, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.parses[id]
	if !ok {
		return Parse{}, fault.New(fault.ParseNotFound, "no parse %s", id)
	}
	return p, nil
}

// UpdateParse changes a parse under the store's lock and returns the
// result. change sees the current parse and edits it in place.
func (m *Memory) UpdateParse(id string, change func(*Parse)) (Parse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.parses[id]
	if !ok {
		return Parse{}, fault.New(fault.ParseNotFound, "no parse %s", id)
	}
	change(&p)
	m.parses[id] = p
	return p, nil
}

// Filter narrows a list of parses. A zero field matches everything.
type Filter struct {
	State      string
	File       string
	OriginPath string
	Labels     map[string]string
}

func (f Filter) matches(p Parse) bool {
	if f.State != "" && p.State != f.State {
		return false
	}
	if f.File != "" && p.File != f.File {
		return false
	}
	if f.OriginPath != "" && (p.Origin == nil || p.Origin.Path != f.OriginPath) {
		return false
	}
	for k, v := range f.Labels {
		if p.Labels[k] != v {
			return false
		}
	}
	return true
}

// ListParses returns the parses of the owners that match, newest first: at
// most limit of them, starting after the parse whose id is after. Nil
// owners is every owner's, and an empty list is nobody's. more reports
// whether others follow.
func (m *Memory) ListParses(owners []string, f Filter, after string, limit int) (out []Parse, more bool) {
	m.mu.RLock()
	all := make([]Parse, 0, len(m.parses))
	for _, p := range m.parses {
		if (owners == nil || slices.Contains(owners, p.Owner)) && f.matches(p) {
			all = append(all, p)
		}
	}
	m.mu.RUnlock()

	// Ids sort by creation time, so the newest first is the largest id first.
	slices.SortFunc(all, func(a, b Parse) int {
		switch {
		case a.ID > b.ID:
			return -1
		case a.ID < b.ID:
			return 1
		}
		return 0
	})
	if after != "" {
		i, _ := slices.BinarySearchFunc(all, after, func(p Parse, id string) int {
			switch {
			case p.ID > id:
				return -1
			case p.ID < id:
				return 1
			}
			return 0
		})
		if i < len(all) && all[i].ID == after {
			i++
		}
		all = all[i:]
	}
	if len(all) > limit {
		return all[:limit], true
	}
	return all, false
}

// KeepRead keeps a page's result under a key that names what was read:
// the owner, the file's bytes, the page, and the readers. A later parse of
// the same owner that would do the same read takes the result and calls no
// model. Only a page that was read whole is kept: a failed or cut page is
// read again.
func (m *Memory) KeepRead(owner, key string, page document.Page, img *render.Image) {
	if key == "" || page.State != document.PageSucceeded || page.Truncated {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.read[owner+"\x00"+key] = readPage{page: page, image: img}
}

// Read returns the result kept under a key, when there is one. The blocks
// are a copy: the caller may change them.
func (m *Memory) Read(owner, key string) (document.Page, *render.Image, bool) {
	if key == "" {
		return document.Page{}, nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	kept, ok := m.read[owner+"\x00"+key]
	if !ok {
		return document.Page{}, nil, false
	}
	kept.page.Blocks = slices.Clone(kept.page.Blocks)
	return kept.page, kept.image, true
}

// The states of a run that describes a parse's figures.
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
)

// FigureRun is one request to describe the figures of a parse, and where
// it stands. A parse has at most one: a later request replaces an earlier
// one that has ended.
type FigureRun struct {
	State string

	// Total is how many figures the run set out to describe. Done and
	// Failed count the ones that ended, and Reused those of Done that were
	// taken from an earlier description and cost no call.
	Total, Done, Failed, Reused int

	// Failures says why each figure that failed did, by its block's ref.
	Failures map[string]document.Error

	// Usage is what the run's calls consumed.
	Usage document.Usage

	StartedAt  time.Time
	FinishedAt *time.Time
}

// StartFigureRun records a run for a parse. It is refused while an earlier
// run of the same parse has not ended: two runs would describe the same
// figures twice and write over each other.
func (m *Memory) StartFigureRun(parseID string, run FigureRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if have, ok := m.runs[parseID]; ok && have.State == RunRunning {
		return fault.New(fault.Conflict, "the figures of parse %s are being described", parseID)
	}
	m.runs[parseID] = run
	return nil
}

// FigureRun returns the run of a parse's figures, when one was started.
func (m *Memory) FigureRun(parseID string) (FigureRun, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	run, ok := m.runs[parseID]
	if ok {
		run.Failures = maps.Clone(run.Failures)
	}
	return run, ok
}

// UpdateFigureRun changes a parse's run under the store's lock. A run
// whose parse was deleted meanwhile is left alone.
func (m *Memory) UpdateFigureRun(parseID string, change func(*FigureRun)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[parseID]
	if !ok {
		return
	}
	run.Failures = maps.Clone(run.Failures)
	change(&run)
	m.runs[parseID] = run
}

// UpdateBlock changes one block of a stored page under the store's lock.
// The page's blocks are copied first, so a page that is being served is
// not changed under its reader, and two changes to one page do not lose
// each other. It reports whether the block is there.
func (m *Memory) UpdateBlock(parseID, ref string, change func(*document.Block)) bool {
	n, order, err := document.ParseRef(ref)
	if err != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := pageKey{parseID, n}
	page, ok := m.pages[key]
	if !ok || order > len(page.Blocks) {
		return false
	}
	page.Blocks = slices.Clone(page.Blocks)
	change(&page.Blocks[order-1])
	m.pages[key] = page
	return true
}

// KeepFigure keeps what a describer said of a figure under a key that
// names the figure and the describers, per owner, the way KeepRead keeps a
// page: the same figure is not described twice.
func (m *Memory) KeepFigure(owner, key string, res reader.FigureResult) {
	if key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.figures[owner+"\x00"+key] = res
}

// Figure returns the description kept under a key, when there is one.
func (m *Memory) Figure(owner, key string) (reader.FigureResult, bool) {
	if key == "" {
		return reader.FigureResult{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	res, ok := m.figures[owner+"\x00"+key]
	res.Labels = slices.Clone(res.Labels)
	return res, ok
}

// DeleteParse removes an owner's parse and everything it wrote. A parse
// that has not ended is not removed.
func (m *Memory) DeleteParse(owner, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.parses[id]
	if !ok || p.Owner != owner {
		return fault.New(fault.ParseNotFound, "no parse %s", id)
	}
	if !p.Terminal() {
		return fault.New(fault.NotTerminal, "parse %s has not ended", id)
	}
	delete(m.parses, id)
	delete(m.documents, id)
	delete(m.runs, id)
	maps.DeleteFunc(m.pages, func(k pageKey, _ document.Page) bool { return k.parse == id })
	maps.DeleteFunc(m.images, func(k pageKey, _ render.Image) bool { return k.parse == id })
	return nil
}

// PutPage stores a page's result, and its image when the page has one. A
// page written twice holds the second result.
func (m *Memory) PutPage(parseID string, page document.Page, img *render.Image) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := pageKey{parseID, page.Number}
	m.pages[key] = page
	if img != nil {
		m.images[key] = *img
	}
}

// Page returns one page of a parse. ok is false for a page not written yet.
func (m *Memory) Page(parseID string, n int) (document.Page, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.pages[pageKey{parseID, n}]
	return p, ok
}

// Pages returns the pages of a parse written so far, in page order.
func (m *Memory) Pages(parseID string) []document.Page {
	m.mu.RLock()
	var out []document.Page
	for k, p := range m.pages {
		if k.parse == parseID {
			out = append(out, p)
		}
	}
	m.mu.RUnlock()
	slices.SortFunc(out, func(a, b document.Page) int { return a.Number - b.Number })
	return out
}

// Image returns the image of a page that a reader saw.
func (m *Memory) Image(parseID string, n int) (render.Image, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	img, ok := m.images[pageKey{parseID, n}]
	return img, ok
}

// PutDocument stores a parse's document index.
func (m *Memory) PutDocument(doc document.Document) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.documents[doc.Parse] = doc
}

// Document returns a parse's document index. ok is false until the parse
// has assembled its pages.
func (m *Memory) Document(parseID string) (document.Document, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	doc, ok := m.documents[parseID]
	return doc, ok
}

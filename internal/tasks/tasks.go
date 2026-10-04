// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package tasks is the protocol a worker process and a task store share:
// what a task is, what a worker is handed when it claims one, what it says
// when the task ends, and the one call, the exchange, that carries both.
// The store is the authority on every value here; a worker holds none of it
// across a restart. The design is specs/004-durable-tasks.md, and the order
// tasks are claimed in is specs/006-fairness-and-priority.md.
//
// The types are the wire form too. An exchange travels to the store as one
// JSON document and comes back as one, so the member names below are part
// of the contract between this package and the store's exchange function.
package tasks

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Kind is what a task does.
type Kind string

// The kinds of task a parse is made of (specs/005-parse-graph.md).
const (
	// Prepare runs intake once per parse and creates the page tasks.
	Prepare Kind = "prepare"
	// Page reads one page with a reader.
	Page Kind = "page"
	// Assemble builds the document from the page results and ends the parse.
	Assemble Kind = "assemble"
	// Extract fills one field from the document of a parse that has ended.
	// One claim of it makes one model call, for a window of the document or
	// for a repair, and the task goes on from claim to claim until the
	// field is filled (specs/011-structured-extraction.md).
	Extract Kind = "extract"
	// Figure has a describer say what one figure of a parse that has ended
	// shows (specs/003-api.md).
	Figure Kind = "figure"
)

// Kinds are the kinds of task a worker of this release runs, which it names
// in every exchange.
var Kinds = []Kind{Prepare, Page, Assemble, Extract, Figure}

// CallsModel reports whether a task of the kind holds a slot in a reader's
// pool. A prepare or an assemble task calls no model and is claimed whatever
// the pools hold.
func (k Kind) CallsModel() bool { return k == Page || k == Extract || k == Figure }

// State is where a task stands.
type State string

// The states of a task. A task exists only when it can run, so there is no
// blocked state.
const (
	Queued    State = "queued"
	Leased    State = "leased"
	Succeeded State = "succeeded"
	Failed    State = "failed"
	Canceled  State = "canceled"
)

// Terminal reports whether a task in the state will never run again unless a
// retry is requested.
func (s State) Terminal() bool { return s == Succeeded || s == Failed || s == Canceled }

// Class is one of the two classes of work. The value is the one stored.
type Class int

// The classes. Interactive work goes ahead of batch work without starving it.
const (
	Interactive Class = 0
	Batch       Class = 1
)

// String is the class as the API names it.
func (c Class) String() string {
	if c == Batch {
		return "batch"
	}
	return "interactive"
}

// The ids of the tasks of a parse. An id is fixed by the parse and the
// task's place in it, so writing a parse's tasks twice writes nothing the
// second time.
const (
	PrepareID  = "prepare"
	AssembleID = "assemble"

	pagePrefix    = "page-"
	extractPrefix = "extract-"
	figurePrefix  = "figure-"
)

// PageID is the id of the task that reads page n.
func PageID(n int) string { return pagePrefix + strconv.Itoa(n) }

// PageOf is the page a page task reads. ok is false for the id of a task
// that is not a page task.
func PageOf(taskID string) (n int, ok bool) {
	digits, found := strings.CutPrefix(taskID, pagePrefix)
	if !found {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 || strconv.Itoa(n) != digits {
		return 0, false
	}
	return n, true
}

// ExtractID is the id of the task that fills the field of the name.
func ExtractID(field string) string { return extractPrefix + field }

// FieldOf is the name of the field an extract task fills. ok is false for
// the id of a task that is not one.
func FieldOf(taskID string) (name string, ok bool) {
	name, ok = strings.CutPrefix(taskID, extractPrefix)
	return name, ok && name != ""
}

// FigureID is the id of the task that describes the figure a block's ref
// names.
func FigureID(ref string) string { return figurePrefix + ref }

// FigureOf is the ref of the figure a figure task describes. ok is false
// for the id of a task that is not one.
func FigureOf(taskID string) (ref string, ok bool) {
	ref, ok = strings.CutPrefix(taskID, figurePrefix)
	return ref, ok && ref != ""
}

// Outcome is how one attempt at a task ended.
type Outcome string

// The outcomes a worker settles a task with. Done, Retryable, Permanent and
// Wait are the rows of the table in specs/004-durable-tasks.md, and Continue
// is the step of a task that makes several calls; a worker that dies settles
// nothing, and the store's sweep is what returns its tasks.
const (
	// Done is a task that produced its output.
	Done Outcome = "succeeded"
	// Retryable is a failure another attempt may not repeat: a network
	// error, a 5xx, a timeout, a reply that fails validation. It spends an
	// attempt and waits a backoff.
	Retryable Outcome = "retryable"
	// Permanent is a failure no attempt changes. The task fails at once.
	Permanent Outcome = "permanent"
	// Wait is a rate-limit reply from the reader's endpoint, or a key source
	// that cannot say yet which key the call is made with. It spends no
	// attempt, and pauses the key scope the task was claimed in.
	Wait Outcome = "wait"
	// Next is a reader that cannot be the one to read this page: it declined
	// the page, or its endpoint rejects the request itself. The task goes to
	// the reader after it in the policy's chain, with attempts of its own
	// and no wait, however far down the chain that is. A task whose parse
	// named its reader, and one with no reader left, fails with the error
	// the settle carries.
	Next Outcome = "next"
	// Continue is a task that made a call, kept what it has so far under the
	// key its settle names, and has another call to make: an extraction
	// between 2 windows, or before a repair. The task returns to the queue at
	// once, with no attempt spent and its attempts as a new task has them,
	// and its next claim carries the key. So each call takes its own slot in
	// the reader's pool and its own turn in the fair queue, and a worker
	// that dies loses one call and not the calls before it.
	Continue Outcome = "continue"
	// Returned is a task given back unfinished by a worker that is
	// stopping. No counter changes.
	Returned Outcome = "returned"
)

// Valid reports whether o is an outcome the store knows.
func (o Outcome) Valid() bool {
	switch o {
	case Done, Retryable, Permanent, Wait, Next, Continue, Returned:
		return true
	}
	return false
}

// Health is what a reader call said about its reader, for the breaker of
// specs/007-model-capacity.md. It is separate from the outcome: a permanent
// failure of one page, a rate limit and a canceled call say nothing about
// the reader, and a request the endpoint rejects outright fails the page
// over to the next reader and still counts against this one.
type Health string

// What a call said about its reader.
const (
	// Silent is a call that said nothing, or no call.
	Silent Health = ""
	// Healthy is a call the reader answered.
	Healthy Health = "success"
	// Unhealthy is a call that failed in a way the next call may too.
	Unhealthy Health = "failure"
)

// Ref names one task.
type Ref struct {
	Parse string `json:"parse"`
	Task  string `json:"task"`
}

// Held names a task a worker still runs, with the token it was claimed
// under. The store answers which of them are no longer the worker's.
type Held struct {
	Parse string `json:"parse"`
	Task  string `json:"task"`
	Token int64  `json:"token"`
}

// Claim is a task handed to a worker. It holds the lease until the worker
// settles the task, loses its own lease, or the parse is canceled.
type Claim struct {
	Parse string `json:"parse"`
	Task  string `json:"task"`
	Kind  Kind   `json:"kind"`

	// Token is the fencing token: the task's output is written under a key
	// that carries it, and the settle is accepted only while it is the
	// task's current one.
	Token int64 `json:"token"`

	Group   string `json:"group"`
	Project string `json:"project"`

	// Attempt is how many attempts the task has spent, and Expiries how
	// many workers died while running it.
	Attempt  int `json:"attempt"`
	Expiries int `json:"expiries"`

	// Pin is the reader the parse named, when it named one.
	Pin string `json:"pin,omitempty"`

	// Reader is the pool the task holds a slot in, and Scope the key scope
	// of that slot: empty when one key serves every group, the group when
	// each has its own. Both are empty for a task that calls no model.
	Reader string `json:"reader,omitempty"`
	Scope  string `json:"scope,omitempty"`

	// Alone is set for a task whose worker died before: the worker runs it
	// with nothing else, and claims nothing more until it settles it, so
	// that a second death is the task's own.
	Alone bool `json:"alone,omitempty"`

	// Context is what the task needs of its parse. It rides on the claim so
	// that running a task costs a worker no statement of its own.
	Context Context `json:"context"`
}

// Context is the part of a parse a claim carries, by the kind of its task.
type Context struct {
	// Owner is the parse's owner.
	Owner string `json:"owner,omitempty"`

	// File is the source snapshot and Pages the caller's page selection,
	// for a prepare task.
	File  *Source `json:"file,omitempty"`
	Pages string  `json:"pages,omitempty"`

	// Languages are the hints a page is read with, for a page task.
	Languages []string `json:"languages,omitempty"`

	// Manifest is what prepare found out about the file, as its settle
	// wrote it: without its list of pages for a page task, and whole for an
	// assemble task.
	Manifest json.RawMessage `json:"manifest,omitempty"`

	// Reuse is the object key of a result an earlier parse of the same
	// owner kept for the same read, for a page task of a parse that takes
	// such reads. Empty when there is none.
	Reuse string `json:"reuse,omitempty"`

	// Index is the object key of the parse's document index, for an extract
	// task of a parse that has one. A parse that ended without one, canceled
	// or out of time, is read through its task rows.
	Index string `json:"index,omitempty"`

	// Request is what an extraction was asked, for an extract task: a Field
	// as one JSON document. It rides as text, so a schema reaches the worker
	// byte for byte, with its members in the order its caller wrote them.
	Request string `json:"request,omitempty"`

	// Progress is the object key an earlier claim of the task left what it
	// had so far under, for an extract task that settled with Continue.
	// Empty on the task's first claim.
	Progress string `json:"progress,omitempty"`

	// Figure is the figure to describe, for a figure task.
	Figure *FigureAsk `json:"figure,omitempty"`
}

// Field is the request of an extraction as it is kept and as its task is
// told of it: the members of the request that decide what the model is
// asked.
type Field struct {
	// Schema is the caller's JSON Schema, as it was sent.
	Schema json.RawMessage `json:"schema"`
	// Instructions are the caller's guidance for the model. May be empty.
	Instructions string `json:"instructions,omitempty"`
	// Citations says whether each value names the blocks it was read from.
	Citations bool `json:"citations"`
}

// FigureAsk is one figure as its task is told of it.
type FigureAsk struct {
	// Page is the object key of the stored result of the figure's page. The
	// figure's box, its caption and the key of the page's image are read
	// from it.
	Page string `json:"page"`
	// Reuse is the object key of a description an earlier run kept for the
	// same figure, of the same owner, the same bytes and the same
	// describers. Empty when there is none, and for a run that describes
	// again what is described.
	Reuse string `json:"reuse,omitempty"`
}

// Source is a file as a prepare task is told of it.
type Source struct {
	Key       string `json:"key"`
	Name      string `json:"name,omitempty"`
	MediaType string `json:"media_type"`
}

// Usage is what one attempt consumed.
type Usage struct {
	Calls        int   `json:"calls,omitempty"`
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
}

// Error is why a task failed: a code of internal/fault and a sentence for a
// developer. It never holds file content or a credential.
type Error struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// Prepared is what the settle of a prepare task carries. The store writes
// the parse's next tasks from it in the transaction of the settle.
type Prepared struct {
	// Manifest is the parse's manifest, stored on the parse as it is.
	Manifest json.RawMessage `json:"manifest,omitempty"`

	// Pages are the selected pages, in the order of the selection. With
	// Native false the store creates one page task per page; a selection of
	// none creates the assemble task at once.
	Pages []int `json:"pages"`

	// Native says the format carried its own structure: prepare wrote the
	// pages itself, so the store counts them done, creates no page task and
	// creates the assemble task.
	Native bool `json:"native,omitempty"`
}

// Assembled is what the settle of an assemble task carries.
type Assembled struct {
	// Index is the object key of the document index.
	Index string `json:"index"`
}

// Settle is the end of one attempt at a task.
type Settle struct {
	Parse string `json:"parse"`
	Task  string `json:"task"`
	Token int64  `json:"token"`

	Outcome Outcome `json:"outcome"`

	// Output is the object key the task wrote its result under, for a Done
	// outcome, and the key of what it has so far, for a Continue.
	Output string `json:"output,omitempty"`

	// Usage is what this attempt consumed. It is recorded whatever the
	// outcome, so what was spent is metered and not only what was useful.
	Usage Usage `json:"usage"`

	// Units are the fairness units the attempt used: the sum of the cost of
	// every reader call it made. The store never corrects a charge below 1.
	Units int `json:"units,omitempty"`

	// Error is why the attempt failed, for a Retryable, a Permanent or a
	// Next outcome. It is kept on the task when the task fails.
	Error *Error `json:"error,omitempty"`

	// Invalid says, on a Retryable outcome, that the reader answered and the
	// answer was not usable. The second such reply from one reader sends a
	// page to the next reader in the chain, once for the page.
	Invalid bool `json:"invalid,omitempty"`

	// Result is what the task says of its output: a small document the
	// store keeps as it is. A page writes the summary a list of pages is
	// answered from, with a Done outcome. An extraction writes what a field
	// says of how it was filled, with the outcome that ends it, whichever
	// that is. A figure says whether its description was taken from an
	// earlier run.
	Result json.RawMessage `json:"result,omitempty"`

	// RetryAfter is how long the endpoint said to wait, for a Wait outcome.
	// Zero takes the store's default pause.
	RetryAfter time.Duration `json:"-"`

	// Health is what the reader call said about its reader.
	Health Health `json:"health,omitempty"`

	// Prepare and Assemble are set on a Done settle of that kind.
	Prepare  *Prepared  `json:"prepare,omitempty"`
	Assemble *Assembled `json:"assemble,omitempty"`
}

// settleWire is Settle with the wait in milliseconds, which is the unit the
// store reads it in.
type settleWire struct {
	settleAlias
	RetryAfterMS int64 `json:"retry_after_ms,omitempty"`
}

// settleAlias is Settle without its methods, so encoding one does not call
// MarshalJSON again.
type settleAlias Settle

// MarshalJSON writes the wait as a number of milliseconds. A wait shorter
// than a millisecond is written as one, so a wait that was given is not read
// as none.
func (s Settle) MarshalJSON() ([]byte, error) {
	w := settleWire{settleAlias: settleAlias(s)}
	if s.RetryAfter > 0 {
		w.RetryAfterMS = max(1, s.RetryAfter.Milliseconds())
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads the form MarshalJSON writes.
func (s *Settle) UnmarshalJSON(data []byte) error {
	var w settleWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*s = Settle(w.settleAlias)
	s.RetryAfter = time.Duration(w.RetryAfterMS) * time.Millisecond
	return nil
}

// Request is what a worker sends in one exchange.
type Request struct {
	// Settles are the tasks that finished since the last exchange.
	Settles []Settle `json:"settles"`

	// Held are the tasks the worker still runs. The reply names those that
	// are no longer its own.
	Held []Held `json:"held"`

	// Free is how many tasks the worker can take.
	Free int `json:"free"`

	// Idle says the worker runs nothing at all, a task it was told it lost
	// included. Only an idle worker is handed a task that must run alone.
	Idle bool `json:"idle"`

	// Shutdown says this is the worker's last exchange: after the settles,
	// every task it still holds returns to the queue with no counter
	// changed, nothing is claimed, and its registration is removed.
	Shutdown bool `json:"shutdown"`

	// Kinds are the kinds of task the worker runs: it is handed no other.
	// A request that names none comes from a worker of the release before
	// an extraction and a figure were tasks, and is handed the 3 kinds a
	// parse is made of, so the 2 releases can run side by side while a
	// fleet is rolled.
	Kinds []Kind `json:"kinds"`
}

// Validate reports the first thing in the request the store would refuse.
// A worker that sends a malformed request has a bug, and the store's own
// error for it names a position in a JSON document and not a field.
func (r Request) Validate() error {
	if r.Free < 0 {
		return fmt.Errorf("tasks: free is %d, below zero", r.Free)
	}
	for i, s := range r.Settles {
		switch {
		case s.Parse == "" || s.Task == "":
			return fmt.Errorf("tasks: settle %d names no task", i)
		case !s.Outcome.Valid():
			return fmt.Errorf("tasks: settle %d of %s/%s has the outcome %q", i, s.Parse, s.Task, s.Outcome)
		case s.Units < 0 || s.Usage.Calls < 0 || s.Usage.InputTokens < 0 || s.Usage.OutputTokens < 0:
			return fmt.Errorf("tasks: settle %d of %s/%s reports a use below zero", i, s.Parse, s.Task)
		case s.RetryAfter < 0:
			return fmt.Errorf("tasks: settle %d of %s/%s waits a time below zero", i, s.Parse, s.Task)
		case s.Outcome == Done && s.Task == PrepareID && s.Prepare == nil:
			// The store would read a prepare that says nothing as a
			// selection of no page, and end the parse with none read.
			return fmt.Errorf("tasks: settle %d of %s/%s succeeded and does not say what it prepared", i, s.Parse, s.Task)
		case s.Outcome == Done && s.Task == AssembleID && (s.Assemble == nil || s.Assemble.Index == ""):
			return fmt.Errorf("tasks: settle %d of %s/%s succeeded and names no document index", i, s.Parse, s.Task)
		case s.Outcome == Continue && s.Output == "":
			// The next claim would start the task over, and what it made
			// so far would be made and charged again.
			return fmt.Errorf("tasks: settle %d of %s/%s goes on and names no key of what it has so far", i, s.Parse, s.Task)
		}
	}
	for i, h := range r.Held {
		if h.Parse == "" || h.Task == "" {
			return fmt.Errorf("tasks: held %d names no task", i)
		}
	}
	return nil
}

// Reply is what the store answers in one exchange.
type Reply struct {
	// Gone says the fleet gave this worker up: its registration is not
	// there, because its lease ran out and another worker returned its
	// tasks to the queue. Nothing in the request was recorded. The worker
	// abandons everything it runs and registers under a new id.
	Gone bool `json:"gone"`

	// Refused are the settles that matched no lease: the task was
	// canceled, reissued, or settled before. Their output is not recorded.
	Refused []Ref `json:"refused"`

	// Lost are the held tasks that are no longer the worker's. It stops
	// them, which ends a reader call in flight.
	Lost []Ref `json:"lost"`

	// Claims are the tasks the worker now holds.
	Claims []Claim `json:"claims"`

	// SleepUntil is when the earliest rate-limit pause ends among the key
	// scopes that had no room for a task of this exchange. It is nil when
	// no pause held a task back. A worker with free slots and nothing to
	// run sleeps until then or for its poll interval, whichever is sooner.
	SleepUntil *time.Time `json:"sleep_until"`
}

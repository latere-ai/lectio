// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"latere.ai/x/pkg/statemachine"

	"latere.ai/x/lectio/document"
)

// The lifecycles the store keeps, each as a statemachine table. The stored
// writers are database functions, so each holds its table in the condition
// of its own statement (a state in a list, or a state read under the row
// lock in the writing transaction); the tests in internal/store/postgres
// drive those writers through every state and every pair of racing writers
// against the table, and the in-memory store checks every change of a state
// against the same table. formal/ renders each table into the Lean and TLA+
// models the lean and tla gates check.

// ParseState is where a parse stands.
type ParseState string

// The states of a parse, as parses.state holds them (all but ParseGone).
const (
	ParseQueued    ParseState = "queued"
	ParseRunning   ParseState = "running"
	ParseSucceeded ParseState = "succeeded"
	ParseFailed    ParseState = "failed"
	ParseCanceled  ParseState = "canceled"
	// ParseGone is the parse whose row was removed, once it ended and its
	// retention was over or its owner deleted it. It is the absence of a
	// row, not a stored value.
	ParseGone ParseState = "gone"
)

// ParseTable is the lifecycle of a parse, held in parses.state. A parse is
// queued when it is submitted and running from the claim of its first task.
// It ends succeeded or failed when its assemble task settles or its
// deadline passes, and canceled when its owner stops it; a cancel and a
// deadline also end a parse that was never claimed. A parse that assemble
// ended with a failed page runs again when its failed pages are retried,
// and a parse that ended and has no retention left is removed.
//
// A parse canceled before it was assembled has no index to retry from, so
// canceled leads only to gone.
var ParseTable = statemachine.Must(statemachine.New("Parse", []ParseState{ParseQueued}, map[ParseState][]ParseState{
	ParseQueued:    {ParseRunning, ParseFailed, ParseCanceled},
	ParseRunning:   {ParseSucceeded, ParseFailed, ParseCanceled},
	ParseSucceeded: {ParseRunning, ParseGone},
	ParseFailed:    {ParseRunning, ParseGone},
	ParseCanceled:  {ParseGone},
	ParseGone:      nil,
}))

// ParseStateOf is the stored state a name spells. ok is false for any other
// text, and for ParseGone, which no row holds.
func ParseStateOf(s string) (ParseState, bool) {
	switch p := ParseState(s); p {
	case ParseQueued, ParseRunning, ParseSucceeded, ParseFailed, ParseCanceled:
		return p, true
	case ParseGone:
	}
	var none ParseState
	return none, false
}

// Set reports whether s names a state. The zero value is no state: a filter
// that leaves it is a filter that matches every parse.
func (s ParseState) Set() bool { return len(s) > 0 }

// Terminal reports whether a parse in the state has ended.
func (s ParseState) Terminal() bool {
	switch s {
	case ParseSucceeded, ParseFailed, ParseCanceled:
		return true
	case ParseQueued, ParseRunning, ParseGone:
	}
	return false
}

// ParseStage is the stage an in-memory parse reports while it runs.
type ParseStage string

// The stages of an in-memory parse. A durable parse's stage is read from its
// row and its counters when it is asked for, so nothing writes it.
const (
	StageQueued     ParseStage = "queued"
	StagePreparing  ParseStage = "preparing"
	StageReading    ParseStage = "reading"
	StageAssembling ParseStage = "assembling"
	StageDone       ParseStage = "done"
)

// StageTable is the lifecycle of the stage an in-memory parse reports. A
// parse is preparing from its claim. It is reading while its pages are read,
// and assembling once its document is written, which a parse that failed
// before its pages were read reaches directly: every parse is assembled
// before it is done, whichever way it ends.
var StageTable = statemachine.Must(statemachine.New("Stage", []ParseStage{StageQueued}, map[ParseStage][]ParseStage{
	StageQueued:     {StagePreparing},
	StagePreparing:  {StageReading, StageAssembling},
	StageReading:    {StageAssembling},
	StageAssembling: {StageDone},
	StageDone:       nil,
}))

// TaskTable is the lifecycle of a task, held in tasks.state. A task is
// queued when it is written. A claim leases it, and a settle, the expiry of
// its worker, or a worker's own return takes it back to the queue or ends
// it succeeded or failed. A cancel ends what is queued or leased. A task
// that ended is queued again when its parse is retried (a failed page) or
// read again (the assemble task that succeeded).
//
// A task row is removed when its parse is, and an extraction's or a
// figure's task row is removed as soon as it ended and its field or figure
// holds how it ended. A removal is not a state.
var TaskTable = statemachine.Must(statemachine.New("Task", []State{Queued}, map[State][]State{
	Queued:    {Leased, Canceled},
	Leased:    {Queued, Succeeded, Failed, Canceled},
	Succeeded: {Queued},
	Failed:    {Queued},
	Canceled:  nil,
}))

// FieldTable is the lifecycle of an extraction, held in fields.state. It is
// pending from its request until its task ends, or until its deadline
// passes, and it is asked again from either end.
var FieldTable = statemachine.Must(statemachine.New("Field", []document.FieldState{document.FieldPending}, map[document.FieldState][]document.FieldState{
	document.FieldPending:   {document.FieldSucceeded, document.FieldFailed},
	document.FieldSucceeded: {document.FieldPending},
	document.FieldFailed:    {document.FieldPending},
}))

// FigureState is where one figure of a figure run stands.
type FigureState string

// The states of a figure, as figures.state holds them.
const (
	FigurePending   FigureState = "pending"
	FigureSucceeded FigureState = "succeeded"
	FigureFailed    FigureState = "failed"
)

// FigureTable is the lifecycle of one figure, held in figures.state. It is
// pending when a run takes it and ends with how that run ended for it. A
// later run takes it pending again. A figure a run lost that still holds a
// description from an earlier run is described, so the start of the next
// run moves failed to succeeded. A figure a run lost that holds no
// description is removed when the next run starts; a removal is not a state.
var FigureTable = statemachine.Must(statemachine.New("Figure", []FigureState{FigurePending}, map[FigureState][]FigureState{
	FigurePending:   {FigureSucceeded, FigureFailed},
	FigureSucceeded: {FigurePending},
	FigureFailed:    {FigurePending, FigureSucceeded},
}))

// RunState is where the run of a parse that describes its figures stands.
type RunState string

// The states of a figure run, as figure_runs.state holds them.
const (
	RunRunning   RunState = "running"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
)

// RunTable is the lifecycle of a parse's figure run, held in
// figure_runs.state. A run with no figure to describe succeeds as it is
// written. A later run replaces one that has ended and never one that is
// running.
var RunTable = statemachine.Must(statemachine.New("Run", []RunState{RunRunning, RunSucceeded}, map[RunState][]RunState{
	RunRunning:   {RunSucceeded, RunFailed},
	RunSucceeded: {RunRunning},
	RunFailed:    {RunRunning, RunSucceeded},
}))

// FileState is where a stored file stands.
type FileState string

// The states of a file. Only the mark that a delete began is stored, as
// files.deleted_at.
const (
	// FileLive is a file with no deleted_at.
	FileLive FileState = "live"
	// FileDeleting is a file whose delete began: it is gone for every
	// caller, and its row stays until its object is removed.
	FileDeleting FileState = "deleting"
	// FileGone is the removed row.
	FileGone FileState = "gone"
)

// FileTable is the lifecycle of a file, derived from files.deleted_at. A
// delete, by its owner or by the retention sweep, marks the row; the row is
// removed once the object is.
var FileTable = statemachine.Must(statemachine.New("File", []FileState{FileLive}, map[FileState][]FileState{
	FileLive:     {FileDeleting},
	FileDeleting: {FileGone},
	FileGone:     nil,
}))

// BreakerState is where a reader's circuit breaker stands, as far as a
// write moves it.
type BreakerState string

// The states of a breaker, derived from pools.opened_at. A breaker that has
// been open for the configured time admits one trial call, which the queue
// reports as a trial; the clock moves it there and no write does.
const (
	BreakerClosed BreakerState = "closed"
	BreakerOpen   BreakerState = "open"
)

// BreakerTable is the lifecycle of a reader's breaker, derived from
// pools.opened_at. The settle of a call that reports a failure another call
// may repeat opens it once the failures reach the bound, and a settle that
// reports success closes it.
var BreakerTable = statemachine.Must(statemachine.New("Breaker", []BreakerState{BreakerClosed}, map[BreakerState][]BreakerState{
	BreakerClosed: {BreakerOpen},
	BreakerOpen:   {BreakerClosed},
}))

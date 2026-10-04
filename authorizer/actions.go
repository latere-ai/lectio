// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"slices"

	"latere.ai/x/pkg/authz"
)

// Core is the name the shared contract carries this vocabulary under. An
// endpoint's refusal of an unknown action names it, and a grant on a
// personal access token qualifies an action with it: "lectio:parse.read".
const Core = "lectio"

// The resource kinds. An action acts on exactly one.
const (
	KindParse  = "Parse"
	KindFile   = "File"
	KindReader = "Reader"
	KindUsage  = "Usage"
	KindQueue  = "Queue"
)

// The actions: every question lectiod asks an authorizer. A constant never
// changes its string and never disappears.
const (
	// ActionParseCreate is asked by a submit, and by every request that
	// queues model work on a parse that exists: a retry, a figure run, an
	// extraction. The allow carries the limits the work is held to.
	ActionParseCreate = "parse.create"
	// ActionParseRead is asked by every read of a parse and of what it
	// produced: its state, its document, pages, blocks, figures, chunks,
	// fields and events.
	ActionParseRead = "parse.read"
	// ActionParseList is asked by the list of parses. The allow's filter
	// narrows the list to the owners and labels the caller may see.
	ActionParseList   = "parse.list"
	ActionParseCancel = "parse.cancel"
	ActionParseDelete = "parse.delete"

	ActionFileCreate = "file.create"
	ActionFileRead   = "file.read"
	ActionFileDelete = "file.delete"

	ActionReaderList = "reader.list"
	ActionUsageRead  = "usage.read"
	ActionQueueRead  = "queue.read"
)

// The members a resource carries beside its kind. FieldID is the id of a
// stored parse or file, which the envelope carries as resource.id.
const (
	FieldID    = "id"
	FieldOwner = "owner"
	// FieldClass, FieldPriority, FieldReader, FieldLabels and FieldOrigin
	// are a parse's options as it was submitted. FieldReader is the reader
	// the parse pins and is absent when the routing policy decides.
	// FieldOrigin is an object of store, path and version.
	FieldClass    = "class"
	FieldPriority = "priority"
	FieldReader   = "reader"
	FieldLabels   = "labels"
	FieldOrigin   = "origin"
	// FieldPages is how many pages the parse selects. It is absent until
	// that is known: a submit knows it only for a selection with no open
	// range, and a stored parse once its pages were counted.
	FieldPages = "pages"
	// FieldSize is a file's size in bytes and FieldMediaType its type. An
	// upload sends the size only when the request declares one.
	FieldSize      = "size"
	FieldMediaType = "media_type"
	// FieldGroup is the fairness group a usage or queue read is about.
	FieldGroup = "group"
)

// row is one action with the kind it acts on and the members its resource
// may carry.
type row struct {
	action string
	kind   string
	fields []string
}

// The members by kind. A create is asked before the object exists, so it
// carries no id unless it is asked about a stored parse.
var (
	parseFields = []string{FieldID, FieldOwner, FieldClass, FieldPriority, FieldReader, FieldLabels, FieldOrigin, FieldPages}
	fileFields  = []string{FieldID, FieldOwner, FieldSize, FieldMediaType}
)

// table is the vocabulary of specs/012-identity-and-authorization.md in
// its order, and the one place an action is paired with its kind and its
// fields.
var table = []row{
	{ActionParseCreate, KindParse, parseFields},
	{ActionParseRead, KindParse, parseFields},
	{ActionParseList, KindParse, nil},
	{ActionParseCancel, KindParse, parseFields},
	{ActionParseDelete, KindParse, parseFields},

	{ActionFileCreate, KindFile, []string{FieldOwner, FieldSize, FieldMediaType}},
	{ActionFileRead, KindFile, fileFields},
	{ActionFileDelete, KindFile, fileFields},

	{ActionReaderList, KindReader, nil},
	{ActionUsageRead, KindUsage, []string{FieldOwner, FieldGroup}},
	{ActionQueueRead, KindQueue, []string{FieldGroup}},
}

// labels is the name a person reads for each kind, where an interface
// groups the actions by what they act on.
var labels = map[string]string{
	KindParse:  "Parses",
	KindFile:   "Files",
	KindReader: "Readers",
	KindUsage:  "Usage",
	KindQueue:  "Queue",
}

// Vocabulary is Lectio's action table as the shared contract reads it:
// the client refuses an action outside it before the wire, the endpoint
// scaffold of latere.ai/x/pkg/authz/server answers 400 for one, and the
// conformance suite drives a case per row. The value is a fresh copy each
// call.
func Vocabulary() authz.Vocabulary {
	actions := make([]authz.Action, len(table))
	for i, r := range table {
		actions[i] = authz.Action{Name: r.action, Kind: r.kind}
	}
	return authz.Vocabulary{Core: Core, Actions: actions}.WithLabels(labels)
}

// Actions lists every action of the vocabulary, in the table's order.
func Actions() []string {
	out := make([]string, len(table))
	for i, r := range table {
		out[i] = r.action
	}
	return out
}

// Kind is the resource kind an action acts on, and "" for a string outside
// the vocabulary.
func Kind(action string) string {
	for _, r := range table {
		if r.action == action {
			return r.kind
		}
	}
	return ""
}

// Known reports whether action is one of the vocabulary.
func Known(action string) bool { return Kind(action) != "" }

// Fields lists the members a resource of the action may carry beside its
// kind, in the order the spec's table writes them. It is nil for an action
// that sends the kind alone and for a string outside the vocabulary. A
// request sends the members it knows and leaves the others out.
func Fields(action string) []string {
	for _, r := range table {
		if r.action == action {
			return slices.Clone(r.fields)
		}
	}
	return nil
}

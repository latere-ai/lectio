// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/authorizer"
)

// The resources a question is about, one type per kind that has fields.
// They are what lectiod sends, under the names latere.ai/x/lectio/authorizer
// publishes. A member that is not known is left out and never sent empty,
// so an authorizer can tell a parse with no pinned reader from one whose
// reader is the empty string.

// Parse is the resource of every parse action but the list. A submit
// carries no ID: the parse does not exist until the create is allowed. A
// retry, a figure run and an extraction ask parse.create about a stored
// parse and carry its ID and its owner.
type Parse struct {
	ID string
	// Owner is the stored parse's owner. On a submit it is the owner the
	// request names, and empty when it names none: in the caller's
	// context.
	Owner    string
	Class    string
	Priority int
	// Reader is the reader the parse pins, empty when the routing policy
	// decides.
	Reader string
	Labels map[string]string
	Origin Origin
	// Pages is how many pages the parse selects, 0 until that is known.
	Pages int
}

// Origin is where a parse's file lives for the caller, as the caller said.
type Origin struct {
	Store, Path, Version string
}

// Resource renders the parse as the envelope carries it.
func (p Parse) Resource() authz.Resource {
	fields := map[string]any{authorizer.FieldPriority: p.Priority}
	text(fields, authorizer.FieldOwner, p.Owner)
	text(fields, authorizer.FieldClass, p.Class)
	text(fields, authorizer.FieldReader, p.Reader)
	if len(p.Labels) > 0 {
		labels := make(map[string]any, len(p.Labels))
		for k, v := range p.Labels {
			labels[k] = v
		}
		fields[authorizer.FieldLabels] = labels
	}
	origin := map[string]any{}
	text(origin, "store", p.Origin.Store)
	text(origin, "path", p.Origin.Path)
	text(origin, "version", p.Origin.Version)
	if len(origin) > 0 {
		fields[authorizer.FieldOrigin] = origin
	}
	if p.Pages > 0 {
		fields[authorizer.FieldPages] = p.Pages
	}
	return authz.NewResource(authorizer.KindParse, p.ID, fields)
}

// File is the resource of every file action. An upload carries no ID, and
// a Size only when the request declares one.
type File struct {
	ID string
	// Owner is the stored file's owner. On an upload it is the owner the
	// request names, and empty when it names none.
	Owner     string
	Size      int64
	MediaType string
}

// Resource renders the file as the envelope carries it.
func (f File) Resource() authz.Resource {
	fields := map[string]any{}
	text(fields, authorizer.FieldOwner, f.Owner)
	if f.Size > 0 {
		fields[authorizer.FieldSize] = f.Size
	}
	text(fields, authorizer.FieldMediaType, f.MediaType)
	return authz.NewResource(authorizer.KindFile, f.ID, fields)
}

// Parses is the resource of parse.list: the kind and nothing else. The
// answer is a decision whose filter narrows the list.
func Parses() authz.Resource { return authz.NewResource(authorizer.KindParse, "", nil) }

// Readers is the resource of reader.list.
func Readers() authz.Resource { return authz.NewResource(authorizer.KindReader, "", nil) }

// Usage is the resource of usage.read: the owner and the group the read
// is about, each empty when the request names none.
func Usage(owner, group string) authz.Resource {
	fields := map[string]any{}
	text(fields, authorizer.FieldOwner, owner)
	text(fields, authorizer.FieldGroup, group)
	return authz.NewResource(authorizer.KindUsage, "", fields)
}

// Queue is the resource of queue.read: the group the read is about, empty
// when the request names none.
func Queue(group string) authz.Resource {
	fields := map[string]any{}
	text(fields, authorizer.FieldGroup, group)
	return authz.NewResource(authorizer.KindQueue, "", fields)
}

// text sets a member that has a value and leaves out one that has none.
func text(fields map[string]any, name, value string) {
	if value != "" {
		fields[name] = value
	}
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"net/http"
	"slices"

	"latere.ai/x/lectio/authorizer"
)

// Route is one operation of api/openapi.yaml with the question it asks.
type Route struct {
	// Method and Path name the operation, the path as the contract spells
	// it, below the base path.
	Method, Path string
	// Action is the action the route asks before it acts. It is empty for
	// a route that takes no token and asks nothing.
	Action string
	// Fields are the members the question's resource carries beside its
	// kind, each sent when it is known. A route whose fields include the
	// id asks about a stored object: it reads the object first, by its id
	// alone, and builds the resource from what is stored.
	Fields []string
}

// What a question carries, by what it is about.
var (
	// newParse is a submit: the options of the request, and no id.
	newParse = []string{
		authorizer.FieldOwner, authorizer.FieldClass, authorizer.FieldPriority, authorizer.FieldReader,
		authorizer.FieldLabels, authorizer.FieldOrigin, authorizer.FieldPages,
	}
	// storedParse is a parse that exists, as it is stored.
	storedParse = append([]string{authorizer.FieldID}, newParse...)
	newFile     = []string{authorizer.FieldOwner, authorizer.FieldSize, authorizer.FieldMediaType}
	storedFile  = append([]string{authorizer.FieldID}, newFile...)
)

// routes is the table of specs/003-api.md with the fields of
// specs/012-identity-and-authorization.md, in the contract's order.
//
// Three routes ask parse.create about a stored parse: a retry, a figure
// run and an extraction each queue model work on it, and the allow's
// limits are what that work is held to. The work stays the parse's
// owner's: the owner an allow names is read for a submit alone.
var routes = []Route{
	{http.MethodPost, "/files", authorizer.ActionFileCreate, newFile},
	{http.MethodGet, "/files/{file}", authorizer.ActionFileRead, storedFile},
	{http.MethodDelete, "/files/{file}", authorizer.ActionFileDelete, storedFile},

	{http.MethodPost, "/parses", authorizer.ActionParseCreate, newParse},
	{http.MethodGet, "/parses", authorizer.ActionParseList, nil},
	{http.MethodGet, "/parses/{parse}", authorizer.ActionParseRead, storedParse},
	{http.MethodDelete, "/parses/{parse}", authorizer.ActionParseDelete, storedParse},
	{http.MethodPost, "/parses/{parse}/cancel", authorizer.ActionParseCancel, storedParse},
	{http.MethodPost, "/parses/{parse}/retry", authorizer.ActionParseCreate, storedParse},
	{http.MethodGet, "/parses/{parse}/events", authorizer.ActionParseRead, storedParse},
	{http.MethodGet, "/parses/{parse}/document", authorizer.ActionParseRead, storedParse},
	{http.MethodGet, "/parses/{parse}/pages", authorizer.ActionParseRead, storedParse},
	{http.MethodGet, "/parses/{parse}/pages/{page}", authorizer.ActionParseRead, storedParse},
	{http.MethodGet, "/parses/{parse}/pages/{page}/image", authorizer.ActionParseRead, storedParse},
	{http.MethodGet, "/parses/{parse}/blocks", authorizer.ActionParseRead, storedParse},
	{http.MethodGet, "/parses/{parse}/blocks/{ref}", authorizer.ActionParseRead, storedParse},
	{http.MethodGet, "/parses/{parse}/blocks/{ref}/image", authorizer.ActionParseRead, storedParse},
	{http.MethodPost, "/parses/{parse}/figures", authorizer.ActionParseCreate, storedParse},
	{http.MethodGet, "/parses/{parse}/figures", authorizer.ActionParseRead, storedParse},
	{http.MethodGet, "/parses/{parse}/chunks", authorizer.ActionParseRead, storedParse},
	{http.MethodPost, "/parses/{parse}/fields", authorizer.ActionParseCreate, storedParse},
	{http.MethodGet, "/parses/{parse}/fields", authorizer.ActionParseRead, storedParse},
	{http.MethodGet, "/parses/{parse}/fields/{name}", authorizer.ActionParseRead, storedParse},

	{http.MethodGet, "/openapi.yaml", "", nil},
	{http.MethodGet, "/readers", authorizer.ActionReaderList, nil},
	{http.MethodGet, "/usage", authorizer.ActionUsageRead, []string{authorizer.FieldOwner, authorizer.FieldGroup}},
	{http.MethodGet, "/queue", authorizer.ActionQueueRead, []string{authorizer.FieldGroup}},
}

// Routes lists every operation of the contract with the action it asks
// and the fields its resource carries, in the contract's order. The value
// is a fresh copy each call.
func Routes() []Route {
	out := make([]Route, len(routes))
	for i, rt := range routes {
		rt.Fields = slices.Clone(rt.Fields)
		out[i] = rt
	}
	return out
}

// RouteOf finds the row of one operation. ok is false for an operation
// the contract does not have.
func RouteOf(method, path string) (rt Route, ok bool) {
	for _, rt := range Routes() {
		if rt.Method == method && rt.Path == path {
			return rt, true
		}
	}
	return Route{}, false
}

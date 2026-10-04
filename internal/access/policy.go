// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"context"
	"slices"
	"strings"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/authorizer"
)

// ReasonUnknownAction is the deny of an action outside the vocabulary, or
// of one asked about another kind than it acts on. The client refuses
// such a question before it is sent, so reaching this is a caller that
// skipped it. The other reasons the owner policy denies with are the
// shared contract's: probe, anonymous, not_owner and grant.
const ReasonUnknownAction = "unknown_action"

// OwnerPolicy is what decides with LECTIO_AUTHORIZER_URL unset. It is a
// policy with tests and not the absence of one:
//
//   - a subject may perform every action on a parse or a file whose owner
//     is that subject, and may create one in its own name;
//   - a list is narrowed to the subject's own, and so is a usage or queue
//     read that names no owner and no group;
//   - the group is the owner, so a usage or queue read about a group is
//     about the subject whose name the group carries;
//   - every subject may list the readers, which are the server's
//     configuration and nobody's;
//   - an admin may read every owner's parses, files, usage and queue, and
//     may change nothing that is not its own;
//   - the probe and a request with no subject are denied.
//
// The owner is read off the resource, because the handler built that
// resource from the stored object before it asked. A resource with no
// owner is an object that does not exist yet, which is what makes a
// create the one action allowed on it.
type OwnerPolicy struct {
	// Admins are the rendered subjects of LECTIO_ADMIN_SUBJECTS.
	Admins []string
}

// Authorize answers one request, so the owner policy and an operator's
// endpoint are one seam to everything above.
//
// The answer is the policy's own decision narrowed by the grants the
// caller's token carries. A personal access token is narrower than the
// person who holds it: the policy says what the person may do, and the
// grants remove what the token was not given. The narrowing turns an
// allow into a deny and never a deny into an allow. An endpoint built on
// latere.ai/x/pkg/authz/server does the same to whatever it decided;
// lectiod reaches this policy through no endpoint, so it is done here.
func (p *OwnerPolicy) Authorize(_ context.Context, req authz.Request) (authz.Decision, error) {
	d := p.decide(req)
	if !d.Allow {
		return d, nil
	}
	grants, readable := grantsOf(req.Claims)
	if !readable {
		// Grants nobody can read are a token whose reach nobody knows,
		// and the closed answer is a deny. It is no outage, so it is no
		// error.
		return authz.Decision{Reason: authz.ReasonGrant}, nil
	}
	return authz.Restrict(authorizer.Core, d, req, grants), nil
}

// grantsOf reads the grants a token carries. readable is false for a
// claim that does not read as grants.
func grantsOf(claims map[string]any) (grants authz.Grants, readable bool) {
	grants, err := authz.ParseGrants(claims)
	return grants, err == nil
}

// Decide is Authorize under the name latere.ai/x/pkg/authz/server calls a
// decider by, so the policy lectiod runs in process is also the endpoint
// an operator serves from it.
func (p *OwnerPolicy) Decide(ctx context.Context, req authz.Request) (authz.Decision, error) {
	return p.Authorize(ctx, req)
}

// decide is the policy's own answer, before the grants narrow it.
func (p *OwnerPolicy) decide(req authz.Request) authz.Decision {
	kind := authorizer.Kind(req.Action)
	switch {
	case strings.EqualFold(req.Resource.ID, authz.ProbeID):
		return authz.Decision{Reason: authz.ReasonProbe}
	case req.Subject == "":
		return authz.Decision{Reason: authz.ReasonAnonymous}
	case kind == "" || kind != req.Resource.Kind:
		return authz.Decision{Reason: ReasonUnknownAction}
	}

	allow := authz.Decision{Allow: true}
	admin := slices.Contains(p.Admins, req.Subject)
	switch req.Action {
	case authorizer.ActionReaderList:
		return allow
	case authorizer.ActionParseRead, authorizer.ActionFileRead:
		if admin {
			return allow
		}
	case authorizer.ActionParseList:
		return scoped(req.Subject, admin)
	case authorizer.ActionUsageRead, authorizer.ActionQueueRead:
		owner, group := req.Resource.String(authorizer.FieldOwner), req.Resource.String(authorizer.FieldGroup)
		switch {
		case admin || (owner == "" && group == ""):
			return scoped(req.Subject, admin)
		case (owner == "" || owner == req.Subject) && (group == "" || group == req.Subject):
			return allow
		}
		return authz.Decision{Reason: authz.ReasonNotOwner}
	}

	// What is left is about one parse or one file: the shared frame's
	// rows, with no admin, because an admin's reach ended above.
	owner := req.Resource.String(authorizer.FieldOwner)
	if req.Resource.ID != "" && owner == "" {
		// A stored object always has an owner. An id with none is a
		// resource the handler did not finish, and it is not read as a
		// create in the caller's name.
		return authz.Decision{Reason: authz.ReasonNotOwner}
	}
	frame := authz.Policy{Create: createOf(kind)}
	return frame.Decide(req, authz.Object{Exists: owner != "", Owner: owner})
}

// scoped is the allow of a read that ranges over owners: everything for
// an admin, and the subject's own for everyone else.
func scoped(subject string, admin bool) authz.Decision {
	if admin {
		return authz.Decision{Allow: true}
	}
	return authz.Decision{Allow: true, Filter: &authz.Filter{Owners: []string{subject}}}
}

// createOf is the action of a kind that makes one, the one action the
// frame allows on an object that does not exist.
func createOf(kind string) string {
	switch kind {
	case authorizer.KindParse:
		return authorizer.ActionParseCreate
	case authorizer.KindFile:
		return authorizer.ActionFileCreate
	}
	return ""
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/access"
)

// wire renders a resource the way the envelope carries it and reads it
// back as an endpoint would: one flat object.
func wire(t *testing.T, res authz.Resource) map[string]any {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAStoredParseOnTheWire(t *testing.T) {
	res := access.Parse{
		ID: "prs_1", Owner: alice, Class: "batch", Priority: -2, Reader: "small",
		Labels: map[string]string{"batch": "2026-10"},
		Origin: access.Origin{Store: "example", Path: "reports/q3.pdf", Version: "4"},
		Pages:  12,
	}.Resource()
	got := wire(t, res)
	want := map[string]any{
		"kind": "Parse", "id": "prs_1", "owner": alice, "class": "batch", "priority": float64(-2), "reader": "small",
		"labels": map[string]any{"batch": "2026-10"},
		"origin": map[string]any{"store": "example", "path": "reports/q3.pdf", "version": "4"},
		"pages":  float64(12),
	}
	if !maps.EqualFunc(got, want, func(a, b any) bool { return jsonEqual(a, b) }) {
		t.Errorf("the parse is sent as %v, want %v", got, want)
	}
	// In process the owner policy reads the same members.
	if res.String(authorizer.FieldOwner) != alice || res.Int(authorizer.FieldPriority) != -2 || res.Int(authorizer.FieldPages) != 12 {
		t.Errorf("the resource reads as %+v", res)
	}
}

func jsonEqual(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(x) == string(y)
}

// What is not known is left out and never sent empty. A parse always has
// a priority, so that one is always sent.
func TestWhatIsNotKnownIsLeftOut(t *testing.T) {
	cases := map[string]struct {
		res  authz.Resource
		want []string
	}{
		"a submit with nothing named":   {access.Parse{}.Resource(), []string{"kind", "priority"}},
		"a submit that names an origin": {access.Parse{Origin: access.Origin{Path: "a.pdf"}}.Resource(), []string{"kind", "origin", "priority"}},
		"an upload with nothing named":  {access.File{}.Resource(), []string{"kind"}},
		"an upload of a declared size":  {access.File{Size: 10, MediaType: "application/pdf"}.Resource(), []string{"kind", "media_type", "size"}},
		"a stored file":                 {access.File{ID: "fil_1", Owner: bob, Size: 10, MediaType: "image/png"}.Resource(), []string{"id", "kind", "media_type", "owner", "size"}},
		"the list of parses":            {access.Parses(), []string{"kind"}},
		"the readers":                   {access.Readers(), []string{"kind"}},
		"usage with nothing named":      {access.Usage("", ""), []string{"kind"}},
		"usage of an owner and a group": {access.Usage(alice, "acme"), []string{"group", "kind", "owner"}},
		"the queue with nothing named":  {access.Queue(""), []string{"kind"}},
		"the queue of a group":          {access.Queue("acme"), []string{"group", "kind"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := slices.Sorted(maps.Keys(wire(t, c.res)))
			if !slices.Equal(got, c.want) {
				t.Errorf("sends %v, want %v", got, c.want)
			}
		})
	}
	if origin := wire(t, access.Parse{Origin: access.Origin{Path: "a.pdf"}}.Resource())["origin"]; !jsonEqual(origin, map[string]any{"path": "a.pdf"}) {
		t.Errorf("an origin with a path alone is sent as %v", origin)
	}
}

// Every member a resource sends is one the vocabulary publishes for the
// actions that send it, so an endpoint written against the package meets
// no member it was not told of.
func TestAResourceSendsOnlyPublishedFields(t *testing.T) {
	parse := access.Parse{
		ID: "prs_1", Owner: alice, Class: "batch", Priority: 1, Reader: "small", Labels: map[string]string{"a": "b"},
		Origin: access.Origin{Store: "s"}, Pages: 3,
	}.Resource()
	file := access.File{ID: "fil_1", Owner: alice, Size: 10, MediaType: "image/png"}.Resource()
	upload := access.File{Owner: alice, Size: 10, MediaType: "image/png"}.Resource()
	cases := map[string]authz.Resource{
		authorizer.ActionParseCreate: parse, authorizer.ActionParseRead: parse, authorizer.ActionParseCancel: parse,
		authorizer.ActionParseDelete: parse, authorizer.ActionParseList: access.Parses(),
		authorizer.ActionFileCreate: upload, authorizer.ActionFileRead: file, authorizer.ActionFileDelete: file,
		authorizer.ActionReaderList: access.Readers(), authorizer.ActionUsageRead: access.Usage(alice, "acme"),
		authorizer.ActionQueueRead: access.Queue("acme"),
	}
	if len(cases) != len(authorizer.Actions()) {
		t.Fatalf("%d actions have a resource here, and the vocabulary has %d", len(cases), len(authorizer.Actions()))
	}
	for action, res := range cases {
		if res.Kind != authorizer.Kind(action) {
			t.Errorf("%s is asked about a %s, and acts on a %s", action, res.Kind, authorizer.Kind(action))
		}
		published := authorizer.Fields(action)
		for name := range wire(t, res) {
			if name != "kind" && !slices.Contains(published, name) {
				t.Errorf("%s sends %q, which the vocabulary does not publish for it", action, name)
			}
		}
		// The fullest resource of an action sends every published member.
		if got := len(wire(t, res)) - 1; got != len(published) {
			t.Errorf("%s sends %d members and the vocabulary publishes %d", action, got, len(published))
		}
	}
}

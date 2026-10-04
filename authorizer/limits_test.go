// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer_test

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/authorizer"
)

// wireKeys are the members of the limits object, the names an authorizer
// already emits. A key added, dropped or renamed is a change of the wire.
var wireKeys = []string{
	"owner", "group", "weight", "project", "project_weight", "max_running", "max_queued",
	"max_priority", "classes", "readers", "max_file_bytes", "max_pages", "pages_per_day",
	"retention_seconds",
}

func ptr[T any](v T) *T { return &v }

// full is a limits object that names every member.
func full() authorizer.WireLimits {
	return authorizer.WireLimits{
		Owner: ptr("https://issuer.example|org"), Group: ptr("org"), Weight: ptr(4),
		Project: ptr("reports"), ProjectWeight: ptr(2), MaxRunning: ptr(8), MaxQueued: ptr(100),
		MaxPriority: ptr(5), Classes: []string{"batch"}, Readers: []string{"small"},
		MaxFileBytes: ptr(int64(1 << 20)), MaxPages: ptr(50), PagesPerDay: ptr(1000),
		RetentionSeconds: ptr(int64(3600)),
	}
}

// decision is an allow whose limits object is raw.
func decision(raw string) authz.Decision {
	return authz.Decision{Allow: true, Limits: json.RawMessage(raw)}
}

func TestTheWireNamesEveryMemberAndNoOther(t *testing.T) {
	raw, err := json.Marshal(full())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	keys := slices.Sorted(maps.Keys(got))
	if want := slices.Sorted(slices.Values(wireKeys)); !slices.Equal(keys, want) {
		t.Errorf("the limits object carries %v, want %v", keys, want)
	}
	// Every member of Limits has a key, so nothing a request is held to
	// lacks a way to be named.
	if fields := reflect.TypeFor[authorizer.Limits]().NumField(); fields != len(wireKeys) {
		t.Errorf("Limits has %d members and the wire names %d", fields, len(wireKeys))
	}
}

func TestAnObjectThatNamesNothingIsEmptyOnTheWire(t *testing.T) {
	raw, err := json.Marshal(authorizer.WireLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Errorf("an object that names nothing renders as %s", raw)
	}
	// A member set to zero is still sent: that is how an answer lifts a
	// default cap.
	raw, err = json.Marshal(authorizer.WireLimits{MaxQueued: ptr(0)})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"max_queued":0}` {
		t.Errorf("a cap of zero renders as %s", raw)
	}
}

func TestDecodeReadsWhatAnAuthorizerRenders(t *testing.T) {
	want := full()
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := authorizer.DecodeLimits(decision(string(raw)))
	if err != nil {
		t.Fatalf("DecodeLimits: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded %+v, want %+v", got, want)
	}
}

func TestDecodeWithNoLimits(t *testing.T) {
	for _, d := range []authz.Decision{{Allow: true}, decision(`{}`), decision(`{"weight": null, "classes": null}`)} {
		got, err := authorizer.DecodeLimits(d)
		if err != nil {
			t.Fatalf("DecodeLimits(%s): %v", d.Limits, err)
		}
		if !reflect.DeepEqual(got, authorizer.WireLimits{}) {
			t.Errorf("DecodeLimits(%s) names %+v", d.Limits, got)
		}
	}
}

func TestDecodeRefusesWhatItCannotHold(t *testing.T) {
	cases := map[string]string{
		"not an object":             `[1]`,
		"a number as text":          `{"weight": "4"}`,
		"a fraction":                `{"max_pages": 1.5}`,
		"a list of numbers":         `{"classes": [1]}`,
		"weight below zero":         `{"weight": -1}`,
		"weight above the most":     `{"weight": 1001}`,
		"project weight above":      `{"project_weight": 1001}`,
		"running below zero":        `{"max_running": -1}`,
		"queued below zero":         `{"max_queued": -1}`,
		"queued past a column":      `{"max_queued": 2147483648}`,
		"priority below zero":       `{"max_priority": -1}`,
		"pages below zero":          `{"max_pages": -1}`,
		"pages per day below zero":  `{"pages_per_day": -1}`,
		"file bytes below zero":     `{"max_file_bytes": -1}`,
		"retention below zero":      `{"retention_seconds": -1}`,
		"retention past a duration": `{"retention_seconds": 9223372037}`,
		"an empty class":            `{"classes": ["batch", ""]}`,
		"an empty reader":           `{"readers": [""]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := authorizer.DecodeLimits(decision(raw))
			if err == nil {
				t.Fatalf("DecodeLimits(%s) = %+v, want an error", raw, got)
			}
			if _, unknown := errors.AsType[*authorizer.UnknownLimit](err); unknown {
				t.Errorf("DecodeLimits(%s) reports an unknown member: %v", raw, err)
			}
			if !strings.Contains(err.Error(), "limits") {
				t.Errorf("the error %q does not say it is about the limits", err)
			}
		})
	}
}

func TestDecodeAcceptsTheEdges(t *testing.T) {
	raw := `{"weight": 1000, "project_weight": 0, "max_queued": 2147483647, "retention_seconds": 9223372036, "max_priority": 0}`
	if _, err := authorizer.DecodeLimits(decision(raw)); err != nil {
		t.Errorf("DecodeLimits(%s): %v", raw, err)
	}
}

// A member this version does not know is a limit it cannot enforce, and
// the error says which, so the refusal is not a silent pass.
func TestDecodeNamesAMemberItDoesNotKnow(t *testing.T) {
	_, err := authorizer.DecodeLimits(decision(`{"tokens_per_day": 5, "weight": 2, "burst": 1}`))
	unknown, ok := errors.AsType[*authorizer.UnknownLimit](err)
	if !ok {
		t.Fatalf("DecodeLimits = %v, want an *UnknownLimit", err)
	}
	if want := []string{"burst", "tokens_per_day"}; !slices.Equal(unknown.Members, want) {
		t.Errorf("the unknown members are %v, want %v", unknown.Members, want)
	}
	if msg := unknown.Error(); !strings.Contains(msg, "burst, tokens_per_day") {
		t.Errorf("the error %q does not name the members", msg)
	}
}

// defaults are a server's configured limits, each member set so that a
// change to it shows.
func defaults() authorizer.Limits {
	return authorizer.Limits{
		Owner: "https://issuer.example|alice", Group: "", Weight: 1, Project: "", ProjectWeight: 1,
		MaxRunning: 16, MaxQueued: 200, MaxPriority: 10, Classes: nil, Readers: nil,
		MaxFileBytes: 256 << 20, MaxPages: 3000, PagesPerDay: 0, Retention: 720 * time.Hour,
	}
}

// Each member of Limits: an allow that carries it changes the limits in
// force, and an allow that does not leaves the default.
func TestEachMemberOverTheDefaults(t *testing.T) {
	cases := []struct {
		member string
		named  authorizer.WireLimits
		want   func(*authorizer.Limits)
	}{
		{"owner", authorizer.WireLimits{Owner: ptr("org")}, func(l *authorizer.Limits) { l.Owner = "org" }},
		{"group", authorizer.WireLimits{Group: ptr("org")}, func(l *authorizer.Limits) { l.Group = "org" }},
		{"weight", authorizer.WireLimits{Weight: ptr(8)}, func(l *authorizer.Limits) { l.Weight = 8 }},
		{"project", authorizer.WireLimits{Project: ptr("reports")}, func(l *authorizer.Limits) { l.Project = "reports" }},
		{"project_weight", authorizer.WireLimits{ProjectWeight: ptr(3)}, func(l *authorizer.Limits) { l.ProjectWeight = 3 }},
		{"max_running", authorizer.WireLimits{MaxRunning: ptr(2)}, func(l *authorizer.Limits) { l.MaxRunning = 2 }},
		{"max_queued", authorizer.WireLimits{MaxQueued: ptr(5)}, func(l *authorizer.Limits) { l.MaxQueued = 5 }},
		{"max_priority", authorizer.WireLimits{MaxPriority: ptr(1)}, func(l *authorizer.Limits) { l.MaxPriority = 1 }},
		{"classes", authorizer.WireLimits{Classes: []string{"batch"}}, func(l *authorizer.Limits) { l.Classes = []string{"batch"} }},
		{"readers", authorizer.WireLimits{Readers: []string{"small"}}, func(l *authorizer.Limits) { l.Readers = []string{"small"} }},
		{"max_file_bytes", authorizer.WireLimits{MaxFileBytes: ptr(int64(1024))}, func(l *authorizer.Limits) { l.MaxFileBytes = 1024 }},
		{"max_pages", authorizer.WireLimits{MaxPages: ptr(10)}, func(l *authorizer.Limits) { l.MaxPages = 10 }},
		{"pages_per_day", authorizer.WireLimits{PagesPerDay: ptr(500)}, func(l *authorizer.Limits) { l.PagesPerDay = 500 }},
		{"retention_seconds", authorizer.WireLimits{RetentionSeconds: ptr(int64(60))}, func(l *authorizer.Limits) { l.Retention = time.Minute }},
	}
	var covered []string
	for _, c := range cases {
		covered = append(covered, c.member)
		t.Run(c.member, func(t *testing.T) {
			want := defaults()
			c.want(&want)
			got := c.named.Over(defaults())
			if !reflect.DeepEqual(got, want) {
				t.Errorf("an allow carrying %s gives %+v, want %+v", c.member, got, want)
			}
			if reflect.DeepEqual(got, defaults()) {
				t.Errorf("an allow carrying %s changes nothing", c.member)
			}
		})
	}
	if !slices.Equal(covered, wireKeys) {
		t.Errorf("the cases cover %v, and the wire names %v", covered, wireKeys)
	}
	if got := (authorizer.WireLimits{}).Over(defaults()); !reflect.DeepEqual(got, defaults()) {
		t.Errorf("an allow that names nothing gives %+v, want the defaults", got)
	}
}

// What a named zero, an empty name and a figure above the server's do.
func TestWhatANamedMemberDoesNotChange(t *testing.T) {
	cases := []struct {
		name  string
		named authorizer.WireLimits
		want  func(*authorizer.Limits)
	}{
		{"an empty owner is the default owner", authorizer.WireLimits{Owner: ptr("")}, nil},
		{"an empty class list is both classes", authorizer.WireLimits{Classes: []string{}}, nil},
		{"a weight of zero keeps the default", authorizer.WireLimits{Weight: ptr(0), ProjectWeight: ptr(0)}, nil},
		{"a file size above the server's is the server's", authorizer.WireLimits{MaxFileBytes: ptr(int64(1 << 40))}, nil},
		{"a file size of zero is the server's", authorizer.WireLimits{MaxFileBytes: ptr(int64(0))}, nil},
		{"a page count above the server's is the server's", authorizer.WireLimits{MaxPages: ptr(5000)}, nil},
		{"a retention above the server's is the server's", authorizer.WireLimits{RetentionSeconds: ptr(int64(31536000))}, nil},
		{"a cap of zero lifts the default cap", authorizer.WireLimits{MaxRunning: ptr(0), MaxQueued: ptr(0)},
			func(l *authorizer.Limits) { l.MaxRunning, l.MaxQueued = 0, 0 }},
		{"a priority bound of zero is a bound", authorizer.WireLimits{MaxPriority: ptr(0)},
			func(l *authorizer.Limits) { l.MaxPriority = 0 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := defaults()
			if c.want != nil {
				c.want(&want)
			}
			if got := c.named.Over(defaults()); !reflect.DeepEqual(got, want) {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

// A server that sets no ceiling of its own takes the allow's.
func TestACeilingWhereTheServerSetsNone(t *testing.T) {
	named := authorizer.WireLimits{MaxFileBytes: ptr(int64(1024)), MaxPages: ptr(10), RetentionSeconds: ptr(int64(60))}
	got := named.Over(authorizer.Limits{})
	if got.MaxFileBytes != 1024 || got.MaxPages != 10 || got.Retention != time.Minute {
		t.Errorf("over no defaults the ceilings are %d bytes, %d pages, %s", got.MaxFileBytes, got.MaxPages, got.Retention)
	}
}

// The limits in force share no list with the defaults or the answer, so a
// request that edits its own changes no other request's.
func TestOverCopiesItsLists(t *testing.T) {
	d := authorizer.Limits{Classes: []string{"interactive"}, Readers: []string{"small"}}
	got := (authorizer.WireLimits{}).Over(d)
	got.Classes[0], got.Readers[0] = "changed", "changed"
	if d.Classes[0] != "interactive" || d.Readers[0] != "small" {
		t.Errorf("the defaults became %v and %v", d.Classes, d.Readers)
	}
	named := authorizer.WireLimits{Classes: []string{"batch"}, Readers: []string{"large"}}
	got = named.Over(d)
	got.Classes[0], got.Readers[0] = "changed", "changed"
	if named.Classes[0] != "batch" || named.Readers[0] != "large" {
		t.Errorf("the answer became %v and %v", named.Classes, named.Readers)
	}
}

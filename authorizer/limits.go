// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"
)

// MaxWeight is the largest share a group or a project may be given. A
// weight is a whole number from 1 to MaxWeight.
const MaxWeight = 1000

// Limits are what a request is held to: the server's configured defaults
// with what the allow named laid over them. Lectio enforces what it is
// told and stores no plan.
type Limits struct {
	// Owner is the owner a created parse or file is recorded under. The
	// allow of parse.create and of file.create names it, so that Lectio
	// reads no claim to learn whose the new object is. Empty is the
	// caller's subject.
	Owner string
	// Group is the fairness group the parse joins: the unit two callers
	// share one queue and one budget in. Empty is the owner.
	Group string
	// Weight is the group's share of service, 1 to MaxWeight. 0 keeps the
	// default.
	Weight int
	// Project is the project of the group the parse joins. Empty is the
	// group's own project.
	Project string
	// ProjectWeight is the project's share of what its group is served, 1
	// to MaxWeight. It moves service between the projects of one group
	// and never between groups. 0 keeps the default.
	ProjectWeight int
	// MaxRunning is how many tasks of the group may be leased at once. 0
	// is no cap.
	MaxRunning int
	// MaxQueued is how many parses of the group may be waiting or running
	// at once. 0 is no cap.
	MaxQueued int
	// MaxPriority bounds a parse's priority on both sides: a submit names
	// one from -MaxPriority to MaxPriority. 0 admits priority 0 alone.
	MaxPriority int
	// Classes are the classes the caller may submit in. Empty is both.
	Classes []string
	// Readers are the readers the caller may pin. Empty is all.
	Readers []string
	// MaxFileBytes is the largest file the caller may upload or name. An
	// allow lowers the server's figure and never raises it.
	MaxFileBytes int64
	// MaxPages is the most pages one parse may select. An allow lowers
	// the server's figure and never raises it.
	MaxPages int
	// PagesPerDay is how many pages the group's parses may count in one
	// day, counted in UTC. 0 is no budget.
	PagesPerDay int
	// Retention is how long a file or a parse's results are kept. An
	// allow lowers the server's figure and never raises it.
	Retention time.Duration
}

// WireLimits is the limits object as an answer carries it. Every member is
// optional: one the object does not name stays nil and is left out when
// the value is rendered, and the server's default for it stays in force.
// A number is a pointer, so a member deliberately set to zero is still
// sent, which is how an answer lifts a default cap or bounds the priority
// at zero.
//
// The allow of file.create is read for owner, group, max_file_bytes and
// retention_seconds. The allow of parse.create is read for every member.
// No other action's limits are read.
type WireLimits struct {
	Owner            *string  `json:"owner,omitempty"`
	Group            *string  `json:"group,omitempty"`
	Weight           *int     `json:"weight,omitempty"`
	Project          *string  `json:"project,omitempty"`
	ProjectWeight    *int     `json:"project_weight,omitempty"`
	MaxRunning       *int     `json:"max_running,omitempty"`
	MaxQueued        *int     `json:"max_queued,omitempty"`
	MaxPriority      *int     `json:"max_priority,omitempty"`
	Classes          []string `json:"classes,omitempty"`
	Readers          []string `json:"readers,omitempty"`
	MaxFileBytes     *int64   `json:"max_file_bytes,omitempty"`
	MaxPages         *int     `json:"max_pages,omitempty"`
	PagesPerDay      *int     `json:"pages_per_day,omitempty"`
	RetentionSeconds *int64   `json:"retention_seconds,omitempty"`
}

// members are the wire names WireLimits declares, read off its tags so the
// type is the one place a member is named.
var members = func() []string {
	t := reflect.TypeFor[WireLimits]()
	out := make([]string, t.NumField())
	for i := range out {
		out[i], _, _ = strings.Cut(t.Field(i).Tag.Get("json"), ",")
	}
	return out
}()

// UnknownLimit is a limits object that names members this version of
// Lectio does not know. A limit Lectio is handed and cannot enforce is a
// refusal and never a silent pass, so lectiod answers the request the
// allow was for with capability_unsupported.
type UnknownLimit struct {
	// Members are the names, sorted.
	Members []string
}

func (e *UnknownLimit) Error() string {
	return "limits names " + strings.Join(e.Members, ", ") + ", which this version does not enforce"
}

// maxRetentionSeconds is the longest retention a duration can hold.
const maxRetentionSeconds = math.MaxInt64 / int64(time.Second)

// DecodeLimits reads a decision's limits object. A decision with none is
// the zero WireLimits, which names nothing. An object that does not
// parse, a figure below zero, and a weight above MaxWeight are errors, and
// lectiod treats the answer as no decision: a ceiling it cannot read is
// not a ceiling it can hold. A member the object names and this version
// does not know is an *UnknownLimit.
func DecodeLimits(d authz.Decision) (WireLimits, error) {
	if len(d.Limits) == 0 {
		return WireLimits{}, nil
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(d.Limits, &named); err != nil {
		return WireLimits{}, fmt.Errorf("limits is not an object: %w", err)
	}
	var unknown []string
	for name := range named {
		if !slices.Contains(members, name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return WireLimits{}, &UnknownLimit{Members: unknown}
	}
	var w WireLimits
	if err := json.Unmarshal(d.Limits, &w); err != nil {
		return WireLimits{}, fmt.Errorf("limits: %w", err)
	}
	if err := w.validate(); err != nil {
		return WireLimits{}, err
	}
	return w, nil
}

// validate refuses a figure Lectio could not hold.
func (w WireLimits) validate() error {
	for _, f := range []struct {
		name string
		n    *int
		most int
	}{
		{"weight", w.Weight, MaxWeight},
		{"project_weight", w.ProjectWeight, MaxWeight},
		{"max_running", w.MaxRunning, math.MaxInt32},
		{"max_queued", w.MaxQueued, math.MaxInt32},
		{"max_priority", w.MaxPriority, math.MaxInt32},
		{"max_pages", w.MaxPages, math.MaxInt32},
		{"pages_per_day", w.PagesPerDay, math.MaxInt32},
	} {
		switch {
		case f.n == nil:
		case *f.n < 0:
			return fmt.Errorf("limits.%s is %d, below zero", f.name, *f.n)
		case *f.n > f.most:
			return fmt.Errorf("limits.%s is %d, above %d", f.name, *f.n, f.most)
		}
	}
	if w.MaxFileBytes != nil && *w.MaxFileBytes < 0 {
		return fmt.Errorf("limits.max_file_bytes is %d, below zero", *w.MaxFileBytes)
	}
	if s := w.RetentionSeconds; s != nil && (*s < 0 || *s > maxRetentionSeconds) {
		return fmt.Errorf("limits.retention_seconds is %d, outside 0 to %d", *s, maxRetentionSeconds)
	}
	for name, list := range map[string][]string{"classes": w.Classes, "readers": w.Readers} {
		if slices.Contains(list, "") {
			return errors.New("limits." + name + " holds an empty name")
		}
	}
	return nil
}

// Over lays what the answer named over the server's defaults and returns
// the limits in force. A member the answer does not name leaves the
// default. Of the members it names:
//
//   - owner, group and project replace the default when they are not
//     empty, and so do classes and readers;
//   - weight and project_weight replace it when above zero;
//   - max_running, max_queued, max_priority and pages_per_day replace it
//     whatever they are, zero included;
//   - max_file_bytes, max_pages and retention_seconds replace it only when
//     above zero and below it, or when the server sets none.
func (w WireLimits) Over(defaults Limits) Limits {
	l := defaults
	l.Classes, l.Readers = slices.Clone(defaults.Classes), slices.Clone(defaults.Readers)

	for _, f := range []struct {
		named *string
		into  *string
	}{{w.Owner, &l.Owner}, {w.Group, &l.Group}, {w.Project, &l.Project}} {
		if f.named != nil && *f.named != "" {
			*f.into = *f.named
		}
	}
	if len(w.Classes) > 0 {
		l.Classes = slices.Clone(w.Classes)
	}
	if len(w.Readers) > 0 {
		l.Readers = slices.Clone(w.Readers)
	}

	if w.Weight != nil && *w.Weight > 0 {
		l.Weight = *w.Weight
	}
	if w.ProjectWeight != nil && *w.ProjectWeight > 0 {
		l.ProjectWeight = *w.ProjectWeight
	}
	for _, f := range []struct{ named, into *int }{
		{w.MaxRunning, &l.MaxRunning}, {w.MaxQueued, &l.MaxQueued},
		{w.MaxPriority, &l.MaxPriority}, {w.PagesPerDay, &l.PagesPerDay},
	} {
		if f.named != nil {
			*f.into = *f.named
		}
	}

	if w.MaxFileBytes != nil {
		l.MaxFileBytes = lower(*w.MaxFileBytes, defaults.MaxFileBytes)
	}
	if w.MaxPages != nil {
		l.MaxPages = int(lower(int64(*w.MaxPages), int64(defaults.MaxPages)))
	}
	if w.RetentionSeconds != nil {
		l.Retention = time.Duration(lower(*w.RetentionSeconds*int64(time.Second), int64(defaults.Retention)))
	}
	return l
}

// lower is the ceiling in force between what an answer named and what the
// server is configured with: the named figure when it is above zero and
// below the server's, or when the server sets none, and the server's
// otherwise.
func lower(named, server int64) int64 {
	if named > 0 && (server == 0 || named < server) {
		return named
	}
	return server
}

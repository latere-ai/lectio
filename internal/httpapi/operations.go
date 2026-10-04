// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"net/http"
	"slices"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/store"
)

// The bounds of a read of the meters (specs/013-limits-and-usage.md).
const (
	// usageIntervals is the most intervals one read may span, so an answer
	// holds at most that many sums per key.
	usageIntervals = 1000
	// usageHours and usageDays are how far back a read that names no
	// beginning goes: a day of hours, or 30 days.
	usageHours = 24
	usageDays  = 30
)

// The intervals the meters are summed over, with how far back a read of
// each goes when it names no beginning. Both are in UTC.
var usageSteps = map[string]struct {
	step time.Duration
	back int
}{
	"hour": {time.Hour, usageHours},
	"day":  {24 * time.Hour, usageDays},
}

// usageView is the answer of a read of the meters: the read as the server
// took it, with its span moved out to whole intervals, and the sums.
type usageView struct {
	By       string           `json:"by"`
	Interval string           `json:"interval"`
	From     time.Time        `json:"from"`
	To       time.Time        `json:"to"`
	Sums     []store.UsageSum `json:"sums"`
}

// ranging lays the filter of an allow over the one owner, or the one
// group, a read names. list is whom the read then ranges over, nil for
// everyone, and none says nobody is left: the filter lists nobody, lists
// others than the one named, or narrows by labels, which neither a meter
// nor a queue carries.
func ranging(named string, by *authz.Filter) (list []string, none bool) {
	if named != "" {
		list = []string{named}
	}
	switch {
	case by == nil:
		return list, false
	case len(by.Labels) > 0:
		return nil, true
	case by.Owners == nil:
		return list, false
	case named == "":
		return slices.Clone(by.Owners), len(by.Owners) == 0
	}
	return list, !slices.Contains(by.Owners, named)
}

// span reads the span of a read of the meters and moves it out to whole
// intervals: from back to the beginning of its interval and to on to the
// end of its own, so no sum in the answer is of a part of an interval. A
// read that names no end ends now, and one that names no beginning goes
// back the default of its interval.
func (s *Server) span(r *http.Request, interval string) (from, to time.Time, err error) {
	q, unit := r.URL.Query(), usageSteps[interval]
	to = s.now()
	if raw := q.Get("to"); raw != "" {
		if to, err = time.Parse(time.RFC3339, raw); err != nil {
			return from, to, invalid("to", "to is a time such as 2026-10-04T12:00:00Z")
		}
	}
	if aligned := to.UTC().Truncate(unit.step); aligned.Equal(to) {
		to = aligned
	} else {
		to = aligned.Add(unit.step)
	}
	from = to.Add(-time.Duration(unit.back) * unit.step)
	if raw := q.Get("from"); raw != "" {
		if from, err = time.Parse(time.RFC3339, raw); err != nil {
			return from, to, invalid("from", "from is a time such as 2026-10-04T12:00:00Z")
		}
		from = from.UTC().Truncate(unit.step)
	}
	switch {
	case !from.Before(to):
		return from, to, invalid("from", "from is before to")
	case to.Sub(from) > usageIntervals*unit.step:
		return from, to, invalid("from", "a read spans at most %d intervals", usageIntervals)
	}
	return from, to, nil
}

// getUsage reads the meters: what was read and what it cost, summed by a
// group, an owner or a reader over hours or days. The question carries the
// owner and the group the request names, and the answer is narrowed to
// them and to the owners its allow's filter lists.
func (s *Server) getUsage(w http.ResponseWriter, r *http.Request, c call) error {
	q := r.URL.Query()
	view := usageView{By: q.Get("by"), Interval: q.Get("interval"), Sums: []store.UsageSum{}}
	if view.By == "" {
		view.By = "group"
	}
	if view.Interval == "" {
		view.Interval = "hour"
	}
	if !slices.Contains([]string{"group", "owner", "reader"}, view.By) {
		return invalid("by", "by is group, owner or reader")
	}
	if _, known := usageSteps[view.Interval]; !known {
		return invalid("interval", "interval is hour or day")
	}
	var err error
	if view.From, view.To, err = s.span(r, view.Interval); err != nil {
		return err
	}

	owner, group := q.Get("owner"), q.Get("group")
	d, err := s.allowed(r, c, access.Usage(owner, group))
	if err != nil {
		return err
	}
	query := store.UsageQuery{By: view.By, Interval: view.Interval, From: view.From, To: view.To}
	if group != "" {
		query.Groups = []string{group}
	}
	var none bool
	if query.Owners, none = ranging(owner, d.Filter); !none {
		sums, err := s.Backend.Usage(r.Context(), query)
		if err != nil {
			return err
		}
		// A read with nothing in it is an empty list, and never null.
		view.Sums = append(view.Sums, sums...)
	}
	httpjson.Write(w, http.StatusOK, view)
	return nil
}

// getQueue reads the queue: what waits and what runs for each group and
// each of its projects, and where the readers' pools stand. The question
// carries the group the request names. The filter of its allow lists the
// groups the caller may see as owners: a group is named by the owner it is
// the group of, which under the owner policy is every owner's own.
func (s *Server) getQueue(w http.ResponseWriter, r *http.Request, c call) error {
	group := r.URL.Query().Get("group")
	d, err := s.allowed(r, c, access.Queue(group))
	if err != nil {
		return err
	}
	view := store.Queue{}
	if groups, none := ranging(group, d.Filter); !none {
		if view, err = s.Backend.Queue(r.Context(), groups); err != nil {
			return err
		}
	}
	// A view with nothing in it holds empty lists, and never null.
	if view.Groups == nil {
		view.Groups = []store.QueueGroup{}
	}
	if view.Pools == nil {
		view.Pools = []store.QueuePool{}
	}
	httpjson.Write(w, http.StatusOK, view)
	return nil
}

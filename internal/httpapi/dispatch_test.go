// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// What the group and the project an allow names do to the order work is
// dispatched in (specs/012-identity-and-authorization.md,
// specs/006-fairness-and-priority.md). The parses are submitted through the
// API, each by its own subject under its own allow, into the durable store.
// No worker runs: the case claims the tasks itself, one at a time, as one
// worker with one slot would, and counts whose page each claim is.

// byCaller answers each subject's submit with the limits a case gave it.
func byCaller(limits map[string]string) func(authz.Request) (authz.Decision, error) {
	return func(req authz.Request) (authz.Decision, error) {
		d := authz.Decision{Allow: true}
		if raw := limits[req.Subject]; raw != "" {
			d.Limits = []byte(raw)
		}
		return d, nil
	}
}

// dispatching is a durable server with no worker, whose authorizer answers
// each subject with its own limits.
type dispatching struct {
	*env
	rec *recorder
	st  *postgres.Store
	// worker is the registration the case claims under, and holds the
	// claim it has not settled yet.
	worker string
	holds  *tasks.Claim
}

func dispatched(t *testing.T, limits map[string]string) *dispatching {
	t.Helper()
	durably(t, true)
	rec := &recorder{answer: byCaller(limits)}
	e := serve(t, func(s *Server, _ *run.Runner) { s.Authz = access.NewAuthorizer(rec, configured()) })
	d := &dispatching{env: e, rec: rec, st: storeOf(t, e)}
	id, err := d.st.Register(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d.worker = id
	return d
}

// queue submits a parse as a caller and returns the answer. The parse
// waits: nothing runs it but the case.
func (d *dispatching) queue(token string) reply {
	d.t.Helper()
	as := d.as(token)
	file := as.upload("scan.png", sheet(d.t))
	return as.do("POST", "/parses", `{"source":{"file":"`+file+`"},"reuse":false}`)
}

// next settles the task the case holds as done and claims one more. A
// prepare is settled with pages pages for a reader, so its parse has that
// many page tasks whatever its file holds. It reports false when nothing
// was left to claim.
func (d *dispatching) next(pages int) (tasks.Claim, bool) {
	d.t.Helper()
	req := tasks.Request{Free: 1, Idle: d.holds == nil}
	if c := d.holds; c != nil {
		s := tasks.Settle{Parse: c.Parse, Task: c.Task, Token: c.Token, Outcome: tasks.Done, Units: 1}
		switch c.Kind {
		case tasks.Prepare:
			selected := make([]int, pages)
			for i := range selected {
				selected[i] = i + 1
			}
			manifest, err := json.Marshal(map[string]any{"media_type": "application/pdf", "source": "reader", "pages_total": pages, "selected": selected})
			if err != nil {
				d.t.Fatal(err)
			}
			s.Prepare = &tasks.Prepared{Manifest: manifest, Pages: selected}
		case tasks.Assemble:
			s.Assemble = &tasks.Assembled{Index: "parses/" + c.Parse + "/document." + strconv.FormatInt(c.Token, 10) + ".json"}
		default:
			s.Output, s.Health = "parses/"+c.Parse+"/pages/"+c.Task+".json", tasks.Healthy
		}
		req.Settles, req.Idle = []tasks.Settle{s}, true
	}
	reply, err := d.st.Exchange(context.Background(), d.worker, req)
	if err != nil || reply.Gone || len(reply.Refused) != 0 {
		d.t.Fatalf("the exchange: %+v, %v", reply, err)
	}
	d.holds = nil
	if len(reply.Claims) == 0 {
		return tasks.Claim{}, false
	}
	d.holds = &reply.Claims[0]
	return reply.Claims[0], true
}

// served claims n page tasks, each parse prepared with pages pages, and
// counts them by the owner of their parse, by its group and by its
// project.
func (d *dispatching) served(n, pages int) (byOwner, byGroup, byProject map[string]int) {
	d.t.Helper()
	byOwner, byGroup, byProject = map[string]int{}, map[string]int{}, map[string]int{}
	for counted := 0; counted < n; {
		c, ok := d.next(pages)
		if !ok {
			d.t.Fatalf("the queue ran dry after %d of %d pages", counted, n)
		}
		if c.Kind != tasks.Page {
			continue
		}
		counted++
		byOwner[c.Context.Owner]++
		byGroup[c.Group]++
		byProject[c.Group+"/"+c.Project]++
	}
	return byOwner, byGroup, byProject
}

// near reports whether got is within tolerance of want.
func near(got, want, tolerance int) bool { return got >= want-tolerance && got <= want+tolerance }

// TestSubjectsOfOneGroupAreServedAsOne: two subjects whose allows name one
// group share its bound on queued parses and are served as one group
// against another: together they receive what the other group receives
// alone. Two subjects with groups of their own are served by weight.
func TestSubjectsOfOneGroupAreServedAsOne(t *testing.T) {
	d := dispatched(t, map[string]string{
		"alice": `{"group": "acme", "max_queued": 2}`,
		"bob":   `{"group": "acme", "max_queued": 2}`,
		"root":  `{"group": "solo", "weight": 1}`,
	})
	// The group may hold 2 parses, whoever submits them.
	for i, token := range []string{"alice-token", "bob-token"} {
		if r := d.queue(token); r.status != http.StatusAccepted {
			t.Fatalf("parse %d of the group: %d %s", i+1, r.status, r.body)
		}
	}
	for _, token := range []string{"alice-token", "bob-token"} {
		if r := d.queue(token); r.status != http.StatusTooManyRequests || r.code(t) != "queue_full" {
			t.Fatalf("a third parse of a group that may hold 2: %d %s", r.status, r.body)
		}
	}
	// Another group holds 2 parses of its own, of one subject.
	for range 2 {
		if r := d.queue("root-token"); r.status != http.StatusAccepted {
			t.Fatalf("a parse of another group: %d %s", r.status, r.body)
		}
	}

	// Every parse has 60 pages. Over 80 page dispatches the two groups are
	// served alike, and the group of two is not served twice.
	owners, groups, _ := d.served(80, 60)
	if !near(groups["acme"], 40, 2) || !near(groups["solo"], 40, 2) {
		t.Errorf("the groups were served %v, want 40 each", groups)
	}
	if !near(owners["alice"]+owners["bob"], 40, 2) || owners["alice"] == 0 || owners["bob"] == 0 {
		t.Errorf("the owners were served %v, want alice and bob to share 40", owners)
	}
}

// TestSubjectsOfDifferentGroupsAreServedByWeight: two subjects whose allows
// name different groups are served in the ratio of the groups' weights.
func TestSubjectsOfDifferentGroupsAreServedByWeight(t *testing.T) {
	d := dispatched(t, map[string]string{
		"alice": `{"group": "light", "weight": 1}`,
		"bob":   `{"group": "heavy", "weight": 3}`,
	})
	for _, token := range []string{"alice-token", "bob-token"} {
		if r := d.queue(token); r.status != http.StatusAccepted {
			t.Fatalf("%s: %d %s", token, r.status, r.body)
		}
	}
	owners, groups, _ := d.served(80, 100)
	if !near(groups["light"], 20, 2) || !near(groups["heavy"], 60, 2) {
		t.Errorf("the groups were served %v, want 20 and 60", groups)
	}
	if owners["alice"] != groups["light"] || owners["bob"] != groups["heavy"] {
		t.Errorf("the owners were served %v and the groups %v", owners, groups)
	}
}

// TestProjectsDivideTheirGroupAndNoOther: two subjects whose allows name
// one group and two projects share the group's bound on queued parses and
// are served in the ratio of their project weights within the group. What
// another group is served does not change with the projects' weights: the
// same parses are dispatched again with the weights turned around, and the
// other group's count is the same.
func TestProjectsDivideTheirGroupAndNoOther(t *testing.T) {
	run := func(aliceWeight, bobWeight int) (owners, groups, projects map[string]int) {
		d := dispatched(t, map[string]string{
			"alice": `{"group": "acme", "max_queued": 2, "project": "reports", "project_weight": ` + strconv.Itoa(aliceWeight) + `}`,
			"bob":   `{"group": "acme", "max_queued": 2, "project": "invoices", "project_weight": ` + strconv.Itoa(bobWeight) + `}`,
			"root":  `{"group": "solo"}`,
		})
		for _, token := range []string{"alice-token", "bob-token", "root-token"} {
			if r := d.queue(token); r.status != http.StatusAccepted {
				t.Fatalf("%s: %d %s", token, r.status, r.body)
			}
		}
		// The group's bound is the group's, and not each project's.
		if r := d.queue("alice-token"); r.status != http.StatusTooManyRequests || r.code(t) != "queue_full" {
			t.Fatalf("a third parse of a group that may hold 2, in a project that holds 1: %d %s", r.status, r.body)
		}
		return d.served(80, 100)
	}
	_, groups, projects := run(1, 3)
	if !near(groups["acme"], 40, 2) || !near(groups["solo"], 40, 2) {
		t.Errorf("the groups were served %v, want 40 each", groups)
	}
	if !near(projects["acme/reports"], 10, 2) || !near(projects["acme/invoices"], 30, 2) {
		t.Errorf("the projects were served %v, want 10 and 30", projects)
	}
	_, turned, around := run(3, 1)
	if turned["solo"] != groups["solo"] {
		t.Errorf("the other group was served %d, and %d with the projects' weights turned around", groups["solo"], turned["solo"])
	}
	if !near(around["acme/reports"], 30, 2) || !near(around["acme/invoices"], 10, 2) {
		t.Errorf("with the weights turned around the projects were served %v, want 30 and 10", around)
	}
}

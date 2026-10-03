// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// mode is one way the store reaches the database.
type mode struct {
	name    string
	serving func(t testing.TB, srv servers, direct string) string
}

// modes are the ways every store case is run: on a direct connection, in the
// query mode that shows how each parameter is bound, and behind PgBouncer in
// transaction mode. A statement that keeps anything on its connection, a
// named prepared statement, a session lock or a setting, fails in the last.
var modes = []mode{
	{"direct", func(_ testing.TB, _ servers, direct string) string { return direct }},
	{"exec", func(t testing.TB, _ servers, direct string) string { return execMode(t, direct) }},
	{"pooler", func(t testing.TB, srv servers, direct string) string {
		if srv.poolerErr != nil {
			// The server is up and the pooler is not: a skip here would let
			// the pooled run go missing without anyone seeing it.
			t.Fatalf("the pooler did not start: %v", srv.poolerErr)
		}
		return onDatabase(t, srv.pooler, nameOf(t, direct))
	}},
}

// everywhere runs a case once per mode, each on a database of its own.
func everywhere(t *testing.T, settings tasks.Settings, run func(t *testing.T, h *harness)) {
	t.Helper()
	srv := server(t)
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) { run(t, open(t, srv, m, settings)) })
	}
}

// epoch is where every case's clock starts. It is far from the wall clock on
// purpose: a statement that read now() where it should read the clock it was
// given shows as a lease or a deadline that is years off.
var epoch = time.Date(2031, 3, 1, 9, 0, 0, 0, time.UTC)

// harness is one store on one database, with a clock the case moves and a
// direct connection for what the store does not expose.
type harness struct {
	t     testing.TB
	store *postgres.Store
	admin *pgx.Conn
	now   time.Time
}

// stub is the reader most cases configure: one pool, wide enough that it
// never decides anything.
const stub = "stub"

// defaults are the settings of a case that does not care: the specs' own,
// with one reader.
func defaults() tasks.Settings {
	return tasks.Settings{
		Pools:     []tasks.Pool{{Reader: stub, MaxInFlight: 1000}},
		ReadChain: []string{stub}, ExtractChain: []string{stub},
	}
}

func open(t testing.TB, srv servers, m mode, settings tasks.Settings) *harness {
	t.Helper()
	ctx := context.Background()
	dsn := database(t, srv)
	if err := postgres.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	store, err := postgres.Open(ctx, m.serving(t, srv, dsn), postgres.Options{Settings: settings})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(store.Close)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	h := &harness{t: t, store: store, admin: admin, now: epoch}
	store.SetClock(h.now)
	// Whatever a case did, the counters the claim reads equal a recount of
	// the rows they count.
	t.Cleanup(h.consistent)
	return h
}

// advance moves the clock.
func (h *harness) advance(d time.Duration) {
	h.now = h.now.Add(d)
	h.store.SetClock(h.now)
}

// value reads one value over the direct connection.
func value[T any](h *harness, sql string, args ...any) T {
	h.t.Helper()
	var out T
	if err := h.admin.QueryRow(context.Background(), sql, args...).Scan(&out); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
	return out
}

// exec runs one statement over the direct connection.
func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.admin.Exec(context.Background(), sql, args...); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
}

// consistent fails the case when a counter of queued or running tasks, of a
// group or of a project, differs from a recount of the task rows.
func (h *harness) consistent() {
	h.t.Helper()
	if h.t.Failed() {
		return
	}
	const drift = `
SELECT count(*) FROM (
  SELECT s.group_id FROM group_service s
   WHERE s.queued  <> (SELECT count(*) FROM tasks t WHERE t.group_id = s.group_id AND t.class = s.class AND t.state = 'queued')
      OR s.running <> (SELECT count(*) FROM tasks t WHERE t.group_id = s.group_id AND t.class = s.class AND t.state = 'leased')
  UNION ALL
  SELECT s.group_id FROM project_service s
   WHERE s.queued  <> (SELECT count(*) FROM tasks t WHERE t.group_id = s.group_id AND t.project_id = s.project_id AND t.class = s.class AND t.state = 'queued')
      OR s.running <> (SELECT count(*) FROM tasks t WHERE t.group_id = s.group_id AND t.project_id = s.project_id AND t.class = s.class AND t.state = 'leased')
  UNION ALL
  SELECT t.group_id FROM tasks t
   WHERE t.state IN ('queued', 'leased')
     AND NOT EXISTS (SELECT 1 FROM project_service s
                      WHERE s.group_id = t.group_id AND s.project_id = t.project_id AND s.class = t.class)
) d`
	if n := value[int64](h, drift); n != 0 {
		h.t.Errorf("%d counters of queued and running tasks differ from a recount of the rows", n)
	}
}

// submit queues a parse. A zero member takes what a case seldom cares about:
// the group is the parse's owner, the class interactive, the deadline an hour.
func (h *harness) submit(sub postgres.Submission) {
	h.t.Helper()
	created, err := h.store.Submit(context.Background(), filled(sub))
	if err != nil || !created {
		h.t.Fatalf("submitting %s: created %t, %v", sub.Parse, created, err)
	}
}

func filled(sub postgres.Submission) postgres.Submission {
	if sub.Owner == "" {
		sub.Owner = sub.Group
	}
	if sub.Owner == "" {
		sub.Owner = "owner"
	}
	if sub.Deadline == 0 {
		sub.Deadline = time.Hour
	}
	return sub
}

// parse reads a parse.
func (h *harness) parse(id string) postgres.Parse {
	h.t.Helper()
	p, err := h.store.Parse(context.Background(), id)
	if err != nil {
		h.t.Fatalf("reading %s: %v", id, err)
	}
	return p
}

// tasks reads the task rows of a parse.
func (h *harness) tasks(id string) []postgres.Task {
	h.t.Helper()
	rows, err := h.store.Tasks(context.Background(), id)
	if err != nil {
		h.t.Fatalf("reading the tasks of %s: %v", id, err)
	}
	return rows
}

// task reads one task row.
func (h *harness) task(parse, task string) postgres.Task {
	h.t.Helper()
	for _, row := range h.tasks(parse) {
		if row.ID == task {
			return row
		}
	}
	h.t.Fatalf("%s has no task %s", parse, task)
	return postgres.Task{}
}

// worker is a worker process as the store sees one: an id, and the tasks it
// holds. It runs nothing; a case decides how each task ends.
type worker struct {
	h    *harness
	id   string
	held map[tasks.Ref]tasks.Claim
}

// worker registers a worker.
func (h *harness) worker() *worker {
	h.t.Helper()
	id, err := h.store.Register(context.Background())
	if err != nil {
		h.t.Fatalf("registering: %v", err)
	}
	return &worker{h: h, id: id, held: map[tasks.Ref]tasks.Claim{}}
}

// exchange makes one exchange: it settles, names what the worker still holds,
// and asks for up to free tasks. What the reply says the worker lost leaves
// its hands, and what it claims joins them.
func (w *worker) exchange(free int, settles ...tasks.Settle) tasks.Reply {
	w.h.t.Helper()
	reply := w.raw(tasks.Request{Free: free}, settles...)
	if reply.Gone {
		w.h.t.Fatalf("the fleet gave %s up", w.id)
	}
	return reply
}

// raw is exchange for a case that sets the request's own members, or that
// expects the worker to have been given up.
func (w *worker) raw(req tasks.Request, settles ...tasks.Settle) tasks.Reply {
	w.h.t.Helper()
	reply, err := w.send(req, settles...)
	if err != nil {
		w.h.t.Fatalf("the exchange of %s: %v", w.id, err)
	}
	return reply
}

// send makes the exchange and keeps the worker's hands in step with the
// reply.
func (w *worker) send(req tasks.Request, settles ...tasks.Settle) (tasks.Reply, error) {
	for _, s := range settles {
		delete(w.held, tasks.Ref{Parse: s.Parse, Task: s.Task})
	}
	req.Settles = settles
	req.Held = nil
	for _, ref := range slices.SortedFunc(maps.Keys(w.held), func(a, b tasks.Ref) int {
		return compare(a.Parse+"/"+a.Task, b.Parse+"/"+b.Task)
	}) {
		req.Held = append(req.Held, tasks.Held{Parse: ref.Parse, Task: ref.Task, Token: w.held[ref].Token})
	}
	req.Idle = len(w.held) == 0
	reply, err := w.h.store.Exchange(context.Background(), w.id, req)
	if err != nil {
		return reply, err
	}
	for _, ref := range reply.Lost {
		delete(w.held, ref)
	}
	for _, c := range reply.Claims {
		w.held[tasks.Ref{Parse: c.Parse, Task: c.Task}] = c
	}
	return reply, nil
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// claim asks for up to free tasks and fails the case when it gets another
// number than want.
func (w *worker) claim(free, want int) []tasks.Claim {
	w.h.t.Helper()
	reply := w.exchange(free)
	if len(reply.Claims) != want {
		w.h.t.Fatalf("%s asked for %d tasks and claimed %d, want %d: %s", w.id, free, len(reply.Claims), want, names(reply.Claims))
	}
	return reply.Claims
}

// settle sends settles and claims nothing, and fails the case when one is
// refused.
func (w *worker) settle(settles ...tasks.Settle) {
	w.h.t.Helper()
	if reply := w.exchange(0, settles...); len(reply.Refused) != 0 {
		w.h.t.Fatalf("settles of %s were refused: %v", w.id, reply.Refused)
	}
}

// names lists claims for a failure message.
func names(claims []tasks.Claim) string {
	out := make([]string, len(claims))
	for i, c := range claims {
		out[i] = c.Group + ":" + c.Parse + "/" + c.Task
	}
	return fmt.Sprint(out)
}

// done is a settle of a claim that succeeded. A page or an extraction used
// one unit unless a case says otherwise; prepare selected no page unless a
// case says which.
func done(c tasks.Claim) tasks.Settle {
	s := tasks.Settle{Parse: c.Parse, Task: c.Task, Token: c.Token, Outcome: tasks.Done, Units: 1}
	switch c.Kind {
	case tasks.Prepare:
		s.Prepare = &tasks.Prepared{Pages: []int{}}
	case tasks.Assemble:
		s.Output = "parses/" + c.Parse + "/index." + strconv.FormatInt(c.Token, 10) + ".json"
		s.Assemble = &tasks.Assembled{Index: s.Output}
	case tasks.Page:
		n, _ := tasks.PageOf(c.Task)
		s.Output = "parses/" + c.Parse + "/pages/" + strconv.Itoa(n) + "." + strconv.FormatInt(c.Token, 10) + ".json"
		s.Health = tasks.Healthy
	}
	return s
}

// prepared is a settle of a prepare task that selected the first n pages for
// a reader.
func prepared(c tasks.Claim, n int) tasks.Settle {
	s := done(c)
	s.Prepare = &tasks.Prepared{Manifest: []byte(`{"media_type":"application/pdf","source":"reader"}`), Pages: pages(n)}
	return s
}

func pages(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i + 1
	}
	return out
}

// ended is a settle of a claim with an outcome that is not success.
func ended(c tasks.Claim, outcome tasks.Outcome, code string) tasks.Settle {
	s := tasks.Settle{Parse: c.Parse, Task: c.Task, Token: c.Token, Outcome: outcome, Units: 1}
	if code != "" {
		s.Error = &tasks.Error{Code: code, Detail: "a test said so"}
	}
	return s
}

// reading queues a parse of n pages and runs its prepare, so its page tasks
// wait in the queue. It must be called while no other task can be claimed,
// which is the state every case starts in.
func (h *harness) reading(w *worker, sub postgres.Submission, n int) {
	h.t.Helper()
	h.submit(sub)
	c := w.claim(1, 1)[0]
	if c.Parse != sub.Parse || c.Kind != tasks.Prepare {
		h.t.Fatalf("claimed %s/%s where %s/prepare was the only task", c.Parse, c.Task, sub.Parse)
	}
	w.settle(prepared(c, n))
}

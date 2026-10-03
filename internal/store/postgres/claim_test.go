// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/lectio/internal/tasks"
)

// The claim inside an exchange reads the queue through tasks_runnable and
// stays under 5 ms at one million queued rows (specs/004-durable-tasks.md).
//
// The plans are asserted on every run, over a queue of 20,000 rows. The
// million rows and the time bound are opt-in, because loading them takes
// about a minute:
//
//	LECTIO_CLAIM_ROWS=1000000 go test -run TestTheClaimReadsTheQueueThroughItsIndex -v ./internal/store/postgres/

// claimRows names how many queued rows the case loads.
const claimRows = "LECTIO_CLAIM_ROWS"

// claimBound is the time an exchange that claims one task stays under.
const claimBound = 5 * time.Millisecond

// load fills the queue with rows page tasks, spread over groups of 1,000
// rows in parses of 100 pages, as one statement per table. It writes what
// the store's functions would have written, the counters included, without
// a million calls of them.
const load = `
WITH g AS (
  INSERT INTO groups (group_id, weight)
  SELECT 'grp_' || lpad(n::text, 5, '0'), 1 + n % 4 FROM generate_series(1, $1::integer / 1000) n
  RETURNING group_id
), p AS (
  INSERT INTO projects (group_id, project_id) SELECT group_id, '' FROM g
), gs AS (
  INSERT INTO group_service (group_id, class, queued) SELECT group_id, 0, 1000 FROM g
), ps AS (
  INSERT INTO project_service (group_id, project_id, class, queued) SELECT group_id, '', 0, 1000 FROM g
), ls AS (
  INSERT INTO lane_service (group_id, project_id, class, lane, queued) SELECT group_id, '', 0, 'page', 1000 FROM g
), parses AS (
  INSERT INTO parses (parse_id, owner, group_id, class, state, pages_total, pages_open, deadline_at, created_at, started_at)
  SELECT 'prs_' || g.group_id || '_' || k, g.group_id, g.group_id, 0, 'running', 100, 100, $2::timestamptz + interval '1000 hours', $2, $2
    FROM g, generate_series(1, 10) k
  RETURNING parse_id, group_id
)
INSERT INTO tasks (parse_id, task_id, kind, group_id, class, seq, state, available_at, created_at)
SELECT parses.parse_id, 'page-' || n, 'page', parses.group_id, 0, n, 'queued', $2, $2
  FROM parses, generate_series(1, 100) n`

// TestTheClaimReadsTheQueueThroughItsIndex: at one million queued rows the
// claim reads tasks through tasks_runnable and never by scanning the table,
// and an exchange that claims one task stays under 5 ms.
func TestTheClaimReadsTheQueueThroughItsIndex(t *testing.T) {
	rows, timed := 20000, false
	if v := os.Getenv(claimRows); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1000 {
			t.Fatalf("%s is not a number of rows of at least 1000", claimRows)
		}
		rows, timed = n, true
	}
	h := direct(t, defaults())
	h.exec(load, rows, h.now)
	h.exec(`ANALYZE`)
	if n := value[int64](h, `SELECT count(*) FROM tasks WHERE state = 'queued'`); n != int64(rows/1000*1000) {
		t.Fatalf("%d rows are queued, want %d", n, rows/1000*1000)
	}

	// A connection of its own, with the plan of every statement the
	// exchange runs sent back as a notice. The exchange is a function, so
	// EXPLAIN of the call shows nothing: the statements that matter are the
	// ones nested in it.
	var mu sync.Mutex
	var plans []string
	cfg, err := pgx.ParseConfig(h.admin.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		defer mu.Unlock()
		plans = append(plans, n.Message)
	}
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	worker := h.worker()

	// exchange settles the claim before and claims one, on this connection.
	var prev *tasks.Claim
	exchange := func() time.Duration {
		t.Helper()
		req := tasks.Request{Free: 1, Idle: true}
		if prev != nil {
			req.Settles = []tasks.Settle{done(*prev)}
		}
		doc, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		var answer string
		start := time.Now()
		err = conn.QueryRow(context.Background(), `SELECT lectio_exchange($1, $2, $3)`, worker.id, string(doc), h.now).Scan(&answer)
		took := time.Since(start)
		if err != nil {
			t.Fatalf("the exchange: %v", err)
		}
		var reply tasks.Reply
		if err := json.Unmarshal([]byte(answer), &reply); err != nil {
			t.Fatal(err)
		}
		if len(reply.Claims) != 1 || len(reply.Refused) != 0 {
			t.Fatalf("the exchange answered %+v", reply)
		}
		prev = &reply.Claims[0]
		return took
	}

	for _, statement := range []string{
		`LOAD 'auto_explain'`,
		`SET auto_explain.log_min_duration = 0`,
		`SET auto_explain.log_nested_statements = on`,
		`SET auto_explain.log_level = 'notice'`,
	} {
		if _, err := conn.Exec(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// The first calls of a session plan each statement for its parameters,
	// and the later ones may run a plan made for any: both are read.
	for range 12 {
		exchange()
	}
	if _, err := conn.Exec(context.Background(), `SET auto_explain.log_min_duration = -1`); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	probes := 0
	for _, plan := range plans {
		if strings.Contains(plan, "Seq Scan on tasks") {
			t.Errorf("a statement of the exchange scans the task table:\n%s", plan)
		}
		// The probe of a lane is the one statement that reads queued tasks
		// in the order of a project's queue. It reads the first row of the
		// index for the group, the project, the class and the lane, in the
		// index's own order: the one sort in its plan is over the lanes'
		// first tasks, above the join, and none is under it.
		if strings.Contains(plan, "q.available_at <=") {
			probes++
			_, scan, found := strings.Cut(plan, "Nested Loop")
			if !found || strings.Count(plan, "Sort Key") != 1 || strings.Contains(scan, "Sort") ||
				!strings.Contains(scan, "Limit") || !strings.Contains(scan, "Index Scan using tasks_runnable on tasks q") ||
				!strings.Contains(scan, "Index Cond: ((group_id = ") || !strings.Contains(scan, "(lane = l.lane))") {
				t.Errorf("the claim does not read the queue in the order of tasks_runnable:\n%s", plan)
			}
		}
	}
	mu.Unlock()
	if probes != 12 {
		t.Fatalf("%d of 12 exchanges showed the plan of the claim's probe", probes)
	}

	// The time of an exchange that settles one task and claims one, as its
	// caller sees it: the statement and its round trip.
	took := make([]time.Duration, 500)
	for i := range took {
		took[i] = exchange()
	}
	slices.Sort(took)
	median, p99, worst := took[len(took)/2], took[len(took)*99/100], took[len(took)-1]
	t.Logf("%d queued rows in %d groups: an exchange that settles one task and claims one takes %v at the median, %v at the 99th percentile, %v at worst",
		rows, rows/1000, median, p99, worst)
	if timed && (median > claimBound || p99 > claimBound) {
		t.Errorf("an exchange that claims one task takes %v at the median and %v at the 99th percentile, want under %v", median, p99, claimBound)
	}

	// The other steady state: the reader's pool is full, so no page of any
	// group can run and a poll claims nothing. It must find that out without
	// reading the queue row by row.
	h.exec(`UPDATE pools SET max_in_flight = 1`)
	poll, err := json.Marshal(tasks.Request{Free: 1, Held: []tasks.Held{{Parse: prev.Parse, Task: prev.Task, Token: prev.Token}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range took {
		var answer string
		start := time.Now()
		if err := conn.QueryRow(context.Background(), `SELECT lectio_exchange($1, $2, $3)`, worker.id, string(poll), h.now).Scan(&answer); err != nil {
			t.Fatalf("the poll: %v", err)
		}
		took[i] = time.Since(start)
		if !strings.Contains(answer, `"claims": []`) || !strings.Contains(answer, `"lost": []`) {
			t.Fatalf("a poll against a full pool answered %s", answer)
		}
	}
	slices.Sort(took)
	median, p99, worst = took[len(took)/2], took[len(took)*99/100], took[len(took)-1]
	t.Logf("%d queued rows in %d groups, the pool full: a poll that claims nothing takes %v at the median, %v at the 99th percentile, %v at worst",
		rows, rows/1000, median, p99, worst)
	if timed && (median > claimBound || p99 > claimBound) {
		t.Errorf("a poll against a full pool takes %v at the median and %v at the 99th percentile, want under %v", median, p99, claimBound)
	}
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Work on a parse that has ended, with processes
// (specs/003-api.md, specs/011-structured-extraction.md): an extraction
// and a run that describes figures are tasks of the task store, so a
// worker that is killed in the middle of either loses its lease and not
// the work.
//
// These cases, and the ones of the keys beside them, run one after the
// other and before the cases that run in parallel: several of those time
// what a worker does between 2 of its exchanges, and every process more
// that runs beside them stretches that time.

// made counts the calls of one kind a calls file holds, in all and by the
// process that made them.
func made(t *testing.T, path, kind string) (all int, by map[string]int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	by = map[string]int{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if pid, what, _ := strings.Cut(line, " "); what == kind {
			by[pid]++
			all++
		}
	}
	return all, by
}

// recount fails the case unless the meter of a kind of task holds the
// calls the rows of that kind recorded, and the counters of queued and
// running tasks equal a recount of the task rows.
func recount(t *testing.T, p *plane, kind, recorded string) {
	t.Helper()
	conn := p.connect()
	metered := value[int](t, conn, `SELECT coalesce(sum(calls), 0) FROM usage WHERE kind = $1`, kind)
	if rows := value[int](t, conn, recorded); metered != rows {
		t.Fatalf("the meter holds %d calls of the kind %s and the rows recorded %d", metered, kind, rows)
	}
	// The meter of every kind equals what the settles recorded on the
	// parses.
	parses := value[string](t, conn, `SELECT sum(pages_done) || '/' || sum(calls) || '/' || sum(input_tokens) || '/' || sum(output_tokens) FROM parses`)
	meters := value[string](t, conn, `SELECT sum(pages) || '/' || sum(calls) || '/' || sum(input_tokens) || '/' || sum(output_tokens) FROM usage`)
	if parses != meters {
		t.Fatalf("the meters read %s and the parses recorded %s", meters, parses)
	}
	if n := value[int](t, conn, drift); n != 0 {
		t.Fatalf("%d counters differ from a recount of the rows", n)
	}
	if n := value[int](t, conn, `SELECT count(*) FROM tasks WHERE kind = $1`, kind); n != 0 {
		t.Fatalf("%d task rows of the kind %s are left", n, kind)
	}
}

// TestAnExtractionOutlivesAKilledWorker: an extraction over 6 windows is 6
// calls, each claimed by itself. The worker that runs it is killed with
// calls made and one in flight. Another worker returns the task after one
// lease, with one expiry and no attempt counted, is handed what the
// extraction had so far, and makes the calls that were not made and no
// other: the model is called once per window, plus at most the call in
// flight at the kill. The field is filled once, and the meter equals a
// recount of what the settles recorded.
func TestAnExtractionOutlivesAKilledWorker(t *testing.T) {
	p := newPlane(t)
	calls := filepath.Join(t.TempDir(), "calls")
	api := p.spawn("api")
	// A page of the stub reader is 2 blocks of text, and neither fits a
	// window with the other.
	doomed := p.spawn("worker", delayEnv, "400ms", callsEnv, calls, windowEnv, "64")
	conn := p.connect()

	id := submitted(t, api.base, "scan.tiff", frames(3))
	if done := ended(t, api.base, id); done["state"] != "succeeded" {
		t.Fatalf("the parse ended %v", done)
	}
	on := api.base + "/v1/parses/" + id
	asked := []byte(`{"name":"title","schema":{"type":"object","required":["title"],"properties":{"title":{"type":"string"}}}}`)
	if status, _, raw := call(t, "POST", on+"/fields", token(), asked); status != http.StatusAccepted {
		t.Fatalf("asking the extraction: %d %s", status, raw)
	}
	// The worker has settled 2 calls and holds the task for a third.
	settled := `SELECT coalesce(max(calls), -1) FROM tasks WHERE kind = 'extract' AND state = 'leased' AND lease_owner = $1`
	until(t, "the worker is in the middle of the extraction", time.Minute, func() bool { return value[int](t, conn, settled, doomed.worker()) >= 2 })
	doomed.signal(syscall.SIGKILL)
	<-doomed.exited
	before := value[int](t, conn, settled, doomed.worker())
	if before < 2 || before > 5 {
		t.Fatalf("the worker had settled %d calls of 6 when it was killed", before)
	}

	second := p.spawn("worker", delayEnv, "20ms", callsEnv, calls, windowEnv, "64")
	until(t, "the dead worker's extraction is returned", 30*time.Second, func() bool { return value[int](t, conn, settled, doomed.worker()) == -1 })
	if row := value[string](t, conn, `SELECT coalesce((SELECT expiries || ' ' || attempt FROM tasks WHERE kind = 'extract'), 'ended')`); row != "1 0" && row != "ended" {
		t.Fatalf("the task came back as %q, want one expiry and no attempt", row)
	}
	field := fieldOf(t, on, token(), "title")
	if field["state"] != "succeeded" || field["windows"] != 6.0 || field["attempts"] != 1.0 ||
		!strings.HasPrefix(field["data"].(map[string]any)["title"].(string), "Page 1 read by ") {
		t.Fatalf("after the kill the extraction is %v\n%s", field, second.logs.String())
	}
	// 6 windows, and at most the one call that was in flight at the kill.
	all, by := made(t, calls, "extract")
	dead, alive := by[strconv.Itoa(doomed.cmd.Process.Pid)], by[strconv.Itoa(second.cmd.Process.Pid)]
	if all < 6 || all > 7 || alive != 6-before || dead < before || dead > before+1 {
		t.Fatalf("the model was called %d times for 6 windows: %d by the worker that was killed after it settled %d, and %d by the other", all, dead, before, alive)
	}
	if n := value[string](t, conn, `SELECT state || ' ' || calls FROM fields WHERE parse_id = $1`, id); n != "succeeded 6" {
		t.Fatalf("the field's row is %q", n)
	}
	recount(t, p, "extract", `SELECT coalesce(sum(calls), 0) FROM fields`)
}

// TestAFigureRunOutlivesAKilledWorker: a run that describes 3 figures is 3
// tasks. The worker that runs them, one at a time, is killed with figures
// described and one in flight. Another worker returns the task after one
// lease and describes what was not described: every figure has one
// description, the describer was called once per figure plus at most the
// call in flight at the kill, the run counts each figure once, and the
// meter equals a recount.
func TestAFigureRunOutlivesAKilledWorker(t *testing.T) {
	p := newPlane(t)
	calls := filepath.Join(t.TempDir(), "calls")
	api := p.spawn("api")
	doomed := p.spawn("worker", delayEnv, "400ms", callsEnv, calls, figuresEnv, "1", "LECTIO_WORKERS", "1")
	conn := p.connect()

	id := submitted(t, api.base, "scan.tiff", frames(3))
	if done := ended(t, api.base, id); done["state"] != "succeeded" {
		t.Fatalf("the parse ended %v", done)
	}
	on := api.base + "/v1/parses/" + id
	status, started, raw := call(t, "POST", on+"/figures", token(), nil)
	if status != http.StatusAccepted || started["run"].(map[string]any)["total"] != 3.0 || len(started["figures"].([]any)) != 3 {
		t.Fatalf("starting the run: %d %s", status, raw)
	}
	held := `SELECT count(*) FROM tasks WHERE kind = 'figure' AND state = 'leased' AND lease_owner = $1`
	until(t, "the worker has described a figure and holds the next", time.Minute, func() bool {
		return value[int](t, conn, `SELECT done FROM figure_runs WHERE parse_id = $1`, id) >= 1 && value[int](t, conn, held, doomed.worker()) == 1
	})
	doomed.signal(syscall.SIGKILL)
	<-doomed.exited
	before := value[int](t, conn, `SELECT done FROM figure_runs WHERE parse_id = $1`, id)

	second := p.spawn("worker", delayEnv, "20ms", callsEnv, calls, figuresEnv, "1")
	var listing map[string]any
	until(t, "the run ends on the other worker", 2*time.Minute, func() bool {
		_, listing, _ = call(t, "GET", on+"/figures", token(), nil)
		return listing["run"].(map[string]any)["state"] != "running"
	})
	run := listing["run"].(map[string]any)
	if run["state"] != "succeeded" || run["done"] != 3.0 || run["failed"] != 0.0 || run["finished_at"] == nil {
		t.Fatalf("after the kill the run is %v\n%s", run, second.logs.String())
	}
	for _, f := range listing["figures"].([]any) {
		fig := f.(map[string]any)
		if desc, _ := fig["description"].(string); !strings.HasPrefix(desc, "A figure of 20 by 20 pixels") || fig["error"] != nil {
			t.Fatalf("a figure of the run: %v", fig)
		}
	}
	if status, _, md := call(t, "GET", on+"/document?format=markdown", token(), nil); status != http.StatusOK || strings.Count(string(md), "*[Figure: A figure of 20 by 20 pixels") != 3 {
		t.Fatalf("the document after the run: %d %s", status, md)
	}
	all, by := made(t, calls, "figure")
	if dead := by[strconv.Itoa(doomed.cmd.Process.Pid)]; all < 3 || all > 4 || dead < before || dead > before+1 || by[strconv.Itoa(second.cmd.Process.Pid)] != 3-before {
		t.Fatalf("the describer was called %d times for 3 figures, %d of them by the worker that was killed after it described %d", all, dead, before)
	}
	if objects := len(p.objects.Keys()); objects == 0 {
		t.Fatal("the bucket holds nothing")
	}
	recount(t, p, "figure", `SELECT coalesce(sum(calls), 0) FROM figure_runs`)
	if described := value[int](t, conn, `SELECT described FROM parses WHERE parse_id = $1`, id); described != 3 {
		t.Fatalf("the parse counts %d described figures", described)
	}
}

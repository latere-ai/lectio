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

	"latere.ai/x/lectio/internal/testfixtures"
)

// TestAParseCompletesOnASeparateWorkerProcess: a parse submitted to an API
// process completes on a worker process, for a PDF of several pages and for
// an image, each read by the stub reader, and the result is read back
// through the API process, which ran none of it.
func TestAParseCompletesOnASeparateWorkerProcess(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	api, worker := p.spawn("api"), p.spawn("worker")

	pdf := ended(t, api.base, submitted(t, api.base, "report.pdf", testfixtures.Read(t, testfixtures.MultipagePDF)))
	if pdf["state"] != "succeeded" || pdf["progress"].(map[string]any)["pages_done"] != 3.0 {
		t.Fatalf("the parse of a PDF ended %v", pdf)
	}
	at := api.base + "/v1/parses/" + pdf["id"].(string)
	status, page, raw := call(t, "GET", at+"/pages/3", "dev", nil)
	if status != http.StatusOK || page["reader"] != "stub" || len(page["blocks"].([]any)) != 3 ||
		!strings.Contains(string(raw), "read by "+strconv.Itoa(worker.cmd.Process.Pid)) {
		t.Fatalf("its third page: %d %s", status, raw)
	}
	if status, _, md := call(t, "GET", at+"/document?format=markdown", "dev", nil); status != http.StatusOK || strings.Count(string(md), "# Page ") != 3 {
		t.Fatalf("its document: %d %q", status, md)
	}

	scan := ended(t, api.base, submitted(t, api.base, "scan.png", striped(t)))
	if scan["state"] != "succeeded" || scan["usage"].(map[string]any)["pages"] != 1.0 {
		t.Fatalf("the parse of an image ended %v", scan)
	}
	if status, _, img := call(t, "GET", api.base+"/v1/parses/"+scan["id"].(string)+"/pages/1/image", "dev", nil); status != http.StatusOK || !strings.HasPrefix(string(img), "\x89PNG") {
		t.Fatalf("the image of its page: %d, %d bytes", status, len(img))
	}
	if _, registered := api.logged("the worker is registered"); registered {
		t.Fatal("the API process registered a worker")
	}
}

// TestAKilledWorkerLosesItsLeaseAndNotTheWork: a worker killed with SIGKILL
// while it runs tasks keeps them for as long as its lease lasts and no
// longer. A live worker then returns them to the queue with one expiry
// counted and no attempt spent, and completes them. Every page succeeds
// once, and the reader is called once per page plus at most once more for
// each call that was in flight at the kill.
func TestAKilledWorkerLosesItsLeaseAndNotTheWork(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	calls := filepath.Join(t.TempDir(), "calls")
	api := p.spawn("api")
	doomed := p.spawn("worker", delayEnv, "700ms", callsEnv, calls)
	conn := p.connect()

	const parses, pages = 4, 3
	ids := make([]string, parses)
	// A TIFF of 3 frames: a process renders it with no engine to load.
	scan := frames(pages)
	for i := range ids {
		ids[i] = submitted(t, api.base, "scan.tiff", scan)
	}
	held := `SELECT count(*) FROM tasks WHERE state = 'leased' AND kind = 'page' AND lease_owner = $1`
	until(t, "the worker runs pages", 20*time.Second, func() bool { return value[int](t, conn, held, doomed.worker()) >= 2 })
	doomed.signal(syscall.SIGKILL)
	killed := time.Now()
	<-doomed.exited
	// What the dead worker held is still leased to it: nobody has acted on
	// its lease yet. Among its tasks are pages, whose calls were in flight,
	// and there may be a prepare or an assemble.
	inFlight := value[int](t, conn, held, doomed.worker())
	holding := value[int](t, conn, `SELECT count(*) FROM tasks WHERE state = 'leased' AND lease_owner = $1`, doomed.worker())
	if inFlight < 1 {
		t.Fatalf("the worker held %d pages when it was killed", inFlight)
	}

	// Another worker is there, and for the length of the lease it leaves
	// the dead one's tasks alone.
	p.spawn("worker", delayEnv, "20ms", callsEnv, calls)
	until(t, "the dead worker's tasks are returned", 20*time.Second, func() bool { return value[int](t, conn, held, doomed.worker()) == 0 })
	lost := time.Since(killed)
	// The lease is 2s, counted from the dead worker's last exchange, which
	// was at most a quarter of a lease before the kill.
	if lost < time.Second || lost > 8*time.Second {
		t.Fatalf("the dead worker's tasks were returned %v after the kill, want about one lease of 2s", lost)
	}
	returned := value[int](t, conn, `SELECT count(*) FROM tasks WHERE expiries = 1`)
	if spent := value[int](t, conn, `SELECT count(*) FROM tasks WHERE expiries > 0 AND (attempt <> 0 OR expiries <> 1)`); returned != holding || spent != 0 {
		t.Fatalf("%d tasks were returned with one expiry, want the %d the worker held at the kill, and %d of them spent an attempt", returned, holding, spent)
	}
	if n := value[int](t, conn, `SELECT count(*) FROM workers WHERE worker_id = $1`, doomed.worker()); n != 0 {
		t.Fatal("the dead worker is still registered")
	}

	for _, id := range ids {
		done := ended(t, api.base, id)
		if progress := done["progress"].(map[string]any); done["state"] != "succeeded" || progress["pages_done"] != float64(pages) || progress["pages_failed"] != 0.0 {
			t.Fatalf("after the kill a parse ended %v", done)
		}
		for n := 1; n <= pages; n++ {
			if status, page, raw := call(t, "GET", api.base+"/v1/parses/"+id+"/pages/"+strconv.Itoa(n), "dev", nil); status != http.StatusOK || page["state"] != "succeeded" {
				t.Fatalf("page %d of a parse that survived the kill: %d %s", n, status, raw)
			}
		}
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if made := strings.Count(string(raw), "\n"); made < parses*pages || made > parses*pages+inFlight {
		t.Fatalf("the reader was called %d times for %d pages with %d calls in flight at the kill", made, parses*pages, inFlight)
	}
	if n := value[int](t, conn, drift); n != 0 {
		t.Fatalf("%d counters differ from a recount of the rows", n)
	}
}

// TestASuspendedWorkerCannotSettleAndRegistersAgain: a worker that is
// suspended past its lease, with a page read and not yet settled, is taken
// for dead: another worker reads the page again and settles it. When the
// first worker resumes, its settle is refused and it works again only under
// a new id. Both workers wrote the page, under two keys; the one the parse
// serves is the one whose settle was accepted.
func TestASuspendedWorkerCannotSettleAndRegistersAgain(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	api := p.spawn("api")
	// The first worker exchanges once in 3 seconds, so a page it has read
	// waits for its settle long enough to suspend the process in between.
	paused := p.spawn("worker", "LECTIO_WORKER_FLUSH", "3s", "LECTIO_WORKERS", "1")
	first := paused.worker()
	id := submitted(t, api.base, "scan.png", striped(t))

	stale := "parses/" + id + "/pages/1.1.json"
	until(t, "the first worker wrote the page", 30*time.Second, func() bool {
		_, wrote := p.objects.Get(stale)
		return wrote
	})
	paused.signal(syscall.SIGSTOP)
	live := p.spawn("worker")

	done := ended(t, api.base, id)
	if done["state"] != "succeeded" {
		t.Fatalf("with its first worker suspended the parse ended %v", done)
	}
	winner := "parses/" + id + "/pages/1.2.json"
	if _, wrote := p.objects.Get(winner); !wrote {
		t.Fatalf("the second worker's result is not under %s: %v", winner, p.objects.Keys())
	}
	_, _, raw := call(t, "GET", api.base+"/v1/parses/"+id+"/pages/1", "dev", nil)
	if !strings.Contains(string(raw), "read by "+strconv.Itoa(live.cmd.Process.Pid)) || strings.Contains(string(raw), "read by "+strconv.Itoa(paused.cmd.Process.Pid)) {
		t.Fatalf("the page the parse serves is not the one whose settle was accepted: %s", raw)
	}
	index, ok := p.objects.Get("parses/" + id + "/document.1.json")
	if !ok || strings.Contains(string(index), stale) || (!strings.Contains(string(index), winner) && !strings.Contains(string(index), "/pages/1.a1.json")) {
		t.Fatalf("the document index does not list the result that won: %s", index)
	}

	paused.signal(syscall.SIGCONT)
	until(t, "the resumed worker registered again", 30*time.Second, func() bool { return paused.worker() != first })
	if _, gone := paused.logged("the fleet gave the worker up: its lease ran out"); !gone {
		t.Fatalf("the resumed worker was not told it was given up:\n%s", paused.logs.String())
	}
	// Its late settle recorded nothing: the parse is as the second worker
	// left it, and the stale object is still where nothing points.
	conn := p.connect()
	if got := value[string](t, conn, `SELECT state || ' ' || pages_done || ' ' || calls FROM parses WHERE parse_id = $1`, id); got != "succeeded 1 1" {
		t.Fatalf("after the late settle the parse is %q", got)
	}
	if _, kept := p.objects.Get(stale); !kept {
		t.Fatal("the stale worker's object is gone")
	}
}

// TestAPageThatReturnsAfterACancelRecordsNothing: a parse canceled while a
// page is being read is canceled when the cancel returns. The page's call
// returns afterwards and its settle is refused: no output, no progress and
// no assemble task are recorded.
func TestAPageThatReturnsAfterACancelRecordsNothing(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	gate := t.TempDir()
	hold := filepath.Join(gate, "hold")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	api := p.spawn("api")
	// The worker exchanges once a second, so the call returns between two
	// exchanges and its settle is sent before the worker hears of the
	// cancel.
	worker := p.spawn("worker", gateEnv, gate, "LECTIO_WORKER_FLUSH", "1s", "LECTIO_WORKERS", "1")
	id := submitted(t, api.base, "scan.png", striped(t))
	until(t, "the page is being read", 30*time.Second, func() bool { return exists(filepath.Join(gate, "reached")) })

	status, canceled, raw := call(t, "POST", api.base+"/v1/parses/"+id+"/cancel", "dev", nil)
	if status != http.StatusOK || canceled["state"] != "canceled" {
		t.Fatalf("the cancel answered %d %s", status, raw)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	until(t, "the worker's settle was refused", 30*time.Second, func() bool {
		_, refused := worker.logged("a settle was refused")
		return refused
	})

	conn := p.connect()
	row := value[string](t, conn, `SELECT state || ' output:' || coalesce(output, '') || ' result:' || coalesce(result::text, '') FROM tasks WHERE parse_id = $1 AND task_id = 'page-1'`, id)
	if row != "canceled output: result:" {
		t.Fatalf("after the late settle the page's task is %q", row)
	}
	parse := value[string](t, conn, `SELECT state || ' ' || pages_done || ' ' || pages_open || ' ' || calls FROM parses WHERE parse_id = $1`, id)
	if parse != "canceled 0 1 0" {
		t.Fatalf("after the late settle the parse is %q", parse)
	}
	if n := value[int](t, conn, `SELECT count(*) FROM tasks WHERE parse_id = $1 AND task_id = 'assemble'`, id); n != 0 {
		t.Fatal("an assemble task was written after the cancel")
	}
	// The call did return and did write: its object is where nothing
	// points, and the page is served as one the parse did not read.
	if _, wrote := p.objects.Get("parses/" + id + "/pages/1.1.json"); !wrote {
		t.Fatalf("the late call wrote no object: %v", p.objects.Keys())
	}
	if status, page, raw := call(t, "GET", api.base+"/v1/parses/"+id+"/pages/1", "dev", nil); status != http.StatusOK || page["state"] != "skipped" {
		t.Fatalf("the page of the canceled parse: %d %s", status, raw)
	}
}

// TestATerminatedWorkerReturnsItsTasks: on SIGTERM a worker gives the tasks
// it runs back within the grace period, with no attempt and no expiry
// counted, removes its registration, and exits clean. Another worker takes
// them at once.
func TestATerminatedWorkerReturnsItsTasks(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	api := p.spawn("api")
	worker := p.spawn("worker", delayEnv, "1m", "LECTIO_SHUTDOWN_GRACE", "500ms")
	id := submitted(t, api.base, "scan.tiff", frames(3))
	conn := p.connect()
	leased := `SELECT count(*) FROM tasks WHERE parse_id = $1 AND kind = 'page' AND state = 'leased'`
	until(t, "the worker runs the 3 pages", 30*time.Second, func() bool { return value[int](t, conn, leased, id) == 3 })

	worker.signal(syscall.SIGTERM)
	asked := time.Now()
	select {
	case <-worker.exited:
	case <-time.After(20 * time.Second):
		t.Fatalf("the worker did not exit:\n%s", worker.logs.String())
	}
	if took := time.Since(asked); worker.err != nil || took > 4*time.Second {
		t.Fatalf("the worker exited with %v, %v after the signal, with a grace period of 500ms", worker.err, took)
	}
	rows := value[string](t, conn, `SELECT string_agg(state || '/' || attempt || '/' || expiries || '/' || coalesce(lease_owner, '-') || '/' || charged, ' ' ORDER BY seq) FROM tasks WHERE parse_id = $1 AND kind = 'page'`, id)
	if rows != "queued/0/0/-/0 queued/0/0/-/0 queued/0/0/-/0" {
		t.Fatalf("after the worker stopped its tasks are %q", rows)
	}
	if n := value[int](t, conn, `SELECT (SELECT count(*) FROM workers) + (`+drift+`)`); n != 0 {
		t.Fatalf("%d workers or counters are left after the stop", n)
	}
	if got := value[string](t, conn, `SELECT state || ' ' || pages_done || ' ' || pages_open FROM parses WHERE parse_id = $1`, id); got != "running 0 3" {
		t.Fatalf("after the worker stopped the parse is %q", got)
	}

	p.spawn("worker")
	if done := ended(t, api.base, id); done["state"] != "succeeded" {
		t.Fatalf("the parse another worker took over ended %v", done)
	}
	if n := value[int](t, conn, `SELECT count(*) FROM tasks WHERE parse_id = $1`, id); n != 0 {
		t.Fatalf("%d task rows are left of a parse that succeeded", n)
	}
}

// TestRestartingTheAPIChangesNothing: the API holds no parse in memory. A
// parse submitted to an API process that is then killed runs on, and a new
// API process serves its state, its result, and the submit repeated under
// the same idempotency key.
func TestRestartingTheAPIChangesNothing(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	first := p.spawn("api")
	p.spawn("worker", delayEnv, "400ms", "LECTIO_WORKERS", "1")

	status, file, raw := call(t, "POST", first.base+"/v1/files?name=scan.tiff", "dev", frames(3), "Content-Type", "image/tiff")
	if status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, raw)
	}
	body := []byte(`{"source":{"file":"` + file["id"].(string) + `"},"labels":{"batch":"oct"}}`)
	status, parse, raw := call(t, "POST", first.base+"/v1/parses", "dev", body, "Idempotency-Key", "k1")
	if status != http.StatusAccepted {
		t.Fatalf("submit: %d %s", status, raw)
	}
	id := parse["id"].(string)
	first.signal(syscall.SIGKILL)
	<-first.exited

	second := p.spawn("api")
	status, again, raw := call(t, "POST", second.base+"/v1/parses", "dev", body, "Idempotency-Key", "k1")
	if (status != http.StatusAccepted && status != http.StatusOK) || again["id"] != id {
		t.Fatalf("the submit repeated at the new process: %d %s", status, raw)
	}
	done := ended(t, second.base, id)
	if done["state"] != "succeeded" || done["labels"].(map[string]any)["batch"] != "oct" || done["progress"].(map[string]any)["pages_done"] != 3.0 {
		t.Fatalf("at the new process the parse ended %v", done)
	}
	if status, doc, raw := call(t, "GET", second.base+"/v1/parses/"+id+"/document", "dev", nil); status != http.StatusOK || len(doc["pages"].([]any)) != 3 {
		t.Fatalf("its document at the new process: %d %s", status, raw)
	}
	if status, listed, raw := call(t, "GET", second.base+"/v1/parses?file="+file["id"].(string), "dev", nil); status != http.StatusOK || len(listed["parses"].([]any)) != 1 {
		t.Fatalf("the parses of the file at the new process: %d %s", status, raw)
	}
}

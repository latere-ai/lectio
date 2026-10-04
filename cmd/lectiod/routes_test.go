// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The retry, the event stream, the meters and the view of the queue through
// the durable server, run as processes over one Postgres and one bucket
// (specs/003-api.md, specs/004-durable-tasks.md,
// specs/006-fairness-and-priority.md, specs/013-limits-and-usage.md).

// sent is one event of a parse's stream.
type sent struct {
	id   int64
	name string
	data map[string]any
}

// following is an open event stream.
type following struct {
	t    *testing.T
	resp *http.Response
	rd   *bufio.Reader
}

// follow opens the event stream of a parse at a server, resuming after
// last when it is not empty. The stream is read for at most 5 minutes.
func follow(t *testing.T, base, id, last string) *following {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/v1/parses/"+id+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token())
	if last != "" {
		req.Header.Set("Last-Event-ID", last)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("the stream answered %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return &following{t: t, resp: resp, rd: bufio.NewReader(resp.Body)}
}

// next reads the next event. ok is false when the stream has ended, by its
// server's choice or by its server's death.
func (f *following) next() (ev sent, ok bool) {
	f.t.Helper()
	for {
		line, err := f.rd.ReadString('\n')
		if err != nil {
			return sent{}, false
		}
		field, value, _ := strings.Cut(strings.TrimSuffix(line, "\n"), ": ")
		switch field {
		case "id":
			if ev.id, err = strconv.ParseInt(value, 10, 64); err != nil {
				f.t.Fatalf("an event's id is %q", value)
			}
		case "event":
			ev.name = value
		case "data":
			if err := json.Unmarshal([]byte(value), &ev.data); err != nil {
				f.t.Fatalf("the data of event %d is %q", ev.id, value)
			}
		case "":
			if ev.name != "" {
				return ev, true
			}
		}
	}
}

// until reads events up to and including the first that satisfies stop.
func (f *following) until(stop func(sent) bool) []sent {
	f.t.Helper()
	var out []sent
	for {
		ev, ok := f.next()
		if !ok {
			f.t.Fatalf("the stream ended after %v", out)
		}
		if out = append(out, ev); stop(ev) {
			return out
		}
	}
}

// rest reads the stream to its end.
func (f *following) rest() []sent {
	var out []sent
	for {
		ev, ok := f.next()
		if !ok {
			return out
		}
		out = append(out, ev)
	}
}

// pagesIn maps the page of each page event to the event's id.
func pagesIn(t *testing.T, events []sent) map[int]int64 {
	t.Helper()
	out := map[int]int64{}
	for _, ev := range events {
		if ev.name != "page" {
			continue
		}
		n := int(ev.data["page"].(float64))
		if _, twice := out[n]; twice {
			t.Fatalf("page %d was sent twice: %v", n, events)
		}
		out[n] = ev.id
	}
	return out
}

// TestARetryOutlivesAKilledWorker: a parse whose pages 2 and 3 its reader
// could not take ends failed with page 1 read. A retry queues the 2 pages
// again. The worker that takes them is killed with SIGKILL while it reads
// them, and another returns them to the queue after a lease, reads them,
// and assembles the parse again: it ends succeeded. Page 1 was read once,
// before the retry, and never again.
func TestARetryOutlivesAKilledWorker(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	dir := t.TempDir()
	calls, broken := filepath.Join(dir, "calls"), filepath.Join(dir, "broken")
	if err := os.WriteFile(broken, []byte("2 3"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := p.spawn("api")
	// A call of the first worker takes 3 seconds, so the kill lands while
	// the retried pages are being read.
	first := p.spawn("worker", failEnv, broken, callsEnv, calls, delayEnv, "3s")
	conn := p.connect()

	id := submitted(t, api.base, "scan.tiff", frames(3))
	failed := ended(t, api.base, id)
	if progress := failed["progress"].(map[string]any); failed["state"] != "failed" || code(failed) != "page_unreadable" ||
		progress["pages_done"] != 1.0 || progress["pages_failed"] != 2.0 {
		t.Fatalf("the parse ended %v", failed)
	}
	at := api.base + "/v1/parses/" + id
	if status, page, raw := call(t, "GET", at+"/pages/1", token(), nil); status != http.StatusOK || page["state"] != "succeeded" {
		t.Fatalf("the page that was read: %d %s", status, raw)
	}

	// The reader is mended and the parse retried.
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	status, retried, raw := call(t, "POST", at+"/retry", token(), nil)
	if status != http.StatusAccepted || retried["state"] != "running" || retried["progress"].(map[string]any)["pages_failed"] != 0.0 {
		t.Fatalf("the retry answered %d %s", status, raw)
	}
	held := `SELECT count(*) FROM tasks WHERE parse_id = $1 AND kind = 'page' AND state = 'leased' AND lease_owner = $2`
	until(t, "the worker reads the 2 pages again", time.Minute, func() bool { return value[int](t, conn, held, id, first.worker()) == 2 })
	first.signal(syscall.SIGKILL)
	<-first.exited
	if got := value[string](t, conn, `SELECT state || ' ' || pages_open || ' ' || pages_done FROM parses WHERE parse_id = $1`, id); got != "running 2 1" {
		t.Fatalf("after the kill the parse is %q", got)
	}

	// Another worker finds the lease run out, takes the pages and ends the
	// parse.
	second := p.spawn("worker", callsEnv, calls, delayEnv, "20ms")
	done := ended(t, api.base, id)
	if progress := done["progress"].(map[string]any); done["state"] != "succeeded" || done["error"] != nil ||
		progress["pages_done"] != 3.0 || progress["pages_failed"] != 0.0 {
		t.Fatalf("after the kill the retried parse ended %v", done)
	}
	for n, pid := range map[int]int{1: first.cmd.Process.Pid, 2: second.cmd.Process.Pid, 3: second.cmd.Process.Pid} {
		status, page, raw := call(t, "GET", at+"/pages/"+strconv.Itoa(n), token(), nil)
		if status != http.StatusOK || page["state"] != "succeeded" || !strings.Contains(string(raw), "read by "+strconv.Itoa(pid)) {
			t.Fatalf("page %d after the retry, to be read by %d: %d %s", n, pid, status, raw)
		}
	}
	if status, doc, raw := call(t, "GET", at+"/document", token(), nil); status != http.StatusOK || len(doc["pages"].([]any)) != 3 || strings.Contains(string(raw), `"failed"`) {
		t.Fatalf("the document after the retry: %d %s", status, raw)
	}
	// The reader was called once for page 1. For each of the others it
	// was called once where it failed and once to read it, and at most
	// once more by the worker that was killed, when the kill found the
	// call in flight.
	read, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	made := map[string]int{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(read)), "\n") {
		_, page, _ := strings.Cut(line, " ")
		made[page]++
	}
	if made["1"] != 1 || made["2"] < 2 || made["2"] > 3 || made["3"] < 2 || made["3"] > 3 {
		t.Fatalf("the reader was called %v times per page", made)
	}
	if n := value[int](t, conn, `SELECT (SELECT count(*) FROM tasks WHERE parse_id = $1) + (`+drift+`)`, id); n != 0 {
		t.Fatalf("%d task rows or drifted counters are left of a parse that succeeded", n)
	}
}

// TestAnEventStreamIsResumedAcrossAnAPIRestart: a stream of a running parse
// sends a page event per page that settled. The API process is killed, and
// a stream opened at the process that replaces it, with the id of an event
// the first one sent, is sent what came after that event under the ids the
// first process gave it, and then the parse's last page and its end. Over
// both streams every page is sent, and none twice to a client that resumed
// after it.
func TestAnEventStreamIsResumedAcrossAnAPIRestart(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	gate := t.TempDir()
	hold := filepath.Join(gate, "hold")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	first := p.spawn("api")
	// One page at a time, in order, and the third waits at the gate.
	p.spawn("worker", gateEnv, gate, gatePageEnv, "3", "LECTIO_WORKERS", "1")
	id := submitted(t, first.base, "scan.tiff", frames(3))
	until(t, "the third page is being read", time.Minute, func() bool { return exists(filepath.Join(gate, "reached")) })

	running := func(ev sent) bool { return ev.name == "state" && ev.data["state"] == "running" }
	before := follow(t, first.base, id, "").until(running)
	early := pagesIn(t, before)
	if len(early) != 2 || early[1] == 0 || early[2] <= early[1] {
		t.Fatalf("with 2 pages read the stream sent %v", before)
	}

	first.signal(syscall.SIGKILL)
	<-first.exited
	second := p.spawn("api")

	// The client saw the first page and no more: the new process sends the
	// second under the id the old one gave it.
	stream := follow(t, second.base, id, strconv.FormatInt(early[1], 10))
	resumed := stream.until(running)
	if again := pagesIn(t, resumed); len(again) != 1 || again[2] != early[2] {
		t.Fatalf("resumed after event %d at a new process the stream sent %v, and the old process had sent %v", early[1], resumed, before)
	}
	if resumed[len(resumed)-1].id != before[len(before)-1].id {
		t.Fatalf("the state of the running parse was event %d at the old process and %d at the new", before[len(before)-1].id, resumed[len(resumed)-1].id)
	}

	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	tail := stream.rest()
	late := pagesIn(t, tail)
	last := tail[len(tail)-1]
	if len(late) != 1 || late[3] <= early[2] || last.name != "state" || last.data["state"] != "succeeded" {
		t.Fatalf("after the last page the stream sent %v", tail)
	}
	for i := 1; i < len(tail); i++ {
		if tail[i].id <= tail[i-1].id {
			t.Fatalf("the ids of the stream do not grow: %v", tail)
		}
	}
	// A stream opened after the end, from the beginning, sends the 3 pages
	// under the same ids, and the same last event.
	whole := follow(t, second.base, id, "").rest()
	if all := pagesIn(t, whole); len(all) != 3 || all[1] != early[1] || all[2] != early[2] || all[3] != late[3] || whole[len(whole)-1].id != last.id {
		t.Fatalf("a stream of the ended parse sent %v", whole)
	}
}

// TestTheQueueOfTwoGroupsIsViewed: a known set of parses of 2 groups waits
// behind a reader that admits 2 calls and holds them. The view of the queue
// holds each group with its parses that have not ended and its queued and
// running tasks, and the pool with its 2 calls in flight. A caller sees its
// own group and its own calls, an admin every group, and nobody the group
// of another by name. When the parses end the queue holds no group.
func TestTheQueueOfTwoGroupsIsViewed(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	gate := t.TempDir()
	hold := filepath.Join(gate, "hold")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	readers := pool(t, 2)
	admin := groupOf("root")
	api := p.spawn("api", "LECTIO_ADMIN_SUBJECTS", admin, "LECTIO_CONFIG", readers)
	p.spawn("worker", gateEnv, gate, "LECTIO_WORKERS", "8", "LECTIO_CONFIG", readers)
	alice, bob, root := tokenOf("alice"), tokenOf("bob"), tokenOf("root")

	// alice: 2 parses of 3 pages. bob: 1 parse of 2 pages.
	ids := map[string][]string{
		"alice": {submittedAs(t, api.base, alice, "a.tiff", frames(3)), submittedAs(t, api.base, alice, "a.tiff", frames(3))},
		"bob":   {submittedAs(t, api.base, bob, "c.tiff", frames(2))},
	}
	conn := p.connect()
	// The 3 prepares ran, 2 pages are held at the gate, and 6 wait.
	until(t, "2 pages are being read and 6 wait", 2*time.Minute, func() bool {
		return value[string](t, conn, `SELECT count(*) FILTER (WHERE state = 'leased' AND calling) || ' ' || count(*) FILTER (WHERE state = 'queued' AND kind = 'page')
		                                 || ' ' || count(*) FILTER (WHERE kind = 'prepare' AND state <> 'succeeded') FROM tasks`) == "2 6 0"
	})

	// tasks sums the queued and the running tasks of a group of a view.
	tasks := func(group any) (queued, running float64) {
		for _, c := range group.(map[string]any)["classes"].([]any) {
			queued += c.(map[string]any)["queued"].(float64)
			running += c.(map[string]any)["running"].(float64)
		}
		return queued, running
	}
	status, view, raw := call(t, "GET", api.base+"/v1/queue", root, nil)
	if status != http.StatusOK || len(view["groups"].([]any)) != 2 {
		t.Fatalf("the queue as an admin sees it: %d %s", status, raw)
	}
	var waiting, reading float64
	held := map[string]float64{}
	for _, g := range view["groups"].([]any) {
		group := g.(map[string]any)
		sub := strings.TrimPrefix(group["group"].(string), provider().URL()+"|")
		queued, running := tasks(g)
		pages := map[string]float64{"alice": 6, "bob": 2}[sub]
		if group["parses"] != float64(len(ids[sub])) || queued+running != pages || group["weight"] != 1.0 || len(group["projects"].([]any)) != 1 {
			t.Errorf("the group of %s is viewed as %v, and it holds %d parses of %v pages", sub, group, len(ids[sub]), pages)
		}
		waiting, reading, held[sub] = waiting+queued, reading+running, running
	}
	pools := view["pools"].([]any)
	if waiting != 6 || reading != 2 || len(pools) != 1 || fmt.Sprint(pools[0]) != "map[breaker:closed in_flight:2 max_in_flight:2 reader:stub scopes:[]]" {
		t.Fatalf("%v tasks wait and %v run, and the pools are viewed as %v", waiting, reading, pools)
	}

	// Each caller sees its own group, and the calls in flight that are its
	// own.
	for sub, tok := range map[string]string{"alice": alice, "bob": bob} {
		status, own, raw := call(t, "GET", api.base+"/v1/queue", tok, nil)
		groups := own["groups"].([]any)
		if status != http.StatusOK || len(groups) != 1 || groups[0].(map[string]any)["group"] != groupOf(sub) ||
			own["pools"].([]any)[0].(map[string]any)["in_flight"] != held[sub] {
			t.Errorf("the queue as %s sees it, with %v of its pages being read: %d %s", sub, held[sub], status, raw)
		}
	}
	// The group of another is nobody's to ask for by name but an admin's.
	other := "?group=" + strings.ReplaceAll(groupOf("bob"), "|", "%7C")
	if status, body, raw := call(t, "GET", api.base+"/v1/queue"+other, alice, nil); status != http.StatusForbidden || code(body) != "forbidden" {
		t.Errorf("alice's view of bob's group: %d %s", status, raw)
	}
	if status, named, raw := call(t, "GET", api.base+"/v1/queue"+other, root, nil); status != http.StatusOK || len(named["groups"].([]any)) != 1 {
		t.Errorf("an admin's view of bob's group: %d %s", status, raw)
	}

	// The gate opens, every parse ends, and no group holds work.
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	for sub, tok := range map[string]string{"alice": alice, "bob": bob} {
		for _, id := range ids[sub] {
			if done := endedAs(t, api.base, tok, id); done["state"] != "succeeded" {
				t.Fatalf("a parse of %s ended %v", sub, done)
			}
		}
	}
	status, view, raw = call(t, "GET", api.base+"/v1/queue", root, nil)
	if status != http.StatusOK || len(view["groups"].([]any)) != 0 || view["pools"].([]any)[0].(map[string]any)["in_flight"] != 0.0 {
		t.Fatalf("the queue with every parse ended: %d %s", status, raw)
	}
}

// meter reads the meters of a caller by reader over days and returns the
// sums of the reader, over every interval, as "pages/calls/input/output".
func meter(t *testing.T, base, bearer, rd string) string {
	t.Helper()
	status, view, raw := call(t, "GET", base+"/v1/usage?by=reader&interval=day", bearer, nil)
	if status != http.StatusOK {
		t.Fatalf("the meters: %d %s", status, raw)
	}
	var sums [4]float64
	for _, s := range view["sums"].([]any) {
		sum := s.(map[string]any)
		if sum["key"] != rd {
			continue
		}
		for i, member := range []string{"pages", "calls", "input_tokens", "output_tokens"} {
			sums[i] += sum[member].(float64)
		}
	}
	return fmt.Sprintf("%.0f/%.0f/%.0f/%.0f", sums[0], sums[1], sums[2], sums[3])
}

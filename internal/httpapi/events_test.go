// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// sse is one event a stream sent.
type sse struct {
	id   int64
	name string
	data map[string]any
}

// follower reads the event stream of a parse.
type follower struct {
	t    *testing.T
	resp *http.Response
	rd   *bufio.Reader
	stop context.CancelFunc

	// retry is the reconnect delay the stream named, and comments how many
	// comment lines it sent.
	retry    string
	comments int
	// seen is every event read so far.
	seen []sse
}

// follow opens the event stream of a parse, resuming after last when it is
// not empty. The stream is read for at most a minute.
func (e *env) follow(parse, last string) *follower {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	e.t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", e.url+"/v1/parses/"+parse+"/events", nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	if last != "" {
		req.Header.Set("Last-Event-ID", last)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		raw, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("the stream answered %d %s: %s", resp.StatusCode, resp.Header.Get("Content-Type"), raw)
	}
	held(e.t, "GET", "/parses/"+parse+"/events", resp.StatusCode, resp.Header, nil)
	return &follower{t: e.t, resp: resp, rd: bufio.NewReader(resp.Body), stop: cancel}
}

// next reads the next event. ok is false when the stream has ended.
func (f *follower) next() (ev sse, ok bool) {
	f.t.Helper()
	var data string
	for {
		line, err := f.rd.ReadString('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				f.t.Fatalf("reading the stream: %v", err)
			}
			return sse{}, false
		}
		line = strings.TrimSuffix(line, "\n")
		field, value, _ := strings.Cut(line, ": ")
		switch {
		case line == "" && ev.name != "":
			if err := json.Unmarshal([]byte(data), &ev.data); err != nil {
				f.t.Fatalf("the data of event %d is %q: %v", ev.id, data, err)
			}
			// The ids of a stream only grow.
			if n := len(f.seen); n > 0 && ev.id <= f.seen[n-1].id {
				f.t.Fatalf("event %d (%s) came after event %d (%s)", ev.id, ev.name, f.seen[n-1].id, f.seen[n-1].name)
			}
			f.seen = append(f.seen, ev)
			return ev, true
		case line == "":
		case strings.HasPrefix(line, ":"):
			f.comments++
		case field == "retry":
			f.retry = value
		case field == "id":
			if ev.id, err = strconv.ParseInt(value, 10, 64); err != nil {
				f.t.Fatalf("an event's id is %q", value)
			}
		case field == "event":
			ev.name = value
		case field == "data":
			data = value
		default:
			f.t.Fatalf("the stream sent the line %q", line)
		}
	}
}

// expect reads the next events and holds each to a name and to what its
// data says, written as "name key=value key=value".
func (f *follower) expect(want ...string) []sse {
	f.t.Helper()
	var out []sse
	for _, w := range want {
		ev, ok := f.next()
		if !ok {
			f.t.Fatalf("the stream ended where %q was to come; it sent %s", w, f.said())
		}
		if got := describe(ev); !matches(got, w) {
			f.t.Fatalf("the stream sent %q where %q was to come; it sent %s", got, w, f.said())
		}
		out = append(out, ev)
	}
	return out
}

// rest reads the stream to its end and returns what it still sent.
func (f *follower) rest() []sse {
	f.t.Helper()
	var out []sse
	for {
		ev, ok := f.next()
		if !ok {
			return out
		}
		out = append(out, ev)
	}
}

// said lists every event read so far.
func (f *follower) said() string {
	parts := make([]string, len(f.seen))
	for i, ev := range f.seen {
		parts[i] = strconv.FormatInt(ev.id, 10) + ":" + describe(ev)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// describe is an event as "name key=value ...", with the members a case
// holds events to.
func describe(ev sse) string {
	out := ev.name
	for _, key := range []string{"page", "state", "stage", "pages_done", "pages_failed"} {
		if v, ok := ev.data[key]; ok {
			out += " " + key + "=" + fmt.Sprint(v)
		}
	}
	if code, _ := at(ev.data, "error", "code").(string); code != "" {
		out += " error=" + code
	}
	return out
}

// pagesOf counts the page events among events.
func pagesOf(events []sse) (n int) {
	for _, ev := range events {
		if ev.name == "page" {
			n++
		}
	}
	return n
}

// matches reports whether an event's description begins with the name
// want begins with and holds every other word of want.
func matches(got, want string) bool {
	have, words := strings.Fields(got), strings.Fields(want)
	if len(have) == 0 || have[0] != words[0] {
		return false
	}
	for _, word := range words[1:] {
		if !slices.Contains(have, word) {
			return false
		}
	}
	return true
}

// idle reads the stream until it sends a comment, and fails when it sends
// an event first: nothing changed.
func (f *follower) idle() {
	f.t.Helper()
	for before := f.comments; f.comments == before; {
		line, err := f.rd.ReadString('\n')
		if err != nil {
			f.t.Fatalf("reading an idle stream: %v", err)
		}
		switch {
		case strings.HasPrefix(line, ":"):
			f.comments++
		case strings.HasPrefix(line, "retry: "):
			f.retry = strings.TrimSpace(strings.TrimPrefix(line, "retry: "))
		case line != "\n":
			f.t.Fatalf("an idle stream sent %q", line)
		}
	}
}

// quick makes a server's streams read their rows every few milliseconds.
func quick(s *Server) {
	s.EventsPoll, s.EventsHeartbeat = 5*time.Millisecond, 40*time.Millisecond
}

// TestAParseIsFollowedAsEvents: the stream of a running parse sends a page
// event for each page that settled, in the order they settled, then the
// parse's progress and state, under ids that only grow. It says nothing
// new while nothing changes, and a comment keeps it open. A client that
// connects again with Last-Event-ID is sent what came after that event,
// under the same ids, and nothing twice. When the parse ends the stream
// sends its last page, its progress and its state, and ends, and a stream
// opened afterwards sends the same events under the same ids.
func TestAParseIsFollowedAsEvents(t *testing.T) {
	durably(t, false)
	reached, release := make(chan struct{}), make(chan struct{})
	e := serve(t, func(s *Server, r *run.Runner) {
		quick(s)
		r.Workers = 1
		r.Readers = map[string]reader.Reader{"stub": &stub.Reader{Fail: func(p reader.Page) error {
			if p.Number == 3 {
				close(reached)
				<-release
			}
			return nil
		}}}
	})
	file := e.upload("scan.tiff", testfixtures.Read(t, testfixtures.MultiTIFF))
	sub := e.do("POST", "/parses", `{"source":{"file":"`+file+`"},"reuse":false}`)
	if sub.status != http.StatusAccepted {
		t.Fatalf("submit: %d %s", sub.status, sub.body)
	}
	pid := sub.json(t)["id"].(string)
	<-reached

	// 2 pages have settled and the third is being read.
	first := e.follow(pid, "")
	began := first.expect("page page=1 state=succeeded", "page page=2 state=succeeded", "progress stage=reading pages_done=2 pages_failed=0", "state state=running")
	if first.retry != "5" {
		t.Fatalf("the stream named the delay %q before a client connects again, want its poll of 5 ms", first.retry)
	}
	if at(began[2].data, "pages_total") != 3.0 {
		t.Fatalf("the progress event is %v", began[2].data)
	}

	// A client that saw the first page only is sent the second under the
	// id it had, and where the parse stands.
	second := e.follow(pid, strconv.FormatInt(began[0].id, 10))
	resumed := second.expect("page page=2", "progress pages_done=2", "state state=running")
	if resumed[0].id != began[1].id || resumed[1].id != began[2].id || resumed[2].id != began[3].id {
		t.Fatalf("a stream resumed after event %d sent %s, and the first stream %s", began[0].id, second.said(), first.said())
	}
	// A client that saw everything is sent nothing until something
	// changes: the streams stay open on comments alone.
	third := e.follow(pid, strconv.FormatInt(began[3].id, 10))
	first.idle()
	third.idle()

	close(release)
	// The last page settles, the parse is assembled, and it ends. A stream
	// may or may not catch the parse between its last page and its end.
	for name, f := range map[string]*follower{"the first stream": first, "the stream that resumed": second, "the stream that had seen everything": third} {
		before := pagesOf(f.seen)
		last := f.rest()
		if len(last) < 3 || !matches(describe(last[0]), "page page=3 state=succeeded") || pagesOf(last) != 1 ||
			!matches(describe(last[len(last)-2]), "progress stage=done pages_done=3") ||
			!matches(describe(last[len(last)-1]), "state state=succeeded") {
			t.Fatalf("%s ended with %s", name, f.said())
		}
		if want := map[string]int{"the first stream": 2, "the stream that resumed": 1}[name]; before != want {
			t.Fatalf("%s had sent %d page events before the end, want %d: %s", name, before, want, f.said())
		}
	}
	final := first.seen[len(first.seen)-1]

	// The parse has ended and its page rows are gone. A stream opened now
	// sends every page under the id it had, where the parse stands, and
	// ends.
	after := e.follow(pid, "")
	got := after.rest()
	if len(got) != 5 || got[0].id != began[0].id || got[1].id != began[1].id || !matches(describe(got[2]), "page page=3") ||
		!matches(describe(got[3]), "progress stage=done pages_done=3") || got[4].id != final.id || got[4].data["state"] != "succeeded" {
		t.Fatalf("a stream of a parse that ended sent %s, and the first stream %s", after.said(), first.said())
	}
	// Resumed in the middle it sends the rest, and after the last event
	// nothing.
	if got := e.follow(pid, strconv.FormatInt(began[1].id, 10)).rest(); len(got) != 3 || !matches(describe(got[0]), "page page=3") {
		t.Fatalf("a stream resumed after the second page sent %d events", len(got))
	}
	if got := e.follow(pid, strconv.FormatInt(final.id, 10)).rest(); len(got) != 0 {
		t.Fatalf("a stream resumed after the last event sent %d events", len(got))
	}
	// An id the stream never sent is read as none.
	for _, id := range []string{"not-an-id", "-4", strconv.FormatInt(final.id+1000, 10)} {
		if got := e.follow(pid, id).rest(); len(got) != 5 || got[4].id != final.id {
			t.Fatalf("a stream resumed after %q sent %d events", id, len(got))
		}
	}
	// Another caller's parse is not there to follow.
	if got := e.as("bob-token").do("GET", "/parses/"+pid+"/events", nil); got.status != http.StatusNotFound || got.code(t) != "parse_not_found" {
		t.Fatalf("the events of another caller's parse: %d %s", got.status, got.body)
	}

	// A format that needs no reader has its pages with prepare: each is
	// told as a page a reader read is.
	csv := e.parsed(e.upload("sample.csv", testfixtures.Read(t, testfixtures.CSV)), "")
	if got := e.follow(csv["id"].(string), "").rest(); len(got) != 3 || !matches(describe(got[0]), "page page=1 state=succeeded") ||
		!matches(describe(got[1]), "progress stage=done pages_done=1") || !matches(describe(got[2]), "state state=succeeded") {
		t.Fatalf("the stream of a native parse sent %v", got)
	}
}

// TestTheEventsOfAParseWithAFailedPage: a parse that ended with a failed
// page keeps its page rows, so a stream opened after it ended sends every
// page, the failed one with its reason, and the state with the parse's
// error. A retry is a change: a stream resumed after the parse's last event
// is sent the parse running again, the page when it settles again, and the
// end. A cancel ends a stream with the state canceled and no page event for
// the pages it stopped.
func TestTheEventsOfAParseWithAFailedPage(t *testing.T) {
	durably(t, false)
	rd := newFlaky()
	e := serve(t, func(s *Server, r *run.Runner) {
		quick(s)
		r.Readers = map[string]reader.Reader{"stub": rd}
	})
	file := e.upload("scan.tiff", testfixtures.Read(t, testfixtures.MultiTIFF))
	pid := e.parsed(file, `,"reuse":false`)["id"].(string)

	ended := e.follow(pid, "")
	got := ended.rest()
	if len(got) != 5 || got[3].name != "progress" || !matches(describe(got[4]), "state state=failed error=page_unreadable") {
		t.Fatalf("the stream of a parse that failed sent %s", ended.said())
	}
	states := map[float64]string{}
	for _, ev := range got[:3] {
		states[ev.data["page"].(float64)] = describe(ev)
	}
	if !matches(states[1], "page state=succeeded") || !matches(states[2], "page state=failed error=page_unreadable") || !matches(states[3], "page state=succeeded") {
		t.Fatalf("the pages of a parse that failed were sent as %v", states)
	}

	rd.broken.Store(false)
	reached, release := rd.hold()
	if r := e.do("POST", "/parses/"+pid+"/retry", nil); r.status != http.StatusAccepted {
		t.Fatalf("retry: %d %s", r.status, r.body)
	}
	<-reached
	again := e.follow(pid, strconv.FormatInt(got[4].id, 10))
	again.expect("progress stage=reading pages_done=2 pages_failed=0", "state state=running")
	close(release)
	last := again.rest()
	if len(last) < 3 || !matches(describe(last[0]), "page page=2 state=succeeded") || !matches(describe(last[len(last)-1]), "state state=succeeded") {
		t.Fatalf("after the retry the stream sent %s", again.said())
	}

	// A parse that is canceled while its page is read.
	rd.broken.Store(false)
	reached, release = rd.hold()
	sub := e.do("POST", "/parses", `{"source":{"file":"`+file+`"},"reuse":false}`)
	stopped := sub.json(t)["id"].(string)
	<-reached
	following := e.follow(stopped, "")
	if c := e.do("POST", "/parses/"+stopped+"/cancel", nil); c.status != http.StatusOK {
		t.Fatalf("cancel: %d %s", c.status, c.body)
	}
	tail := following.rest()
	close(release)
	if n := len(tail); n < 2 || !matches(describe(tail[n-1]), "state state=canceled") {
		t.Fatalf("the stream of a canceled parse sent %s", following.said())
	}
	for _, ev := range tail {
		if ev.name == "page" && ev.data["page"] == 2.0 {
			t.Fatalf("a page that was stopped was sent as settled: %s", following.said())
		}
	}
}

// TestAStreamIsHeldForABoundedTime: a stream of a parse that does not end
// is ended by the server after EventsHold, with the parse still running. A
// client that connects again after the last event it saw is sent nothing
// it had, and the end when it comes.
func TestAStreamIsHeldForABoundedTime(t *testing.T) {
	durably(t, false)
	rd := newFlaky()
	rd.broken.Store(false)
	reached, release := rd.hold()
	e := serve(t, func(s *Server, r *run.Runner) {
		quick(s)
		s.EventsHold = 150 * time.Millisecond
		r.Readers = map[string]reader.Reader{"stub": rd}
	})
	file := e.upload("scan.tiff", testfixtures.Read(t, testfixtures.MultiTIFF))
	pid := e.do("POST", "/parses", `{"source":{"file":"`+file+`"},"reuse":false}`).json(t)["id"].(string)
	<-reached

	began := time.Now()
	held := e.follow(pid, "")
	got := held.rest()
	// It said the parse is running, and nothing of an end.
	if n := len(got); n < 2 || !slices.ContainsFunc(got, func(ev sse) bool { return ev.data["state"] == "running" }) || got[n-1].data["state"] == "succeeded" {
		t.Fatalf("a stream the server ended sent %s", held.said())
	}
	if took := time.Since(began); took < 150*time.Millisecond || took > 30*time.Second {
		t.Fatalf("the server held the stream for %v, want its bound of 150ms", took)
	}
	close(release)
	// Each stream is held for the bound again, so the end is read over as
	// many connections as it takes.
	last, pages := got[len(got)-1].id, 0
	for deadline := time.Now().Add(30 * time.Second); ; {
		if time.Now().After(deadline) {
			t.Fatal("the parse's end was not sent")
		}
		more := e.follow(pid, strconv.FormatInt(last, 10)).rest()
		for _, ev := range more {
			if ev.id <= last {
				t.Fatalf("a stream resumed after %d sent event %d", last, ev.id)
			}
			if last = ev.id; ev.name == "page" {
				pages++
			}
		}
		if n := len(more); n > 0 && more[n-1].data["state"] == "succeeded" {
			break
		}
	}
	// The pages that settled while no stream was open were sent by the
	// next one: every page but those the first stream had sent.
	if seen := pagesOf(got); seen+pages != 3 {
		t.Fatalf("%d pages were sent before the server ended the stream and %d after, want 3 in all", seen, pages)
	}
}

// scripted is a backend whose events a case writes: each read of the rows
// answers the next step of the script.
type scripted struct {
	*Memory
	parse store.Parse
	steps []func() (store.Events, error)
	reads int
}

func (s *scripted) Parse(context.Context, string) (store.Parse, error) { return s.parse, nil }

func (s *scripted) Events(context.Context, string, int64, int) (store.Events, error) {
	step := s.steps[min(s.reads, len(s.steps)-1)]
	s.reads++
	return step()
}

// TestWhatEndsAStream: a read of the rows that fails before anything was
// sent is answered as an error. Once the stream has begun, a read that
// fails ends it and is logged, a parse that was deleted ends it in silence,
// and a stream further behind than one read holds reads again at once and
// says where the parse stands after the last page only.
func TestWhatEndsAStream(t *testing.T) {
	running := store.Parse{ID: "prs_1", Owner: "alice", State: store.StateRunning, Stage: store.StageReading, PagesTotal: 1200}
	done := running
	done.State, done.Stage, done.PagesDone = store.StateSucceeded, store.StageDone, 1200
	batch := func(from int) []store.PageEvent {
		out := make([]store.PageEvent, eventsBatch)
		for i := range out {
			out[i] = store.PageEvent{Seq: int64(from + i), Page: from + i, State: "succeeded"}
		}
		return out
	}
	steady := func() (store.Events, error) { return store.Events{Parse: running, Seq: 2}, nil }

	for name, tc := range map[string]struct {
		steps  []func() (store.Events, error)
		status int
		// events and logged are what the stream sent and whether its end
		// was logged.
		events int
		logged bool
	}{
		"the first read fails": {
			[]func() (store.Events, error){func() (store.Events, error) { return store.Events{}, errors.New("the database is away") }},
			http.StatusInternalServerError, 0, true,
		},
		"a later read fails": {
			[]func() (store.Events, error){steady, func() (store.Events, error) { return store.Events{}, errors.New("the database is away") }},
			http.StatusOK, 2, true,
		},
		"the parse is deleted": {
			[]func() (store.Events, error){steady, func() (store.Events, error) {
				return store.Events{}, fault.New(fault.ParseNotFound, "no parse prs_1")
			}},
			http.StatusOK, 2, false,
		},
		"more pages than one read holds": {
			[]func() (store.Events, error){
				func() (store.Events, error) { return store.Events{Parse: done, Seq: 1202, Pages: batch(1)}, nil },
				func() (store.Events, error) { return store.Events{Parse: done, Seq: 1202, Pages: batch(501)}, nil },
				func() (store.Events, error) {
					return store.Events{Parse: done, Seq: 1202, Pages: batch(1001)[:200]}, nil
				},
			},
			http.StatusOK, 1202, false,
		},
	} {
		var logged bytes.Buffer
		backend := &scripted{Memory: &Memory{Store: store.NewMemory()}, parse: running, steps: tc.steps}
		s := &Server{Backend: backend, Auth: callers, Authz: ownerPolicy(), Log: slog.New(slog.NewTextHandler(&logged, nil))}
		quick(s)
		srv := httptest.NewServer(s.Handler())
		e := &env{t: t, server: s, url: srv.URL, token: "alice-token"}
		if tc.status != http.StatusOK {
			if got := e.do("GET", "/parses/prs_1/events", nil); got.status != tc.status || got.code(t) != "internal" {
				t.Errorf("%s: %d %s", name, got.status, got.body)
			}
		} else {
			f := e.follow("prs_1", "")
			got := f.rest()
			if len(got) != tc.events {
				t.Errorf("%s: the stream sent %d events, want %d", name, len(got), tc.events)
			}
			if tc.events == 1202 && (got[1199].name != "page" || got[1200].name != "progress" || got[1201].name != "state" || backend.reads != 3) {
				t.Errorf("%s: after %d reads the stream ended with %s, %s, %s", name, backend.reads, got[1199].name, got[1200].name, got[1201].name)
			}
		}
		srv.Close()
		if said := strings.Contains(logged.String(), "the database is away"); said != tc.logged {
			t.Errorf("%s: the log is %q", name, logged.String())
		}
	}

	// A client that leaves ends its stream: the server stops reading the
	// rows for it, which closing the server would otherwise wait on for as
	// long as a stream is held, and it logs nothing.
	var logged bytes.Buffer
	backend := &scripted{Memory: &Memory{Store: store.NewMemory()}, parse: running, steps: []func() (store.Events, error){steady}}
	s := &Server{Backend: backend, Auth: callers, Authz: ownerPolicy(), Log: slog.New(slog.NewTextHandler(&logged, nil))}
	quick(s)
	srv := httptest.NewServer(s.Handler())
	f := (&env{t: t, server: s, url: srv.URL, token: "alice-token"}).follow("prs_1", "")
	f.expect("progress stage=reading", "state state=running")
	f.idle()
	f.stop()
	closed := make(chan struct{})
	go func() { srv.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("the stream of a client that left was still held")
	}
	if logged.Len() != 0 {
		t.Errorf("a client that left was logged: %s", logged.String())
	}
}

// deadlined is a response whose write deadlines are recorded, and whose
// writes fail when a case says so.
type deadlined struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	broken    bool
}

func (d *deadlined) SetWriteDeadline(at time.Time) error {
	d.deadlines = append(d.deadlines, at)
	return nil
}

func (d *deadlined) Write(b []byte) (int, error) {
	if d.broken {
		return 0, errors.New("the connection is gone")
	}
	return d.ResponseRecorder.Write(b)
}

// TestAStreamBoundsItsWritesAndLeavesNoDeadlineBehind: each write of a
// stream is bounded, and the bound is lifted when the stream ends, so the
// next request on the connection is not cut by it. A write that fails ends
// the stream and is logged.
func TestAStreamBoundsItsWritesAndLeavesNoDeadlineBehind(t *testing.T) {
	done := store.Parse{ID: "prs_1", Owner: "alice", State: store.StateSucceeded, Stage: store.StageDone, PagesTotal: 1, PagesDone: 1}
	for name, broken := range map[string]bool{"a stream that ends": false, "a stream whose write fails": true} {
		var logged bytes.Buffer
		backend := &scripted{Memory: &Memory{Store: store.NewMemory()}, parse: done, steps: []func() (store.Events, error){
			func() (store.Events, error) {
				return store.Events{Parse: done, Seq: 4, Pages: []store.PageEvent{{Seq: 3, Page: 1, State: "succeeded"}}}, nil
			},
		}}
		s := &Server{Backend: backend, Auth: callers, Authz: ownerPolicy(), Log: slog.New(slog.NewTextHandler(&logged, nil))}
		rec := &deadlined{ResponseRecorder: httptest.NewRecorder(), broken: broken}
		req := httptest.NewRequest("GET", "/v1/parses/prs_1/events", nil)
		req.Header.Set("Authorization", "Bearer alice-token")
		s.Handler().ServeHTTP(rec, req)

		n := len(rec.deadlines)
		if n < 2 || !rec.deadlines[n-1].IsZero() || rec.deadlines[0].IsZero() || time.Until(rec.deadlines[0]) > eventsWrite {
			t.Errorf("%s: the write deadlines were %v", name, rec.deadlines)
		}
		body := rec.Body.String()
		if !broken && (!strings.Contains(body, "id: 9\nevent: page\n") || !strings.Contains(body, "id: 13\nevent: progress\n") || !strings.HasSuffix(body, "id: 14\nevent: state\ndata: {\"state\":\"succeeded\"}\n\n")) {
			t.Errorf("%s: the stream was %q", name, body)
		}
		if said := strings.Contains(logged.String(), "the connection is gone"); said != broken {
			t.Errorf("%s: the log is %q", name, logged.String())
		}
	}
}

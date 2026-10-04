// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store"
)

// The bounds of an event stream (specs/003-api.md). A stream is a poll of
// its parse's rows: there is no bus between the processes, so any replica
// serves any stream and each open stream costs one statement per poll.
const (
	// eventsPoll is how often a stream reads its parse's rows, and so the
	// longest an event waits to be sent. It is also the delay a client is
	// told to wait before it connects again.
	eventsPoll = time.Second
	// eventsHeartbeat is the longest a stream says nothing. A comment is
	// sent then, so a proxy that cuts idle connections does not cut a
	// stream whose parse is waiting.
	eventsHeartbeat = 15 * time.Second
	// eventsHold is the longest one stream is held. The stream then ends
	// and its client connects again with the id of the last event it saw,
	// so no connection, and no decision of the authorizer, is held for
	// longer.
	eventsHold = 5 * time.Minute
	// eventsBatch is the most pages one read of the rows returns. A stream
	// that is further behind reads again at once.
	eventsBatch = 500
	// eventsWrite bounds one write, so a client that stopped reading does
	// not hold its stream open.
	eventsWrite = 10 * time.Second
)

// The id of an event. The store counts the changes of a parse, and a page's
// row holds the change that settled it. An event's id is eventSlots times
// the change it reports plus the slot of its kind, so every event of a
// parse has an id of its own, the ids only grow, and the events of one
// change are sent in the order page, progress, state. Nothing of an id is
// held in a process: the same rows give the same ids on every replica.
const (
	eventSlots   = 3
	slotPage     = 0
	slotProgress = 1
	slotState    = 2
)

// The data of the events.
type (
	// pageEvent says a page settled: read, or failed with the reason.
	pageEvent struct {
		Page  int             `json:"page"`
		State string          `json:"state"`
		Error *document.Error `json:"error,omitempty"`
	}
	// stateEvent is the parse's state, with why it failed when it did.
	stateEvent struct {
		State string          `json:"state"`
		Error *document.Error `json:"error,omitempty"`
	}
)

// lastEventID reads the id a client resumes after. A header that is absent
// or is no id resumes after nothing: the stream starts from its beginning.
func lastEventID(r *http.Request) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("Last-Event-ID")), 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

// eventStream is one stream being written.
type eventStream struct {
	w  http.ResponseWriter
	rc *http.ResponseController

	// last is the id of the last event the client was sent, on this
	// connection or, by its Last-Event-ID, on one before.
	last int64
	// state is the state last sent on this connection, so a change of the
	// parse that left its state alone does not send the state again.
	state string
	// wrote is when the stream last wrote anything.
	wrote time.Time
}

// write sends bytes and flushes them, within eventsWrite.
func (e *eventStream) write(format string, args ...any) error {
	if err := e.rc.SetWriteDeadline(time.Now().Add(eventsWrite)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(e.w, format, args...); err != nil {
		return err
	}
	e.wrote = time.Now()
	return e.rc.Flush()
}

// event sends one event under its id.
func (e *eventStream) event(id int64, name string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if err := e.write("id: %d\nevent: %s\ndata: %s\n\n", id, name, raw); err != nil {
		return err
	}
	e.last = id
	return nil
}

// send writes what one read of the rows holds that the client has not been
// sent: the pages that settled, in the order they settled, and then where
// the parse stands. more reports that the read was full, so pages may wait
// behind it and where the parse stands is not sent yet: it is sent after
// the last page, so that a client that resumes after it has every page.
func (e *eventStream) send(ev store.Events) (more bool, err error) {
	for _, page := range ev.Pages {
		if err := e.event(page.Seq*eventSlots+slotPage, "page", pageEvent{Page: page.Page, State: page.State, Error: page.Error}); err != nil {
			return false, err
		}
	}
	if len(ev.Pages) == eventsBatch {
		return true, nil
	}
	if id := ev.Seq*eventSlots + slotProgress; id > e.last {
		if err := e.event(id, "progress", viewParse(ev.Parse).Progress); err != nil {
			return false, err
		}
	}
	if id := ev.Seq*eventSlots + slotState; id > e.last && ev.Parse.State != e.state {
		if err := e.event(id, "state", stateEvent{State: ev.Parse.State, Error: ev.Parse.Error}); err != nil {
			return false, err
		}
		e.state = ev.Parse.State
	}
	return false, nil
}

// streamParse follows a parse as server-sent events until the parse ends:
// one page event per page that settles, and the parse's progress and state
// as they change. The events are read from the parse's rows on an interval
// and nothing is kept between 2 reads, so a client that connects again
// with Last-Event-ID, to this replica or another, is sent what settled
// after that event. A stream is held for at most EventsHold.
func (s *Server) streamParse(w http.ResponseWriter, r *http.Request, c call) error {
	p, _, err := s.parse(r, c)
	if err != nil {
		return err
	}
	ctx := r.Context()
	out := &eventStream{w: w, rc: http.NewResponseController(w), last: lastEventID(r)}
	// The first read is made before anything is written, so what it fails
	// with is answered as any error is.
	ev, err := s.Backend.Events(ctx, p.ID, out.last/eventSlots, eventsBatch)
	if err == nil && out.last > ev.Seq*eventSlots+slotState {
		// The id is past everything this parse has sent: it is not an id of
		// this stream, and the stream starts from its beginning.
		out.last = 0
		ev, err = s.Backend.Events(ctx, p.ID, 0, eventsBatch)
	}
	if err != nil {
		return err
	}

	poll := cmp.Or(s.EventsPoll, eventsPoll)
	heartbeat := cmp.Or(s.EventsHeartbeat, eventsHeartbeat)
	hold := time.NewTimer(cmp.Or(s.EventsHold, eventsHold))
	defer hold.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// From here on the answer is the stream: an error ends it, and the
	// client connects again.
	err = out.write("retry: %d\n\n", poll.Milliseconds())
	for err == nil {
		var more bool
		if more, err = out.send(ev); err != nil {
			break
		}
		if !more {
			if ev.Parse.Terminal() {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-hold.C:
				return nil
			case <-tick.C:
			}
			if time.Since(out.wrote) >= heartbeat {
				if err = out.write(": waiting\n\n"); err != nil {
					break
				}
			}
		}
		ev, err = s.Backend.Events(ctx, p.ID, out.last/eventSlots, eventsBatch)
	}
	if ctx.Err() == nil && fault.CodeOf(err) != fault.ParseNotFound {
		// A client that left and a parse that was deleted end a stream as a
		// matter of course. Anything else is worth a line.
		s.log().WarnContext(ctx, "an event stream ended early", "parse", p.ID, "error", err)
	}
	return nil
}

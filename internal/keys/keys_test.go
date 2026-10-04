// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/lectio/reader"
)

// bearer is the token the stub endpoint requires. A case that finds it, or
// a key the stub issued, in an error or a log line has found a leak.
const bearer = "kt-bearer-do-not-print"

// clock is a clock a case moves.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// asked is one request the stub endpoint received.
type asked struct {
	Method, Authorization, ContentType string
	Body                               question
}

// plane is a stub of an operator's key endpoint: it issues a key per group,
// each good for the lifetime the case sets, records every request, and
// answers what the case scripted for a group instead when it scripted
// something.
type plane struct {
	*httptest.Server
	clock *clock

	mu       sync.Mutex
	requests []asked
	issued   map[string]int
	lifetime time.Duration
	// script answers a request for a group in place of a key. It reports
	// false to let the key be issued.
	script script
	// inFlight and most count the requests being answered per group, now
	// and at their highest.
	inFlight, most map[string]int
}

// script answers a request of a group. It reports false to let the stub
// issue a key.
type script func(w http.ResponseWriter, r *http.Request, group string) bool

func newPlane(t *testing.T) *plane {
	t.Helper()
	p := &plane{
		clock:  &clock{at: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)},
		issued: map[string]int{}, lifetime: time.Hour, inFlight: map[string]int{}, most: map[string]int{},
	}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request: %v", err)
		}
		q := asked{Method: r.Method, Authorization: r.Header.Get("Authorization"), ContentType: r.Header.Get("Content-Type")}
		if err := json.Unmarshal(raw, &q.Body); err != nil {
			t.Errorf("the request is not JSON: %v", err)
		}
		group := q.Body.Group
		p.mu.Lock()
		p.requests = append(p.requests, q)
		p.inFlight[group]++
		p.most[group] = max(p.most[group], p.inFlight[group])
		script := p.script
		p.mu.Unlock()
		defer func() {
			p.mu.Lock()
			p.inFlight[group]--
			p.mu.Unlock()
		}()
		if script != nil && script(w, r, group) {
			return
		}
		p.mu.Lock()
		p.issued[group]++
		key, expires := keyOf(group, p.issued[group]), p.clock.now().Add(p.lifetime)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"key": key, "expires_at": expires.Format(time.RFC3339Nano)}); err != nil {
			t.Errorf("writing the answer: %v", err)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

// keyOf is the nth key the stub issues a group.
func keyOf(group string, n int) string { return "sk-issued-" + group + "-" + strconv.Itoa(n) }

// answers scripts the stub.
func (p *plane) answers(script script) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.script = script
}

// lasting sets how long the keys the stub issues from now on are good for.
func (p *plane) lasting(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lifetime = d
}

// seen is a copy of the requests the stub received, and the most it
// answered at once for a group.
func (p *plane) seen(group string) (requests []asked, atOnce int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]asked(nil), p.requests...), p.most[group]
}

// count is how many requests the stub received for a group.
func (p *plane) count(group string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, q := range p.requests {
		if q.Body.Group == group {
			n++
		}
	}
	return n
}

// source is an endpoint source over the stub, on the stub's clock, with its
// log in the returned buffer.
func (p *plane) source() (*Endpoint, *logs) {
	out := &logs{}
	return NewEndpoint(Options{
		URL: p.URL, Token: reader.NewCredential(bearer), Now: p.clock.now,
		Log: slog.New(slog.NewJSONHandler(out, nil)),
	}), out
}

// logs is a log a case reads while the source writes it.
type logs struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// status scripts one status, with headers in pairs, for every group.
func status(code int, headers ...string) script {
	return func(w http.ResponseWriter, _ *http.Request, _ string) bool {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(code)
		return true
	}
}

// body scripts a 200 with a body, for every group.
func body(text string) script {
	return func(w http.ResponseWriter, _ *http.Request, _ string) bool {
		_, _ = io.WriteString(w, text)
		return true
	}
}

var ctx = context.Background()

// TestTheStaticSourceGivesEveryGroupTheOneKey: with one key from
// configuration every group reads with it, and printing the source shows no
// key.
func TestTheStaticSourceGivesEveryGroupTheOneKey(t *testing.T) {
	var source Source = Static{Credential: reader.NewCredential("sk-operator-do-not-print")}
	for _, group := range []string{"acme", "globex"} {
		if key, err := source.Key(ctx, group, "alice", "prs_1"); err != nil || key.Reveal() != "sk-operator-do-not-print" {
			t.Fatalf("the key of %s: %q, %v", group, key.Reveal(), err)
		}
	}
	if key, err := (Static{}).Key(ctx, "acme", "alice", "prs_1"); err != nil || !key.IsZero() {
		t.Fatalf("with no key configured the source gives %q, %v", key.Reveal(), err)
	}
	for _, shown := range []string{fmt.Sprint(source), fmt.Sprintf("%+v", source), fmt.Sprintf("%#v", source)} {
		if strings.Contains(shown, "sk-operator") {
			t.Fatalf("the static source prints its key: %s", shown)
		}
	}
}

// TestAGroupsKeyIsAskedOnceAndHeldUntilShortlyBeforeItExpires: the first
// page of a group asks the endpoint, with the bearer and the group, the
// owner and the parse in the body. Later pages of the group read with the
// same key and ask nothing, another group has a key of its own, and the key
// is asked again once less than the margin is left of it.
func TestAGroupsKeyIsAskedOnceAndHeldUntilShortlyBeforeItExpires(t *testing.T) {
	p := newPlane(t)
	e, _ := p.source()

	first, err := e.Key(ctx, "acme", "org:acme", "prs_1")
	if err != nil || first.Reveal() != keyOf("acme", 1) {
		t.Fatalf("the first key of acme: %q, %v", first.Reveal(), err)
	}
	want := asked{Method: http.MethodPost, Authorization: "Bearer " + bearer, ContentType: "application/json",
		Body: question{Group: "acme", Owner: "org:acme", Parse: "prs_1"}}
	if requests, _ := p.seen("acme"); len(requests) != 1 || requests[0] != want {
		t.Fatalf("the endpoint was asked %+v, want %+v", requests, want)
	}
	for _, parse := range []string{"prs_1", "prs_2"} {
		if again, err := e.Key(ctx, "acme", "org:acme", parse); err != nil || again.Reveal() != first.Reveal() {
			t.Fatalf("a later page of %s read with %q, %v", parse, again.Reveal(), err)
		}
	}
	other, err := e.Key(ctx, "globex", "org:globex", "prs_9")
	if err != nil || other.Reveal() != keyOf("globex", 1) {
		t.Fatalf("the key of globex: %q, %v", other.Reveal(), err)
	}
	if p.count("acme") != 1 || p.count("globex") != 1 {
		t.Fatalf("the endpoint was asked %d times for acme and %d for globex, want 1 and 1", p.count("acme"), p.count("globex"))
	}

	// One second more than the margin is left: the key is still handed out.
	p.clock.advance(time.Hour - Margin - time.Second)
	if held, err := e.Key(ctx, "acme", "org:acme", "prs_3"); err != nil || held.Reveal() != first.Reveal() || p.count("acme") != 1 {
		t.Fatalf("with more than the margin left the key was %q after %d requests, %v", held.Reveal(), p.count("acme"), err)
	}
	// The margin is left: the key is asked again, before it expires.
	p.clock.advance(time.Second)
	second, err := e.Key(ctx, "acme", "org:acme", "prs_3")
	if err != nil || second.Reveal() != keyOf("acme", 2) || p.count("acme") != 2 {
		t.Fatalf("at the margin the key was %q after %d requests, %v", second.Reveal(), p.count("acme"), err)
	}
	if requests, _ := p.seen("acme"); requests[len(requests)-1].Body.Parse != "prs_3" {
		t.Fatalf("the second request names the parse %q", requests[len(requests)-1].Body.Parse)
	}
}

// TestCallsOfOneGroupWaitForOneRequest: while a group's key is being asked
// for, every other call for the group waits for that answer and sends
// nothing, and a group beside it is not held up. A call that ends while it
// waits returns as unavailable and leaves the request to the others.
func TestCallsOfOneGroupWaitForOneRequest(t *testing.T) {
	p := newPlane(t)
	e, _ := p.source()
	entered, release := make(chan struct{}, 16), make(chan struct{})
	p.answers(func(_ http.ResponseWriter, _ *http.Request, group string) bool {
		if group == "acme" {
			entered <- struct{}{}
			<-release
		}
		return false
	})

	const callers = 12
	got := make(chan string, callers)
	for range callers {
		go func() {
			key, err := e.Key(ctx, "acme", "org:acme", "prs_1")
			if err != nil {
				got <- err.Error()
				return
			}
			got <- key.Reveal()
		}()
	}
	<-entered

	// The request is in flight and held. A call that gives up while it
	// waits is told the key is not there yet, and names no wait.
	short, cancel := context.WithCancel(ctx)
	cancel()
	_, err := e.Key(short, "acme", "org:acme", "prs_1")
	if u, ok := errors.AsType[*Unavailable](err); !ok || !errors.Is(err, context.Canceled) || u.RetryAfter != 0 || RetryAfterOf(err) != 0 {
		t.Fatalf("a call that ended while it waited: %v", err)
	}
	// Another group is asked for and answered meanwhile.
	if key, err := e.Key(ctx, "globex", "org:globex", "prs_2"); err != nil || key.Reveal() != keyOf("globex", 1) {
		t.Fatalf("a group beside the one in flight: %q, %v", key.Reveal(), err)
	}

	close(release)
	for range callers {
		if key := <-got; key != keyOf("acme", 1) {
			t.Fatalf("a call that waited read with %q, want the one key the endpoint issued", key)
		}
	}
	if _, atOnce := p.seen("acme"); p.count("acme") != 1 || atOnce != 1 {
		t.Fatalf("the endpoint saw %d requests for acme, %d at once, want 1", p.count("acme"), atOnce)
	}
}

// TestARefusalIsAboutTheGroupAndIsHeldBriefly: a 402 says the group has no
// budget and a 403 that it is issued no key. Each is a refusal a page fails
// on, is answered from memory for RefusalTTL so that the queued pages of the
// group cost one request, and is asked again after it.
func TestARefusalIsAboutTheGroupAndIsHeldBriefly(t *testing.T) {
	for code, want := range map[int]error{http.StatusPaymentRequired: ErrBudget, http.StatusForbidden: ErrForbidden} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			p := newPlane(t)
			e, out := p.source()
			p.answers(func(w http.ResponseWriter, _ *http.Request, group string) bool {
				if group != "acme" {
					return false
				}
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{"key":"sk-in-a-refusal","error":"no"}`)
				return true
			})
			for range 3 {
				_, err := e.Key(ctx, "acme", "org:acme", "prs_1")
				if !errors.Is(err, want) || RetryAfterOf(err) != 0 {
					t.Fatalf("a %d is %v, want %v", code, err, want)
				}
				if _, isDown := errors.AsType[*Unavailable](err); isDown {
					t.Fatalf("a %d is taken for an endpoint that is down: %v", code, err)
				}
			}
			if p.count("acme") != 1 {
				t.Fatalf("3 pages of a refused group cost %d requests, want 1", p.count("acme"))
			}
			// The refusal is the group's own.
			if key, err := e.Key(ctx, "globex", "org:globex", "prs_2"); err != nil || key.Reveal() != keyOf("globex", 1) {
				t.Fatalf("a group beside the refused one: %q, %v", key.Reveal(), err)
			}
			p.clock.advance(RefusalTTL - time.Millisecond)
			if _, err := e.Key(ctx, "acme", "org:acme", "prs_1"); !errors.Is(err, want) || p.count("acme") != 1 {
				t.Fatalf("within RefusalTTL: %v after %d requests", err, p.count("acme"))
			}
			// The operator raised the budget, or admitted the group.
			p.answers(nil)
			p.clock.advance(time.Millisecond)
			if key, err := e.Key(ctx, "acme", "org:acme", "prs_1"); err != nil || key.Reveal() != keyOf("acme", 1) || p.count("acme") != 2 {
				t.Fatalf("after RefusalTTL: %q, %v after %d requests", key.Reveal(), err, p.count("acme"))
			}
			if !strings.Contains(out.String(), "the key endpoint refused the group a key") || strings.Contains(out.String(), "sk-in-a-refusal") {
				t.Fatalf("the log of a refusal:\n%s", out.String())
			}
		})
	}
}

// TestAnAnswerThatIsNoKeyAndNoRefusalIsUnavailability: a wrong bearer, a
// failure, a rate limit, a body that is not the contract's, a key with no
// expiry, an expiry that is past or within the margin, a connection that is
// refused and a request that outlasts its bound each leave the group with
// no key and no refusal. The source says when it asks again, answers from
// memory until then, and never puts a key, the bearer or the answer's body
// in what it returns or logs.
func TestAnAnswerThatIsNoKeyAndNoRefusalIsUnavailability(t *testing.T) {
	for name, script := range map[string]script{
		"a wrong bearer":            status(http.StatusUnauthorized),
		"a failure":                 status(http.StatusInternalServerError),
		"a gateway that is down":    status(http.StatusBadGateway),
		"a rate limit":              status(http.StatusTooManyRequests),
		"another status":            status(http.StatusNotFound),
		"a redirect":                status(http.StatusNotModified),
		"a body that is not JSON":   body(`sk-leaked-in-a-body`),
		"a key that is no string":   body(`{"key": 7, "expires_at": "2026-10-04T13:00:00Z"}`),
		"an expiry that is no time": body(`{"key": "sk-leaked-in-a-body", "expires_at": "tomorrow"}`),
		"no key":                    body(`{"expires_at": "2026-10-04T13:00:00Z"}`),
		"an empty key":              body(`{"key": "", "expires_at": "2026-10-04T13:00:00Z"}`),
		"no expiry":                 body(`{"key": "sk-leaked-in-a-body"}`),
		"an expiry that is past":    body(`{"key": "sk-leaked-in-a-body", "expires_at": "2026-10-04T11:59:59Z"}`),
		"an expiry at the margin":   body(`{"key": "sk-leaked-in-a-body", "expires_at": "2026-10-04T12:01:00Z"}`),
		// The stub holds the request until the source gives it up.
		"a request that hangs": func(_ http.ResponseWriter, r *http.Request, _ string) bool {
			<-r.Context().Done()
			return true
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPlane(t)
			e, out := p.source()
			e.timeout = 50 * time.Millisecond
			p.answers(script)

			_, err := e.Key(ctx, "acme", "org:acme", "prs_1")
			u, ok := errors.AsType[*Unavailable](err)
			if !ok || errors.Is(err, ErrBudget) || errors.Is(err, ErrForbidden) {
				t.Fatalf("the answer is %v, want unavailability", err)
			}
			if u.RetryAfter != RetryMin || RetryAfterOf(err) != RetryMin {
				t.Fatalf("the source asks again in %v, want RetryMin", u.RetryAfter)
			}
			// Half a wait later a page is answered from memory, with what is
			// left of the wait.
			p.clock.advance(RetryMin / 2)
			_, err = e.Key(ctx, "acme", "org:acme", "prs_1")
			if RetryAfterOf(err) != RetryMin/2 || p.count("acme") != 1 {
				t.Fatalf("within the wait: %v, %d requests", err, p.count("acme"))
			}
			// The endpoint answers again: the key is asked and issued.
			p.answers(nil)
			p.clock.advance(RetryMin / 2)
			if key, err := e.Key(ctx, "acme", "org:acme", "prs_1"); err != nil || key.Reveal() != keyOf("acme", 1) || p.count("acme") != 2 {
				t.Fatalf("after the wait: %q, %v, %d requests", key.Reveal(), err, p.count("acme"))
			}
			for _, shown := range []string{u.Error(), fmt.Sprintf("%+v", u), fmt.Sprintf("%#v", u), out.String()} {
				for _, secret := range []string{bearer, "sk-leaked", "sk-issued"} {
					if strings.Contains(shown, secret) {
						t.Fatalf("what the source returned or logged holds %q: %s", secret, shown)
					}
				}
			}
			if !strings.Contains(out.String(), "the key endpoint issued no key") || !strings.Contains(out.String(), `"group":"acme"`) {
				t.Fatalf("the log does not name the group that waits:\n%s", out.String())
			}
		})
	}

	t.Run("an endpoint nothing listens at", func(t *testing.T) {
		p := newPlane(t)
		e, _ := p.source()
		p.Close()
		_, err := e.Key(ctx, "acme", "org:acme", "prs_1")
		if u, ok := errors.AsType[*Unavailable](err); !ok || u.Err == nil || u.RetryAfter != RetryMin || strings.Contains(err.Error(), bearer) {
			t.Fatalf("with the endpoint gone: %v", err)
		}
	})
	t.Run("an address no request can be built for", func(t *testing.T) {
		e := NewEndpoint(Options{URL: "http://plane.example/%zz", Token: reader.NewCredential(bearer)})
		_, err := e.Key(ctx, "acme", "org:acme", "prs_1")
		if u, ok := errors.AsType[*Unavailable](err); !ok || u.Err == nil || strings.Contains(err.Error(), bearer) {
			t.Fatalf("with an address that does not parse: %v", err)
		}
	})
}

// TestTheWaitGrowsWithEachAnswerInARowAndIsBounded: the wait before the
// endpoint is asked again doubles from RetryMin with each answer in a row
// that issued no key, stops at RetryMax, and starts over once a key was
// issued. A Retry-After the endpoint names is the wait, bounded the same.
func TestTheWaitGrowsWithEachAnswerInARowAndIsBounded(t *testing.T) {
	p := newPlane(t)
	e, _ := p.source()
	p.answers(status(http.StatusServiceUnavailable))
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, RetryMax, RetryMax} {
		_, err := e.Key(ctx, "acme", "org:acme", "prs_1")
		if RetryAfterOf(err) != want || p.count("acme") != i+1 {
			t.Fatalf("answer %d in a row: the wait is %v after %d requests, want %v", i+1, RetryAfterOf(err), p.count("acme"), want)
		}
		p.clock.advance(want)
	}
	// Another group's wait is its own.
	if _, err := e.Key(ctx, "globex", "org:globex", "prs_2"); RetryAfterOf(err) != RetryMin {
		t.Fatalf("the first wait of another group is %v", RetryAfterOf(err))
	}
	// A key ends the row.
	p.answers(nil)
	p.lasting(Margin + time.Second)
	if _, err := e.Key(ctx, "acme", "org:acme", "prs_1"); err != nil {
		t.Fatal(err)
	}
	p.clock.advance(time.Second)
	p.answers(status(http.StatusServiceUnavailable))
	if _, err := e.Key(ctx, "acme", "org:acme", "prs_1"); RetryAfterOf(err) != RetryMin {
		t.Fatalf("the first wait after a key is %v, want RetryMin", RetryAfterOf(err))
	}

	for header, want := range map[string]time.Duration{"7": 7 * time.Second, " 12 ": 12 * time.Second, "3600": RetryMax, "0": RetryMin, "-4": RetryMin, "soon": RetryMin, "": RetryMin} {
		p := newPlane(t)
		e, _ := p.source()
		p.answers(status(http.StatusTooManyRequests, "Retry-After", header))
		if _, err := e.Key(ctx, "acme", "org:acme", "prs_1"); RetryAfterOf(err) != want {
			t.Errorf("Retry-After %q: the wait is %v, want %v", header, RetryAfterOf(err), want)
		}
	}
}

// TestWhatIsHeldOfAGroupIsDroppedOnceItStandsNoLonger: the memory follows
// the groups that are being read. What is held of a group is dropped
// RetryMax after its answer stopped standing, and a group with a request in
// flight is kept.
func TestWhatIsHeldOfAGroupIsDroppedOnceItStandsNoLonger(t *testing.T) {
	p := newPlane(t)
	e, _ := p.source()
	p.lasting(Margin + 10*time.Second)
	held := func() string {
		e.mu.Lock()
		defer e.mu.Unlock()
		var groups []string
		for _, g := range []string{"acme", "globex", "initech", "umbrella"} {
			if e.groups[g] != nil {
				groups = append(groups, g)
			}
		}
		return strings.Join(groups, " ")
	}
	for _, group := range []string{"acme", "globex"} {
		if _, err := e.Key(ctx, group, "o", "prs_1"); err != nil {
			t.Fatal(err)
		}
	}
	// The keys stopped standing after 10 seconds. RetryMax later they are
	// still held, and a moment after that the next answer drops them.
	p.clock.advance(10*time.Second + RetryMax)
	if _, err := e.Key(ctx, "initech", "o", "prs_1"); err != nil || held() != "acme globex initech" {
		t.Fatalf("RetryMax after the keys stopped standing the source holds %q, %v", held(), err)
	}
	p.clock.advance(RetryMax)
	entered, release := make(chan struct{}), make(chan struct{})
	p.answers(func(_ http.ResponseWriter, _ *http.Request, group string) bool {
		if group == "initech" {
			close(entered)
			<-release
		}
		return false
	})
	done := make(chan error, 1)
	go func() {
		_, err := e.Key(ctx, "initech", "o", "prs_1")
		done <- err
	}()
	<-entered
	if _, err := e.Key(ctx, "umbrella", "o", "prs_1"); err != nil || held() != "initech umbrella" {
		t.Fatalf("after another RetryMax the source holds %q, %v", held(), err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestAnEndpointSourcePrintsNothingItHolds: the bearer and the keys in
// memory are members a formatter reaches, so the source prints as its kind.
func TestAnEndpointSourcePrintsNothingItHolds(t *testing.T) {
	p := newPlane(t)
	options := Options{URL: p.URL, Token: reader.NewCredential(bearer), HTTP: p.Client()}
	e := NewEndpoint(options)
	key, err := e.Key(ctx, "acme", "org:acme", "prs_1")
	if err != nil {
		t.Fatal(err)
	}
	var source Source = e
	for _, shown := range []string{
		fmt.Sprint(source), fmt.Sprintf("%+v", source), fmt.Sprintf("%#v", source), e.String(),
		fmt.Sprintf("%+v", options), fmt.Sprintf("%#v", options), fmt.Sprintf("%v %+v", key, key),
	} {
		if strings.Contains(shown, bearer) || strings.Contains(shown, "sk-issued") {
			t.Fatalf("the source prints what it holds: %s", shown)
		}
	}
	if raw, err := json.Marshal(map[string]any{"source": source, "key": key}); err != nil || strings.Contains(string(raw), bearer) || strings.Contains(string(raw), "sk-issued") {
		t.Fatalf("the source as JSON: %s, %v", raw, err)
	}
}

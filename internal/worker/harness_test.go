// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/objects"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// The suite is hermetic: the task store is a script. What a worker does
// against the real store, with processes that are killed, is proven by the
// tests of cmd/lectiod.

// scripted is a task store that hands out the claims a case queued and
// records what the worker says.
type scripted struct {
	mu sync.Mutex

	// registers counts the registrations, and refuse how many of the next
	// ones fail.
	registers int
	refuse    int

	// queue are the claims to hand out, in order, as the worker has room.
	queue []tasks.Claim

	// requests are the exchanges the worker made, and at when each arrived.
	requests []tasks.Request
	at       []time.Time

	// script, when set, edits the reply of the nth exchange, counted from
	// 1, or fails it.
	script func(n int, req tasks.Request, reply *tasks.Reply) error

	// rows are the task rows of each parse, and rowsErr what Tasks fails
	// with.
	rows    map[string][]postgres.Task
	rowsErr error

	// described are the figures of each parse a run took, and describedErr
	// what Figures fails with.
	described    map[string]postgres.Figures
	describedErr error
}

func (s *scripted) Register(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuse > 0 {
		s.refuse--
		return "", errors.New("the database does not answer")
	}
	s.registers++
	return "wrk_" + strconv.Itoa(s.registers), nil
}

func (s *scripted) Exchange(_ context.Context, _ string, req tasks.Request) (tasks.Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	s.at = append(s.at, time.Now())
	reply := tasks.Reply{}
	for free := req.Free; free > 0 && len(s.queue) > 0 && !req.Shutdown; free-- {
		next := s.queue[0]
		if next.Alone && (!req.Idle || len(reply.Claims) > 0) {
			break
		}
		reply.Claims, s.queue = append(reply.Claims, next), s.queue[1:]
		if next.Alone {
			break
		}
	}
	if s.script != nil {
		if err := s.script(len(s.requests), req, &reply); err != nil {
			return tasks.Reply{}, err
		}
	}
	return reply, nil
}

func (s *scripted) Tasks(_ context.Context, parseID string) ([]postgres.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[parseID], s.rowsErr
}

func (s *scripted) Figures(_ context.Context, parseID string) (postgres.Figures, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	got, started := s.described[parseID]
	return got, started, s.describedErr
}

// settles are the settles the worker sent so far, in order.
func (s *scripted) settles() []tasks.Settle {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []tasks.Settle
	for _, req := range s.requests {
		out = append(out, req.Settles...)
	}
	return out
}

// seen is a copy of the exchanges so far.
func (s *scripted) seen() []tasks.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tasks.Request(nil), s.requests...)
}

// eventually waits for a condition that the worker's goroutines bring
// about, and fails the case when it does not come.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("it did not happen: %s", what)
}

// gate is a reader whose calls wait until they are released or their
// context ends, so a case holds a task in flight for as long as it needs.
type gate struct {
	stub.Reader
	entered chan struct{}
	release chan struct{}
}

func newGate() *gate {
	return &gate{entered: make(chan struct{}, 64), release: make(chan struct{})}
}

func (g *gate) ReadPage(ctx context.Context, page reader.Page) (reader.Result, error) {
	g.entered <- struct{}{}
	select {
	case <-g.release:
		return g.Reader.ReadPage(ctx, page)
	case <-ctx.Done():
		return reader.Result{}, reader.FromTransport(ctx.Err())
	}
}

// issuing is a key source of a case: it issues a key that names what it was
// asked for, or fails with the error the case set.
type issuing struct {
	err error
	// asked is the group, the owner and the parse of the last call. Tasks
	// that run at once ask at once.
	mu    sync.Mutex
	asked string
}

func (i *issuing) Key(_ context.Context, group, owner, parseID string) (reader.Credential, error) {
	asked := group + "/" + owner + "/" + parseID
	i.mu.Lock()
	i.asked = asked
	i.mu.Unlock()
	if i.err != nil {
		return reader.Credential{}, i.err
	}
	return reader.NewCredential(asked), nil
}

// sheet is a PNG of one color with a dark bar across it, or of one color
// alone, which is a page with nothing on it.
func sheet(t *testing.T, blank bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 60, 40))
	for y := range 40 {
		for x := range 60 {
			c := color.RGBA{255, 255, 255, 255}
			if !blank && y > 13 && y < 26 {
				c = color.RGBA{0, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// drawn renders every frame of a TIFF as an image that is not blank, so a
// file of several pages is read without a PDF engine. Any other file goes to
// the renderer for images.
type drawn struct{ frame []byte }

func (d drawn) Render(ctx context.Context, data []byte, mediaType string, n int, want reader.Description) (render.Image, error) {
	if mediaType != "image/tiff" {
		return render.Images{}.Render(ctx, data, mediaType, n, want)
	}
	return render.Image{Data: d.frame, MediaType: "image/png", Width: 60, Height: 40}, nil
}

// bench is a worker over a scripted store and a memory object store, with
// times short enough that a case waits for nothing.
type bench struct {
	t       *testing.T
	store   *scripted
	objects *blob.Memory
	w       *Worker
}

func newBench(t *testing.T, readers map[string]reader.Reader) *bench {
	t.Helper()
	if readers == nil {
		readers = map[string]reader.Reader{"stub": &stub.Reader{}}
	}
	b := &bench{t: t, store: &scripted{rows: map[string][]postgres.Task{}}, objects: blob.NewMemory()}
	b.w = &Worker{
		Store: b.store, Objects: b.objects,
		Pipeline: &parse.Pipeline{Limits: pages.DefaultLimits(), Renderer: drawn{sheet(t, false)}},
		Readers:  readers, Costs: map[string]int{"stub": 3},
		Slots: 4, Lease: 2 * time.Second, Flush: 5 * time.Millisecond, Poll: 10 * time.Millisecond, Grace: 300 * time.Millisecond,
		CacheBytes: 1 << 20, Log: slog.New(slog.DiscardHandler),
	}
	b.w.init()
	return b
}

// put stores an object.
func (b *bench) put(key string, data []byte, contentType string) {
	b.t.Helper()
	if err := b.objects.Put(context.Background(), key, data, contentType); err != nil {
		b.t.Fatal(err)
	}
}

// page is a claim of a page task of a parse whose working copy is under
// work, a file of the media type.
func page(parseID string, n int, token int64, work, mediaType string) tasks.Claim {
	m, err := json.Marshal(objects.Manifest{
		MediaType: mediaType, PagesTotal: n, Source: "reader", Work: work, Token: 1,
	})
	if err != nil {
		panic(err)
	}
	return tasks.Claim{
		Parse: parseID, Task: tasks.PageID(n), Kind: tasks.Page, Token: token, Group: "acme", Reader: "stub",
		Context: tasks.Context{Owner: "alice", Manifest: m, Languages: []string{"de"}},
	}
}

// run starts the worker's loop and returns a stop that ends it and gives
// what Run returned.
func (b *bench) run() (stop func() error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.w.Run(ctx) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() { cancel(); err = <-done })
		return err
	}
	b.t.Cleanup(func() { _ = stop() })
	return stop
}

// counting is an object store that counts its reads by key, and holds each
// read until hold is closed when a case sets one.
type counting struct {
	blob.Store
	mu   sync.Mutex
	gets map[string]int
	hold chan struct{}
}

func (c *counting) Get(ctx context.Context, key string) ([]byte, string, error) {
	c.mu.Lock()
	c.gets[key]++
	c.mu.Unlock()
	if c.hold != nil {
		<-c.hold
	}
	return c.Store.Get(ctx, key)
}

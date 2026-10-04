// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lectio/internal/testservers"
)

// The soak and the throughput run of specs/004-durable-tasks.md. Each runs
// a fleet of lectiod processes for minutes, so neither is part of the
// suite: each runs when its variable is set.
//
//	LECTIO_SOAK=1 go test -run TestSoak -v -timeout 30m ./cmd/lectiod/
//	LECTIO_THROUGHPUT=1 go test -run TestThroughput -v -timeout 30m ./cmd/lectiod/
//
// LECTIO_SOAK_PARSES, LECTIO_THROUGHPUT_WORKERS and LECTIO_THROUGHPUT_PAGES
// scale a run down for a machine that cannot host the whole of it.

// number reads a count from the environment.
func number(name string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return fallback
}

// frames is a TIFF of n frames, each 40 by 40 pixels with stripes, so every
// frame is a page a reader is called for and rendering one costs nothing.
// It is the smallest TIFF there is: one strip per frame, 8 bits of gray, no
// compression.
func frames(n int) []byte {
	const side = 40
	strip := make([]byte, side*side)
	for i := range strip {
		strip[i] = byte(255 * (i / side % 2))
	}
	var out bytes.Buffer
	le := binary.LittleEndian
	put := func(v any) { _ = binary.Write(&out, le, v) }
	out.WriteString("II")
	put(uint16(42))
	put(uint32(8)) // the first directory follows the header
	for i := range n {
		const entries = 9
		directory := uint32(out.Len())
		data := directory + 2 + entries*12 + 4
		next := data + uint32(len(strip))
		if i == n-1 {
			next = 0
		}
		put(uint16(entries))
		for _, e := range [entries][2]uint32{
			{256, side}, {257, side}, {258, 8}, {259, 1}, {262, 1}, {273, data}, {277, 1}, {278, side}, {279, uint32(len(strip))},
		} {
			put(uint16(e[0])) // the tag
			put(uint16(4))    // a LONG
			put(uint32(1))    // one value
			put(e[1])
		}
		put(next)
		out.Write(strip)
	}
	return out.Bytes()
}

// pool writes a Reader document for the stub reader with a bound of calls
// in flight, and returns the file to name in LECTIO_CONFIG. With no
// document the stub's pool admits 8 calls across the fleet, which is what a
// run of many workers would then measure.
func pool(t *testing.T, inFlight int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reader.yaml")
	doc := "apiVersion: lectio.latere.ai/v1\nkind: Reader\nmetadata: { name: stub }\nspec: { adapter: stub, maxInFlight: " + strconv.Itoa(inFlight) + " }\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// parses submits n parses of a file, several at once, and returns their ids.
func (p *plane) parses(base, name string, data []byte, n int) []string {
	p.t.Helper()
	first := submitted(p.t, base, name, data)
	_, parse, _ := call(p.t, "GET", base+"/v1/parses/"+first, "dev", nil)
	body := []byte(`{"source":{"file":"` + parse["file"].(string) + `"},"reuse":false}`)
	ids := make([]string, n)
	ids[0] = first
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := 1 + w; i < n; i += 8 {
				status, got, raw := call(p.t, "POST", base+"/v1/parses", "dev", body)
				if status != 202 && status != 200 {
					p.t.Errorf("submit %d: %d %s", i, status, raw)
					return
				}
				ids[i] = got["id"].(string)
			}
		})
	}
	wg.Wait()
	return ids
}

// TestSoak: across 1,000 parses run by 8 worker processes that are killed
// at random, every parse ends succeeded with each of its pages settled
// once, no page result is missing from the bucket, and the counters of
// queued and running tasks equal a recount.
func TestSoak(t *testing.T) {
	if os.Getenv("LECTIO_SOAK") == "" {
		t.Skip("set LECTIO_SOAK=1 to run 1,000 parses under random kills")
	}
	const fleet, pagesEach = 8, 3
	total := number("LECTIO_SOAK_PARSES", 1000)
	p := newPlane(t)
	calls := filepath.Join(t.TempDir(), "calls")
	// A page whose worker is killed runs alone afterwards, and one that is
	// unlucky 3 times would be failed as the cause: the bound is raised so
	// that random kills fail no page.
	readers := pool(t, fleet*8)
	worker := func() *process {
		return p.spawn("worker", delayEnv, "250ms", callsEnv, calls, "LECTIO_WORKERS", "8", "LECTIO_TASK_EXPIRIES", "50", "LECTIO_CONFIG", readers)
	}
	api := p.spawn("api", "LECTIO_TASK_EXPIRIES", "50", "LECTIO_CONFIG", readers)
	workers := make([]*process, fleet)
	for i := range workers {
		workers[i] = worker()
	}
	conn := p.connect()
	began := time.Now()
	ids := p.parses(api.base, "scan.tiff", frames(pagesEach), total)
	if t.Failed() {
		t.FailNow()
	}

	open := `SELECT count(*) FROM parses WHERE state IN ('queued', 'running')`
	kills := 0
	for next := time.Now().Add(500 * time.Millisecond); value[int](t, conn, open) > 0; time.Sleep(50 * time.Millisecond) {
		if time.Since(began) > 20*time.Minute {
			t.Fatalf("%d parses have not ended after 20 minutes", value[int](t, conn, open))
		}
		if time.Now().Before(next) {
			continue
		}
		// One worker of the 8 dies without a word, and another takes its
		// place.
		i := rand.IntN(fleet)
		workers[i].signal(syscall.SIGKILL)
		<-workers[i].exited
		workers[i] = worker()
		kills++
		next = time.Now().Add(time.Duration(300+rand.IntN(600)) * time.Millisecond)
	}
	took := time.Since(began)

	if n := value[int](t, conn, `SELECT count(*) FROM parses WHERE state <> 'succeeded' OR pages_done <> $1 OR pages_total <> $1 OR pages_open <> 0 OR pages_failed <> 0`, pagesEach); n != 0 {
		t.Fatalf("%d of %d parses did not end with each of their %d pages settled once", n, total, pagesEach)
	}
	if n := value[int](t, conn, `SELECT (SELECT count(*) FROM tasks) + (`+drift+`) + (SELECT coalesce(sum(queued + running), 0) FROM group_service)::integer`); n != 0 {
		t.Fatalf("%d task rows, drifted counters or counted tasks are left", n)
	}
	// Every page of every parse is in the bucket under the key its index
	// names.
	missing := 0
	for _, id := range ids {
		key := value[string](t, conn, `SELECT index_key FROM parses WHERE parse_id = $1`, id)
		raw, ok := p.objects.Get(key)
		var index struct {
			Keys []struct {
				Key string `json:"key"`
			} `json:"keys"`
		}
		if !ok || json.Unmarshal(raw, &index) != nil || len(index.Keys) != pagesEach {
			missing++
			continue
		}
		for _, entry := range index.Keys {
			if _, ok := p.objects.Get(entry.Key); !ok {
				missing++
			}
		}
	}
	if missing != 0 {
		t.Fatalf("%d page results or indexes are missing from the bucket", missing)
	}
	read, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	made := strings.Count(string(read), "\n")
	if made < total*pagesEach {
		t.Fatalf("the reader was called %d times for %d pages", made, total*pagesEach)
	}
	t.Logf("soak: %d parses of %d pages by %d workers in %v, with %d workers killed; the reader was called %d times, %d of them for a page that was read again after a kill",
		total, pagesEach, fleet, took.Round(time.Second), kills, made, made-total*pagesEach)
}

// TestThroughput: 25 worker processes with 200 slots and a stub reader of
// one-second pages, through one backend connection behind PgBouncer in
// transaction mode, sustain 190 pages a second with at most 130 statements
// a second and no lease lost.
func TestThroughput(t *testing.T) {
	if os.Getenv("LECTIO_THROUGHPUT") == "" {
		t.Skip("set LECTIO_THROUGHPUT=1 to run a fleet of workers behind a pool of one")
	}
	fleet := number("LECTIO_THROUGHPUT_WORKERS", 25)
	const slots, pagesEach = 8, 50
	total := number("LECTIO_THROUGHPUT_PAGES", 12000)
	p := newPlane(t)
	pooler, err := testservers.StartPooler(1)
	if err != nil {
		t.Fatalf("the pooler of one connection: %v", err)
	}
	pooled := testservers.OnDatabase(t, pooler, testservers.NameOf(t, p.dsn))
	// The lease and the intervals are the specs' own defaults.
	settings := []string{
		"LECTIO_DATABASE_POOL_URL", pooled, "LECTIO_TASK_LEASE", "60s", "LECTIO_SWEEP_INTERVAL", "30s",
		"LECTIO_WORKER_FLUSH", "200ms", "LECTIO_WORKER_POLL", "1s", "LECTIO_WORKERS", strconv.Itoa(slots),
		"LECTIO_CONFIG", pool(t, fleet*slots),
	}
	api := p.spawn("api", settings...)
	workers := make([]*process, fleet)
	for i := range workers {
		workers[i] = p.spawn("worker", append([]string{delayEnv, "1s"}, settings...)...)
	}
	// The counters of the case's database are read over a connection to
	// another one, so reading them is not counted among them.
	stats, err := pgx.Connect(context.Background(), mustPostgres(t).Direct)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stats.Close(context.Background()) }()
	statements := func() int64 {
		return value[int64](t, stats, `SELECT xact_commit + xact_rollback FROM pg_stat_database WHERE datname = $1`, testservers.NameOf(t, p.dsn))
	}
	conn := p.connect()
	done := func() int { return value[int](t, conn, `SELECT coalesce(sum(pages_done), 0)::integer FROM parses`) }

	p.parses(api.base, "scan.tiff", frames(pagesEach), total/pagesEach)
	if t.Failed() {
		t.FailNow()
	}
	// The window opens once every slot has been filled and closes before
	// the queue runs dry: between a tenth and nine tenths of the pages.
	var from, to time.Time
	var pagesFrom, pagesTo, own int
	var statementsFrom, statementsTo int64
	for began := time.Now(); to.IsZero(); time.Sleep(time.Second) {
		if time.Since(began) > 20*time.Minute {
			t.Fatalf("%d of %d pages were read after 20 minutes", done(), total)
		}
		n := done()
		switch {
		case from.IsZero() && n >= total/10:
			from, pagesFrom, statementsFrom, own = time.Now(), n, statements(), 0
		case !from.IsZero() && n >= total*9/10:
			to, pagesTo, statementsTo = time.Now(), n, statements()
		case !from.IsZero():
			own++ // this loop's own statement in the case's database
		}
	}
	lost := value[int](t, conn, `SELECT count(*) FROM tasks WHERE expiries > 0`)
	for _, w := range workers {
		if _, gone := w.logged("the fleet gave the worker up: its lease ran out"); gone {
			lost++
		}
	}
	window := to.Sub(from).Seconds()
	pagesPerSecond := float64(pagesTo-pagesFrom) / window
	statementsPerSecond := float64(statementsTo-statementsFrom-int64(own)) / window
	t.Logf("throughput: %d workers with %d slots behind a pool of one: %.1f pages a second and %.1f statements a second over %.0f seconds, %d leases lost",
		fleet, fleet*slots, pagesPerSecond, statementsPerSecond, window, lost)
	if lost != 0 {
		t.Errorf("%d leases were lost", lost)
	}
	// The bounds are the spec's for 25 workers with 200 slots, and scale
	// with a smaller fleet.
	if want := 190.0 * float64(fleet) / 25; pagesPerSecond < want {
		t.Errorf("%.1f pages a second, want at least %.0f", pagesPerSecond, want)
	}
	if most := 130.0 * float64(fleet) / 25; statementsPerSecond > most {
		t.Errorf("%.1f statements a second, want at most %.0f", statementsPerSecond, most)
	}
}

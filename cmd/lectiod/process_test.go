// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lectio/internal/config"
	"latere.ai/x/lectio/internal/testservers"
	"latere.ai/x/lectio/reader"
)

// The process-level proofs of specs/004-durable-tasks.md. A claim about
// durability is a claim about what a process leaves behind when it stops,
// so each case here runs lectiod as real processes, over one Postgres and
// one bucket, and kills, suspends or terminates one. The process is this
// test binary run again: TestMain sees childEnv and runs lectiod's main.

// childEnv marks a process a test started as a lectiod: the test binary is
// run again with it set, and TestMain runs the server and not the tests.
const childEnv = "LECTIO_TEST_PROCESS"

// What a test tells the reader of a process it started.
const (
	// delayEnv makes every call take that long. The wait ends early when
	// the call is told to stop.
	delayEnv = "LECTIO_TEST_DELAY"
	// gateEnv names a directory. A call writes the file reached there and
	// then waits for as long as the file hold is there. It does not stop
	// when it is told to: it stands for a call that returns late.
	gateEnv = "LECTIO_TEST_GATE"
	// callsEnv names a file every call appends one line to.
	callsEnv = "LECTIO_TEST_CALLS"
)

// child runs lectiod's own main, which is configured by the environment the
// test gave the process, with the stub reader wrapped as the test asked.
func child() {
	wrapReaders = func(r config.Readers) config.Readers {
		for name, rd := range r.Readers {
			r.Readers[name] = &staged{Reader: rd, pid: os.Getpid()}
		}
		return r
	}
	main()
}

// staged is a reader of a process a test started: the configured reader,
// made to wait as the process's environment says, and to sign what it
// returns with the process's id, so two processes that read one page
// return two different results.
type staged struct {
	reader.Reader
	pid int
}

func (s *staged) ReadPage(ctx context.Context, page reader.Page) (reader.Result, error) {
	if path := os.Getenv(callsEnv); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return reader.Result{}, reader.Errorf(reader.Permanent, "the calls file: %v", err)
		}
		_, err = fmt.Fprintf(f, "%d %d\n", s.pid, page.Number)
		if err = errors.Join(err, f.Close()); err != nil {
			return reader.Result{}, reader.Errorf(reader.Permanent, "the calls file: %v", err)
		}
	}
	if dir := os.Getenv(gateEnv); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "reached"), nil, 0o600); err != nil {
			return reader.Result{}, reader.Errorf(reader.Permanent, "the gate: %v", err)
		}
		for exists(filepath.Join(dir, "hold")) {
			time.Sleep(5 * time.Millisecond)
		}
		// The call returns whatever became of its task meanwhile.
		ctx = context.WithoutCancel(ctx)
	}
	if d, err := time.ParseDuration(os.Getenv(delayEnv)); err == nil {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}
	res, err := s.Reader.ReadPage(ctx, page)
	if err == nil && len(res.Blocks) > 0 {
		res.Blocks[0].Text += " read by " + strconv.Itoa(s.pid)
	}
	return res, err
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// mustPostgres is the suite's Postgres, for a case that already has one.
func mustPostgres(t *testing.T) testservers.Postgres {
	t.Helper()
	srv, err := testservers.StartPostgres()
	if err != nil {
		t.Fatalf("the Postgres of the suite: %v", err)
	}
	return srv
}

// connect opens a direct connection to the plane's database, for what the
// API does not show: task rows, workers and counters.
func (p *plane) connect() *pgx.Conn {
	p.t.Helper()
	conn, err := pgx.Connect(context.Background(), p.dsn)
	if err != nil {
		p.t.Fatalf("connecting: %v", err)
	}
	p.t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// value reads one value from the plane's database.
func value[T any](t *testing.T, conn *pgx.Conn, sql string, args ...any) T {
	t.Helper()
	var out T
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&out); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return out
}

// until waits for a condition another process brings about.
func until(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("it did not happen within %v: %s", within, what)
}

// process is a lectiod process a test started.
type process struct {
	t    *testing.T
	cmd  *exec.Cmd
	logs *buffer
	// base is where the process serves the API, empty for a worker, and
	// internal where it serves its probes.
	base, internal string
	exited         chan struct{}
	err            error
}

// spawn starts lectiod as a process of its own, in a role, over the plane's
// database and bucket, and waits until it listens.
func (p *plane) spawn(role string, more ...string) *process {
	p.t.Helper()
	pairs := p.env(role, more...)
	// The process gets a temporary directory the test removes, so one that
	// is killed leaves nothing behind, and nothing else of the test's own
	// environment.
	scratch := p.t.TempDir()
	environ := []string{childEnv + "=1", "TMPDIR=" + scratch, "GOCOVERDIR=" + scratch, "HOME=" + scratch}
	for i := 0; i+1 < len(pairs); i += 2 {
		environ = append(environ, pairs[i]+"="+pairs[i+1])
	}
	proc := &process{t: p.t, logs: &buffer{}, exited: make(chan struct{})}
	proc.cmd = exec.Command(os.Args[0])
	proc.cmd.Env, proc.cmd.Stderr, proc.cmd.Stdout = environ, proc.logs, proc.logs
	if err := proc.cmd.Start(); err != nil {
		p.t.Fatalf("starting a lectiod process: %v", err)
	}
	go func() { proc.err = proc.cmd.Wait(); close(proc.exited) }()
	p.t.Cleanup(func() {
		// A process the case left suspended is resumed so that it can die.
		_ = proc.cmd.Process.Signal(syscall.SIGCONT)
		_ = proc.cmd.Process.Kill()
		<-proc.exited
	})

	until(p.t, "the process listens", 30*time.Second, func() bool {
		select {
		case <-proc.exited:
			p.t.Fatalf("the process ended before it listened: %v\n%s", proc.err, proc.logs.String())
		default:
		}
		entry, ok := proc.logged("listening")
		if !ok {
			return false
		}
		proc.internal = "http://" + entry["internal"].(string)
		if role != config.RoleWorker {
			proc.base = "http://" + entry["addr"].(string)
		}
		return true
	})
	return proc
}

// logged returns the last log entry of the process with a message.
func (p *process) logged(msg string) (map[string]any, bool) {
	var found map[string]any
	for line := range strings.SplitSeq(p.logs.String(), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == msg {
			found = entry
		}
	}
	return found, found != nil
}

// worker is the id the process last registered under. A process registers a
// moment after it listens, so the first call waits for it.
func (p *process) worker() string {
	p.t.Helper()
	var entry map[string]any
	until(p.t, "the process registered a worker", 30*time.Second, func() (ok bool) {
		entry, ok = p.logged("the worker is registered")
		return ok
	})
	return entry["worker"].(string)
}

// signal sends the process a signal.
func (p *process) signal(sig syscall.Signal) {
	p.t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		p.t.Fatalf("signaling the process: %v", err)
	}
}

// striped is a PNG that is not blank, so a reader is called for it.
func striped(t *testing.T) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 40, 40))
	for i := range img.Pix {
		img.Pix[i] = byte(255 * (i / 40 % 2))
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

// submitted uploads a file as the tests' one caller and submits a parse of
// it that takes no earlier read, and returns the parse's id without waiting
// for it.
func submitted(t *testing.T, base, name string, data []byte) string {
	t.Helper()
	return submittedAs(t, base, token(), name, data)
}

// submittedAs is submitted for the caller a bearer names.
func submittedAs(t *testing.T, base, bearer, name string, data []byte) string {
	t.Helper()
	status, file, raw := call(t, "POST", base+"/v1/files?name="+name, bearer, data, "Content-Type", "application/octet-stream")
	if status != 201 && status != 200 {
		t.Fatalf("upload: %d %s", status, raw)
	}
	status, parse, raw := call(t, "POST", base+"/v1/parses", bearer, []byte(`{"source":{"file":"`+file["id"].(string)+`"},"reuse":false}`))
	if status != 202 && status != 200 {
		t.Fatalf("submit: %d %s", status, raw)
	}
	return parse["id"].(string)
}

// drift counts the counters of queued and running tasks, of a group, a
// project or a lane, that differ from a recount of the task rows.
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
  SELECT s.group_id FROM lane_service s
   WHERE s.queued <> (SELECT count(*) FROM tasks t WHERE t.group_id = s.group_id AND t.project_id = s.project_id AND t.class = s.class AND t.lane = s.lane AND t.state = 'queued')
) d`

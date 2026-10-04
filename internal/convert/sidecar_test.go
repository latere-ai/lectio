// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package convert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/testfixtures"
)

// TestMain lets the test binary stand in for both programs a sidecar
// starts, so that the suite needs nothing on PATH: the sidecar's own
// program under LimitArg, and the office suite. Which one it is, it reads
// from its arguments, since the suite is started with no environment.
func TestMain(m *testing.M) {
	switch {
	case len(os.Args) > 1 && os.Args[1] == LimitArg:
		fmt.Fprintln(os.Stderr, Limit(os.Args[2:]))
		os.Exit(2)
	case len(os.Args) > 2 && os.Args[1] == "linger":
		linger(os.Args[2])
	case slices.Contains(os.Args, "--convert-to"):
		os.Exit(fakeSuite(os.Args))
	}
	os.Exit(m.Run())
}

// fakeSuite is the office suite as a test wants it. The file it is asked to
// convert is its script: lines of key=value that say how this conversion
// goes. It answers with the exit code it returns.
//
//	mode=        how it ends: fail, nothing, empty, restart, hang, slow, limit; anything else converts
//	record=PATH  a file to write its arguments, its environment and its profile's settings to
//	reply=TEXT   the conversion to write
//	copy=PATH    a file whose bytes are the conversion to write
//	size=N       the size of the conversion to write
//	socket=PATH  a file to leave outside the scratch directory, as the suite leaves its socket
func fakeSuite(args []string) int {
	input := args[len(args)-1]
	raw, err := os.ReadFile(input)
	if err != nil {
		return 70
	}
	script := map[string]string{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			script[key] = value
		}
	}
	var outdir, profile, ext string
	for i, arg := range args {
		switch {
		case arg == "--outdir":
			outdir = args[i+1]
		case arg == "--convert-to":
			ext, _, _ = strings.Cut(args[i+1], ":")
		case strings.HasPrefix(arg, "-env:UserInstallation=file://"):
			profile = strings.TrimPrefix(arg, "-env:UserInstallation=file://")
		}
	}
	output := filepath.Join(outdir, "input."+ext)

	if path := script["record"]; path != "" {
		registry, _ := os.ReadFile(filepath.Join(profile, "user", "registrymodifications.xcu"))
		cwd, _ := os.Getwd()
		record := strings.Join(args[1:], "\n") + "\n--\n" + strings.Join(os.Environ(), "\n") + "\n--\n" + cwd + "\n--\n" + string(registry)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return 71
		}
		if _, err := f.WriteString(record + "\n==\n"); err != nil {
			return 71
		}
		if err := f.Close(); err != nil {
			return 71
		}
	}

	if path := script["socket"]; path != "" {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return 77
		}
	}

	reply := []byte(script["reply"])
	switch script["mode"] {
	case "fail":
		return 3
	case "nothing":
		return 0
	case "empty":
		reply = nil
	case "restart":
		// The first start under a profile asks for a second.
		started := filepath.Join(profile, "started")
		if _, err := os.Stat(started); err != nil {
			if err := os.WriteFile(started, nil, 0o600); err != nil {
				return 72
			}
			return restart
		}
	case "hang":
		// A suite that starts another program and then never ends.
		child := exec.Command(os.Args[0], "linger", script["lock"])
		if err := child.Start(); err != nil {
			return 73
		}
		time.Sleep(time.Hour)
	case "slow":
		time.Sleep(300 * time.Millisecond)
	case "limit":
		var limit syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_AS, &limit); err != nil {
			return 74
		}
		reply = []byte(strconv.FormatUint(limit.Cur, 10))
	}
	if path := script["copy"]; path != "" {
		if reply, err = os.ReadFile(path); err != nil {
			return 75
		}
	}
	if n, err := strconv.Atoi(script["size"]); err == nil {
		reply = bytes.Repeat([]byte("x"), n)
	}
	if err := os.WriteFile(output, reply, 0o600); err != nil {
		return 76
	}
	return 0
}

// linger is a program the fake suite starts and leaves behind. It holds a
// lock on a file for as long as it lives, which is how a test knows whether
// it does: a lock is given up when its holder dies, reaped or not.
func linger(path string) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		os.Exit(80)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		os.Exit(80)
	}
	if err := os.WriteFile(path+".held", nil, 0o600); err != nil {
		os.Exit(80)
	}
	time.Sleep(time.Hour)
}

// logs is a log a test reads.
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

// sidecar is a sidecar whose suite is the fake, and its log.
func sidecar(t *testing.T) (*Sidecar, *logs) {
	t.Helper()
	log := &logs{}
	return &Sidecar{
		Suite: os.Args[0], Self: os.Args[0], Dir: t.TempDir(),
		Timeout: 30 * time.Second, MaxBytes: 1 << 20,
		Log: slog.New(slog.NewJSONHandler(log, nil)),
	}, log
}

// post sends a file to a sidecar and returns its answer.
func post(t *testing.T, s *Sidecar, from, to, script string) (int, string, http.Header) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(script))
	req.Header.Set("Content-Type", from)
	req.Header.Set("Accept", to)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), rec.Header()
}

// leftovers are the entries of the directory scratch directories are made in.
func leftovers(t *testing.T, s *Sidecar) []string {
	t.Helper()
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// The suite is started with the arguments that pin how the file is read and
// written, a profile of its own that turns macros and links off, an
// environment that holds nothing of the sidecar's, and a scratch directory
// that is gone when the conversion is.
func TestTheSuiteIsGivenNothingOfTheSidecar(t *testing.T) {
	t.Setenv("LECTIO_MODEL_KEY", "sk-never-inherited")
	s, log := sidecar(t)
	record := filepath.Join(t.TempDir(), "record")
	status, body, header := post(t, s, detect.MIMEPPTX+"; charset=binary", detect.MIMEPDF, "record="+record+"\nreply=%PDF-1.7 a deck with a secret word\n")
	if status != http.StatusOK || body != "%PDF-1.7 a deck with a secret word" || header.Get("Content-Type") != detect.MIMEPDF || header.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("answer = %d %q %v", status, body, header)
	}

	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimSuffix(string(raw), "\n==\n"), "\n--\n")
	if len(parts) != 4 {
		t.Fatalf("the record holds %d parts: %q", len(parts), raw)
	}
	args, env, cwd, registry := strings.Split(parts[0], "\n"), strings.Split(parts[1], "\n"), parts[2], parts[3]

	// The scratch directory is the one the suite was started in.
	scratch := cwd
	if resolved, err := filepath.EvalSymlinks(s.Dir); err != nil || filepath.Dir(scratch) != resolved {
		t.Fatalf("the suite ran in %s, outside %s (%v)", scratch, s.Dir, err)
	}
	scratch = filepath.Join(s.Dir, filepath.Base(scratch))
	wantArgs := []string{
		"--headless", "--invisible", "--nodefault", "--nofirststartwizard", "--nolockcheck", "--nologo", "--norestore",
		"-env:UserInstallation=file://" + scratch + "/profile",
		"--infilter=Impress MS PowerPoint 2007 XML",
		"--convert-to", "pdf:impress_pdf_Export",
		"--outdir", scratch + "/out",
		scratch + "/input.pptx",
	}
	if !slices.Equal(args, wantArgs) {
		t.Errorf("arguments:\n got %q\nwant %q", args, wantArgs)
	}
	if wantEnv := []string{"HOME=" + scratch + "/home", "TMPDIR=" + scratch + "/tmp"}; !slices.Equal(env, wantEnv) {
		t.Errorf("environment:\n got %q\nwant %q", env, wantEnv)
	}
	for _, setting := range []string{`oor:name="DisableMacrosExecution" oor:op="fuse"><value>true<`, `oor:name="BlockUntrustedRefererLinks" oor:op="fuse"><value>true<`, `oor:name="MacroSecurityLevel" oor:op="fuse"><value>3<`} {
		if !strings.Contains(registry, setting) {
			t.Errorf("the profile does not set %s:\n%s", setting, registry)
		}
	}
	if left := leftovers(t, s); len(left) != 0 {
		t.Errorf("the scratch directory was not removed: %q", left)
	}
	// The log says what was converted and how large, and nothing of it.
	if out := log.String(); !strings.Contains(out, `"msg":"converted"`) || !strings.Contains(out, `"from":"pptx"`) || !strings.Contains(out, `"bytes_out":34`) || strings.Contains(out, "secret") {
		t.Errorf("log:\n%s", out)
	}
}

// A conversion that passes its time limit is killed with everything the
// suite started, and leaves nothing in the scratch directory.
func TestAConversionPastItsTimeLimitIsKilledWithItsChildren(t *testing.T) {
	s, _ := sidecar(t)
	s.Timeout = 3 * time.Second
	lock := filepath.Join(t.TempDir(), "lock")
	// The suite leaves a socket outside its scratch directory when it is
	// killed. The sidecar takes it down, and leaves the socket of a suite
	// that was running before the conversion began.
	sockets := t.TempDir()
	socket := filepath.Join(sockets, "OSL_PIPE_65532_SingleOfficeIPC_0123456789abcdef")
	another := filepath.Join(sockets, "OSL_PIPE_501_SingleOfficeIPC_fedcba9876543210")
	if err := os.WriteFile(another, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before := strays
	strays = filepath.Join(sockets, "OSL_PIPE_*_SingleOfficeIPC_*")
	t.Cleanup(func() { strays = before })

	started := time.Now()
	status, body, _ := post(t, s, detect.MIMEPPT, detect.MIMEPDF, "mode=hang\nlock="+lock+"\nsocket="+socket+"\n")
	took := time.Since(started)
	var p problem
	if err := json.Unmarshal([]byte(body), &p); err != nil || status != http.StatusUnprocessableEntity || p.Error.Code != fault.DocumentCorrupt || !strings.Contains(p.Error.Detail, "within 3s") {
		t.Fatalf("answer = %d %q (%v)", status, body, err)
	}
	if took < s.Timeout || took > s.Timeout+5*time.Second {
		t.Fatalf("the conversion ended after %s, with a limit of %s", took, s.Timeout)
	}
	if _, err := os.Stat(lock + ".held"); err != nil {
		t.Fatalf("the suite's child never ran, so the test proves nothing: %v", err)
	}
	// The child held the lock while it lived. It is free once the child
	// is dead.
	f, err := os.OpenFile(lock, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("a program the suite started outlived the conversion: %v", err)
		}
	}
	if left := leftovers(t, s); len(left) != 0 {
		t.Fatalf("the scratch directory was not removed: %q", left)
	}
	if entries, err := os.ReadDir(sockets); err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(another) {
		t.Fatalf("the suite's socket is gone and no other: %v (%v)", entries, err)
	}
}

// The memory limit is set on the suite's own process before the suite
// starts, so it holds for the suite and for whatever the suite starts.
func TestTheSuiteRunsUnderTheMemoryLimit(t *testing.T) {
	s, _ := sidecar(t)
	// A limit no address space reaches, so that the fake, built with the
	// race detector, still starts under it.
	s.MemoryBytes = 1 << 62
	status, body, _ := post(t, s, detect.MIMERTF, detect.MIMEPDF, "mode=limit\n")
	if status != http.StatusOK || body != "4611686018427387904" {
		t.Fatalf("the suite saw the limit %q (%d), want %d", body, status, s.MemoryBytes)
	}
	// With no limit set, the suite is started as it is.
	s.MemoryBytes = 0
	if status, body, _ := post(t, s, detect.MIMERTF, detect.MIMEPDF, "mode=limit\n"); status != http.StatusOK || body == "4611686018427387904" {
		t.Fatalf("with no limit the suite saw %q (%d)", body, status)
	}

	for _, args := range [][]string{nil, {"1024"}, {"many", "/bin/true"}, {"4611686018427387904", "/nonexistent/lectio-suite"}} {
		if err := Limit(args); err == nil {
			t.Errorf("Limit(%q) returned no error", args)
		}
	}
	// A sidecar whose own program is not there cannot start its suite.
	s.MemoryBytes, s.Self = 1<<62, "/nonexistent/lectio-convert"
	if status, _, _ := post(t, s, detect.MIMERTF, detect.MIMEPDF, "reply=x\n"); status != http.StatusInternalServerError {
		t.Fatalf("a sidecar with no program of its own: %d", status)
	}
}

func TestHowAConversionEnds(t *testing.T) {
	big := "size=" + strconv.Itoa(2<<20) + "\n"
	for name, tc := range map[string]struct {
		from, to, script string
		status           int
		code             fault.Code
	}{
		"a first start that asks for a second": {detect.MIMEDOC, detect.MIMEDOCX, "mode=restart\nreply=PK converted\n", http.StatusOK, ""},
		"a suite that fails":                   {detect.MIMEODT, detect.MIMEPDF, "mode=fail\n", http.StatusUnprocessableEntity, fault.DocumentCorrupt},
		"a suite that writes nothing":          {detect.MIMEODP, detect.MIMEPDF, "mode=nothing\n", http.StatusUnprocessableEntity, fault.DocumentCorrupt},
		"a suite that writes an empty file":    {detect.MIMEKeynote, detect.MIMEPDF, "mode=empty\n", http.StatusUnprocessableEntity, fault.DocumentCorrupt},
		"a conversion over the limit":          {detect.MIMEPPTX, detect.MIMEPDF, big, http.StatusRequestEntityTooLarge, fault.FileTooLarge},
		"a file over the limit":                {detect.MIMEPPTX, detect.MIMEPDF, strings.Repeat("x", 1<<20+1), http.StatusRequestEntityTooLarge, fault.FileTooLarge},
		"a pair that is not converted":         {detect.MIMEDOCX, detect.MIMEPDF, "reply=x\n", http.StatusUnsupportedMediaType, fault.UnsupportedMediaType},
		"a type that is no type":               {"not a type", detect.MIMEPDF, "reply=x\n", http.StatusUnsupportedMediaType, fault.UnsupportedMediaType},
	} {
		s, _ := sidecar(t)
		status, body, _ := post(t, s, tc.from, tc.to, tc.script)
		var p problem
		if status != http.StatusOK {
			if err := json.Unmarshal([]byte(body), &p); err != nil || p.Error.Detail == "" {
				t.Errorf("%s: the answer is no problem: %q (%v)", name, body, err)
			}
		}
		if status != tc.status || p.Error.Code != tc.code {
			t.Errorf("%s: %d %q, want %d %q", name, status, p.Error.Code, tc.status, tc.code)
		}
		if left := leftovers(t, s); len(left) != 0 {
			t.Errorf("%s: the scratch directory was not removed: %q", name, left)
		}
	}

	// What is the sidecar's own failure is not the file's.
	missing, log := sidecar(t)
	missing.Suite = "/nonexistent/soffice"
	if status, body, _ := post(t, missing, detect.MIMERTF, detect.MIMEPDF, "reply=x\n"); status != http.StatusInternalServerError || !strings.Contains(body, `"code":"internal"`) {
		t.Fatalf("a suite that is not there: %d %q", status, body)
	}
	if !strings.Contains(log.String(), "/nonexistent/soffice") {
		t.Fatalf("the log does not say why:\n%s", log.String())
	}
	nowhere, _ := sidecar(t)
	nowhere.Dir = filepath.Join(nowhere.Dir, "absent")
	if status, _, _ := post(t, nowhere, detect.MIMERTF, detect.MIMEPDF, "reply=x\n"); status != http.StatusInternalServerError {
		t.Fatalf("a sidecar with no place for its scratch directory: %d", status)
	}

	// The route takes a POST and nothing else, and a sidecar built with no
	// log is silent.
	quiet := &Sidecar{Suite: os.Args[0], Dir: t.TempDir(), Timeout: 30 * time.Second, MaxBytes: 64}
	rec := httptest.NewRecorder()
	quiet.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("a GET: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	quiet.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("another path: %d", rec.Code)
	}
}

// One conversion runs at a time. A call that arrives meanwhile waits, and a
// call that stops waiting was never started.
func TestOneConversionRunsAtATime(t *testing.T) {
	s, _ := sidecar(t)
	record := filepath.Join(t.TempDir(), "record")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL, MaxBytes: 64})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	spans := make([][2]time.Time, 3)
	for i := range spans {
		wg.Go(func() {
			spans[i][0] = time.Now()
			if _, err := c.Convert(context.Background(), []byte("mode=slow\nreply=x\nrecord="+record+"\n"), detect.MIMERTF, detect.MIMEPDF); err != nil {
				t.Error(err)
			}
			spans[i][1] = time.Now()
		})
	}
	// A fourth call gives up while the others hold the slot. It is made
	// once the suite has written its record, which it does before a
	// conversion's 300ms begin, so the slot is held for longer than the
	// call waits.
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(record); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("no conversion started")
		}
	}
	impatient, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Convert(impatient, []byte("reply=x\nrecord="+record+"\n"), detect.MIMERTF, detect.MIMEPDF); fault.CodeOf(err) != fault.Internal {
		t.Errorf("a call that stopped waiting: %v", err)
	}
	wg.Wait()

	// 3 conversions of 300ms each, one after another, end 900ms or
	// more after the first began.
	first, last := spans[0][0], spans[0][1]
	for _, span := range spans {
		if span[0].Before(first) {
			first = span[0]
		}
		if span[1].After(last) {
			last = span[1]
		}
	}
	if took := last.Sub(first); took < 900*time.Millisecond {
		t.Fatalf("3 conversions of 300ms took %s together: they ran at once", took)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if runs := strings.Count(string(raw), "\n==\n"); runs != 3 {
		t.Fatalf("the suite was started %d times, and 3 calls were served", runs)
	}
}

// A call whose caller goes away mid-conversion ends the suite and leaves
// nothing behind.
func TestAnAbandonedConversionIsStopped(t *testing.T) {
	s, log := sidecar(t)
	lock := filepath.Join(t.TempDir(), "lock")
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader("mode=hang\nlock="+lock+"\n")).WithContext(ctx)
	req.Header.Set("Content-Type", detect.MIMEPPT)
	req.Header.Set("Accept", detect.MIMEPDF)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}()
	// Once the suite's child holds its lock, the conversion is under way.
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(lock + ".held"); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("the conversion never started")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the conversion went on after its caller was gone")
	}
	if left := leftovers(t, s); len(left) != 0 {
		t.Fatalf("the scratch directory was not removed: %q", left)
	}
	if !strings.Contains(log.String(), "abandoned") {
		t.Fatalf("log:\n%s", log.String())
	}

	// A call that is already gone when it would start takes no slot.
	gone, stop := context.WithCancel(context.Background())
	stop()
	handler := s.Handler()
	held := httptest.NewRequest(http.MethodPost, Path, strings.NewReader("mode=slow\nreply=x\n"))
	held.Header.Set("Content-Type", detect.MIMEPPT)
	held.Header.Set("Accept", detect.MIMEPDF)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler.ServeHTTP(httptest.NewRecorder(), held)
	}()
	time.Sleep(50 * time.Millisecond)
	late := httptest.NewRequest(http.MethodPost, Path, strings.NewReader("reply=x\n")).WithContext(gone)
	late.Header.Set("Content-Type", detect.MIMEPPT)
	late.Header.Set("Accept", detect.MIMEPDF)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, late)
	if rec.Body.Len() != 0 {
		t.Fatalf("a call that was gone was answered: %q", rec.Body.String())
	}
	<-finished
}

// TestThePipelineConvertsThroughTheSidecar is the whole path with the suite
// replaced: a legacy word-processing file goes from the pipeline to the
// client, to the sidecar, to the suite, and comes back as a document the
// pipeline reads from its own structure; a presentation comes back as a PDF
// whose pages are counted.
func TestThePipelineConvertsThroughTheSidecar(t *testing.T) {
	s, _ := sidecar(t)
	s.MaxBytes = 4 << 20
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	// The fake suite converts by its script, so each file here is a script
	// that names the fixture to answer with. It opens with the first bytes
	// of a legacy office file, which keep it from being taken for text.
	const legacy = "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1\n"
	fixture := func(name string) string {
		path := filepath.Join(t.TempDir(), "conversion")
		if err := os.WriteFile(path, testfixtures.Read(t, name), 0o600); err != nil {
			t.Fatal(err)
		}
		return legacy + "copy=" + path + "\n"
	}
	p := &parse.Pipeline{Limits: pages.DefaultLimits(), Renderer: render.Images{}, Converter: c}

	got, err := p.Prepare(context.Background(), []byte(fixture(testfixtures.ReportDOCX)), detect.DeclaredType{FileName: "letter.doc"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Manifest.MediaType != detect.MIMEDOCX || got.Manifest.Source != document.SourceNative || len(got.Native) != 1 || got.Native[0].Blocks[0].Text != "Field station report" {
		t.Fatalf("a converted word-processing file: %+v", got.Manifest)
	}
	got, err = p.Prepare(context.Background(), []byte(fixture(testfixtures.MultipagePDF)), detect.DeclaredType{FileName: "deck.ppt"}, "")
	if err != nil || got.Manifest.MediaType != detect.MIMEPDF || got.Manifest.Source != document.SourceReader || got.Manifest.PagesTotal != 3 {
		t.Fatalf("a converted presentation: %+v, %v", got.Manifest, err)
	}
	// What the suite could not convert fails the parse as the file's fault.
	if _, err := p.Prepare(context.Background(), []byte(legacy+"mode=fail\n"), detect.DeclaredType{FileName: "deck.ppt"}, ""); fault.CodeOf(err) != fault.DocumentCorrupt {
		t.Fatalf("a file the suite could not convert: %v", err)
	}
}

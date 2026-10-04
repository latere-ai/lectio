// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package convert

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/testfixtures"
)

// answering is a sidecar that answers every call the same way.
func answering(t *testing.T, answer http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(answer)
	t.Cleanup(srv.Close)
	return srv.URL
}

func client(t *testing.T, url string) *Client {
	t.Helper()
	c, err := New(Config{URL: url, MaxBytes: 64, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTheClientSendsTheFileAndReturnsTheConversion(t *testing.T) {
	url := answering(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || r.Method != http.MethodPost || r.URL.Path != "/base"+Path ||
			r.Header.Get("Content-Type") != detect.MIMEPPTX || r.Header.Get("Accept") != detect.MIMEPDF || string(body) != "a deck" {
			t.Errorf("the call: %s %s, %v, body %q (%v)", r.Method, r.URL.Path, r.Header, body, err)
		}
		w.Header().Set("Content-Type", "Application/PDF; charset=binary")
		_, _ = io.WriteString(w, "%PDF-1.7 converted")
	})
	// A path on the sidecar's address is kept, with or without its slash.
	for _, base := range []string{url + "/base", " " + url + "/base/ "} {
		out, err := client(t, base).Convert(context.Background(), []byte("a deck"), detect.MIMEPPTX, detect.MIMEPDF)
		if err != nil || string(out) != "%PDF-1.7 converted" {
			t.Fatalf("Convert = %q, %v", out, err)
		}
	}
}

func TestTheClientMapsWhatTheSidecarAnswers(t *testing.T) {
	// A sidecar never sends a file on. One that tries is not followed.
	var followed atomic.Int32
	elsewhere := answering(t, func(http.ResponseWriter, *http.Request) { followed.Add(1) })

	for name, tc := range map[string]struct {
		answer http.HandlerFunc
		code   fault.Code
		says   string
	}{
		"a pair it does not convert":  {problemOf(http.StatusUnsupportedMediaType, `{"error":{"code":"unsupported_media_type","detail":"the converter does not convert this type into the one asked for"}}`), fault.UnsupportedMediaType, "does not convert"},
		"a file over its limit":       {problemOf(http.StatusRequestEntityTooLarge, `{"error":{"code":"file_too_large","detail":"the file is over the limit of 16 bytes"}}`), fault.FileTooLarge, "16 bytes"},
		"a file it could not convert": {problemOf(http.StatusUnprocessableEntity, `{"error":{"code":"document_corrupt","detail":"the file was not converted within 2m0s"}}`), fault.DocumentCorrupt, "2m0s"},
		"a refusal that says nothing": {problemOf(http.StatusUnprocessableEntity, `not a problem`), fault.DocumentCorrupt, "refused the file"},
		"a refusal with no detail":    {problemOf(http.StatusUnprocessableEntity, `{"error":{"code":"document_corrupt"}}`), fault.DocumentCorrupt, "refused the file"},
		"a failure of its own":        {problemOf(http.StatusInternalServerError, `{"error":{"code":"internal","detail":"the converter could not run its suite"}}`), fault.Internal, "answered 500"},
		"a status the contract lacks": {problemOf(http.StatusTeapot, ``), fault.Internal, "answered 418"},
		"a redirect": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere, http.StatusTemporaryRedirect)
		}, fault.Internal, "answered 307"},
		"another type than asked for": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html>")
		}, fault.Internal, "another type"},
		"no conversion": {func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Content-Type", detect.MIMEPDF) }, fault.Internal, "no conversion"},
		"a conversion over the limit": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", detect.MIMEPDF)
			_, _ = w.Write(bytes.Repeat([]byte("x"), 65))
		}, fault.FileTooLarge, "limit of 64 bytes"},
		"an answer that breaks off": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", detect.MIMEPDF)
			w.Header().Set("Content-Length", "40")
			_, _ = io.WriteString(w, "%PDF-1.7 and then")
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close()
			}
		}, fault.Internal, "broke off"},
	} {
		_, err := client(t, answering(t, tc.answer)).Convert(context.Background(), []byte("a file"), detect.MIMEPPTX, detect.MIMEPDF)
		if fault.CodeOf(err) != tc.code || !strings.Contains(fault.DetailOf(err), tc.says) {
			t.Errorf("%s: %v; want %s saying %q", name, err, tc.code, tc.says)
		}
	}
	if followed.Load() != 0 {
		t.Fatal("the client followed a redirect, and sent the file where the sidecar pointed")
	}

	// A conversion at the limit is one within it.
	atLimit := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", detect.MIMEPDF)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 64))
	})
	if out, err := client(t, atLimit).Convert(context.Background(), nil, detect.MIMEPPTX, detect.MIMEPDF); err != nil || len(out) != 64 {
		t.Fatalf("a conversion at the limit: %d bytes, %v", len(out), err)
	}
}

func problemOf(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestTheClientDoesNotWaitPastItsTimeLimit(t *testing.T) {
	release := make(chan struct{})
	slow := answering(t, func(http.ResponseWriter, *http.Request) { <-release })
	// Registered after the server, so it runs before the server closes
	// and waits for its handlers.
	t.Cleanup(func() { close(release) })
	c, err := New(Config{URL: slow, MaxBytes: 64, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = c.Convert(context.Background(), []byte("a file"), detect.MIMEPPTX, detect.MIMEPDF)
	if fault.CodeOf(err) != fault.Internal || !strings.Contains(fault.DetailOf(err), "did not answer") {
		t.Fatalf("a sidecar that does not answer: %v", err)
	}
	if waited := time.Since(started); waited > 3*time.Second {
		t.Fatalf("the client waited %s past a limit of 100ms", waited)
	}

	// A sidecar that is not there is the same failure, and so is a call
	// whose context has ended.
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if _, err := client(t, gone.URL).Convert(context.Background(), nil, detect.MIMEPPTX, detect.MIMEPDF); fault.CodeOf(err) != fault.Internal {
		t.Fatalf("a sidecar that is not there: %v", err)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client(t, slow).Convert(ended, nil, detect.MIMEPPTX, detect.MIMEPDF); fault.CodeOf(err) != fault.Internal {
		t.Fatalf("a call under a context that ended: %v", err)
	}
}

func TestNewRefusesAnAddressItCannotReach(t *testing.T) {
	for _, url := range []string{"", "converter:8090", "ftp://converter", "http://", "unix:relative/sk-secret.sock", "unix://host/sk-secret.sock", "http://sk-secret\x7f"} {
		_, err := New(Config{URL: url, MaxBytes: 64})
		if err == nil {
			t.Errorf("New(%q) built a client", url)
		} else if strings.Contains(err.Error(), "sk-secret") {
			t.Errorf("the error repeats the address: %v", err)
		}
	}
	if _, err := New(Config{URL: "http://converter:8090"}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("a client with no bound on what comes back: %v", err)
	}
	c, err := New(Config{URL: "https://converter.example", MaxBytes: 64})
	if err != nil || c.timeout != DefaultTimeout || c.endpoint != "https://converter.example"+Path {
		t.Fatalf("defaults: %+v, %v", c, err)
	}
}

// socketDir is a directory short enough to hold a socket: a socket's path
// is bounded at about a hundred bytes, which the directory a test is given
// for temporary files can pass.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lc")
	if err == nil && len(dir) > 80 {
		if err = os.RemoveAll(dir); err == nil {
			dir, err = os.MkdirTemp("/tmp", "lc")
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

// A sidecar with no network is reached through a socket in a directory it
// shares with the pipeline, and the call is traced when the pipeline says
// how.
func TestTheClientReachesASidecarOnASocket(t *testing.T) {
	socket := filepath.Join(socketDir(t), "convert.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", r.Header.Get("Accept"))
		_, _ = io.WriteString(w, "over the socket: "+r.URL.Path)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	var traced atomic.Int32
	c, err := New(Config{URL: "unix://" + socket, MaxBytes: 64, Trace: func(base http.RoundTripper) http.RoundTripper {
		return roundTripper(func(r *http.Request) (*http.Response, error) {
			traced.Add(1)
			return base.RoundTrip(r)
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Convert(context.Background(), []byte("a file"), detect.MIMEDOC, detect.MIMEDOCX)
	if err != nil || string(out) != "over the socket: "+Path || traced.Load() != 1 {
		t.Fatalf("Convert = %q, %v, traced %d times", out, err, traced.Load())
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Every type the detector routes to conversion is one the sidecar
// converts, into the type the pipeline asks for.
func TestEveryConvertedTypeHasAConversion(t *testing.T) {
	for _, fixture := range converted {
		from, err := detect.Detect(testfixtures.Read(t, fixture), detect.DeclaredType{FileName: fixture})
		if err != nil {
			t.Fatal(err)
		}
		if to := target(t, from); !Converts(from, to) || !Converts(from+"; charset=binary", strings.ToUpper(to)) {
			t.Errorf("%s: %s is not converted to %s", fixture, from, to)
		}
	}
	if len(converted) != len(conversions) {
		t.Errorf("%d fixtures need conversion and the sidecar does %d conversions; keep the two sets equal", len(converted), len(conversions))
	}
	for _, pair := range [][2]string{{detect.MIMEDOCX, detect.MIMEPDF}, {detect.MIMEDOC, detect.MIMEPDF}, {detect.MIMEPDF, detect.MIMEDOCX}, {"", ""}, {"a, b", detect.MIMEPDF}} {
		if Converts(pair[0], pair[1]) {
			t.Errorf("%q to %q is converted, and the format table does not have it", pair[0], pair[1])
		}
	}
}

// converted are the fixtures of the types that need conversion.
var converted = []string{
	testfixtures.DOC, testfixtures.PPTX, testfixtures.PPT, testfixtures.RTF,
	testfixtures.ODT, testfixtures.ODP, testfixtures.Keynote,
}

// target is the media type the pipeline asks a converter for.
func target(t *testing.T, from string) string {
	t.Helper()
	switch class, _ := detect.ClassOf(from); class {
	case detect.ClassConvertDOCToDOCX:
		return detect.MIMEDOCX
	case detect.ClassConvertToPDF:
		return detect.MIMEPDF
	}
	t.Fatalf("%s is not routed to conversion", from)
	return ""
}

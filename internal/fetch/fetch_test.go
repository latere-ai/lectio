// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
)

// server is a test server that counts the requests that reach it.
func server(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// only permits connections to the given servers and no other address.
func only(servers ...*httptest.Server) func(netip.AddrPort) bool {
	return func(ap netip.AddrPort) bool {
		for _, s := range servers {
			if strings.HasSuffix(s.URL, ap.String()) {
				return true
			}
		}
		return false
	}
}

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.216.34":        true,
		"2606:2800:220:1::1":   true,
		"127.0.0.1":            false,
		"127.8.9.10":           false,
		"::1":                  false,
		"0.0.0.0":              false,
		"0.1.2.3":              false,
		"::":                   false,
		"10.1.2.3":             false,
		"172.16.0.1":           false,
		"192.168.1.1":          false,
		"169.254.169.254":      false, // the metadata address of most clouds
		"100.64.0.1":           false,
		"192.0.2.1":            false,
		"198.18.0.1":           false,
		"224.0.0.1":            false,
		"255.255.255.255":      false,
		"fe80::1":              false,
		"fc00::1":              false,
		"fd12:3456::1":         false,
		"ff02::1":              false,
		"::ffff:127.0.0.1":     false, // IPv4 loopback written as IPv6
		"::ffff:10.0.0.1":      false,
		"::ffff:93.184.216.34": true,
		"64:ff9b::7f00:1":      false, // loopback behind NAT64
		"2002:7f00:1::":        false, // loopback behind 6to4
		"2001:db8::1":          false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Public(%s) = %v, want %v", addr, got, want)
		}
	}
	if Public(netip.Addr{}) {
		t.Error("no address is not a public one")
	}
}

// TestFetchRefusesNonPublicAddresses is the proof behind SECURITY.md: a
// server that listens on a refused address is never connected to, by a URL
// that names it, by a name that resolves to it, or by a redirect.
func TestFetchRefusesNonPublicAddresses(t *testing.T) {
	private, hits := server(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("secret")) })
	port := private.URL[strings.LastIndex(private.URL, ":"):]

	f := &Fetcher{AllowHTTP: true}
	for _, target := range []string{
		private.URL,
		"http://localhost" + port,
		"http://[::1]" + port,
		"http://[::ffff:127.0.0.1]" + port,
		"http://0.0.0.0" + port,
		"http://10.0.0.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://192.168.0.1/",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := f.Fetch(ctx, target)
		cancel()
		if fault.CodeOf(err) != fault.SourceUnreachable {
			t.Errorf("%s: %v", target, err)
		}
		// What the caller is told names no address and nothing it held.
		if d := fault.DetailOf(err); strings.Contains(d, "127.0.0.1") || strings.Contains(d, "secret") {
			t.Errorf("%s: detail %q", target, d)
		}
	}

	// A permitted server that redirects to one that is not.
	public, publicHits := server(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, private.URL+"/file.pdf", http.StatusFound)
	})
	guarded := &Fetcher{AllowHTTP: true, Permit: only(public)}
	if _, err := guarded.Fetch(context.Background(), public.URL); fault.CodeOf(err) != fault.SourceUnreachable {
		t.Errorf("a redirect to a refused address: %v", err)
	}
	if publicHits.Load() != 1 {
		t.Errorf("the permitted server was asked %d times", publicHits.Load())
	}

	if hits.Load() != 0 {
		t.Fatalf("the refused server was reached %d times", hits.Load())
	}

	// A host the operator allowed is fetched wherever it resolves, named
	// alone or with its port.
	host, _ := url.Parse(private.URL)
	for _, entry := range []string{host.Hostname(), host.Host} {
		allowed := &Fetcher{AllowHTTP: true, Allow: []string{entry}}
		if got, err := allowed.Fetch(context.Background(), private.URL); err != nil || string(got.Data) != "secret" {
			t.Fatalf("allowing %s: %q, %v", entry, got.Data, err)
		}
	}

	// The allowance is the host's own: its redirect to another address is
	// checked like any connection.
	other, otherHits := server(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("other secret")) })
	redirecting, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	})
	hop, _ := url.Parse(redirecting.URL)
	narrow := &Fetcher{AllowHTTP: true, Allow: []string{hop.Host}}
	if _, err := narrow.Fetch(context.Background(), redirecting.URL); fault.CodeOf(err) != fault.SourceUnreachable || otherHits.Load() != 0 {
		t.Fatalf("an allowed host that redirects elsewhere: %v, %d hits", err, otherHits.Load())
	}
	if (&Fetcher{}).allows("no-port") || !(&Fetcher{Allow: []string{"store.internal"}}).allows("Store.Internal:9000") {
		t.Fatal("an entry with no port covers every port of its host, and nothing else")
	}
}

func TestFetch(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/reports/q3.pdf":
			w.Header().Set("Content-Type", "application/pdf; qs=1")
			_, _ = w.Write([]byte("%PDF-1.7"))
		case "/download":
			w.Header().Set("Content-Disposition", `attachment; filename="../named.png"`)
			_, _ = w.Write([]byte("png"))
		case "/moved":
			http.Redirect(w, r, "/reports/q3.pdf", http.StatusFound)
		case "/":
			_, _ = w.Write([]byte("root"))
		}
	})
	f := &Fetcher{AllowHTTP: true, Permit: only(srv), MaxBytes: 100}
	for target, want := range map[string]File{
		"/reports/q3.pdf?sig=abc": {Data: []byte("%PDF-1.7"), Name: "q3.pdf", MediaType: "application/pdf"},
		"/download":               {Data: []byte("png"), Name: "named.png", MediaType: "text/plain"},
		"/moved":                  {Data: []byte("%PDF-1.7"), Name: "q3.pdf", MediaType: "application/pdf"},
		"/":                       {Data: []byte("root"), MediaType: "text/plain"},
		"":                        {Data: []byte("root"), MediaType: "text/plain"},
	} {
		got, err := f.Fetch(context.Background(), srv.URL+target)
		if err != nil || string(got.Data) != string(want.Data) || got.Name != want.Name || got.MediaType != want.MediaType {
			t.Errorf("%s: %+v, %v", target, got, err)
		}
	}
}

func TestFetchFailures(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gone":
			http.NotFound(w, r)
		case "/large":
			_, _ = w.Write(make([]byte, 200))
		case "/stream":
			// No length is declared, so the limit is found while reading.
			w.(http.Flusher).Flush()
			_, _ = w.Write(make([]byte, 200))
		case "/cut":
			w.Header().Set("Content-Length", "50")
			_, _ = w.Write([]byte("short"))
		case "/slow":
			// Held until the client gives up and closes the connection.
			<-r.Context().Done()
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/to-ftp":
			http.Redirect(w, r, "ftp://example.com/file", http.StatusFound)
		}
	})
	f := &Fetcher{AllowHTTP: true, Permit: only(srv), MaxBytes: 100, Timeout: 500 * time.Millisecond}
	for target, want := range map[string]fault.Code{
		srv.URL + "/gone":              fault.SourceUnreachable,
		srv.URL + "/large":             fault.FileTooLarge,
		srv.URL + "/stream":            fault.FileTooLarge,
		srv.URL + "/cut":               fault.SourceUnreachable,
		srv.URL + "/slow":              fault.SourceUnreachable,
		srv.URL + "/loop":              fault.SourceUnreachable,
		srv.URL + "/to-ftp":            fault.SourceUnreachable,
		"ftp://example.com/file":       fault.InvalidRequest,
		"https://user:pw@example.com/": fault.InvalidRequest,
		"https:///nohost":              fault.InvalidRequest,
		"://":                          fault.InvalidRequest,
		"https://exa mple.com/":        fault.InvalidRequest,
	} {
		if _, err := f.Fetch(context.Background(), target); fault.CodeOf(err) != want {
			t.Errorf("%s: %v, want %s", target, err, want)
		}
	}

	// https only, unless http is allowed.
	if _, err := (&Fetcher{}).Fetch(context.Background(), srv.URL); fault.CodeOf(err) != fault.SourceUnreachable {
		t.Errorf("http without AllowHTTP: %v", err)
	}
	// A file with no limit set is read whole.
	if got, err := (&Fetcher{AllowHTTP: true, Permit: only(srv)}).Fetch(context.Background(), srv.URL+"/large"); err != nil || len(got.Data) != 200 {
		t.Errorf("no limit: %d bytes, %v", len(got.Data), err)
	}
}

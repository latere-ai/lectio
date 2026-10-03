// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package fetch gets a file from a URL a caller named. The caller chooses
// the URL, so the server would otherwise connect wherever it is told: to
// its own loopback, to a metadata endpoint, to a host on its private
// network. The guard is at the socket. Every connection the fetch opens,
// the first and the one after each redirect, is checked against the
// address it is about to connect to, after the name was resolved, so a
// name that resolves to a private address and a redirect to one are both
// refused, and resolving twice cannot get one past the check. The design
// is specs/014-sources-and-retention.md.
package fetch

import (
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/pkg/otel"
)

// maxRedirects is how many redirects one fetch follows.
const maxRedirects = 5

// notPublic are the ranges that are not publicly routable and that the
// standard library's address classes do not already name.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // this network
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space, carrier NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64: an IPv4 address in disguise
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4: an IPv4 address in disguise
}

// Public reports whether an address is publicly routable: not loopback,
// private, link-local, multicast, unspecified, or in a range set aside for
// something other than hosts on the internet. An IPv4 address written as
// IPv6 is judged as the IPv4 address it is.
func Public(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() {
		return false
	}
	return !slices.ContainsFunc(notPublic, func(p netip.Prefix) bool { return p.Contains(addr) })
}

// File is what a fetch returned.
type File struct {
	Data []byte

	// Name and MediaType are what the response said the file is. They are
	// hints for telling its type, as a caller's own would be.
	Name      string
	MediaType string
}

// Fetcher fetches files. The zero value fetches over https from public
// addresses only, with no bound on size.
type Fetcher struct {
	// MaxBytes bounds one file. Zero means no bound.
	MaxBytes int64

	// Timeout bounds one fetch, redirects included. Zero takes one minute.
	Timeout time.Duration

	// AllowHTTP permits http as well as https.
	AllowHTTP bool

	// Allow lists hosts, as written in a URL, that are fetched whatever
	// they resolve to: an operator's own object store on a private network.
	Allow []string

	// Permit decides whether a connection to an address may be opened. Nil
	// permits public addresses.
	Permit func(netip.AddrPort) bool

	once   sync.Once
	client *http.Client
}

// errRefused marks a connection the guard did not open.
var errRefused = errors.New("the address is not publicly routable")

type allowedKey struct{}

func (f *Fetcher) init() {
	f.once.Do(func() {
		dialer := &net.Dialer{
			Timeout: 10 * time.Second,
			// Control runs on the socket after the name was resolved and
			// before it connects: address is the one about to be dialed.
			Control: func(_, address string, _ syscall.RawConn) error {
				ap, err := netip.ParseAddrPort(address)
				if err != nil {
					return errRefused
				}
				if f.Permit != nil {
					if f.Permit(ap) {
						return nil
					}
					return errRefused
				}
				if !Public(ap.Addr()) {
					return errRefused
				}
				return nil
			},
		}
		open := &net.Dialer{Timeout: 10 * time.Second}
		transport := &http.Transport{
			// No proxy: behind one, the socket's address would be the
			// proxy's and the check would pass for any destination.
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if allowed, _ := ctx.Value(allowedKey{}).(bool); allowed {
					return open.DialContext(ctx, network, addr)
				}
				return dialer.DialContext(ctx, network, addr)
			},
			// One connection per fetch: a kept connection would carry the
			// next request without a check of its own.
			DisableKeepAlives:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}
		f.client = &http.Client{
			Transport: otel.Transport(transport),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > maxRedirects {
					return errors.New("too many redirects")
				}
				_, err := f.check(req.URL)
				return err
			},
		}
	})
}

// check validates a URL before a request is made to it, and reports
// whether its host is one the operator allowed.
func (f *Fetcher) check(u *url.URL) (allowed bool, err error) {
	switch {
	case u.Scheme == "https", u.Scheme == "http" && f.AllowHTTP:
	case u.Scheme == "http":
		return false, fault.New(fault.SourceUnreachable, "only https addresses are fetched")
	default:
		return false, fault.New(fault.InvalidRequest, "a source url is an https address")
	}
	if u.Hostname() == "" {
		return false, fault.New(fault.InvalidRequest, "the source url names no host")
	}
	if u.User != nil {
		return false, fault.New(fault.InvalidRequest, "a source url carries no user or password")
	}
	return slices.Contains(f.Allow, strings.ToLower(u.Hostname())), nil
}

// Fetch gets the file at rawURL. A URL that is not one to fetch is
// fault.InvalidRequest; a file over the limit is fault.FileTooLarge; and
// anything that keeps the file from arriving, a refused address among
// them, is fault.SourceUnreachable.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (File, error) {
	f.init()
	u, err := url.Parse(rawURL)
	if err != nil {
		return File{}, fault.New(fault.InvalidRequest, "the source url does not parse")
	}
	allowed, err := f.check(u)
	if err != nil {
		return File{}, err
	}

	timeout := f.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if allowed {
		// An allowed host is trusted with its redirects too.
		ctx = context.WithValue(ctx, allowedKey{}, true)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return File{}, fault.New(fault.InvalidRequest, "the source url does not parse")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		if code := fault.CodeOf(err); code == fault.SourceUnreachable || code == fault.InvalidRequest {
			// A redirect to a URL that is not one to fetch.
			return File{}, fault.New(fault.SourceUnreachable, "the address redirects to one that is not fetched")
		}
		if errors.Is(err, errRefused) {
			return File{}, fault.New(fault.SourceUnreachable, "%s", errRefused.Error())
		}
		// The transport's error names the address and may name a token in
		// its query, so it is wrapped and not repeated.
		return File{}, fault.Wrap(fault.SourceUnreachable, err, "the address could not be reached")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return File{}, fault.New(fault.SourceUnreachable, "the address answered %d", resp.StatusCode)
	}
	if f.MaxBytes > 0 && resp.ContentLength > f.MaxBytes {
		return File{}, fault.New(fault.FileTooLarge, "the file is %d bytes, over the limit of %d", resp.ContentLength, f.MaxBytes)
	}
	var body io.Reader = resp.Body
	if f.MaxBytes > 0 {
		body = io.LimitReader(resp.Body, f.MaxBytes+1)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return File{}, fault.Wrap(fault.SourceUnreachable, err, "the file stopped arriving")
	}
	if f.MaxBytes > 0 && int64(len(data)) > f.MaxBytes {
		return File{}, fault.New(fault.FileTooLarge, "the file is over the limit of %d bytes", f.MaxBytes)
	}

	out := File{Data: data, Name: path.Base(resp.Request.URL.Path)}
	if out.Name == "." || out.Name == "/" {
		out.Name = ""
	}
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		out.Name = path.Base(params["filename"])
	}
	if mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil {
		out.MediaType = mediaType
	}
	return out, nil
}

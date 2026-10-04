// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package convert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"latere.ai/x/lectio/internal/fault"
)

// DefaultTimeout bounds one call when a configuration sets none: the time
// a conversion is given by a sidecar with its own defaults, and as long
// again waiting for the conversion before it.
const DefaultTimeout = 5 * time.Minute

// maxProblem bounds how much of an answer that is not a conversion is read.
const maxProblem = 64 << 10

// Config is how a client reaches its sidecar.
type Config struct {
	// URL is where the sidecar listens: http://host:port, or
	// unix:///path/to/socket for a sidecar that has no network and listens
	// on a socket in a directory it shares with the pipeline.
	URL string

	// Timeout bounds one call, waiting included. Zero takes DefaultTimeout.
	Timeout time.Duration

	// MaxBytes is the largest conversion that is accepted.
	MaxBytes int64

	// Trace, when it is not nil, wraps the transport the calls go out on,
	// so that a call carries its caller's trace. The transport it is given
	// is nil for the default one.
	Trace func(http.RoundTripper) http.RoundTripper
}

// Client converts through a sidecar. It is a parse.Converter.
type Client struct {
	endpoint string
	http     *http.Client
	timeout  time.Duration
	maxBytes int64
}

// New builds a client. It refuses an address it cannot reach a sidecar by,
// and a configuration with no bound on what comes back. The error never
// repeats the address: a setting may hold a secret by mistake.
func New(cfg Config) (*Client, error) {
	if cfg.MaxBytes <= 0 {
		return nil, errors.New("convert: no limit on the size of a conversion is set")
	}
	c := &Client{timeout: cfg.Timeout, maxBytes: cfg.MaxBytes}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}

	var transport http.RoundTripper
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	switch {
	case err == nil && u.Scheme == "unix" && u.Host == "" && strings.HasPrefix(u.Path, "/"):
		socket := u.Path
		transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
		// The host names nothing: the socket is the address.
		c.endpoint = "http://converter" + Path
	case err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "":
		c.endpoint = u.Scheme + "://" + u.Host + strings.TrimSuffix(u.Path, "/") + Path
	default:
		return nil, errors.New("convert: the converter's address is neither an http URL nor unix:///path/to/socket")
	}
	if cfg.Trace != nil {
		transport = cfg.Trace(transport)
	}
	c.http = &http.Client{
		Transport: transport,
		// A sidecar answers the call or refuses it. It never sends the
		// file on to another address.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c, nil
}

// Convert returns data, of media type from, as media type to.
//
// A failure is one of four things. The sidecar refused the pair, the file
// is past a size limit, or the suite could not convert the file within its
// bounds: each comes back with the code the sidecar gave it, and is the
// file's. Anything else, a sidecar that cannot be reached, does not answer
// in time, or answers what the contract does not have, is internal: it is
// not the file's and not its caller's.
func (c *Client) Convert(ctx context.Context, data []byte, from, to string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "the call to the converter could not be built")
	}
	req.Header.Set("Content-Type", from)
	req.Header.Set("Accept", to)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "the converter did not answer")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, refusal(res)
	}
	if mediaType(res.Header.Get("Content-Type")) != mediaType(to) {
		return nil, fault.New(fault.Internal, "the converter answered with another type than it was asked for")
	}
	// One byte past the limit is read, so a conversion at the limit is
	// told from one over it.
	out, err := io.ReadAll(io.LimitReader(res.Body, c.maxBytes+1))
	switch {
	case err != nil:
		return nil, fault.Wrap(fault.Internal, err, "the converter's answer broke off")
	case int64(len(out)) > c.maxBytes:
		return nil, fault.New(fault.FileTooLarge, "the conversion is over the limit of %d bytes", c.maxBytes)
	case len(out) == 0:
		return nil, fault.New(fault.Internal, "the converter answered with no conversion")
	}
	return out, nil
}

// refusal reads an answer that is not a conversion. The status decides the
// code, and the detail is the sidecar's when it sent one.
func refusal(res *http.Response) error {
	code := fault.Internal
	switch res.StatusCode {
	case http.StatusUnsupportedMediaType:
		code = fault.UnsupportedMediaType
	case http.StatusRequestEntityTooLarge:
		code = fault.FileTooLarge
	case http.StatusUnprocessableEntity:
		code = fault.DocumentCorrupt
	}
	if code == fault.Internal {
		return fault.New(code, "the converter answered %d", res.StatusCode)
	}
	// An answer whose body breaks off or does not parse still has its
	// status, which is what decided the code.
	var p problem
	if raw, err := io.ReadAll(io.LimitReader(res.Body, maxProblem)); err != nil || json.Unmarshal(raw, &p) != nil || p.Error.Detail == "" {
		p.Error.Detail = "the converter refused the file"
	}
	return fault.New(code, "%s", p.Error.Detail)
}

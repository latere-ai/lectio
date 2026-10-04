// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package blob_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"latere.ai/x/pkg/s3"
	"latere.ai/x/pkg/s3/s3test"

	"latere.ai/x/lectio/internal/blob"
)

// What holds of the store over S3 alone: what it does when the server fails
// or refuses, what its settings must be, and where the configured prefix is.

// TestAnS3PutOutlastsAServerThatFailsTwice: a 503 is the server's failure
// and not an answer, so the client sends the request again, and the put
// succeeds on the third attempt with the object stored once.
func TestAnS3PutOutlastsAServerThatFailsTwice(t *testing.T) {
	store, fake := openFake(t, "")
	fake.Fail(2, http.StatusServiceUnavailable)
	put(t, store, "parses/prs_a/pages/1.1.json", []byte(`{"number":1}`), "application/json")
	if n := fake.Count(http.MethodPut); n != 3 {
		t.Fatalf("the put took %d requests, want 3", n)
	}
	if data, ok := fake.Get("parses/prs_a/pages/1.1.json"); !ok || string(data) != `{"number":1}` {
		t.Fatalf("after the retries the bucket holds %q, %v", data, ok)
	}
	if contentType, _ := fake.ContentType("parses/prs_a/pages/1.1.json"); contentType != "application/json" {
		t.Fatalf("after the retries the object's content type is %q", contentType)
	}
}

// TestAnS3FailureIsNotAnAbsentObject: a server that refuses every request,
// and one that fails every request, make every method return an error, and
// the error of Get is never ErrNotFound: a caller must not take a store that
// is down, or a key that may not read the bucket, for a page that is not
// there. A refusal is returned at once, and a failure after the client's
// attempts are spent.
func TestAnS3FailureIsNotAnAbsentObject(t *testing.T) {
	for _, c := range []struct {
		name     string
		status   int
		requests int
	}{
		{"refused", http.StatusForbidden, 1},
		{"failing", http.StatusInternalServerError, s3.DefaultRetry.MaxAttempts},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, fake := openFake(t, "")
			put(t, store, "parses/prs_a/pages/1.1.json", []byte("{}"), "application/json")
			before := len(fake.Requests())
			fake.Fail(1000, c.status)

			calls := map[string]func() error{
				"Put": func() error { return store.Put(t.Context(), "parses/prs_a/pages/2.1.json", []byte("{}"), "") },
				"Get": func() error {
					_, _, err := store.Get(t.Context(), "parses/prs_a/pages/1.1.json")
					return err
				},
				"Delete": func() error { return store.Delete(t.Context(), "parses/prs_a/pages/1.1.json") },
				"List": func() error {
					_, err := store.List(t.Context(), "")
					return err
				},
			}
			for name, call := range calls {
				err := call()
				if err == nil || errors.Is(err, blob.ErrNotFound) || !strings.HasPrefix(err.Error(), "blob: ") {
					t.Errorf("%s against a %s server = %v, want an error under the package's name that is not ErrNotFound", name, c.name, err)
				}
				if e, ok := errors.AsType[*s3.Error](err); !ok || e.Status != c.status {
					t.Errorf("%s does not carry the server's answer %d: %v", name, c.status, err)
				}
			}
			if got := len(fake.Requests()) - before; got != len(calls)*c.requests {
				t.Errorf("the %d calls took %d requests, want %d each", len(calls), got, c.requests)
			}

			// The server recovers and nothing a failed call asked for was done.
			fake.Fail(0, c.status)
			if keys := fake.Keys(); !slices.Equal(keys, []string{"parses/prs_a/pages/1.1.json"}) {
				t.Fatalf("after the failed calls the bucket holds %q", keys)
			}
		})
	}
}

// TestNewS3NamesTheSettingThatIsMissing: a store that cannot be opened says
// which environment variable to set, and never repeats a value: the secret
// key is a secret, and an endpoint may carry a credential.
func TestNewS3NamesTheSettingThatIsMissing(t *testing.T) {
	whole := blob.S3Config{
		Endpoint: "https://user:endpoint-password@s3.example.com", Region: "region-value", Bucket: "bucket-value",
		Prefix: "prefix-value", AccessKey: "access-key-value", SecretKey: "secret-key-value",
	}
	if _, err := blob.NewS3(whole); err != nil {
		t.Fatalf("a whole configuration was refused: %v", err)
	}
	values := []string{"endpoint-password", "s3.example.com", "region-value", "bucket-value", "prefix-value", "access-key-value", "secret-key-value", "not a url", "//twice"}
	for _, c := range []struct {
		variable string
		change   func(*blob.S3Config)
	}{
		{"LECTIO_S3_ENDPOINT", func(c *blob.S3Config) { c.Endpoint = "" }},
		{"LECTIO_S3_ENDPOINT", func(c *blob.S3Config) { c.Endpoint = "not a url" }},
		{"LECTIO_S3_ENDPOINT", func(c *blob.S3Config) { c.Endpoint = "s3.example.com" }},
		{"LECTIO_S3_ENDPOINT", func(c *blob.S3Config) { c.Endpoint = "https://user:endpoint-password@s3.example.com:port" }},
		{"LECTIO_S3_REGION", func(c *blob.S3Config) { c.Region = "" }},
		{"LECTIO_BUCKET", func(c *blob.S3Config) { c.Bucket = "" }},
		{"LECTIO_S3_ACCESS_KEY", func(c *blob.S3Config) { c.AccessKey = "" }},
		{"LECTIO_S3_SECRET_KEY", func(c *blob.S3Config) { c.SecretKey = "" }},
		{"LECTIO_BUCKET_PREFIX", func(c *blob.S3Config) { c.Prefix = "/prefix-value" }},
		{"LECTIO_BUCKET_PREFIX", func(c *blob.S3Config) { c.Prefix = "prefix-value//twice" }},
		{"LECTIO_BUCKET_PREFIX", func(c *blob.S3Config) { c.Prefix = "prefix-value/../bucket-value" }},
	} {
		cfg := whole
		c.change(&cfg)
		store, err := blob.NewS3(cfg)
		if err == nil || store != nil || !strings.Contains(err.Error(), c.variable) {
			t.Errorf("NewS3 with a bad %s = %v, %v, want an error that names the variable", c.variable, store, err)
			continue
		}
		for _, value := range values {
			if strings.Contains(err.Error(), value) {
				t.Errorf("the error about %s repeats the value %q: %v", c.variable, value, err)
			}
		}
	}
}

// TestAnS3PrefixIsOnTheStoredKeyAndOffTheListedOne: several installations
// share a bucket, each under its prefix. The prefix is on every key the
// store sends and on none it returns, and it ends in a slash whether or not
// it was configured with one, so the installation "one" lists nothing of
// the installation "one-more".
func TestAnS3PrefixIsOnTheStoredKeyAndOffTheListedOne(t *testing.T) {
	for _, prefix := range []string{"installations/one", "installations/one/"} {
		store, fake := openFake(t, prefix)
		fake.Put("installations/one-more/parses/prs_z/pages/1.1.json", []byte("another installation's"))
		fake.Put("parses/prs_a/pages/1.1.json", []byte("at the root of the bucket"))

		put(t, store, "parses/prs_a/pages/1.1.json", []byte(`{"number":1}`), "application/json")
		put(t, store, "parses/prs_a/document.1.json", []byte("{}"), "application/json")
		want := []string{
			"installations/one-more/parses/prs_z/pages/1.1.json",
			"installations/one/parses/prs_a/document.1.json",
			"installations/one/parses/prs_a/pages/1.1.json",
			"parses/prs_a/pages/1.1.json",
		}
		if keys := fake.Keys(); !slices.Equal(keys, want) {
			t.Fatalf("under the prefix %q the bucket holds %q, want %q", prefix, keys, want)
		}
		for listed, want := range map[string][]string{
			"":                    {"parses/prs_a/document.1.json", "parses/prs_a/pages/1.1.json"},
			"parses/":             {"parses/prs_a/document.1.json", "parses/prs_a/pages/1.1.json"},
			"parses/prs_a/pages/": {"parses/prs_a/pages/1.1.json"},
			"installations/":      nil,
		} {
			if keys := list(t, store, listed); !slices.Equal(keys, want) {
				t.Errorf("under the prefix %q List(%q) = %q, want %q", prefix, listed, keys, want)
			}
		}
		if data, _ := get(t, store, "parses/prs_a/pages/1.1.json"); string(data) != `{"number":1}` {
			t.Errorf("under the prefix %q the store read %q", prefix, data)
		}
		if err := store.Delete(t.Context(), "parses/prs_a/pages/1.1.json"); err != nil {
			t.Fatal(err)
		}
		if data, ok := fake.Get("parses/prs_a/pages/1.1.json"); !ok || string(data) != "at the root of the bucket" {
			t.Errorf("a delete under the prefix %q reached the key at the root of the bucket: %q, %v", prefix, data, ok)
		}
	}
}

// TestAnS3BucketIsAddressedAsConfigured: with path style the bucket is the
// first segment of the request's path, and without it the bucket is a label
// of the host. With no client of the caller's, the store sends through the
// S3 client's own.
func TestAnS3BucketIsAddressedAsConfigured(t *testing.T) {
	fake := s3test.New(t, "bucket")
	cfg := blob.S3Config{
		Endpoint: fake.URL(), Region: s3test.Region, Bucket: "bucket",
		AccessKey: s3test.Key, SecretKey: s3test.Secret, PathStyle: true,
	}
	inPath, err := blob.NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	put(t, inPath, "parses/prs_a/pages/1.1.json", []byte("{}"), "application/json")

	// The host label makes a name no resolver knows, so the client that
	// sends for this store connects to the endpoint whatever the name is.
	address := strings.TrimPrefix(fake.URL(), "http://")
	cfg.PathStyle = false
	cfg.HTTPClient = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}}
	t.Cleanup(cfg.HTTPClient.CloseIdleConnections)
	inHost, err := blob.NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	put(t, inHost, "parses/prs_a/pages/2.1.json", []byte("{}"), "application/json")

	requests := fake.Requests()
	if len(requests) != 2 || requests[0].Path != "/bucket/parses/prs_a/pages/1.1.json" || requests[1].Path != "/parses/prs_a/pages/2.1.json" {
		t.Fatalf("the two puts were sent as %+v", requests)
	}
	if keys := list(t, inHost, ""); !slices.Equal(keys, []string{"parses/prs_a/pages/1.1.json", "parses/prs_a/pages/2.1.json"}) {
		t.Fatalf("the two stores wrote %q into one bucket", keys)
	}
}

// answering starts a server that answers every request with one handler and
// opens the store on it. It stands in for a provider that answers what the
// endpoint in this process never does.
func answering(t *testing.T, handler http.HandlerFunc) *blob.S3 {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	store, err := blob.NewS3(blob.S3Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "bucket",
		AccessKey: "AKIDEXAMPLE", SecretKey: "secret", PathStyle: true, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestAnS3ListingThatCannotContinueIsAnError: a page that says more keys
// follow and holds none gives nothing to continue after. The listing fails:
// the keys read so far are a part, and a part must not pass for the whole.
func TestAnS3ListingThatCannotContinueIsAnError(t *testing.T) {
	var requests atomic.Int32
	store := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		if _, err := w.Write([]byte(`<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>`)); err != nil {
			t.Errorf("answering the listing: %v", err)
		}
	})
	keys, err := store.List(t.Context(), "")
	if err == nil || keys != nil || errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a listing that cannot continue returned %q, %v", keys, err)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("the listing asked %d times for a page it cannot continue after", n)
	}
}

// TestAnS3ListingIsInByteOrderWhateverTheServerSends: the store sorts what
// it lists, so its order does not rest on the provider's.
func TestAnS3ListingIsInByteOrderWhateverTheServerSends(t *testing.T) {
	store := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if _, err := w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated>` +
			`<Contents><Key>parses/b</Key></Contents><Contents><Key>parses/a/x</Key></Contents><Contents><Key>parses/a.x</Key></Contents>` +
			`</ListBucketResult>`)); err != nil {
			t.Errorf("answering the listing: %v", err)
		}
	})
	if keys := list(t, store, "parses/"); !slices.Equal(keys, []string{"parses/a.x", "parses/a/x", "parses/b"}) {
		t.Fatalf("the listing is %q", keys)
	}
}

// TestAnS3BodyThatEndsEarlyIsAnError: a connection that closes before the
// whole object arrived gives no object. The store returns an error and never
// the part it read.
func TestAnS3BodyThatEndsEarlyIsAnError(t *testing.T) {
	store := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000")
		// The handler returns after a part of what it promised, and the
		// server closes the connection.
		if _, err := w.Write([]byte(`{"number":`)); err != nil {
			t.Errorf("answering the get: %v", err)
		}
	})
	data, contentType, err := store.Get(t.Context(), "parses/prs_a/pages/1.1.json")
	if err == nil || data != nil || contentType != "" || errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a body that ended early read as %q, %q, %v", data, contentType, err)
	}
}

// TestAnS3ObjectWithNoContentTypeIsAnOctetStream: a provider that sends no
// content type for an object leaves the store's own default.
func TestAnS3ObjectWithNoContentTypeIsAnOctetStream(t *testing.T) {
	store := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		// A nil value keeps the server from guessing a type from the body.
		w.Header()["Content-Type"] = nil
		if _, err := w.Write([]byte("bytes")); err != nil {
			t.Errorf("answering the get: %v", err)
		}
	})
	if data, contentType := get(t, store, "parses/prs_a/work/source.1.bin"); string(data) != "bytes" || contentType != "application/octet-stream" {
		t.Fatalf("an object sent with no content type read as %q, %q", data, contentType)
	}
}

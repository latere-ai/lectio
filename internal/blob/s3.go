// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package blob

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"latere.ai/x/pkg/s3"
)

// S3Config names a bucket of an S3 compatible provider and the credential
// that may use it. Each field is set from the environment variable beside it.
type S3Config struct {
	Endpoint string // LECTIO_S3_ENDPOINT
	Region   string // LECTIO_S3_REGION
	Bucket   string // LECTIO_BUCKET

	// Prefix is prepended to every key, so several installations share a
	// bucket. A trailing slash is added to a prefix that has none, so one
	// installation's prefix never matches the keys of another whose prefix
	// it starts. The prefix counts against the provider's bound on the
	// length of a key.
	Prefix string // LECTIO_BUCKET_PREFIX

	AccessKey string // LECTIO_S3_ACCESS_KEY
	SecretKey string // LECTIO_S3_SECRET_KEY

	// PathStyle addresses the bucket as the first segment of the path and
	// not as a label of the host. A server reached by an address or under
	// one name needs it.
	PathStyle bool // LECTIO_S3_PATH_STYLE

	// HTTPClient sends the requests when it is not nil; a caller passes its
	// instrumented client.
	HTTPClient *http.Client
}

// S3 is a Store over one bucket. Its requests are signed, a failure of the
// server or of the transport is tried again under the client's retry policy,
// and a refusal is returned at once.
type S3 struct {
	client *s3.Client
	prefix string
}

// NewS3 checks the settings and returns the store. It sends nothing, so a
// bucket that does not exist or a key that may not use it shows on the first
// call. An error names the variable of the setting it is about and never the
// value: a value may be a secret, and an endpoint may carry one.
func NewS3(cfg S3Config) (*S3, error) {
	for _, setting := range []struct{ name, value string }{
		{"LECTIO_S3_ENDPOINT", cfg.Endpoint},
		{"LECTIO_S3_REGION", cfg.Region},
		{"LECTIO_BUCKET", cfg.Bucket},
		{"LECTIO_S3_ACCESS_KEY", cfg.AccessKey},
		{"LECTIO_S3_SECRET_KEY", cfg.SecretKey},
	} {
		if setting.value == "" {
			return nil, fmt.Errorf("blob: %s is not set", setting.name)
		}
	}
	if u, err := url.Parse(cfg.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("blob: LECTIO_S3_ENDPOINT is not an absolute URL")
	}
	prefix := cfg.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if validPrefix(prefix) != nil {
		return nil, errors.New("blob: LECTIO_BUCKET_PREFIX is not a path of segments separated by slashes")
	}
	var opts []s3.Option
	if cfg.PathStyle {
		opts = append(opts, s3.WithPathStyle())
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, s3.WithHTTPClient(cfg.HTTPClient))
	}
	client, err := s3.New(cfg.Endpoint, cfg.Region, cfg.Bucket, cfg.AccessKey, cfg.SecretKey, opts...)
	if err != nil {
		// The client's own sentence quotes the endpoint, so it is not passed on.
		return nil, errors.New("blob: the S3 client refused the settings")
	}
	return &S3{client: client, prefix: prefix}, nil
}

// Put sends the bytes with their digests, so a body that was damaged on the
// way is refused by the server and does not land.
func (s *S3) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	body := s3.BytesBody(data)
	// The type is always sent: a provider's own default for an object put
	// with none differs from one provider to the next.
	body.ContentType = cmp.Or(contentType, DefaultContentType)
	if _, err := s.client.PutObject(ctx, s.prefix+key, body); err != nil {
		return fmt.Errorf("blob: putting an object: %w", err)
	}
	return nil
}

// Get reads the object whole.
func (s *S3) Get(ctx context.Context, key string) ([]byte, string, error) {
	if err := ValidKey(key); err != nil {
		return nil, "", err
	}
	rc, head, err := s.client.GetObject(ctx, s.prefix+key, "")
	switch {
	case errors.Is(err, s3.ErrNotFound):
		return nil, "", ErrNotFound
	case err != nil:
		return nil, "", fmt.Errorf("blob: getting an object: %w", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", fmt.Errorf("blob: reading an object: %w", err)
	}
	return data, cmp.Or(head.ContentType, DefaultContentType), nil
}

// Delete removes the object. The provider answers the same for a key that
// held none.
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	if err := s.client.DeleteObject(ctx, s.prefix+key); err != nil {
		return fmt.Errorf("blob: deleting an object: %w", err)
	}
	return nil
}

// List reads the listing page by page, each page starting after the last key
// of the one before, and returns the keys without the configured prefix.
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	if err := validPrefix(prefix); err != nil {
		return nil, err
	}
	var keys []string
	opts := s3.ListOptions{Prefix: s.prefix + prefix}
	for {
		page, err := s.client.ListObjects(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("blob: listing objects: %w", err)
		}
		for _, o := range page.Objects {
			keys = append(keys, strings.TrimPrefix(o.Key, s.prefix))
		}
		if !page.Truncated {
			break
		}
		if len(page.Objects) == 0 {
			// More keys follow and none came to continue after: the listing
			// cannot be completed, and a part of it must not pass for all.
			return nil, errors.New("blob: listing objects: the store cut a page short and sent no key to continue after")
		}
		opts.StartAfter = page.Objects[len(page.Objects)-1].Key
	}
	// A provider lists in byte order already. The sort holds the order for
	// one that does not.
	slices.Sort(keys)
	return keys, nil
}

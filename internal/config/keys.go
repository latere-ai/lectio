// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"strings"

	"latere.ai/x/lectio/reader"
)

// The key sources: where the key a page is read with comes from.
const (
	// KeysStatic is one key from LECTIO_MODEL_KEY for every group.
	KeysStatic = "static"
	// KeysEndpoint is a key an operator's endpoint issues for each group.
	KeysEndpoint = "endpoint"
)

// readKeys reads the key source of specs/013-limits-and-usage.md. It runs
// after the role is read, because the 3 variables are not every role's.
//
// LECTIO_KEYS is read by every role: it decides whether a rate limit pauses
// a reader for all groups or for one, which is a setting of the task store,
// and every process that opens the store writes its settings.
//
// LECTIO_KEYS_URL and LECTIO_KEYS_TOKEN are read only by a process that
// runs tasks. The API faces callers, so it holds no credential that obtains
// a tenant's key: in the role api neither variable is read, and neither
// being absent is an error there. The same holds for the check that
// LECTIO_MODEL_KEY is not set beside the endpoint, since the API reads no
// page with either.
func (s *Settings) readKeys(getenv func(string) string) []error {
	s.Keys = KeysStatic
	switch strings.TrimSpace(getenv("LECTIO_KEYS")) {
	case "", KeysStatic:
	case KeysEndpoint:
		s.Keys = KeysEndpoint
	default:
		return []error{errors.New("LECTIO_KEYS is not static or endpoint")}
	}
	if s.Keys == KeysEndpoint && s.Dev {
		// The in-process runner reads every page with one key and has no
		// scope to pause, so it cannot hold a key per group.
		return []error{errors.New("LECTIO_KEYS is endpoint while LECTIO_DEV is true: a development server reads every page with the key of LECTIO_MODEL_KEY")}
	}
	if s.Role == RoleAPI && !s.Dev {
		return nil
	}

	url, token := strings.TrimSpace(getenv("LECTIO_KEYS_URL")), strings.TrimSpace(getenv("LECTIO_KEYS_TOKEN"))
	if s.Keys == KeysStatic {
		// An address or a bearer with no source that asks is an
		// installation that believes its tenants read with their own keys
		// while every page is read with the operator's.
		if url != "" || token != "" {
			return []error{errors.New("LECTIO_KEYS_URL or LECTIO_KEYS_TOKEN is set while LECTIO_KEYS is static: the endpoint is asked only with LECTIO_KEYS=endpoint")}
		}
		return nil
	}

	var errs []error
	switch {
	case url == "":
		errs = append(errs, errors.New("LECTIO_KEYS_URL is not set while LECTIO_KEYS is endpoint: a group's key is asked there"))
	case !absolute(url):
		errs = append(errs, errors.New("LECTIO_KEYS_URL is not an absolute http or https URL"))
	}
	if token == "" {
		errs = append(errs, errors.New("LECTIO_KEYS_TOKEN is not set while LECTIO_KEYS is endpoint: the key endpoint requires a bearer"))
	}
	if !s.ModelKey.IsZero() {
		errs = append(errs, errors.New("LECTIO_MODEL_KEY is set while LECTIO_KEYS is endpoint: a page is read with a key from one source, and these are 2"))
	}
	s.KeysURL, s.KeysToken = url, reader.NewCredential(token)
	return errs
}

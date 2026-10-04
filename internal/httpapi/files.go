// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"time"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/id"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/store"
)

// maxName is the longest file name that is kept.
const maxName = 255

// fileView is a file as the API returns it.
type fileView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	MediaType string    `json:"media_type"`
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt is when the file may be removed, absent for one that is
	// kept.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func viewFile(f store.File) fileView {
	return fileView{
		ID: f.ID, Name: f.Name, Size: f.Size, SHA256: f.SHA256, MediaType: f.MediaType,
		CreatedAt: f.CreatedAt, ExpiresAt: f.ExpiresAt,
	}
}

// createFile stores an upload: the body itself, or the part named "file"
// of a multipart form. The question is asked once the request says what is
// being uploaded and before any of it is read, so the allow's limit holds
// the upload itself.
func (s *Server) createFile(w http.ResponseWriter, r *http.Request, c call) error {
	name, declared := r.URL.Query().Get("name"), r.Header.Get("Content-Type")
	var body io.Reader = r.Body
	// A raw body's length is the file's. A form's length is the form's, so
	// an upload in a form declares no size.
	size := max(r.ContentLength, 0)

	if mediaType, _, _ := mime.ParseMediaType(declared); mediaType == "multipart/form-data" {
		size = 0
		form, err := r.MultipartReader()
		if err != nil {
			return invalid("file", "the multipart body does not parse")
		}
		for {
			part, err := form.NextPart()
			if err != nil {
				return invalid("file", "the multipart body has no part named file")
			}
			if part.FormName() != "file" {
				continue
			}
			if name == "" {
				name = part.FileName()
			}
			body, declared = part, part.Header.Get("Content-Type")
			break
		}
	}
	if name = path.Base(name); name == "." || name == "/" {
		name = ""
	}
	if len(name) > maxName {
		return invalid("name", "a file name is at most %d bytes", maxName)
	}
	if mediaType, _, err := mime.ParseMediaType(declared); err == nil {
		declared = mediaType
	}

	d, err := s.allowed(r, c, access.File{Size: size, MediaType: declared}.Resource())
	if err != nil {
		return err
	}
	limit := s.maxBytes(d.Limits)
	if limit > 0 && size > limit {
		return fault.New(fault.FileTooLarge, "the file is %d bytes, over the limit of %d bytes", size, limit)
	}
	data, err := readFile(w, body, limit)
	if err != nil {
		return err
	}
	f, created, err := s.putFile(r.Context(), d.Limits.Owner, name, declared, data, d.Limits.Retention)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpjson.Write(w, status, viewFile(f))
	return nil
}

// maxBytes is the largest file a request may bring: the server's own
// bound, or the lower one its allow names. An allow never raises it.
func (s *Server) maxBytes(l authorizer.Limits) int64 {
	if l.MaxFileBytes > 0 && (s.Limits.MaxBytes == 0 || l.MaxFileBytes < s.Limits.MaxBytes) {
		return l.MaxFileBytes
	}
	return s.Limits.MaxBytes
}

// readFile reads an upload up to limit bytes. Zero is no limit.
func readFile(w http.ResponseWriter, body io.Reader, limit int64) ([]byte, error) {
	if limit > 0 {
		body = http.MaxBytesReader(w, io.NopCloser(body), limit)
	}
	data, err := io.ReadAll(body)
	if _, over := errors.AsType[*http.MaxBytesError](err); over {
		return nil, fault.New(fault.FileTooLarge, "the file is over the limit of %d bytes", limit)
	}
	if err != nil {
		return nil, invalid("file", "the body stopped before its end")
	}
	if len(data) == 0 {
		return nil, invalid("file", "the body is empty")
	}
	return data, nil
}

// fileRetention is how long a file that a submit fetched is kept: the
// server's retention of a file, or the shorter one the submit's allow names
// for what the parse writes. The allow of a submit says how long a parse is
// kept, which is the longer of the server's two settings, so it shortens a
// file's time only when it is below it.
func (s *Server) fileRetention(l authorizer.Limits) time.Duration {
	if l.Retention > 0 && (s.FileRetention == 0 || l.Retention < s.FileRetention) {
		return l.Retention
	}
	return s.FileRetention
}

// putFile tells what the bytes are and stores them as the owner's file,
// kept for retention. The same bytes are one file per owner, so created is
// false for bytes the owner already has.
func (s *Server) putFile(ctx context.Context, owner, name, declared string, data []byte, retention time.Duration) (f store.File, created bool, err error) {
	if mediaType, _, err := mime.ParseMediaType(declared); err == nil {
		declared = mediaType
	}
	mediaType, err := detect.Detect(data, detect.DeclaredType{MIME: declared, FileName: name})
	if err != nil {
		return store.File{}, false, err
	}
	return s.Backend.PutFile(ctx, store.File{
		ID: s.IDs.New(id.File), Owner: owner, Name: name, MediaType: mediaType,
		SHA256: store.Digest(data), Size: int64(len(data)), CreatedAt: s.now(), Data: data, Retention: retention,
	})
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request, c call) error {
	f, err := s.file(r, c)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, viewFile(f))
	return nil
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request, c call) error {
	f, err := s.file(r, c)
	if err != nil {
		return err
	}
	if err := s.Backend.DeleteFile(r.Context(), f.Owner, f.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

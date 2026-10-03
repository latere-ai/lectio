// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/id"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/pkg/httpjson"
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
}

func viewFile(f store.File) fileView {
	return fileView{ID: f.ID, Name: f.Name, Size: f.Size, SHA256: f.SHA256, MediaType: f.MediaType, CreatedAt: f.CreatedAt}
}

// createFile stores an upload: the body itself, or the part named "file"
// of a multipart form.
func (s *Server) createFile(w http.ResponseWriter, r *http.Request, owner string) error {
	name, declared := r.URL.Query().Get("name"), r.Header.Get("Content-Type")
	var body io.Reader = r.Body

	if mediaType, _, _ := mime.ParseMediaType(declared); mediaType == "multipart/form-data" {
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

	data, err := s.readFile(w, body)
	if err != nil {
		return err
	}
	f, created, err := s.putFile(owner, name, declared, data)
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

// readFile reads an upload up to the file limit.
func (s *Server) readFile(w http.ResponseWriter, body io.Reader) ([]byte, error) {
	if s.Limits.MaxBytes > 0 {
		body = http.MaxBytesReader(w, io.NopCloser(body), s.Limits.MaxBytes)
	}
	data, err := io.ReadAll(body)
	if _, over := errors.AsType[*http.MaxBytesError](err); over {
		return nil, fault.New(fault.FileTooLarge, "the file is over the limit of %d bytes", s.Limits.MaxBytes)
	}
	if err != nil {
		return nil, invalid("file", "the body stopped before its end")
	}
	if len(data) == 0 {
		return nil, invalid("file", "the body is empty")
	}
	return data, nil
}

// putFile tells what the bytes are and stores them as the owner's file.
// The same bytes are one file per owner, so created is false for bytes the
// owner already has.
func (s *Server) putFile(owner, name, declared string, data []byte) (f store.File, created bool, err error) {
	if mediaType, _, err := mime.ParseMediaType(declared); err == nil {
		declared = mediaType
	}
	mediaType, err := detect.Detect(data, detect.DeclaredType{MIME: declared, FileName: name})
	if err != nil {
		return store.File{}, false, err
	}
	f, created = s.Store.PutFile(store.File{
		ID: s.IDs.New(id.File), Owner: owner, Name: name, MediaType: mediaType,
		SHA256: store.Digest(data), Size: int64(len(data)), CreatedAt: s.now(), Data: data,
	})
	return f, created, nil
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request, owner string) error {
	f, err := s.Store.File(owner, r.PathValue("file"))
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, viewFile(f))
	return nil
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request, owner string) error {
	if err := s.Store.DeleteFile(owner, r.PathValue("file")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

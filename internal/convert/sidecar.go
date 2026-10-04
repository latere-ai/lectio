// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package convert

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"latere.ai/x/lectio/internal/fault"
)

// LimitArg is the first argument under which the sidecar's program does
// not serve: it applies a memory limit to itself and becomes the suite.
const LimitArg = "limit"

// restart is the code the suite exits with when it has just made its
// profile and asks to be started again.
const restart = 81

// registry is the settings of the profile each conversion is given, the
// second line behind a sidecar that has no network: no macro runs,
// whatever a document says of its own trust; no link out of a document is
// followed; and a text document's links are not updated when it is loaded.
const registry = `<?xml version="1.0" encoding="UTF-8"?>
<oor:items xmlns:oor="http://openoffice.org/2001/registry" xmlns:xs="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
<item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="DisableMacrosExecution" oor:op="fuse"><value>true</value></prop></item>
<item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value></prop></item>
<item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="BlockUntrustedRefererLinks" oor:op="fuse"><value>true</value></prop></item>
<item oor:path="/org.openoffice.Office.Writer/Content/Update"><prop oor:name="Link" oor:op="fuse"><value>0</value></prop></item>
</oor:items>
`

// strays is what the suite leaves outside its scratch directory. It listens
// for a second copy of itself on a socket it puts in this one place,
// whatever directory it is given for temporary files, and a suite that is
// killed does not take the socket down. One conversion runs at a time, so
// a socket that was not there when a conversion began and is there when it
// has ended is that conversion's. One that was there before is somebody
// else's suite, and is left alone.
var strays = "/tmp/OSL_PIPE_*_SingleOfficeIPC_*"

// The ways a conversion ends without a conversion that are the file's.
var (
	errTime   = errors.New("the suite passed its time limit")
	errFailed = errors.New("the suite wrote no conversion")
)

// Sidecar holds the office suite and answers the one call. It runs the
// suite on a file somebody else wrote, so the suite is given nothing and
// bounded in everything: a scratch directory and a profile of its own for
// each conversion, an environment that holds nothing of the sidecar's, a
// limit on time and one on memory, and its whole process group killed when
// it ends, however it ends. What it is not given here is the network, which
// only whoever runs the sidecar can take from it.
type Sidecar struct {
	// Suite is the suite's program.
	Suite string

	// Self is the sidecar's own program. Started with LimitArg it applies
	// the memory limit and becomes the suite, which is how a limit is put
	// on a program this one does not link.
	Self string

	// Dir is where the scratch directories are made. Empty takes the
	// system's directory for temporary files.
	Dir string

	// Timeout bounds one conversion.
	Timeout time.Duration

	// MemoryBytes bounds the address space of the suite and of whatever
	// it starts. Zero sets no bound.
	MemoryBytes uint64

	// MaxBytes is the largest file taken and the largest conversion
	// returned.
	MaxBytes int64

	// Log takes one line per conversion: types, sizes and how it ended,
	// never a file's content or its name.
	Log *slog.Logger
}

// Handler serves the one route. One conversion runs at a time: the suite
// is a large program, and a second copy doubles what a hostile file can
// make it hold.
func (s *Sidecar) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.New(slog.DiscardHandler)
	}
	slot := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+Path, func(w http.ResponseWriter, r *http.Request) {
		to := mediaType(r.Header.Get("Accept"))
		conv, ok := conversions[[2]string{mediaType(r.Header.Get("Content-Type")), to}]
		if !ok {
			s.refuse(w, r, http.StatusUnsupportedMediaType, fault.UnsupportedMediaType, "the converter does not convert this type into the one asked for", nil)
			return
		}
		// The slot is taken before the body is read, so a call that waits
		// holds a connection and no part of its file.
		select {
		case slot <- struct{}{}:
			defer func() { <-slot }()
		case <-r.Context().Done():
			return
		}
		s.convert(w, r, conv, to)
	})
	return mux
}

// convert does one conversion, into the media type to, in a scratch
// directory that is gone when it returns.
func (s *Sidecar) convert(w http.ResponseWriter, r *http.Request, conv conversion, to string) {
	ctx, started := r.Context(), time.Now()
	scratch, err := os.MkdirTemp(s.Dir, "lectio-convert-")
	if err != nil {
		s.refuse(w, r, http.StatusInternalServerError, fault.Internal, "the converter could not make its scratch directory", err)
		return
	}
	// The pattern is fixed and well formed, so Glob does not fail.
	before, _ := filepath.Glob(strays)
	defer func() {
		err := os.RemoveAll(scratch)
		after, _ := filepath.Glob(strays)
		for _, socket := range after {
			if !slices.Contains(before, socket) {
				err = errors.Join(err, os.Remove(socket))
			}
		}
		if err != nil {
			s.Log.ErrorContext(ctx, "what a conversion left was not removed", "error", err)
		}
	}()

	input := filepath.Join(scratch, "input."+conv.ext)
	received, err := receive(input, http.MaxBytesReader(w, r.Body, s.MaxBytes))
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		s.refuse(w, r, http.StatusRequestEntityTooLarge, fault.FileTooLarge, "the file is over the limit of "+strconv.FormatInt(s.MaxBytes, 10)+" bytes", nil)
		return
	} else if err != nil {
		s.refuse(w, r, http.StatusInternalServerError, fault.Internal, "the converter could not take the file", err)
		return
	}

	output, err := s.run(ctx, scratch, input, conv)
	var size int64
	if err == nil {
		// The suite's exit code says little: it is 0 for a file it could
		// not load. The conversion it wrote, or did not, decides.
		if info, statErr := os.Stat(output); statErr != nil || info.Size() == 0 {
			err = errFailed
		} else {
			size = info.Size()
		}
	}
	switch {
	case ctx.Err() != nil:
		// Whoever asked is gone, and there is nobody to answer.
		s.Log.WarnContext(ctx, "a conversion was abandoned", "from", conv.ext, "to", conv.out, "bytes_in", received)
		return
	case errors.Is(err, errTime):
		s.refuse(w, r, http.StatusUnprocessableEntity, fault.DocumentCorrupt, "the file was not converted within "+s.Timeout.String(), err)
		return
	case errors.Is(err, errFailed):
		s.refuse(w, r, http.StatusUnprocessableEntity, fault.DocumentCorrupt, "the file could not be converted: it is damaged, or it needs more memory than a conversion is given", err)
		return
	case err != nil:
		s.refuse(w, r, http.StatusInternalServerError, fault.Internal, "the converter could not run its suite", err)
		return
	case size > s.MaxBytes:
		s.refuse(w, r, http.StatusRequestEntityTooLarge, fault.FileTooLarge, "the conversion is over the limit of "+strconv.FormatInt(s.MaxBytes, 10)+" bytes", nil)
		return
	}

	f, err := os.Open(output)
	if err != nil {
		s.refuse(w, r, http.StatusInternalServerError, fault.Internal, "the converter could not read its conversion", err)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", to)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if _, err := io.Copy(w, f); err != nil {
		s.Log.WarnContext(ctx, "a conversion was not sent whole", "error", err)
		return
	}
	s.Log.InfoContext(ctx, "converted", "from", conv.ext, "to", conv.out, "bytes_in", received, "bytes_out", size, "ms", time.Since(started).Milliseconds())
}

// receive writes the file to the scratch directory as it arrives, so the
// sidecar never holds a file in memory.
func receive(path string, body io.Reader) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, body)
	return n, errors.Join(err, f.Close())
}

// refuse answers with a status and a problem, and logs why.
func (s *Sidecar) refuse(w http.ResponseWriter, r *http.Request, status int, code fault.Code, detail string, cause error) {
	var p problem
	p.Error.Code, p.Error.Detail = code, detail
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		s.Log.WarnContext(r.Context(), "a refusal was not sent", "error", err)
	}
	s.Log.WarnContext(r.Context(), "refused", "status", status, "code", string(code), "detail", detail, "error", cause)
}

// run has the suite convert the file and returns where the conversion is
// to be. The suite gets a profile, a home and a directory for temporary
// files of its own, all inside the scratch directory, so nothing it writes
// outlives the conversion or is seen by the next.
func (s *Sidecar) run(ctx context.Context, scratch, input string, conv conversion) (string, error) {
	profile, home, tmp, out := filepath.Join(scratch, "profile"), filepath.Join(scratch, "home"), filepath.Join(scratch, "tmp"), filepath.Join(scratch, "out")
	for _, dir := range []string{filepath.Join(profile, "user"), home, tmp, out} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(profile, "user", "registrymodifications.xcu"), []byte(registry), 0o600); err != nil {
		return "", err
	}

	args := []string{
		"--headless", "--invisible", "--nodefault", "--nofirststartwizard", "--nolockcheck", "--nologo", "--norestore",
		"-env:UserInstallation=file://" + profile,
		"--infilter=" + conv.reads,
		"--convert-to", conv.out + ":" + conv.writes,
		"--outdir", out,
		input,
	}
	name := s.Suite
	if s.MemoryBytes > 0 {
		name, args = s.Self, append([]string{LimitArg, strconv.FormatUint(s.MemoryBytes, 10), s.Suite}, args...)
	}

	limited, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	var err error
	for range 2 {
		// A profile that was just made has the suite exit and ask to be
		// started again, which is the one time it is.
		cmd := exec.CommandContext(limited, name, args...)
		cmd.Dir = scratch
		// Nothing of the sidecar's environment is passed on.
		cmd.Env = []string{"HOME=" + home, "TMPDIR=" + tmp}
		cmd.WaitDelay = time.Second
		ownGroup(cmd)
		err = cmd.Run()
		// The suite may leave processes behind when it ends, and does
		// when it is killed. None outlives its conversion.
		if kerr := killGroup(cmd); kerr != nil {
			s.Log.WarnContext(ctx, "what the suite left running could not be killed", "error", kerr)
		}
		if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != restart {
			break
		}
	}
	switch _, exited := errors.AsType[*exec.ExitError](err); {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case limited.Err() != nil:
		return "", errTime
	case err != nil && !exited:
		// The suite did not start.
		return "", err
	}
	return filepath.Join(out, "input."+conv.out), nil
}

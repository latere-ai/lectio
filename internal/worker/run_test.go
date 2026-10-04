// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/keys"
	"latere.ai/x/lectio/internal/objects"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// prepare is a claim of the prepare task of a parse of the file under key.
func prepare(parseID, key, name, mediaType string) tasks.Claim {
	return tasks.Claim{
		Parse: parseID, Task: tasks.PrepareID, Kind: tasks.Prepare, Token: 1, Group: "acme",
		Context: tasks.Context{Owner: "alice", File: &tasks.Source{Key: key, Name: name, MediaType: mediaType}},
	}
}

// TestAParseRunsThroughItsThreeTasks: prepare counts the pages and names the
// working copy, each page writes its image and its result under its own
// token and says what a list of pages needs, and assemble writes the index
// that names the key of every page.
func TestAParseRunsThroughItsThreeTasks(t *testing.T) {
	b := newBench(t, nil)
	ctx := context.Background()
	b.put("sources/o/aa/fil_1", testfixtures.Read(t, testfixtures.MultiTIFF), "image/tiff")

	prepared := b.w.run(ctx, prepare("prs_a", "sources/o/aa/fil_1", "scan.tiff", "image/tiff"))
	if prepared.Outcome != tasks.Done || prepared.Prepare == nil || prepared.Prepare.Native || len(prepared.Prepare.Pages) != 3 {
		t.Fatalf("prepare settled %+v, %+v", prepared, prepared.Error)
	}
	var m objects.Manifest
	if err := json.Unmarshal(prepared.Prepare.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	// Nothing was opened or converted, so the pages are read from the
	// snapshot itself and no copy is written.
	if m.Work != "sources/o/aa/fil_1" || m.Token != 1 || m.MediaType != "image/tiff" || m.PagesTotal != 3 || m.Source != document.SourceReader {
		t.Fatalf("the manifest is %+v", m)
	}

	var rows []postgres.Task
	for n := 1; n <= 3; n++ {
		c := page("prs_a", n, int64(n+1), m.Work, m.MediaType)
		c.Attempt = 1
		s := b.w.run(ctx, c)
		if s.Outcome != tasks.Done || s.Output != blob.PageKey("prs_a", n, int64(n+1)) || s.Health != tasks.Healthy ||
			s.Usage.Calls != 1 || s.Usage.OutputTokens != 3 || s.Units != 3 || string(s.Result) != `{"blocks":3,"source":"reader"}` {
			t.Fatalf("page %d settled %+v, %+v, result %s", n, s, s.Error, s.Result)
		}
		stored, err := objects.GetPage(ctx, b.objects, s.Output)
		if err != nil || stored.Revision != objects.FirstReading || stored.Page.Number != n || stored.Page.Attempts != 2 ||
			stored.Page.Reader != "stub" || stored.Image != blob.ImageKey("prs_a", n, int64(n+1), "image/png") {
			t.Fatalf("the stored result of page %d is %+v, %v", n, stored, err)
		}
		if img, mediaType, err := b.objects.Get(ctx, stored.Image); err != nil || mediaType != "image/png" || len(img) == 0 {
			t.Fatalf("the image of page %d: %d bytes of %q, %v", n, len(img), mediaType, err)
		}
		rows = append(rows, postgres.Task{ID: tasks.PageID(n), State: tasks.Succeeded, Output: s.Output})
	}

	b.store.rows["prs_a"] = rows
	assemble := tasks.Claim{Parse: "prs_a", Task: tasks.AssembleID, Kind: tasks.Assemble, Token: 7, Context: tasks.Context{Manifest: prepared.Prepare.Manifest}}
	s := b.w.run(ctx, assemble)
	if s.Outcome != tasks.Done || s.Assemble == nil || s.Assemble.Index != blob.IndexKey("prs_a", 7) || s.Output != s.Assemble.Index {
		t.Fatalf("assemble settled %+v, %+v", s, s.Error)
	}
	idx, err := objects.GetIndex(ctx, b.objects, s.Output)
	if err != nil || idx.Parse != "prs_a" || len(idx.Pages) != 3 || len(idx.Keys) != 3 || idx.Usage.Pages != 3 || strings.Join(idx.Renderings, ",") != "markdown,text" {
		t.Fatalf("the index is %+v, %v", idx, err)
	}
	for i, entry := range idx.Keys {
		// A page the passes marked is written again under assemble's token;
		// one they left alone stays where its task wrote it.
		if entry.Number != i+1 || entry.Revision != objects.FirstReading ||
			(entry.Key != rows[i].Output && entry.Key != blob.AssembledPageKey("prs_a", i+1, 7)) {
			t.Fatalf("the index lists page %d as %+v", i+1, entry)
		}
		if stored, err := objects.GetPage(ctx, b.objects, entry.Key); err != nil || stored.Page.State != document.PageSucceeded || len(stored.Page.Blocks) != 3 {
			t.Fatalf("the page the index names: %+v, %v", stored, err)
		}
	}
}

// TestANativeFileIsWrittenByPrepare: a format that carries its own structure
// has no page to read. Prepare writes its pages under its own token, and
// assemble finds them there.
func TestANativeFileIsWrittenByPrepare(t *testing.T) {
	b := newBench(t, nil)
	ctx := context.Background()
	b.put("sources/o/bb/fil_2", testfixtures.Read(t, testfixtures.CSV), "text/csv")
	c := prepare("prs_n", "sources/o/bb/fil_2", "sample.csv", "text/csv")
	c.Token = 4
	s := b.w.run(ctx, c)
	if s.Outcome != tasks.Done || !s.Prepare.Native || len(s.Prepare.Pages) != 1 {
		t.Fatalf("prepare of a native file settled %+v, %+v", s, s.Error)
	}
	stored, err := objects.GetPage(ctx, b.objects, blob.PageKey("prs_n", 1, 4))
	if err != nil || stored.Page.Source != document.SourceNative || len(stored.Page.Blocks) != 1 || stored.Image != "" {
		t.Fatalf("the native page is %+v, %v", stored, err)
	}

	done := b.w.run(ctx, tasks.Claim{Parse: "prs_n", Task: tasks.AssembleID, Kind: tasks.Assemble, Token: 1, Context: tasks.Context{Manifest: s.Prepare.Manifest}})
	if done.Outcome != tasks.Done {
		t.Fatalf("assemble of a native parse settled %+v, %+v", done, done.Error)
	}
	idx, err := objects.GetIndex(ctx, b.objects, done.Output)
	if err != nil || len(idx.Keys) != 1 || idx.Keys[0].Key != blob.PageKey("prs_n", 1, 4) || idx.Pages[0].Source != document.SourceNative {
		t.Fatalf("the index of a native parse is %+v, %v", idx, err)
	}
}

// TestAFileThatWasOpenedIsReadFromAWorkingCopy: a signed container holds the
// file the pages are read from, so prepare writes what was inside under the
// parse's prefix and its own token, and the manifest names it.
func TestAFileThatWasOpenedIsReadFromAWorkingCopy(t *testing.T) {
	b := newBench(t, nil)
	ctx := context.Background()
	b.put("sources/o/cc/fil_3", testfixtures.Read(t, testfixtures.WrappedPDF), "application/pkcs7-mime")
	c := prepare("prs_w", "sources/o/cc/fil_3", "report.pdf.p7m", "")
	c.Token = 2
	s := b.w.run(ctx, c)
	if s.Outcome != tasks.Done {
		t.Fatalf("prepare of a signed container settled %+v, %+v", s, s.Error)
	}
	var m objects.Manifest
	if err := json.Unmarshal(s.Prepare.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.Work != blob.WorkKey("prs_w", 2, "application/pdf") || m.MediaType != "application/pdf" {
		t.Fatalf("the manifest of an opened file is %+v", m)
	}
	if data, mediaType, err := b.objects.Get(ctx, m.Work); err != nil || mediaType != "application/pdf" || !strings.HasPrefix(string(data), "%PDF") {
		t.Fatalf("the working copy: %d bytes of %q, %v", len(data), mediaType, err)
	}
}

// broken is an object store whose every call fails.
type broken struct{ blob.Store }

var errDown = errors.New("the object store does not answer")

func (broken) Put(context.Context, string, []byte, string) error { return errDown }
func (broken) Get(context.Context, string) ([]byte, string, error) {
	return nil, "", errDown
}

// TestPrepareFailsForTheFilesOwnReasons: a file that is gone or of a type
// that is not read fails the parse at once, with the file's own code. A
// store that does not answer says nothing about the file: the task is tried
// again.
func TestPrepareFailsForTheFilesOwnReasons(t *testing.T) {
	b := newBench(t, nil)
	ctx := context.Background()
	b.put("sources/o/dd/fil_4", []byte{0, 1, 2, 3, 4, 5, 6, 7}, "application/octet-stream")
	b.put("sources/o/ee/fil_5", testfixtures.Read(t, testfixtures.MultiTIFF), "image/tiff")

	for name, tc := range map[string]struct {
		claim   tasks.Claim
		outcome tasks.Outcome
		code    string
	}{
		"a parse with no file":           {tasks.Claim{Parse: "p", Task: tasks.PrepareID, Kind: tasks.Prepare}, tasks.Permanent, "file_not_found"},
		"a file that is gone":            {prepare("p", "sources/o/zz/fil_9", "a.pdf", "application/pdf"), tasks.Permanent, "file_not_found"},
		"a type that is not read":        {prepare("p", "sources/o/dd/fil_4", "blob.bin", ""), tasks.Permanent, "unsupported_media_type"},
		"a selection that names none":    {withPages(prepare("p", "sources/o/ee/fil_5", "scan.tiff", "image/tiff"), "9"), tasks.Permanent, "invalid_pages"},
		"a kind the worker runs none of": {tasks.Claim{Parse: "p", Task: "extract-invoice", Kind: tasks.Extract}, tasks.Permanent, "internal"},
	} {
		s := b.w.run(ctx, tc.claim)
		if s.Outcome != tc.outcome || s.Error == nil || s.Error.Code != tc.code || s.Error.Detail == "" {
			t.Errorf("%s: settled %s, %+v", name, s.Outcome, s.Error)
		}
	}

	down := newBench(t, nil)
	down.w.Objects = broken{}
	if s := down.w.run(ctx, prepare("p", "sources/o/ee/fil_5", "scan.tiff", "image/tiff")); s.Outcome != tasks.Retryable || s.Error.Code != "internal" {
		t.Fatalf("with the object store down prepare settled %s, %+v", s.Outcome, s.Error)
	}
	// The file is read, and what prepare writes cannot be written.
	writes := newBench(t, nil)
	writes.put("sources/o/cc/fil_3", testfixtures.Read(t, testfixtures.WrappedPDF), "application/pkcs7-mime")
	writes.put("sources/o/bb/fil_2", testfixtures.Read(t, testfixtures.CSV), "text/csv")
	if _, err := writes.w.cache.get(ctx, writes.objects, "sources/o/cc/fil_3"); err != nil {
		t.Fatal(err)
	}
	if _, err := writes.w.cache.get(ctx, writes.objects, "sources/o/bb/fil_2"); err != nil {
		t.Fatal(err)
	}
	writes.w.Objects = broken{}
	for _, c := range []tasks.Claim{prepare("p", "sources/o/cc/fil_3", "report.pdf.p7m", ""), prepare("p", "sources/o/bb/fil_2", "sample.csv", "text/csv")} {
		if s := writes.w.run(ctx, c); s.Outcome != tasks.Retryable || s.Error.Code != "internal" {
			t.Fatalf("with nothing writable prepare of %s settled %s, %+v", c.Context.File.Name, s.Outcome, s.Error)
		}
	}
}

func withPages(c tasks.Claim, selection string) tasks.Claim {
	c.Context.Pages = selection
	return c
}

// TestAReadersErrorDecidesWhatThePageDoesNext is the table of reader errors
// of specs/005-parse-graph.md at the worker: the class of the reader's error
// decides the settle, and nothing else does.
func TestAReadersErrorDecidesWhatThePageDoesNext(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		outcome tasks.Outcome
		health  tasks.Health
		code    string
		invalid bool
		wait    time.Duration
	}{
		"rate limited":  {&reader.Error{Class: reader.RateLimited, RetryAfter: 7 * time.Second}, tasks.Wait, tasks.Silent, "", false, 7 * time.Second},
		"retryable":     {reader.Errorf(reader.Retryable, "a 502"), tasks.Retryable, tasks.Unhealthy, "reader_unavailable", false, 0},
		"unclassified":  {errTransport{}, tasks.Retryable, tasks.Unhealthy, "reader_unavailable", false, 0},
		"invalid":       {reader.Errorf(reader.Invalid, "not JSON"), tasks.Retryable, tasks.Silent, "page_unreadable", true, 0},
		"budget":        {reader.Errorf(reader.Budget, "spent"), tasks.Permanent, tasks.Silent, "budget_exhausted", false, 0},
		"permanent":     {reader.Errorf(reader.Permanent, "too large"), tasks.Permanent, tasks.Silent, "page_unreadable", false, 0},
		"refused":       {reader.Errorf(reader.Refused, "declined"), tasks.Next, tasks.Silent, "page_unreadable", false, 0},
		"misconfigured": {reader.Errorf(reader.Misconfigured, "no such model"), tasks.Next, tasks.Unhealthy, "reader_unavailable", false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			var seen reader.Page
			rd := &stub.Reader{Fail: func(p reader.Page) error { seen = p; return tc.err }}
			b := newBench(t, map[string]reader.Reader{"stub": rd})
			b.w.Keys = &issuing{}
			b.put("sources/o/aa/fil_1", sheet(t, false), "image/png")
			s := b.w.run(context.Background(), page("prs_a", 1, 5, "sources/o/aa/fil_1", "image/png"))
			code := ""
			if s.Error != nil {
				code = s.Error.Code
			}
			if s.Outcome != tc.outcome || s.Health != tc.health || code != tc.code || s.Invalid != tc.invalid || s.RetryAfter != tc.wait {
				t.Fatalf("settled %+v, error %+v", s, s.Error)
			}
			// What a call spent is recorded whatever it returned, and nothing
			// is written for a page that was not read.
			if s.Usage.Calls != 1 || s.Units != 3 || s.Output != "" || rd.Calls(1) != 1 {
				t.Fatalf("a failed call is accounted as %+v, units %d, after %d calls", s.Usage, s.Units, rd.Calls(1))
			}
			if left, err := b.objects.List(context.Background(), blob.ParsePrefix("prs_a")); err != nil || len(left) != 0 {
				t.Fatalf("a page that was not read left %v, %v", left, err)
			}
			if seen.Credential.Reveal() != "acme/alice/prs_a" || strings.Join(seen.Languages, ",") != "de" {
				t.Fatalf("the reader was called with the key %q and the languages %v", seen.Credential.Reveal(), seen.Languages)
			}
		})
	}
}

// TestAPageWithNoKeyWaitsOrFailsAndCallsNoReader is the key source at the
// worker (specs/013-limits-and-usage.md). A source that cannot say yet ends
// the attempt as a wait for as long as the source names, or for the store's
// own pause when it names none. A group with no budget fails the page as a
// spent budget, and a group that is issued no key fails it at once without
// moving it to another reader. In each case no reader is called, no call is
// charged, nothing is said about a reader, and nothing is fetched or written.
func TestAPageWithNoKeyWaitsOrFailsAndCallsNoReader(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		outcome tasks.Outcome
		code    string
		wait    time.Duration
	}{
		"an endpoint that is down":      {&keys.Unavailable{RetryAfter: 4 * time.Second, Reason: "the key endpoint answered 503"}, tasks.Wait, "", 4 * time.Second},
		"a wait wrapped by its caller":  {fmt.Errorf("resolving: %w", &keys.Unavailable{RetryAfter: 2 * time.Second}), tasks.Wait, "", 2 * time.Second},
		"a source that names no wait":   {&keys.Unavailable{Reason: "the call ended"}, tasks.Wait, "", 0},
		"an error of no kind":           {errors.New("a source of another kind failed"), tasks.Wait, "", 0},
		"a group with no budget":        {keys.ErrBudget, tasks.Permanent, "budget_exhausted", 0},
		"a group that is issued no key": {fmt.Errorf("asking: %w", keys.ErrForbidden), tasks.Permanent, "reader_not_permitted", 0},
	} {
		t.Run(name, func(t *testing.T) {
			rd := &stub.Reader{}
			b := newBench(t, map[string]reader.Reader{"stub": rd})
			source := &issuing{err: tc.err}
			fetched := &counting{Store: b.objects, gets: map[string]int{}}
			b.w.Keys, b.w.Objects = source, fetched
			b.put("sources/o/aa/fil_1", sheet(t, false), "image/png")
			s := b.w.run(context.Background(), page("prs_a", 1, 5, "sources/o/aa/fil_1", "image/png"))
			code := ""
			if s.Error != nil {
				code = s.Error.Code
			}
			if s.Outcome != tc.outcome || code != tc.code || s.RetryAfter != tc.wait {
				t.Fatalf("settled %+v, error %+v", s, s.Error)
			}
			if s.Usage != (tasks.Usage{}) || s.Units != 0 || s.Health != tasks.Silent || s.Invalid || s.Output != "" || rd.Calls(1) != 0 {
				t.Fatalf("a page with no key is accounted as %+v, units %d, health %q, after %d calls", s.Usage, s.Units, s.Health, rd.Calls(1))
			}
			if source.asked != "acme/alice/prs_a" {
				t.Fatalf("the source was asked for %q", source.asked)
			}
			if left, err := b.objects.List(context.Background(), blob.ParsePrefix("prs_a")); err != nil || len(left) != 0 || len(fetched.gets) != 0 {
				t.Fatalf("a page with no key left %v and fetched %v, %v", left, fetched.gets, err)
			}
		})
	}

	// A page that is taken from an earlier read calls no reader, so it asks
	// for no key.
	b := newBench(t, nil)
	source := &issuing{err: keys.ErrBudget}
	b.w.Keys = source
	kept := objects.Page{Revision: objects.FirstReading, Page: document.Page{Number: 4, State: document.PageSucceeded, Source: document.SourceReader, Blocks: []document.Block{}}}
	if err := objects.PutPage(context.Background(), b.objects, "parses/prs_old/pages/4.1.json", kept); err != nil {
		t.Fatal(err)
	}
	reused := page("prs_a", 1, 5, "sources/o/aa/fil_1", "image/png")
	reused.Context.Reuse = "parses/prs_old/pages/4.1.json"
	if s := b.w.run(context.Background(), reused); s.Outcome != tasks.Done || source.asked != "" {
		t.Fatalf("a page taken from an earlier read settled %+v after asking for the key of %q", s, source.asked)
	}
}

// errTransport is an error an adapter returned without a class.
type errTransport struct{}

func (errTransport) Error() string { return "connection reset" }

// TestAPageFailsForItsOwnReasonsWithNoCall: a page with nothing on it is
// written with no call. A page that cannot be rendered, a task that carries
// no manifest, a working copy that is gone and a reader this process does
// not have each end the attempt with no call made.
func TestAPageFailsForItsOwnReasonsWithNoCall(t *testing.T) {
	rd := &stub.Reader{}
	b := newBench(t, map[string]reader.Reader{"stub": rd})
	ctx := context.Background()
	b.put("sources/o/aa/blank", sheet(t, true), "image/png")
	b.put("sources/o/aa/pdf", testfixtures.Read(t, testfixtures.MinimalPDF), "application/pdf")

	blank := b.w.run(ctx, page("prs_a", 1, 1, "sources/o/aa/blank", "image/png"))
	if blank.Outcome != tasks.Done || blank.Usage.Calls != 0 || blank.Units != 0 || blank.Health != tasks.Silent || string(blank.Result) != `{"blocks":0,"source":"reader"}` {
		t.Fatalf("a blank page settled %+v, result %s", blank, blank.Result)
	}

	other := page("prs_a", 1, 1, "sources/o/aa/blank", "image/png")
	other.Reader = "elsewhere"
	bad := page("prs_a", 1, 1, "sources/o/aa/blank", "image/png")
	bad.Context.Manifest = json.RawMessage(`{`)
	notAPage := page("prs_a", 1, 1, "sources/o/aa/blank", "image/png")
	notAPage.Task = "page-x"
	for name, tc := range map[string]struct {
		claim   tasks.Claim
		outcome tasks.Outcome
		code    string
	}{
		"a page no engine renders":    {page("prs_a", 1, 1, "sources/o/aa/pdf", "application/pdf"), tasks.Permanent, "unsupported_media_type"},
		"a working copy that is gone": {page("prs_a", 1, 1, "sources/o/aa/none", "image/png"), tasks.Permanent, "file_not_found"},
		"a reader that is not here":   {other, tasks.Next, "reader_unavailable"},
		"a manifest that is not one":  {bad, tasks.Permanent, "internal"},
		"a task that is no page":      {notAPage, tasks.Permanent, "internal"},
	} {
		s := b.w.run(ctx, tc.claim)
		if s.Outcome != tc.outcome || s.Error == nil || s.Error.Code != tc.code || s.Usage.Calls != 0 || s.Health != tasks.Silent {
			t.Errorf("%s: settled %+v, %+v", name, s, s.Error)
		}
	}
	if rd.Calls(1) != 0 {
		t.Fatalf("the reader was called %d times", rd.Calls(1))
	}

	// The page is read and its image, or its result, cannot be written.
	b.put("sources/o/aa/fil_1", sheet(t, false), "image/png")
	if _, err := b.w.cache.get(ctx, b.objects, "sources/o/aa/fil_1"); err != nil {
		t.Fatal(err)
	}
	b.w.Objects = broken{}
	if s := b.w.run(ctx, page("prs_a", 1, 1, "sources/o/aa/fil_1", "image/png")); s.Outcome != tasks.Retryable || s.Error.Code != "internal" || s.Usage.Calls != 1 {
		t.Fatalf("with nothing writable a page settled %+v, %+v", s, s.Error)
	}
	if s := b.w.run(ctx, page("prs_a", 1, 1, "sources/o/aa/blank", "image/png")); s.Outcome != tasks.Retryable || s.Error.Code != "internal" {
		t.Fatalf("with nothing writable a blank page settled %+v, %+v", s, s.Error)
	}
}

// own is a reader that calls no model: it reads the page from the file's
// own text, or declines it.
type own struct{ decline bool }

func (own) Describe() reader.Description {
	return reader.Description{Name: "own", Accepts: []string{"image/png"}, Text: true, Local: true}
}

func (o own) ReadPage(context.Context, reader.Page) (reader.Result, error) {
	if o.decline {
		return reader.Result{}, reader.Errorf(reader.Refused, "the file carries no text of its own for the page")
	}
	return reader.Result{
		Blocks:    []document.Block{{Kind: document.KindText, Text: "Net 30 days.", Order: 1}},
		TextLayer: true,
	}, nil
}

// TestAReaderThatCallsNoModelIsMeteredAsNoCall: a page read from the file's
// own text, and a page such a reader declines, take their place in the
// queue's account and record no model call. No key is asked for either,
// so a group that is issued none has the page read all the same.
func TestAReaderThatCallsNoModelIsMeteredAsNoCall(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		reader  own
		outcome tasks.Outcome
	}{
		"read":     {own{}, tasks.Done},
		"declined": {own{decline: true}, tasks.Next},
	} {
		b := newBench(t, map[string]reader.Reader{"own": tc.reader})
		source := &issuing{err: keys.ErrForbidden}
		b.w.Costs, b.w.Keys = map[string]int{"own": 2}, source
		b.put("sources/o/aa/fil_1", sheet(t, false), "image/png")
		c := page("prs_a", 1, 1, "sources/o/aa/fil_1", "image/png")
		c.Reader = "own"
		s := b.w.run(ctx, c)
		if s.Outcome != tc.outcome || s.Usage != (tasks.Usage{}) || s.Units != 2 {
			t.Fatalf("%s: settled %+v, %+v", name, s, s.Error)
		}
		if source.asked != "" {
			t.Fatalf("%s: the key source was asked for %s", name, source.asked)
		}
		switch tc.outcome {
		case tasks.Done:
			if string(s.Result) != `{"blocks":1,"source":"text_layer"}` {
				t.Fatalf("%s: the page's summary is %s", name, s.Result)
			}
		default:
			if s.Error == nil || s.Error.Code != "page_unreadable" || s.Error.Detail != "the reader declined the page" {
				t.Fatalf("%s: the page failed with %+v", name, s.Error)
			}
		}
	}
}

// TestAPageIsTakenFromAnEarlierRead: a page the claim names a kept result
// for is copied under the task's own keys, image included, marked reused,
// and no model is called. A kept result that is gone is read again.
func TestAPageIsTakenFromAnEarlierRead(t *testing.T) {
	rd := &stub.Reader{}
	b := newBench(t, map[string]reader.Reader{"stub": rd})
	ctx := context.Background()
	b.put("sources/o/aa/fil_1", sheet(t, false), "image/png")
	first := b.w.run(ctx, page("prs_a", 1, 1, "sources/o/aa/fil_1", "image/png"))
	if first.Outcome != tasks.Done {
		t.Fatalf("the first read settled %+v", first)
	}

	again := page("prs_b", 1, 6, "sources/o/aa/fil_1", "image/png")
	again.Context.Reuse = first.Output
	s := b.w.run(ctx, again)
	if s.Outcome != tasks.Done || s.Output != blob.PageKey("prs_b", 1, 6) || s.Usage.Calls != 0 || s.Units != 0 || s.Health != tasks.Silent ||
		string(s.Result) != `{"blocks":3,"source":"reader","reused":true}` || rd.Calls(1) != 1 {
		t.Fatalf("a page taken from a read settled %+v, result %s, after %d calls", s, s.Result, rd.Calls(1))
	}
	stored, err := objects.GetPage(ctx, b.objects, s.Output)
	if err != nil || !stored.Page.Reused || stored.Page.Usage == nil || *stored.Page.Usage != (document.Usage{Pages: 1}) ||
		len(stored.Page.Blocks) != 3 || stored.Image != blob.ImageKey("prs_b", 1, 6, "image/png") {
		t.Fatalf("the page taken from a read is stored as %+v, %v", stored, err)
	}
	if _, _, err := b.objects.Get(ctx, stored.Image); err != nil {
		t.Fatalf("the image was not copied: %v", err)
	}

	// The kept result outlived its image: the page is taken without one.
	kept, err := objects.GetPage(ctx, b.objects, first.Output)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.objects.Delete(ctx, kept.Image); err != nil {
		t.Fatal(err)
	}
	bare := page("prs_c", 1, 2, "sources/o/aa/fil_1", "image/png")
	bare.Context.Reuse = first.Output
	if s := b.w.run(ctx, bare); s.Outcome != tasks.Done || rd.Calls(1) != 1 {
		t.Fatalf("a page taken from a read with no image settled %+v", s)
	} else if stored, err := objects.GetPage(ctx, b.objects, s.Output); err != nil || stored.Image != "" {
		t.Fatalf("it is stored as %+v, %v", stored, err)
	}

	// The kept result is gone: the page is read.
	gone := page("prs_d", 1, 3, "sources/o/aa/fil_1", "image/png")
	gone.Context.Reuse = "parses/prs_x/pages/1.1.json"
	if s := b.w.run(ctx, gone); s.Outcome != tasks.Done || rd.Calls(1) != 2 || string(s.Result) != `{"blocks":3,"source":"reader"}` {
		t.Fatalf("a page whose kept result is gone settled %+v, result %s, after %d calls", s, s.Result, rd.Calls(1))
	}

	// The store does not answer: nothing is known of the kept result, and
	// the task is tried again.
	b.w.Objects = broken{}
	if s := b.w.run(ctx, again); s.Outcome != tasks.Retryable || s.Error.Code != "internal" || rd.Calls(1) != 2 {
		t.Fatalf("with the object store down a page with a kept result settled %+v, %+v", s, s.Error)
	}
}

// TestAssembleAccountsForEveryPage: a page that failed is written from its
// task's error, a page with no row is recorded as skipped, and the index
// lists each. What assemble cannot read ends the attempt.
func TestAssembleAccountsForEveryPage(t *testing.T) {
	b := newBench(t, nil)
	ctx := context.Background()
	b.put("sources/o/aa/fil_1", testfixtures.Read(t, testfixtures.MultiTIFF), "image/tiff")
	read := b.w.run(ctx, page("prs_a", 1, 1, "sources/o/aa/fil_1", "image/tiff"))
	if read.Outcome != tasks.Done {
		t.Fatalf("the page settled %+v, %+v", read, read.Error)
	}
	manifest := json.RawMessage(`{"media_type":"image/tiff","pages_total":3,"selected":[1,2,3],"source":"reader","work":"sources/o/aa/fil_1","token":1}`)
	claim := tasks.Claim{Parse: "prs_a", Task: tasks.AssembleID, Kind: tasks.Assemble, Token: 2, Context: tasks.Context{Manifest: manifest}}
	b.store.rows["prs_a"] = []postgres.Task{
		{ID: tasks.PrepareID, State: tasks.Succeeded},
		{ID: "page-1", State: tasks.Succeeded, Output: read.Output},
		{ID: "page-2", State: tasks.Failed, Attempt: 5, Error: &tasks.Error{Code: "reader_unavailable", Detail: "the reader could not be reached"}},
	}
	s := b.w.run(ctx, claim)
	if s.Outcome != tasks.Done {
		t.Fatalf("assemble settled %+v, %+v", s, s.Error)
	}
	idx, err := objects.GetIndex(ctx, b.objects, s.Output)
	if err != nil || len(idx.Pages) != 3 || idx.Pages[1].State != document.PageFailed || idx.Pages[1].Error.Code != "reader_unavailable" || idx.Pages[2].State != document.PageSkipped {
		t.Fatalf("the index is %+v, %v", idx, err)
	}
	failed, err := objects.GetPage(ctx, b.objects, idx.Keys[1].Key)
	if err != nil || idx.Keys[1].Key != blob.AssembledPageKey("prs_a", 2, 2) || failed.Page.Attempts != 5 || failed.Page.Error.Detail == "" || len(failed.Page.Blocks) != 0 {
		t.Fatalf("the failed page is stored as %+v under %q, %v", failed, idx.Keys[1].Key, err)
	}
	if skipped, err := objects.GetPage(ctx, b.objects, idx.Keys[2].Key); err != nil || skipped.Page.State != document.PageSkipped || skipped.Page.Error != nil {
		t.Fatalf("the skipped page is stored as %+v, %v", skipped, err)
	}

	// A parse that could not start has a document with no pages.
	b.store.rows["prs_e"] = nil
	empty := b.w.run(ctx, tasks.Claim{Parse: "prs_e", Task: tasks.AssembleID, Kind: tasks.Assemble, Token: 1})
	if idx, err := objects.GetIndex(ctx, b.objects, empty.Output); empty.Outcome != tasks.Done || err != nil || len(idx.Pages) != 0 {
		t.Fatalf("assemble of no page settled %+v with the index %+v, %v", empty, idx, err)
	}

	for name, tc := range map[string]struct {
		change  func(*bench, *tasks.Claim)
		outcome tasks.Outcome
		code    string
	}{
		"a manifest that is not one":      {func(_ *bench, c *tasks.Claim) { c.Context.Manifest = json.RawMessage(`[`) }, tasks.Permanent, "internal"},
		"rows that cannot be read":        {func(b *bench, _ *tasks.Claim) { b.store.rowsErr = errDown }, tasks.Retryable, "internal"},
		"a result that is gone":           {func(b *bench, _ *tasks.Claim) { b.store.rows["prs_a"][1].Output = "parses/prs_a/pages/1.9.json" }, tasks.Permanent, "file_not_found"},
		"an index that cannot be written": {func(b *bench, _ *tasks.Claim) { b.w.Objects = readOnly{b.objects} }, tasks.Retryable, "internal"},
	} {
		c := claim
		tc.change(b, &c)
		if s := b.w.run(ctx, c); s.Outcome != tc.outcome || s.Error == nil || s.Error.Code != tc.code {
			t.Errorf("%s: settled %+v, %+v", name, s, s.Error)
		}
		b.store.rowsErr, b.w.Objects = nil, b.objects
		b.store.rows["prs_a"][1].Output = read.Output
	}
}

// readOnly is an object store that reads and cannot write.
type readOnly struct{ blob.Store }

func (readOnly) Put(context.Context, string, []byte, string) error { return errDown }

// TestTheCacheHoldsWorkingCopiesWithinItsBound: the pages of one file fetch
// it once, a fetch that failed is asked again, and the copy used longest ago
// is the first to go.
func TestTheCacheHoldsWorkingCopiesWithinItsBound(t *testing.T) {
	ctx := context.Background()
	store := &counting{Store: blob.NewMemory(), gets: map[string]int{}}
	for _, key := range []string{"a", "b", "c"} {
		if err := store.Put(ctx, key, make([]byte, 400), ""); err != nil {
			t.Fatal(err)
		}
	}
	c := newCache(1000)
	for _, key := range []string{"a", "a", "b", "a", "c", "a", "b"} {
		if data, err := c.get(ctx, store, key); err != nil || len(data) != 400 {
			t.Fatalf("%s: %d bytes, %v", key, len(data), err)
		}
	}
	// a and b fit; c pushed out b, the copy used longest ago; b came back
	// and pushed out c.
	if store.gets["a"] != 1 || store.gets["b"] != 2 || store.gets["c"] != 1 || c.size != 800 {
		t.Fatalf("the fetches were %v and the cache holds %d bytes", store.gets, c.size)
	}
	if _, err := c.get(ctx, store, "none"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a key that holds nothing: %v", err)
	}
	if _, err := c.get(ctx, store, "none"); !errors.Is(err, blob.ErrNotFound) || store.gets["none"] != 2 {
		t.Fatalf("a fetch that failed was kept: %v after %d fetches", err, store.gets["none"])
	}

	// A copy over the bound is still handed to the page that asked.
	small := newCache(0)
	if data, err := small.get(ctx, store, "a"); err != nil || len(data) != 400 {
		t.Fatalf("a copy over the bound: %d bytes, %v", len(data), err)
	}

	// Two pages that ask at once make one fetch, and one that stops waiting
	// leaves the fetch to the other.
	slow := &counting{Store: store.Store, gets: map[string]int{}, hold: make(chan struct{})}
	shared := newCache(1000)
	results := make(chan error, 2)
	go func() { _, err := shared.get(ctx, slow, "a"); results <- err }()
	eventually(t, "the fetch began", func() bool { slow.mu.Lock(); defer slow.mu.Unlock(); return slow.gets["a"] == 1 })
	stopped, cancel := context.WithCancel(ctx)
	go func() { _, err := shared.get(stopped, slow, "a"); results <- err }()
	cancel()
	if err := <-results; !errors.Is(err, context.Canceled) {
		t.Fatalf("a page that stopped waiting got %v", err)
	}
	close(slow.hold)
	if err := <-results; err != nil || slow.gets["a"] != 1 {
		t.Fatalf("the fetch ended %v after %d fetches", err, slow.gets["a"])
	}
}

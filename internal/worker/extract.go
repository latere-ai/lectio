// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"encoding/json"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/assemble"
	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/extract"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/objects"
	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/reader"
)

// extractUnit is a document and its extractor, in the reasons a failed
// extraction is recorded with. A document no extractor could take, and one
// whose extractors' replies were never usable, did not yield an object in
// the shape of the schema.
var extractUnit = unit{what: "document", who: "extractor", unreadable: fault.SchemaNotSatisfied}

// extract makes one call of an extraction and says what comes next
// (specs/011-structured-extraction.md). The first claim of the task reads
// the document, cuts it into the windows its extractor reads it in, and
// keeps them. Every claim then makes one call: for the next window, or to
// repair the reply the last call gave. A claim that has another call to
// make keeps what the extraction has so far under its own token and
// settles with Continue, so the next call is claimed by itself: it takes a
// slot and a turn of its own, and a worker that dies loses one call. The
// last claim writes the field's result. An object that does not satisfy
// the schema after its repairs fails the field with what the validator
// found, and never the parse.
//
// A reply is held to the schema on the worker's bench: off this goroutine,
// for a bounded time, so a check that does not end costs the field and not
// the slot this claim runs in.
func (w *Worker) extract(ctx context.Context, c tasks.Claim, s *tasks.Settle) {
	name, ok := tasks.FieldOf(c.Task)
	var asked tasks.Field
	if err := json.Unmarshal([]byte(c.Context.Request), &asked); err != nil || !ok {
		permanent(s, fault.Internal, "the task carries no field or no request")
		return
	}
	schema, err := extract.Compile(asked.Schema)
	if err != nil {
		// The schema was checked when it was asked, so this is ours.
		permanent(s, fault.Internal, "the schema that was accepted does not compile")
		return
	}
	ext := w.Extractors[c.Reader]
	if ext == nil {
		// This process has no such extractor: another may take the field.
		s.Outcome = tasks.Next
		s.Error = &tasks.Error{Code: string(fault.ReaderUnavailable), Detail: "the extractor the field was claimed for is not configured"}
		return
	}
	// The key is resolved before anything is read: an extraction that has
	// none yet goes back to the queue having cost nothing.
	var credential reader.Credential
	if w.Keys != nil {
		if credential, err = w.Keys.Key(ctx, c.Group, c.Context.Owner, c.Parse); err != nil {
			keyless(s, err)
			return
		}
	}

	in, progress, ok := w.resume(ctx, c, name, ext, s)
	if !ok {
		return
	}
	if len(in.Windows) == 0 {
		// A document with no text is asked nothing.
		step, result, findings, err := w.Bench.Empty(ctx, schema)
		if err != nil {
			unchecked(s)
			return
		}
		w.conclude(ctx, c, s, name, in, progress, step, result, findings)
		return
	}
	text, previous, problems := progress.Ask(in)
	res, err := ext.Extract(ctx, reader.ExtractRequest{
		Schema: asked.Schema, Instructions: asked.Instructions, Text: text, Citations: asked.Citations,
		Constrain: schema.Constrainable && ext.Describe().Constrained, Previous: previous, Problems: problems,
		Credential: credential,
	})
	if err != nil {
		w.failed(c, s, err, extractUnit)
		return
	}
	s.Usage = tasks.Usage{Calls: 1, InputTokens: res.Usage.InputTokens, OutputTokens: res.Usage.OutputTokens}
	s.Units, s.Health = w.cost(c.Reader), tasks.Healthy
	if !asked.Citations {
		// What the caller did not ask for is not kept, whatever the model
		// returned.
		res.Citations = nil
	}
	step, result, findings, err := w.Bench.Take(ctx, schema, in, &progress, res)
	if err != nil {
		unchecked(s)
		return
	}
	w.conclude(ctx, c, s, name, in, progress, step, result, findings)
}

// unchecked gives a task back whose reply could not be held to its schema
// because every check of this process was taken by one that does not end.
// No attempt is spent and nothing is kept: the call it made is metered, and
// the task is claimed again by a worker that can check its reply. This
// process is handed no extraction until a check of it ends.
func unchecked(s *tasks.Settle) {
	s.Outcome = tasks.Returned
}

// conclude ends a claim of an extraction for the step the extraction takes
// after it: the result is written and the field filled, the field fails
// with what the validator found, or what the extraction has so far is kept
// under this claim's token and the task goes on. Either end carries what
// the field says of how it was filled.
func (w *Worker) conclude(ctx context.Context, c tasks.Claim, s *tasks.Settle, name string, in extract.Input, progress extract.Progress, step extract.Step, result extract.Result, findings []extract.Finding) {
	summary, err := json.Marshal(progress.Summary(in))
	if err != nil {
		permanent(s, fault.Internal, "the field's summary does not encode")
		return
	}
	switch step {
	case extract.Filled:
		key := blob.FieldKey(c.Parse, name, c.Token)
		if err := objects.Put(ctx, w.Objects, key, result); err != nil {
			stored(s, "the field's result", err)
			return
		}
		s.Outcome, s.Output, s.Result = tasks.Done, key, summary
	case extract.Unsatisfied:
		permanent(s, fault.SchemaNotSatisfied, "%s", extract.Broken(findings))
		s.Result = summary
	default:
		key := blob.FieldProgressKey(c.Parse, name, c.Token)
		if err := objects.Put(ctx, w.Objects, key, progress); err != nil {
			stored(s, "what the extraction has so far", err)
			return
		}
		s.Outcome, s.Output = tasks.Continue, key
	}
}

// resume returns the document as the extraction reads it and where the
// extraction stands. A claim that carries the key of what an earlier claim
// kept reads both from there. The first claim, and a claim for another
// extractor than the one the document was cut for, reads the document, cuts
// it, and keeps the cut under its own token. The task store keeps an
// extraction with the extractor that made its first call, so the second
// case is a task that moved down the policy's chain, which it does at most
// once for each extractor. ok is false when the attempt has ended.
func (w *Worker) resume(ctx context.Context, c tasks.Claim, name string, ext reader.Extractor, s *tasks.Settle) (in extract.Input, progress extract.Progress, ok bool) {
	if c.Context.Progress != "" {
		if err := objects.Get(ctx, w.Objects, c.Context.Progress, &progress); err != nil {
			stored(s, "what the extraction had so far", err)
			return in, progress, false
		}
		// A task that moved down the policy's chain is read by an
		// extractor with a bound of its own on a call's text: the document
		// is cut again for it, and the calls start over.
		if progress.Extractor == c.Reader {
			if err := objects.Get(ctx, w.Objects, progress.Input, &in); err != nil {
				stored(s, "the document as the extraction reads it", err)
				return in, progress, false
			}
			return in, progress, true
		}
	}
	pages, err := w.document(ctx, c)
	if err != nil {
		stored(s, "the document", err)
		return in, progress, false
	}
	if in, err = extract.Plan(c.Reader, pages, ext.Describe().MaxInput); err != nil {
		permanent(s, fault.CodeOf(err), "%s", fault.DetailOf(err))
		return in, progress, false
	}
	key := blob.FieldInputKey(c.Parse, name, c.Token)
	if err := objects.Put(ctx, w.Objects, key, in); err != nil {
		stored(s, "the document as the extraction reads it", err)
		return in, progress, false
	}
	return in, extract.Progress{Input: key, Extractor: c.Reader, Parts: []extract.Part{}}, true
}

// document reads the document of a parse that has ended, as the API serves
// it: the pages the document index lists, or, for a parse that ended
// without one, canceled or out of time, the pages its task rows name, with
// the running headers and footers marked as assembly marks them. A figure
// that was described is read with what was found printed in it.
func (w *Worker) document(ctx context.Context, c tasks.Claim) ([]document.Page, error) {
	var pages []document.Page
	if c.Context.Index != "" {
		idx, err := objects.GetIndex(ctx, w.Objects, c.Context.Index)
		if err != nil {
			return nil, err
		}
		var keys []string
		for _, entry := range idx.Keys {
			if entry.Key != "" {
				keys = append(keys, entry.Key)
			}
		}
		read, err := objects.GetPages(ctx, w.Objects, keys)
		if err != nil {
			return nil, err
		}
		for _, p := range read {
			pages = append(pages, p.Page)
		}
	} else {
		var m objects.Manifest
		if len(c.Context.Manifest) > 0 {
			if err := json.Unmarshal(c.Context.Manifest, &m); err != nil {
				return nil, err
			}
		}
		rows, err := w.Store.Tasks(ctx, c.Parse)
		if err != nil {
			return nil, err
		}
		read, err := w.results(ctx, c.Parse, m, rows)
		if err != nil {
			return nil, err
		}
		for _, r := range read {
			pages = append(pages, r.stored.Page)
		}
		assemble.Document(c.Parse, pages)
	}

	described, started, err := w.Store.Figures(ctx, c.Parse)
	if err != nil || !started {
		return pages, err
	}
	keys := map[string]string{}
	for _, f := range described.Figures {
		keys[f.Ref] = f.Output
	}
	return pages, objects.Describe(ctx, w.Objects, pages, keys)
}

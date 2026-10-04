// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/figures"
	"latere.ai/x/lectio/internal/keys"
	"latere.ai/x/lectio/internal/objects"
	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// illustratedPage is where the page of the figures of this suite is stored.
const illustratedPage = "parses/prs_a/pages/1.2.json"

// illustrated stores a page that holds a paragraph, a figure with a place
// on the page, the figure's caption and a figure with no place, and the
// image the page was read from.
func (b *bench) illustrated() {
	b.t.Helper()
	b.put("parses/prs_a/pages/1.2.png", sheet(b.t, false), "image/png")
	page := objects.Page{Revision: objects.FirstReading, Image: "parses/prs_a/pages/1.2.png", Page: document.Page{
		Number: 1, State: document.PageSucceeded, Blocks: document.Number(1, []document.Block{
			{Kind: document.KindText, Text: "A paragraph.", Order: 1, Box: &document.Box{0.1, 0.05, 0.9, 0.15}},
			{Kind: document.KindFigure, Order: 2, Box: &document.Box{0.25, 0.2, 0.75, 0.8}},
			{Kind: document.KindCaption, Text: "Figure 1: what it is.", Order: 3},
			{Kind: document.KindFigure, Order: 4},
		}),
	}}
	if err := objects.PutPage(context.Background(), b.objects, illustratedPage, page); err != nil {
		b.t.Fatal(err)
	}
}

// described is a claim of the task that describes the figure of a ref, on
// the page stored under the key.
func described(ref string, token int64, page, reuse string) tasks.Claim {
	return tasks.Claim{
		Parse: "prs_a", Task: tasks.FigureID(ref), Kind: tasks.Figure, Token: token, Group: "acme", Reader: "stub",
		Context: tasks.Context{Owner: "alice", Languages: []string{"de"}, Figure: &tasks.FigureAsk{Page: page, Reuse: reuse}},
	}
}

// describing is a bench whose describer is the one given, over the
// illustrated page.
func describing(t *testing.T, d reader.Describer) *bench {
	t.Helper()
	b := newBench(t, nil)
	b.w.Describers = map[string]reader.Describer{"stub": d}
	b.w.Keys = &issuing{}
	b.illustrated()
	return b
}

// kept reads a stored description.
func (b *bench) kept(key string) figures.Description {
	b.t.Helper()
	var got figures.Description
	if err := objects.Get(context.Background(), b.objects, key, &got); err != nil {
		b.t.Fatalf("reading the description under %s: %v", key, err)
	}
	return got
}

// TestAFigureIsCutFromItsPageAndDescribed: a figure's task reads where the
// figure is from its page's stored result, cuts it from the image the
// reader saw, and has the describer it was claimed for describe it alone,
// with its caption, the languages hinted and the group's key. What came
// back is written under the task's token, and the settle carries the call.
func TestAFigureIsCutFromItsPageAndDescribed(t *testing.T) {
	var seen reader.FigureRequest
	d := &stub.Describer{Fail: func(req reader.FigureRequest) error { seen = req; return nil }}
	b := describing(t, d)
	s := b.w.run(context.Background(), described("1.2", 7, illustratedPage, ""))
	if s.Outcome != tasks.Done || s.Output != "parses/prs_a/figures/1.2.7.json" || s.Result != nil || s.Health != tasks.Healthy ||
		s.Units != 3 || s.Usage.Calls != 1 || s.Usage.OutputTokens != 1 {
		t.Fatalf("settled %+v, error %+v", s, s.Error)
	}
	// The page is 60 by 40, and the figure covers half its width and 6
	// tenths of its height.
	if seen.Width != 30 || seen.Height != 24 || seen.MediaType != "image/png" || seen.Caption != "Figure 1: what it is." ||
		strings.Join(seen.Languages, ",") != "de" || seen.Credential.Reveal() != "acme/alice/prs_a" {
		t.Fatalf("the describer was asked %+v", seen)
	}
	got := b.kept(s.Output)
	if !strings.HasPrefix(got.Description, "A figure of 30 by 24 pixels") || got.Type != reader.FigureOther || got.Model != stub.Name || len(got.Labels) != 1 {
		t.Fatalf("the description kept is %+v", got)
	}
}

// TestADescriptionAnEarlierRunKeptIsTakenAndNotMadeAgain: a claim that is
// handed the key of a description kept for the same figure copies it under
// its own key and says so. No describer is called and no key is asked for.
// A kept description that is gone is no reason to fail: the figure is
// described.
func TestADescriptionAnEarlierRunKeptIsTakenAndNotMadeAgain(t *testing.T) {
	ctx := context.Background()
	d := &stub.Describer{}
	b := describing(t, d)
	source := &issuing{err: keys.ErrBudget}
	b.w.Keys = source
	earlier := figures.Description{Type: reader.FigureChart, Description: "Revenue by quarter.", Model: "m"}
	if err := objects.Put(ctx, b.objects, "parses/prs_old/figures/3.1.2.json", earlier); err != nil {
		t.Fatal(err)
	}
	s := b.w.run(ctx, described("1.2", 7, illustratedPage, "parses/prs_old/figures/3.1.2.json"))
	if s.Outcome != tasks.Done || string(s.Result) != `{"reused":true}` || s.Usage.Calls != 0 || d.Calls() != 0 || source.asked != "" {
		t.Fatalf("settled %+v after %d calls, having asked for the key of %q", s, d.Calls(), source.asked)
	}
	if got := b.kept("parses/prs_a/figures/1.2.7.json"); got.Description != earlier.Description || got.Type != reader.FigureChart {
		t.Fatalf("the parse's own copy is %+v", got)
	}

	b.w.Keys = &issuing{}
	if s := b.w.run(ctx, described("1.2", 8, illustratedPage, "parses/prs_old/figures/gone.json")); s.Outcome != tasks.Done || s.Result != nil || d.Calls() != 1 {
		t.Fatalf("a kept description that is gone: %+v after %d calls", s, d.Calls())
	}
	// A kept description that cannot be read, and a copy that cannot be
	// written, end the claim to be tried again.
	b.put("parses/prs_old/figures/bad.json", []byte("["), "application/json")
	if s := b.w.run(ctx, described("1.2", 9, illustratedPage, "parses/prs_old/figures/bad.json")); s.Outcome != tasks.Retryable || d.Calls() != 1 {
		t.Fatalf("a kept description that is no description: %+v", s)
	}
	b.w.Objects = &sealed{Store: b.objects, refuse: func(string) bool { return true }}
	if s := b.w.run(ctx, described("1.2", 9, illustratedPage, "parses/prs_old/figures/3.1.2.json")); s.Outcome != tasks.Retryable || s.Error.Code != "internal" {
		t.Fatalf("a copy that cannot be written: %+v", s)
	}
	if s := b.w.run(ctx, described("1.2", 9, illustratedPage, "")); s.Outcome != tasks.Retryable || s.Usage.Calls != 1 {
		t.Fatalf("a description that cannot be written: %+v", s)
	}
}

// TestADescribersErrorDecidesWhatTheFigureDoesNext: the class of the
// describer's error decides the settle, as a reader's does for a page.
func TestADescribersErrorDecidesWhatTheFigureDoesNext(t *testing.T) {
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
		"invalid":       {reader.Errorf(reader.Invalid, "not JSON"), tasks.Retryable, tasks.Silent, "figure_unreadable", true, 0},
		"budget":        {reader.Errorf(reader.Budget, "spent"), tasks.Permanent, tasks.Silent, "budget_exhausted", false, 0},
		"permanent":     {reader.Errorf(reader.Permanent, "too large"), tasks.Permanent, tasks.Silent, "figure_unreadable", false, 0},
		"refused":       {reader.Errorf(reader.Refused, "declined"), tasks.Next, tasks.Silent, "figure_unreadable", false, 0},
		"misconfigured": {reader.Errorf(reader.Misconfigured, "no such model"), tasks.Next, tasks.Unhealthy, "reader_unavailable", false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			d := &stub.Describer{Fail: func(reader.FigureRequest) error { return tc.err }}
			b := describing(t, d)
			s := b.w.run(context.Background(), described("1.2", 7, illustratedPage, ""))
			code := ""
			if s.Error != nil {
				code = s.Error.Code
			}
			if s.Outcome != tc.outcome || s.Health != tc.health || code != tc.code || s.Invalid != tc.invalid || s.RetryAfter != tc.wait {
				t.Fatalf("settled %+v, error %+v", s, s.Error)
			}
			if s.Usage.Calls != 1 || s.Units != 3 || s.Output != "" || d.Calls() != 1 {
				t.Fatalf("a failed call is accounted as %+v, units %d, after %d calls", s.Usage, s.Units, d.Calls())
			}
			if left, err := b.objects.List(context.Background(), "parses/prs_a/figures/"); err != nil || len(left) != 0 {
				t.Fatalf("a figure that was not described left %v, %v", left, err)
			}
		})
	}
}

// TestAFigureEndsForItsOwnReasonsWithNoCall: a task that carries no figure,
// a describer this process does not have, a group with no key, a page or
// an image that is gone, a ref that names no figure with a place on the
// page, a page with no image and an image a figure cannot be cut from each
// end the claim with no call and nothing charged.
func TestAFigureEndsForItsOwnReasonsWithNoCall(t *testing.T) {
	ctx := context.Background()
	d := &stub.Describer{}
	b := describing(t, d)
	ended := func(c tasks.Claim) (tasks.Outcome, string) {
		t.Helper()
		s := b.w.run(ctx, c)
		if s.Usage != (tasks.Usage{}) || s.Units != 0 || d.Calls() != 0 || s.Output != "" {
			t.Fatalf("a claim that ends for its own reasons made a call: %+v", s)
		}
		if s.Error == nil {
			return s.Outcome, ""
		}
		return s.Outcome, s.Error.Code
	}

	bare := described("1.2", 1, illustratedPage, "")
	bare.Context.Figure = nil
	if o, code := ended(bare); o != tasks.Permanent || code != "internal" {
		t.Fatalf("a task with no figure: %s %s", o, code)
	}
	misnamed := described("1.2", 1, illustratedPage, "")
	misnamed.Task = "figure-"
	if o, code := ended(misnamed); o != tasks.Permanent || code != "internal" {
		t.Fatalf("a task that names no figure: %s %s", o, code)
	}
	elsewhere := described("1.2", 1, illustratedPage, "")
	elsewhere.Reader = "gone"
	if o, code := ended(elsewhere); o != tasks.Next || code != "reader_unavailable" {
		t.Fatalf("a describer this process does not have: %s %s", o, code)
	}
	for name, ref := range map[string]string{"a figure with no place": "1.4", "a block that is no figure": "1.1", "a ref the page does not hold": "1.9"} {
		if o, code := ended(described(ref, 1, illustratedPage, "")); o != tasks.Permanent || code != "figure_unreadable" {
			t.Fatalf("%s: %s %s", name, o, code)
		}
	}
	if o, code := ended(described("1.2", 1, "parses/prs_a/pages/9.1.json", "")); o != tasks.Permanent || code != "figure_unreadable" {
		t.Fatalf("a page that is gone: %s %s", o, code)
	}

	for name, tc := range map[string]struct {
		source error
		want   tasks.Outcome
		code   string
	}{
		"a key endpoint that is down":   {&keys.Unavailable{RetryAfter: time.Second}, tasks.Wait, ""},
		"a group with no budget":        {keys.ErrBudget, tasks.Permanent, "budget_exhausted"},
		"a group that is issued no key": {keys.ErrForbidden, tasks.Permanent, "reader_not_permitted"},
	} {
		b.w.Keys = &issuing{err: tc.source}
		if o, code := ended(described("1.2", 1, illustratedPage, "")); o != tc.want || code != tc.code {
			t.Fatalf("%s: %s %s", name, o, code)
		}
	}
	b.w.Keys = &issuing{}

	// A page that was not read from an image, one whose image is gone, one
	// whose image does not decode, and a figure whose box covers no pixel.
	rewrite := func(change func(*objects.Page)) {
		t.Helper()
		page, err := objects.GetPage(ctx, b.objects, illustratedPage)
		if err != nil {
			t.Fatal(err)
		}
		change(&page)
		if err := objects.PutPage(ctx, b.objects, illustratedPage, page); err != nil {
			t.Fatal(err)
		}
	}
	rewrite(func(p *objects.Page) { p.Page.Blocks[1].Box = &document.Box{0.5, 0.5, 0.5, 0.5} })
	if o, code := ended(described("1.2", 1, illustratedPage, "")); o != tasks.Permanent || code != "figure_unreadable" {
		t.Fatalf("a box that covers no pixel: %s %s", o, code)
	}
	b.put("parses/prs_a/pages/1.2.png", []byte("no image"), "image/png")
	if o, code := ended(described("1.2", 1, illustratedPage, "")); o != tasks.Permanent || code != "figure_unreadable" {
		t.Fatalf("an image that does not decode: %s %s", o, code)
	}
	if err := b.objects.Delete(ctx, "parses/prs_a/pages/1.2.png"); err != nil {
		t.Fatal(err)
	}
	if o, code := ended(described("1.2", 1, illustratedPage, "")); o != tasks.Permanent || code != "figure_unreadable" {
		t.Fatalf("an image that is gone: %s %s", o, code)
	}
	rewrite(func(p *objects.Page) { p.Image = "" })
	if o, code := ended(described("1.2", 1, illustratedPage, "")); o != tasks.Permanent || code != "figure_unreadable" {
		t.Fatalf("a page with no image: %s %s", o, code)
	}
	b.put(illustratedPage, []byte("["), "application/json")
	if o, code := ended(described("1.2", 1, illustratedPage, "")); o != tasks.Retryable || code != "internal" {
		t.Fatalf("a page that is no page: %s %s", o, code)
	}
	if left, err := b.objects.List(ctx, blob.ParsePrefix("prs_a")+"figures/"); err != nil || len(left) != 0 {
		t.Fatalf("figures that were not described left %v, %v", left, err)
	}
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOnlyPagesAndExtractionsCallAModel(t *testing.T) {
	for kind, want := range map[Kind]bool{Prepare: false, Page: true, Assemble: false, Extract: true} {
		if kind.CallsModel() != want {
			t.Errorf("%s calls a model: %t, want %t", kind, kind.CallsModel(), want)
		}
	}
}

func TestATaskEndsInThreeStates(t *testing.T) {
	for state, want := range map[State]bool{Queued: false, Leased: false, Succeeded: true, Failed: true, Canceled: true} {
		if state.Terminal() != want {
			t.Errorf("%s is terminal: %t, want %t", state, state.Terminal(), want)
		}
	}
}

func TestAClassIsNamedAsTheAPINamesIt(t *testing.T) {
	if Interactive.String() != "interactive" || Batch.String() != "batch" {
		t.Fatalf("the classes are named %q and %q", Interactive, Batch)
	}
}

// TestATaskIDIsFixedByItsPlaceInTheParse: the id of a page task carries its
// page, and only an id PageID could have written is read back as one.
func TestATaskIDIsFixedByItsPlaceInTheParse(t *testing.T) {
	if PageID(12) != "page-12" || ExtractID("invoice") != "extract-invoice" {
		t.Fatalf("ids: %q, %q", PageID(12), ExtractID("invoice"))
	}
	if n, ok := PageOf(PageID(3000)); !ok || n != 3000 {
		t.Fatalf("PageOf(page-3000) = %d, %t", n, ok)
	}
	for _, id := range []string{PrepareID, AssembleID, "page-", "page-0", "page--1", "page-007", "page-1x", "extract-page-1"} {
		if n, ok := PageOf(id); ok {
			t.Errorf("PageOf(%q) = %d, and it is no page task", id, n)
		}
	}
}

func TestTheOutcomesAreTheStoresOwn(t *testing.T) {
	for _, o := range []Outcome{Done, Retryable, Permanent, Wait, Returned} {
		if !o.Valid() {
			t.Errorf("%q is not valid", o)
		}
	}
	if Outcome("").Valid() || Outcome("finished").Valid() {
		t.Error("an outcome the store does not know is valid")
	}
}

// TestASettleTravelsWithItsWaitInMilliseconds: the wire form of a settle
// carries the endpoint's wait as whole milliseconds, never as none when one
// was given, and a settle read back equals the one written.
func TestASettleTravelsWithItsWaitInMilliseconds(t *testing.T) {
	s := Settle{
		Parse: "prs_a", Task: "page-1", Token: 7, Outcome: Wait, RetryAfter: 7 * time.Second,
		Usage: Usage{Calls: 1, InputTokens: 10, OutputTokens: 2}, Units: 5, Health: Unhealthy,
		Error:   &Error{Code: "reader_unavailable", Detail: "a 503"},
		Prepare: &Prepared{Manifest: json.RawMessage(`{"pages_total":2}`), Pages: []int{1, 2}},
	}
	doc, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"retry_after_ms":7000`, `"token":7`, `"outcome":"wait"`, `"units":5`, `"health":"failure"`, `"pages":[1,2]`} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("the settle %s lacks %s", doc, want)
		}
	}
	var back Settle
	if err := json.Unmarshal(doc, &back); err != nil {
		t.Fatal(err)
	}
	if back.RetryAfter != s.RetryAfter || back.Parse != s.Parse || back.Token != s.Token || back.Usage != s.Usage ||
		*back.Error != *s.Error || len(back.Prepare.Pages) != 2 || string(back.Prepare.Manifest) != string(s.Prepare.Manifest) {
		t.Fatalf("the settle read back is %+v", back)
	}

	short, err := json.Marshal(Settle{Outcome: Wait, RetryAfter: 200 * time.Microsecond})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(short), `"retry_after_ms":1`) {
		t.Errorf("a wait below a millisecond is written as %s", short)
	}
	none, err := json.Marshal(Settle{Outcome: Done})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(none), "retry_after") {
		t.Errorf("a settle with no wait writes one: %s", none)
	}
	if err := json.Unmarshal([]byte(`{"token":"seven"}`), &back); err == nil {
		t.Error("a settle whose token is no number was read")
	}
}

func TestARequestIsCheckedBeforeItIsSent(t *testing.T) {
	ok := Request{
		Free:    2,
		Settles: []Settle{{Parse: "prs_a", Task: "page-1", Outcome: Done}},
		Held:    []Held{{Parse: "prs_a", Task: "page-2", Token: 1}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a well-formed request is refused: %v", err)
	}
	for name, bad := range map[string]Request{
		"free below zero":     {Free: -1},
		"a settle of no task": {Settles: []Settle{{Parse: "prs_a", Outcome: Done}}},
		"an unknown outcome":  {Settles: []Settle{{Parse: "prs_a", Task: "page-1", Outcome: "finished"}}},
		"a use below zero":    {Settles: []Settle{{Parse: "prs_a", Task: "page-1", Outcome: Done, Units: -1}}},
		"tokens below zero":   {Settles: []Settle{{Parse: "prs_a", Task: "page-1", Outcome: Done, Usage: Usage{InputTokens: -1}}}},
		"a wait below zero":   {Settles: []Settle{{Parse: "prs_a", Task: "page-1", Outcome: Wait, RetryAfter: -time.Second}}},
		"a held of no task":   {Held: []Held{{Task: "page-1"}}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestTheZeroSettingsAreTheSpecsOwn: a store opened with no settings runs
// with the defaults the specs give.
func TestTheZeroSettingsAreTheSpecsOwn(t *testing.T) {
	s := Settings{Pools: []Pool{{Reader: "default", MaxInFlight: 16}}, ReadChain: []string{"default"}}.WithDefaults()
	if s.Lease != 60*time.Second || s.SweepInterval != 30*time.Second || s.Attempts != 5 || s.Expiries != 3 ||
		s.BackoffBase != time.Second || s.BackoffCap != 60*time.Second || s.InteractiveWeight != 4 || s.BatchWeight != 1 ||
		s.PoolRecovery != 30*time.Second || s.PoolResume != 10*time.Second || s.PoolPause != 5*time.Second ||
		s.BreakerFailures != 3 || s.BreakerOpen != 30*time.Second || s.KeysPerGroup || s.Pools[0].Cost != 1 {
		t.Fatalf("the defaults are %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("the defaults are refused: %v", err)
	}
	if err := (Settings{}).WithDefaults().Validate(); err != nil {
		t.Fatalf("settings with no reader are refused: %v", err)
	}

	// What was set is kept, and the caller's pools are not written to.
	given := Settings{Lease: time.Minute * 5, Attempts: 2, InteractiveWeight: 9, Pools: []Pool{{Reader: "big", MaxInFlight: 2, Cost: 5}, {Reader: "small", MaxInFlight: 2}}}
	kept := given.WithDefaults()
	if kept.Lease != 5*time.Minute || kept.Attempts != 2 || kept.InteractiveWeight != 9 || kept.Pools[0].Cost != 5 || kept.Pools[1].Cost != 1 || given.Pools[1].Cost != 0 {
		t.Fatalf("settings that were given became %+v, from %+v", kept, given)
	}
}

func TestSettingsAStoreCannotRunWithAreNamed(t *testing.T) {
	base := func() Settings {
		return Settings{Pools: []Pool{{Reader: "default", MaxInFlight: 16}}, ReadChain: []string{"default"}}.WithDefaults()
	}
	for want, change := range map[string]func(*Settings){
		"the lease":                    func(s *Settings) { s.Lease = time.Microsecond },
		"the sweep interval":           func(s *Settings) { s.SweepInterval = -time.Second },
		"the attempts bound":           func(s *Settings) { s.Attempts = -1 },
		"the expiries bound":           func(s *Settings) { s.Expiries = -1 },
		"the backoff base":             func(s *Settings) { s.BackoffBase = -time.Second },
		"the backoff cap":              func(s *Settings) { s.BackoffCap = time.Millisecond },
		"the interactive weight":       func(s *Settings) { s.InteractiveWeight = MaxWeight + 1 },
		"the batch weight":             func(s *Settings) { s.BatchWeight = -1 },
		"the pool recovery interval":   func(s *Settings) { s.PoolRecovery = -time.Second },
		"the pool resume period":       func(s *Settings) { s.PoolResume = -time.Second },
		"the pool pause":               func(s *Settings) { s.PoolPause = -time.Second },
		"the breaker's failure count":  func(s *Settings) { s.BreakerFailures = -1 },
		"the breaker's open period":    func(s *Settings) { s.BreakerOpen = -time.Second },
		"a pool names no reader":       func(s *Settings) { s.Pools = append(s.Pools, Pool{MaxInFlight: 1, Cost: 1}) },
		"has two pools":                func(s *Settings) { s.Pools = append(s.Pools, s.Pools[0]) },
		"bound of calls in flight":     func(s *Settings) { s.Pools[0].MaxInFlight = 0 },
		`the reader "default"'s cost`:  func(s *Settings) { s.Pools[0].Cost = -2 },
		`the read chain names "gone"`:  func(s *Settings) { s.ReadChain = []string{"default", "gone"} },
		`the extract chain names "x"`:  func(s *Settings) { s.ExtractChain = []string{"x"} },
		"outside 1 to 1000":            func(s *Settings) { s.BatchWeight = 5000 },
		"the backoff cap is below the": func(s *Settings) { s.BackoffBase, s.BackoffCap = time.Minute, time.Second },
	} {
		s := base()
		change(&s)
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error naming %q, got %v", want, err)
		}
	}
}

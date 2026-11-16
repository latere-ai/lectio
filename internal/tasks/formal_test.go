// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"flag"
	"slices"
	"testing"

	"latere.ai/x/pkg/statemachine/statemachinetest"

	"latere.ai/x/lectio/document"
)

var update = flag.Bool("update", false, "rewrite the formal models under formal/")

// TestFormalModels holds formal/, which the lean and tla gates check, to the
// lifecycle tables.
func TestFormalModels(t *testing.T) {
	statemachinetest.Models(t, "../../formal", *update,
		ParseTable.Graph(), StageTable.Graph(), TaskTable.Graph(), FieldTable.Graph(),
		FigureTable.Graph(), RunTable.Graph(), FileTable.Graph(), BreakerTable.Graph())
}

// TestEveryStateIsInItsTable keeps each state type and its table to one set
// of states, so a state added to one is added to the other.
func TestEveryStateIsInItsTable(t *testing.T) {
	check(t, "Parse", ParseTable.States(), ParseQueued, ParseRunning, ParseSucceeded, ParseFailed, ParseCanceled, ParseGone)
	check(t, "Stage", StageTable.States(), StageQueued, StagePreparing, StageReading, StageAssembling, StageDone)
	check(t, "Task", TaskTable.States(), Queued, Leased, Succeeded, Failed, Canceled)
	check(t, "Field", FieldTable.States(), document.FieldPending, document.FieldSucceeded, document.FieldFailed)
	check(t, "Figure", FigureTable.States(), FigurePending, FigureSucceeded, FigureFailed)
	check(t, "Run", RunTable.States(), RunRunning, RunSucceeded, RunFailed)
	check(t, "File", FileTable.States(), FileLive, FileDeleting, FileGone)
	check(t, "Breaker", BreakerTable.States(), BreakerClosed, BreakerOpen)
}

func check[S ~string](t *testing.T, name string, states []S, consts ...S) {
	t.Helper()
	want := slices.Sorted(slices.Values(consts))
	if !slices.Equal(states, want) {
		t.Errorf("%s table states %v, constants %v", name, states, want)
	}
}

func TestParseStateOf(t *testing.T) {
	for _, s := range ParseTable.States() {
		got, ok := ParseStateOf(string(s))
		if want := s != ParseGone; ok != want || (ok && got != s) {
			t.Errorf("ParseStateOf(%q) = %q, %v", s, got, ok)
		}
	}
	if _, ok := ParseStateOf("cancelled"); ok {
		t.Error(`ParseStateOf("cancelled") names a state`)
	}
}

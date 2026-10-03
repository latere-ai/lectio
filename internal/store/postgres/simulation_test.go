// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// The dispatch simulation. The dispatch decision is a function of the rows
// it reads, so it is proven by driving the real exchange function over a
// real queue, one task at a time: each step settles the task the step before
// claimed and claims one. Nothing is slept through and nothing is mocked;
// the clock is the one the case passes.
//
// The loop itself runs in the database, a few hundred steps per statement,
// so that 100,000 dispatches are not 100,000 round trips. The function that
// loops is a temporary one, created on the case's own connection: it is the
// test's and no part of the schema.

const simulate = `
CREATE OR REPLACE FUNCTION pg_temp.simulate(p_worker text, p_steps integer, p_prev jsonb, p_pages jsonb, p_blank text[], p_now timestamptz)
RETURNS jsonb LANGUAGE plpgsql AS $$
DECLARE
  v_prev   jsonb := p_prev;
  v_settle jsonb;
  v_reply  jsonb;
  v_seq    text := '';
BEGIN
  FOR i IN 1..p_steps LOOP
    v_settle := NULL;
    IF v_prev IS NOT NULL THEN
      -- The task succeeded and used what its reader costs, or nothing when
      -- the case says the group's pages are blank.
      v_settle := jsonb_build_object(
        'parse', v_prev->'parse', 'task', v_prev->'task', 'token', v_prev->'token', 'outcome', 'succeeded',
        'units', CASE WHEN v_prev->>'group' = ANY (p_blank) THEN 0 ELSE (v_prev->>'cost')::integer END);
      IF v_prev->>'kind' = 'prepare' THEN
        v_settle := v_settle || jsonb_build_object('prepare', jsonb_build_object('pages',
          (SELECT coalesce(jsonb_agg(n), '[]'::jsonb)
             FROM generate_series(1, coalesce((p_pages->>(v_prev->>'parse'))::integer, 0)) n)));
      ELSIF v_prev->>'kind' = 'assemble' THEN
        v_settle := v_settle || jsonb_build_object('assemble', jsonb_build_object('index', 'index'));
      END IF;
    END IF;
    v_reply := lectio_exchange(p_worker, jsonb_build_object(
      'settles', CASE WHEN v_settle IS NULL THEN '[]'::jsonb ELSE jsonb_build_array(v_settle) END,
      'held', '[]'::jsonb, 'free', 1, 'idle', true, 'shutdown', false)::text, p_now)::jsonb;
    IF (v_reply->>'gone')::boolean OR jsonb_array_length(v_reply->'refused') > 0 THEN
      RAISE EXCEPTION 'the simulation''s exchange answered %', v_reply;
    END IF;
    v_prev := v_reply->'claims'->0;
    EXIT WHEN v_prev IS NULL;
    v_prev := v_prev || jsonb_build_object(
      'cost', coalesce((SELECT cost FROM pools WHERE reader = v_prev->>'reader'), 1),
      'class', (SELECT class FROM tasks WHERE parse_id = v_prev->>'parse' AND task_id = v_prev->>'task'));
    -- The dispatches are gathered as text: appending to a document would
    -- copy it at every step.
    v_seq := v_seq || CASE WHEN v_seq = '' THEN '' ELSE ',' END || v_prev::text;
  END LOOP;
  RETURN jsonb_build_object('seq', ('[' || v_seq || ']')::jsonb, 'prev', v_prev);
END $$`

// dispatch is one task the simulation was handed.
type dispatch struct {
	Parse   string      `json:"parse"`
	Task    string      `json:"task"`
	Kind    tasks.Kind  `json:"kind"`
	Group   string      `json:"group"`
	Project string      `json:"project"`
	Class   tasks.Class `json:"class"`
	Reader  string      `json:"reader"`
	// Cost is what the task was charged at its claim: its reader's cost,
	// or 1 for a task that calls no model.
	Cost int `json:"cost"`
}

// simulation is one worker that takes one task at a time and succeeds at it.
type simulation struct {
	h      *harness
	worker string
	prev   *string

	// pages is how many pages the prepare of each parse selects, and blank
	// the groups whose pages turn out blank and use nothing.
	pages map[string]int
	blank []string
}

// direct is a harness on a direct connection.
func direct(t testing.TB, settings tasks.Settings) *harness {
	t.Helper()
	return open(t, server(t), modes[0], settings)
}

// simulate starts a simulation on a store.
func (h *harness) simulate() *simulation {
	h.t.Helper()
	h.exec(simulate)
	return &simulation{h: h, worker: h.worker().id, pages: map[string]int{}, blank: []string{}}
}

// queue submits parses of a group, each of n pages, and returns their ids.
// The parses go in one round trip, so a case can queue thousands. Their
// deadline is far enough that a case which moves the clock by hours does not
// end them.
func (s *simulation) queue(sub postgres.Submission, parses, n int) []string {
	s.h.t.Helper()
	if sub.Deadline == 0 {
		sub.Deadline = 1000 * time.Hour
	}
	batch := &pgx.Batch{}
	ids := make([]string, parses)
	for i := range ids {
		each := filled(sub)
		each.Parse = fmt.Sprintf("prs_%s_%s_%d_%04d", sub.Group, sub.Project, len(s.pages), i)
		doc, err := postgres.SubmitDocument(each)
		if err != nil {
			s.h.t.Fatal(err)
		}
		batch.Queue(`SELECT lectio_submit($1, $2)`, doc, s.h.now)
		ids[i] = each.Parse
	}
	for _, id := range ids {
		s.pages[id] = n
	}
	results := s.h.admin.SendBatch(context.Background(), batch)
	for range ids {
		var answer string
		if err := results.QueryRow().Scan(&answer); err != nil || answer != "created" {
			s.h.t.Fatalf("submitting a parse of %s: %q, %v", sub.Group, answer, err)
		}
	}
	if err := results.Close(); err != nil {
		s.h.t.Fatal(err)
	}
	return ids
}

// run makes up to n dispatches and returns them in order. It returns fewer
// when the queue holds nothing more that can run.
func (s *simulation) run(n int) []dispatch {
	s.h.t.Helper()
	pages, err := json.Marshal(s.pages)
	if err != nil {
		s.h.t.Fatal(err)
	}
	out := make([]dispatch, 0, n)
	for len(out) < n {
		steps := min(250, n-len(out))
		var doc string
		err := s.h.admin.QueryRow(context.Background(), `SELECT pg_temp.simulate($1, $2, $3::jsonb, $4::jsonb, $5, $6)::text`,
			s.worker, steps, s.prev, string(pages), s.blank, s.h.now).Scan(&doc)
		if err != nil {
			s.h.t.Fatalf("the simulation: %v", err)
		}
		var chunk struct {
			Seq  []dispatch      `json:"seq"`
			Prev json.RawMessage `json:"prev"`
		}
		if err := json.Unmarshal([]byte(doc), &chunk); err != nil {
			s.h.t.Fatalf("the simulation's answer: %v", err)
		}
		out = append(out, chunk.Seq...)
		s.prev = nil
		if len(chunk.Prev) > 0 && string(chunk.Prev) != "null" {
			prev := string(chunk.Prev)
			s.prev = &prev
		}
		if len(chunk.Seq) < steps {
			break
		}
	}
	return out
}

// served counts dispatches by a key.
func served(ds []dispatch, key func(dispatch) string) map[string]int {
	out := map[string]int{}
	for _, d := range ds {
		out[key(d)]++
	}
	return out
}

// units sums what the dispatches were charged, by a key.
func units(ds []dispatch, key func(dispatch) string) map[string]int {
	out := map[string]int{}
	for _, d := range ds {
		out[key(d)] += d.Cost
	}
	return out
}

func byGroup(d dispatch) string   { return d.Group }
func byProject(d dispatch) string { return d.Project }
func byClass(d dispatch) string   { return d.Class.String() }

// streak is the longest run of consecutive dispatches of one key.
func streak(ds []dispatch, key func(dispatch) string, of string) int {
	longest, run := 0, 0
	for _, d := range ds {
		if key(d) == of {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// ahead is how far a key ever was ahead of its share of the dispatches, in
// tasks: the most, over every prefix of the dispatches, by which its count
// passed the share of one in every parts.
func ahead(ds []dispatch, key func(dispatch) string, of string, parts int) int {
	most, count := 0, 0
	for i, d := range ds {
		if key(d) == of {
			count++
		}
		most = max(most, count-(i+1)/parts)
	}
	return most
}

// within fails the case when a count is further than slack from what its
// weight gives it.
func within(t *testing.T, what string, got, want, slack int) {
	t.Helper()
	if got < want-slack || got > want+slack {
		t.Errorf("%s was served %d, want %d within %d", what, got, want, slack)
	}
}

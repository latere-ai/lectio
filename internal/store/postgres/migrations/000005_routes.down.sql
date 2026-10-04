-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- Back to a parse that fails with page_unreadable whatever its pages failed
-- with, keeps the rows of its failed tasks alone, cannot be read again,
-- counts none of its changes, and is metered on its own row alone.
-- lectio_settled is the one of 000004 again and lectio_settle the one of
-- 000002.

DROP FUNCTION IF EXISTS lectio_usage(text);
DROP FUNCTION IF EXISTS lectio_events(text, bigint, integer);
DROP FUNCTION IF EXISTS lectio_retry(text, text, timestamptz);
DROP TRIGGER IF EXISTS parses_changed ON parses;
DROP FUNCTION IF EXISTS lectio_changed();

-- lectio_settled as 000004 wrote it. It moves a parse forward when one of
-- its tasks reaches succeeded or failed, in the transaction that settled the
-- task. The graph of
-- a parse is fixed, so there is no edge to follow: prepare writes the page
-- tasks, the page that takes pages_open to 0 writes assemble, and assemble
-- ends the parse. p_settle is the settle the worker sent, or NULL when the
-- store failed the task itself.
--
-- prepare is where the pages of a parse are counted, so it is where the 2
-- limits on pages hold: a parse that selects more than its allow lets one
-- parse select fails with too_many_pages, and one whose pages its group's
-- day does not hold fails with budget_exhausted, before any page is read.
CREATE OR REPLACE FUNCTION lectio_settled(p_parse text, p_kind text, p_state text, p_error jsonb, p_settle jsonb, p_now timestamptz)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_p      parses%ROWTYPE;
  v_pages  jsonb;
  v_native boolean;
  v_n      integer;
  v_open   integer;
  v_lane   text;
  v_budget integer;
  v_day    date := (p_now AT TIME ZONE 'UTC')::date;
  v_refuse jsonb;
  v_reused boolean := coalesce((p_settle->'result'->>'reused')::boolean, false);
BEGIN
  IF p_kind = 'extract' THEN
    -- A field's task changes its field and never the parse.
    RETURN;
  END IF;
  IF p_kind IN ('prepare', 'assemble') AND p_state = 'failed' THEN
    PERFORM lectio_stop(p_parse, 'failed', p_error, p_now);
    RETURN;
  END IF;

  IF p_kind = 'prepare' THEN
    v_pages  := coalesce(nullif(p_settle->'prepare'->'pages', 'null'::jsonb), '[]'::jsonb);
    v_native := coalesce((p_settle->'prepare'->>'native')::boolean, false);
    v_n      := jsonb_array_length(v_pages);
    SELECT * INTO STRICT v_p FROM parses WHERE parse_id = p_parse;
    v_budget := coalesce((SELECT pages_per_day FROM groups WHERE group_id = v_p.group_id), 0);
    IF v_p.max_pages > 0 AND v_n > v_p.max_pages THEN
      v_refuse := jsonb_build_object('code', 'too_many_pages',
        'detail', 'the parse selects ' || v_n || ' pages, and its limit is ' || v_p.max_pages);
    ELSIF v_budget > 0 AND v_n > 0 AND NOT lectio_reserve(v_p.group_id, v_day, v_n, v_budget) THEN
      v_refuse := jsonb_build_object('code', 'budget_exhausted',
        'detail', 'the parse selects ' || v_n || ' pages, and its group has fewer left of its ' || v_budget || ' for the day');
    END IF;
    IF v_refuse IS NOT NULL THEN
      -- The parse says how many pages it would have read, and reads none.
      UPDATE parses SET manifest = p_settle->'prepare'->'manifest', pages_total = v_n WHERE parse_id = p_parse;
      PERFORM lectio_stop(p_parse, 'failed', v_refuse, p_now);
      RETURN;
    END IF;
    UPDATE parses SET manifest = p_settle->'prepare'->'manifest', pages_total = v_n,
           pages_open = CASE WHEN v_native THEN 0 ELSE v_n END,
           pages_done = CASE WHEN v_native THEN v_n ELSE 0 END,
           reserved = CASE WHEN v_budget > 0 THEN v_n ELSE 0 END,
           reserved_day = CASE WHEN v_budget > 0 AND v_n > 0 THEN v_day END
     WHERE parse_id = p_parse
    RETURNING * INTO STRICT v_p;
    IF v_native OR v_n = 0 THEN
      PERFORM lectio_enqueue(p_parse, 'assemble', 'assemble', NULL, p_now);
      RETURN;
    END IF;
    -- Every page row is written at once: the total is known to progress from
    -- this moment, and the order inside a project interleaves parses by seq.
    WITH written AS (
      INSERT INTO tasks (parse_id, task_id, kind, group_id, project_id, class, priority, seq, pin, state, available_at, created_at)
      SELECT p_parse, 'page-' || (e.n #>> '{}'), 'page', v_p.group_id, v_p.project_id, v_p.class, v_p.priority,
             e.i::integer, v_p.pin, 'queued', p_now, p_now
        FROM jsonb_array_elements(v_pages) WITH ORDINALITY AS e(n, i)
      ON CONFLICT DO NOTHING
      RETURNING lane
    )
    SELECT min(lane), count(*)::integer INTO v_lane, v_n FROM written;
    -- The pages of one parse are one lane: they share its pin.
    PERFORM lectio_count(v_p.group_id, v_p.project_id, v_p.class, v_lane, v_n, 0);
    RETURN;
  END IF;

  IF p_kind = 'page' THEN
    -- A page is counted when it settles, not when it succeeds: a failed page
    -- releases assemble as a succeeded one does.
    UPDATE parses SET pages_open = pages_open - 1,
           pages_done   = pages_done   + CASE WHEN p_state = 'succeeded' THEN 1 ELSE 0 END,
           pages_failed = pages_failed + CASE WHEN p_state = 'failed' THEN 1 ELSE 0 END,
           pages_reused = pages_reused + CASE WHEN p_state = 'succeeded' AND v_reused THEN 1 ELSE 0 END
     WHERE parse_id = p_parse
    RETURNING * INTO STRICT v_p;
    v_open := v_p.pages_open;
    -- A page that was read whole is kept under what was read, for the next
    -- parse of the same owner that would do the same read. A page whose
    -- reply was cut is read again, a page that was taken from an earlier
    -- read is already kept, and a parse whose readers promise nothing about
    -- their results has no key to keep one under.
    IF p_state = 'succeeded' AND NOT v_reused AND v_p.read_base IS NOT NULL AND p_settle->>'output' IS NOT NULL
       AND NOT coalesce((p_settle->'result'->>'truncated')::boolean, false) THEN
      INSERT INTO reads (owner, read_key, parse_id, output, created_at)
      VALUES (v_p.owner, v_p.read_base || ':' || substr(p_settle->>'task', 6), p_parse, p_settle->>'output', p_now)
      ON CONFLICT (owner, read_key) DO UPDATE
        SET parse_id = EXCLUDED.parse_id, output = EXCLUDED.output, created_at = EXCLUDED.created_at;
    END IF;
    IF v_open = 0 THEN
      PERFORM lectio_enqueue(p_parse, 'assemble', 'assemble', NULL, p_now);
    END IF;
    RETURN;
  END IF;

  -- assemble succeeded: the parse ends here, and there is no task after it.
  -- Everything that was read is kept whichever way it ends, for the parse's
  -- retention from now, and the pages that failed go back to their day.
  UPDATE parses SET
         state = CASE WHEN pages_failed <= allow_failed_pages THEN 'succeeded' ELSE 'failed' END,
         error = CASE WHEN pages_failed <= allow_failed_pages THEN NULL ELSE jsonb_build_object(
                   'code', 'page_unreadable',
                   'detail', pages_failed || ' of ' || pages_total || ' pages could not be read') END,
         index_key = p_settle->'assemble'->>'index', finished_at = p_now, expires_at = p_now + retention
   WHERE parse_id = p_parse;
  PERFORM lectio_refund(p_parse);
  -- The parse row keeps the counters and the document index the output keys.
  DELETE FROM tasks WHERE parse_id = p_parse AND state = 'succeeded';
END $$;

DROP FUNCTION IF EXISTS lectio_failure(text);

-- lectio_settle as 000002 wrote it. It ends one attempt at a task under the
-- fence: it matches only a task that is leased to this worker under the
-- token the worker was given.
-- A task that was canceled, reissued after its worker was taken for dead, or
-- settled before matches no row, and nothing is recorded for it. It reports
-- whether the settle was accepted.
--
-- Two outcomes move a page down the policy's chain, to the reader after the
-- one it was read by, where it has attempts of its own. A retryable failure
-- that says the reply was not usable moves it at the second such reply, once
-- for the page. The outcome next moves it at once and as far as the chain
-- goes: the reader declined the page, or its endpoint rejects the request
-- itself. A task pinned to a reader never moves, and a task with no reader
-- left fails with the error the settle carries.
CREATE OR REPLACE FUNCTION lectio_settle(p_worker text, p_s jsonb, p_cfg settings, p_now timestamptz)
RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE
  v_t         tasks%ROWTYPE;
  v_outcome   text := p_s->>'outcome';
  v_state     text;
  v_error     jsonb := p_s->'error';
  v_attempt   integer;
  v_available timestamptz;
  v_delay     interval;
  v_wait      interval;
  v_units     integer;
  v_chain     text[];
  v_next      integer;
  v_moves     boolean;
  v_chain_at  integer;
  v_invalid   integer;
  v_escalated boolean;
  v_calls     integer := coalesce((p_s->'usage'->>'calls')::integer, 0);
  v_in        bigint  := coalesce((p_s->'usage'->>'input_tokens')::bigint, 0);
  v_out       bigint  := coalesce((p_s->'usage'->>'output_tokens')::bigint, 0);
BEGIN
  SELECT * INTO v_t FROM tasks
   WHERE parse_id = p_s->>'parse' AND task_id = p_s->>'task'
     AND state = 'leased' AND lease_owner = p_worker AND lease_token = (p_s->>'token')::bigint
     FOR UPDATE;
  IF NOT FOUND THEN
    RETURN false;
  END IF;

  v_attempt   := v_t.attempt;
  v_available := v_t.available_at;
  v_chain_at  := v_t.chain_at;
  v_invalid   := v_t.invalid;
  v_escalated := v_t.escalated;
  -- The position after the reader that read the task: a reader that was
  -- passed over at the claim is not tried again by a task that moves on.
  v_chain := CASE v_t.kind WHEN 'page' THEN p_cfg.read_chain WHEN 'extract' THEN p_cfg.extract_chain ELSE '{}'::text[] END;
  v_next  := coalesce(array_position(v_chain, v_t.reader), v_t.chain_at + 1);
  v_moves := v_t.pin IS NULL AND v_next < coalesce(array_length(v_chain, 1), 0);
  CASE v_outcome
    WHEN 'succeeded' THEN
      v_state := 'succeeded';
      v_error := NULL;
    WHEN 'permanent' THEN
      v_state := 'failed';
    WHEN 'retryable' THEN
      v_attempt := v_t.attempt + 1;
      IF coalesce((p_s->>'invalid')::boolean, false) THEN
        v_invalid := v_t.invalid + 1;
      END IF;
      IF v_invalid >= 2 AND NOT v_t.escalated AND v_moves AND coalesce((p_s->>'invalid')::boolean, false) THEN
        v_state     := 'queued';
        v_chain_at  := v_next;
        v_attempt   := 0;
        v_invalid   := 0;
        v_escalated := true;
        v_available := p_now;
      ELSIF v_attempt >= p_cfg.attempts THEN
        v_state := 'failed';
      ELSE
        -- min(cap, base * 2^(attempt-1)), plus jitter uniform in half of it,
        -- so tasks that failed together do not come back together.
        v_state := 'queued';
        v_delay := least(p_cfg.backoff_cap, p_cfg.backoff_base * power(2, least(v_attempt - 1, 30)));
        v_available := p_now + v_delay + v_delay * (random() / 2);
      END IF;
    WHEN 'next' THEN
      IF v_moves THEN
        v_state     := 'queued';
        v_chain_at  := v_next;
        v_attempt   := 0;
        v_invalid   := 0;
        v_available := p_now;
      ELSE
        v_state := 'failed';
      END IF;
    WHEN 'wait' THEN
      -- The reader said to wait. That is not a failure: no attempt is spent,
      -- and the task is not looked at again until the pause ends.
      v_state := 'queued';
      v_error := v_t.error;
      v_wait  := coalesce((p_s->>'retry_after_ms')::bigint, 0) * interval '1 millisecond';
      IF v_wait <= interval '0' THEN
        v_wait := p_cfg.pool_pause;
      END IF;
      v_available := lectio_limited(v_t.reader, v_t.scope, v_t.leased_at, v_wait, p_cfg, p_now);
    WHEN 'returned' THEN
      v_state := 'queued';
      v_error := v_t.error;
    ELSE
      RAISE EXCEPTION 'lectio: the settle of %/% has the outcome %', v_t.parse_id, v_t.task_id, v_outcome;
  END CASE;
  IF v_state = 'failed' AND v_error IS NULL THEN
    v_error := jsonb_build_object('code', 'internal');
  END IF;

  -- The breaker is the reader's, and reads what the call said about it: a
  -- failure another call may repeat counts, a success resets, and a wait, a
  -- failure of the page itself and a returned task say nothing.
  IF v_t.reader IS NOT NULL AND p_s->>'health' = 'failure' THEN
    UPDATE pools SET failures = failures + 1,
           opened_at = CASE
             WHEN opened_at IS NULL AND failures + 1 >= p_cfg.breaker_failures THEN p_now
             -- A call claimed while the breaker was open is the trial, and a
             -- trial that fails opens the breaker again from now.
             WHEN opened_at IS NOT NULL AND v_t.leased_at > opened_at THEN p_now
             ELSE opened_at END,
           updated_at = p_now
     WHERE reader = v_t.reader;
  ELSIF v_t.reader IS NOT NULL AND p_s->>'health' = 'success' THEN
    -- Written only when there is something to clear, so a reader that works
    -- costs no write per page.
    UPDATE pools SET failures = 0, opened_at = NULL, trial_at = NULL, updated_at = p_now
     WHERE reader = v_t.reader AND (failures <> 0 OR opened_at IS NOT NULL);
  END IF;

  -- The charge is corrected to what the attempt used, and never below 1:
  -- that is the worker slot it held.
  v_units := greatest(1, coalesce((p_s->>'units')::integer, 0));
  PERFORM lectio_correct(v_t, v_units,
    CASE WHEN v_state = 'queued' THEN lectio_lane(v_t.kind, v_t.pin, v_chain_at) END);

  UPDATE tasks SET
         state = v_state, attempt = v_attempt, available_at = v_available, calling = false,
         chain_at = v_chain_at, invalid = v_invalid, escalated = v_escalated,
         lease_owner = CASE WHEN v_state = 'queued' THEN NULL ELSE lease_owner END,
         reader      = CASE WHEN v_state = 'queued' THEN NULL ELSE reader END,
         scope       = CASE WHEN v_state = 'queued' THEN NULL ELSE scope END,
         charged     = CASE WHEN v_state = 'queued' THEN 0 ELSE v_units END,
         output      = CASE WHEN v_state = 'succeeded' THEN p_s->>'output' ELSE output END,
         result      = CASE WHEN v_state = 'succeeded' THEN nullif(p_s->'result', 'null'::jsonb) ELSE result END,
         calls = calls + v_calls, input_tokens = input_tokens + v_in, output_tokens = output_tokens + v_out,
         error = v_error,
         settled_at = CASE WHEN v_state = 'queued' THEN NULL ELSE p_now END
   WHERE parse_id = v_t.parse_id AND task_id = v_t.task_id;

  -- What an attempt spent is recorded whatever its outcome.
  IF v_calls <> 0 OR v_in <> 0 OR v_out <> 0 THEN
    UPDATE parses SET calls = calls + v_calls, input_tokens = input_tokens + v_in,
           output_tokens = output_tokens + v_out
     WHERE parse_id = v_t.parse_id;
  END IF;

  IF v_state <> 'queued' THEN
    PERFORM lectio_settled(v_t.parse_id, v_t.kind, v_state, v_error, p_s, p_now);
  END IF;
  RETURN true;
END $$;

DROP FUNCTION IF EXISTS lectio_meter(text, text, text, text, integer, integer, bigint, bigint, timestamptz);
DROP TABLE IF EXISTS usage;

DROP TABLE IF EXISTS page_events;
DROP INDEX IF EXISTS tasks_events;
ALTER TABLE tasks DROP COLUMN event;
ALTER TABLE parses
  DROP COLUMN events,
  DROP COLUMN retried_at;

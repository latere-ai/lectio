-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- Back to the schema of 000005: a parse has no extraction and no figure run,
-- the task store dispatches pages alone, and the policy holds no order of
-- describers. The extractions and the figures that are queued or running are
-- dropped first, with the counters of queued and running tasks moved with
-- them, so the counters stay a count of the rows. Each function 000006
-- replaced is the one an earlier migration wrote again, and what the meter
-- recorded of extractions and figures stays in it.

-- No statement below waits longer than 5 seconds for a table's lock, as in
-- the up file: this file too is one transaction, and fails whole.
SET LOCAL lock_timeout = '5s';

SELECT lectio_drop(t.parse_id, t.kind, NULL)
  FROM (SELECT DISTINCT parse_id, kind FROM tasks WHERE kind IN ('extract', 'figure')) t;

DROP FUNCTION IF EXISTS lectio_overdue(timestamptz);
DROP FUNCTION IF EXISTS lectio_run_end(text, timestamptz);
DROP FUNCTION IF EXISTS lectio_figures(text);
DROP FUNCTION IF EXISTS lectio_figures_start(text, timestamptz);
DROP FUNCTION IF EXISTS lectio_field(text, text);
DROP FUNCTION IF EXISTS lectio_fields(text);
DROP FUNCTION IF EXISTS lectio_field_view(fields);
DROP FUNCTION IF EXISTS lectio_field_create(text, timestamptz);
DROP FUNCTION IF EXISTS lectio_fields_release(text, timestamptz);
DROP FUNCTION IF EXISTS lectio_drop(text, text, text);
DROP FUNCTION IF EXISTS lectio_runs(text[], text[]);
DROP FUNCTION IF EXISTS lectio_parse_close(text, timestamptz);
DROP FUNCTION IF EXISTS lectio_parse_delete(text, text, timestamptz);
DROP FUNCTION IF EXISTS lectio_claim(text, integer, boolean, settings, timestamptz, text[]);

-- lectio_configure, as 000001 wrote it.
--
-- lectio_configure writes what the process was configured with: the queue's
-- settings, the class weights, and one pool per reader. A pool that exists
-- keeps its breaker state, and a pool whose reader is gone from the
-- configuration is removed with its scopes. p_config is a JSON document
-- bound as text.
CREATE OR REPLACE FUNCTION lectio_configure(p_config text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  c jsonb := p_config::jsonb;
BEGIN
  INSERT INTO settings (one, lease, sweep_interval, attempts, expiries, backoff_base, backoff_cap,
                        pool_recovery, pool_resume, pool_pause, breaker_failures, breaker_open,
                        scope_by_group, read_chain, extract_chain)
  VALUES (true,
          (c->>'lease_ms')::bigint * interval '1 millisecond',
          (c->>'sweep_ms')::bigint * interval '1 millisecond',
          (c->>'attempts')::integer,
          (c->>'expiries')::integer,
          (c->>'backoff_base_ms')::bigint * interval '1 millisecond',
          (c->>'backoff_cap_ms')::bigint * interval '1 millisecond',
          (c->>'pool_recovery_ms')::bigint * interval '1 millisecond',
          (c->>'pool_resume_ms')::bigint * interval '1 millisecond',
          (c->>'pool_pause_ms')::bigint * interval '1 millisecond',
          (c->>'breaker_failures')::integer,
          (c->>'breaker_open_ms')::bigint * interval '1 millisecond',
          (c->>'scope_by_group')::boolean,
          ARRAY(SELECT jsonb_array_elements_text(c->'read_chain')),
          ARRAY(SELECT jsonb_array_elements_text(c->'extract_chain')))
  ON CONFLICT (one) DO UPDATE SET
    lease = EXCLUDED.lease, sweep_interval = EXCLUDED.sweep_interval,
    attempts = EXCLUDED.attempts, expiries = EXCLUDED.expiries,
    backoff_base = EXCLUDED.backoff_base, backoff_cap = EXCLUDED.backoff_cap,
    pool_recovery = EXCLUDED.pool_recovery, pool_resume = EXCLUDED.pool_resume,
    pool_pause = EXCLUDED.pool_pause, breaker_failures = EXCLUDED.breaker_failures,
    breaker_open = EXCLUDED.breaker_open, scope_by_group = EXCLUDED.scope_by_group,
    read_chain = EXCLUDED.read_chain, extract_chain = EXCLUDED.extract_chain;

  -- A class keeps its virtual time when its weight changes.
  INSERT INTO class_service (class, weight)
  VALUES (0, (c->>'interactive_weight')::integer), (1, (c->>'batch_weight')::integer)
  ON CONFLICT (class) DO UPDATE SET weight = EXCLUDED.weight;

  INSERT INTO pools (reader, max_in_flight, cost)
  SELECT r->>'reader', (r->>'max_in_flight')::integer, (r->>'cost')::integer
    FROM jsonb_array_elements(c->'pools') r
  ON CONFLICT (reader) DO UPDATE
    SET max_in_flight = EXCLUDED.max_in_flight, cost = EXCLUDED.cost, updated_at = now();
  DELETE FROM pool_scopes
   WHERE reader NOT IN (SELECT r->>'reader' FROM jsonb_array_elements(c->'pools') r);
  DELETE FROM pools
   WHERE reader NOT IN (SELECT r->>'reader' FROM jsonb_array_elements(c->'pools') r);
END $$;

-- lectio_lanes, as 000002 wrote it.
--
-- lectio_lanes answers the lanes whose tasks can run when the readers of
-- p_room have room and the readers of p_skip are passed over: the lane of
-- the tasks that call no model, each position of a chain whose reader has
-- room, and the lanes pinned to a reader with room. A pinned task passes
-- over nothing: it waits for its reader.
CREATE OR REPLACE FUNCTION lectio_lanes(p_room text[], p_skip text[], p_cfg settings)
RETURNS text[] LANGUAGE sql IMMUTABLE AS $$
  SELECT ARRAY['']
      || ARRAY(SELECT lectio_lane('page', NULL, i)
                 FROM generate_series(0, coalesce(array_length((p_cfg).read_chain, 1), 0) - 1) i
                WHERE lectio_reader((p_cfg).read_chain, i, p_skip) = ANY (p_room))
      || ARRAY(SELECT lectio_lane('extract', NULL, i)
                 FROM generate_series(0, coalesce(array_length((p_cfg).extract_chain, 1), 0) - 1) i
                WHERE lectio_reader((p_cfg).extract_chain, i, p_skip) = ANY (p_room))
      || ARRAY(SELECT 'page:' || r FROM unnest(p_room) r)
      || ARRAY(SELECT 'extract:' || r FROM unnest(p_room) r);
$$;

-- lectio_stop, as 000004 wrote it.
--
-- lectio_stop ends a parse that has not ended, as canceled or as failed with
-- an error, and cancels its queued and leased tasks in the same transaction.
-- A canceled task holds no slot from here on, and a settle for it matches no
-- lease. The parse's retention runs from here, and the pages it reserved and
-- did not read go back to their day. It reports whether the parse was still
-- open.
CREATE OR REPLACE FUNCTION lectio_stop(p_parse text, p_state text, p_error jsonb, p_now timestamptz)
RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE
  v_p parses%ROWTYPE;
  v_l record;
BEGIN
  UPDATE parses SET state = p_state, error = p_error, finished_at = p_now, expires_at = p_now + retention
   WHERE parse_id = p_parse AND state IN ('queued', 'running')
  RETURNING * INTO v_p;
  IF NOT FOUND THEN
    RETURN false;
  END IF;
  FOR v_l IN
    WITH hit AS (
      SELECT task_id, state, lane FROM tasks
       WHERE parse_id = p_parse AND state IN ('queued', 'leased') FOR UPDATE
    ), gone AS (
      UPDATE tasks t SET state = 'canceled', calling = false, settled_at = p_now
        FROM hit WHERE t.parse_id = p_parse AND t.task_id = hit.task_id
    )
    SELECT lane, count(*) FILTER (WHERE state = 'queued')::integer AS queued,
           count(*) FILTER (WHERE state = 'leased')::integer AS running
      FROM hit GROUP BY lane ORDER BY lane
  LOOP
    PERFORM lectio_count(v_p.group_id, v_p.project_id, v_p.class, v_l.lane, -v_l.queued, -v_l.running);
  END LOOP;
  PERFORM lectio_refund(p_parse);
  RETURN true;
END $$;

-- lectio_settled, as 000005 wrote it.
--
-- lectio_settled moves a parse forward when one of its tasks reaches
-- succeeded or failed, in the transaction that settled the task. The graph of
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
    IF v_native AND v_n > 0 THEN
      -- The pages of a format that carries its own structure are there with
      -- prepare and have no task. Each is one change of the parse after the
      -- one that counted them, in the order of the selection, so a stream
      -- tells them one by one as it tells pages a reader read.
      INSERT INTO page_events (parse_id, settled)
      SELECT p_parse, jsonb_agg(jsonb_build_array(e.n, v_p.events + e.i) ORDER BY e.i)
        FROM jsonb_array_elements(v_pages) WITH ORDINALITY AS e(n, i);
      UPDATE parses SET events = events + v_n WHERE parse_id = p_parse;
      -- They are pages read, under no reader and with no call, so a page
      -- means the same in the meter for every format.
      PERFORM lectio_meter(v_p.group_id, v_p.owner, 'page', '', v_n, 0, 0, 0, p_now);
    END IF;
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
    -- The page's row holds the change that settled it, which is the id its
    -- event is sent under. A page the store failed itself came with no
    -- settle: it is the parse's one failed page that holds no change yet.
    IF p_settle IS NOT NULL THEN
      UPDATE tasks SET event = v_p.events WHERE parse_id = p_parse AND task_id = p_settle->>'task';
    ELSE
      UPDATE tasks SET event = v_p.events
       WHERE parse_id = p_parse AND kind = 'page' AND state = 'failed' AND event IS NULL;
    END IF;
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
      -- A parse that was read again still holds the row of the assemble that
      -- ended it. That row runs again, under the tokens after its last, so
      -- the index it writes lies beside the earlier one and never over it.
      UPDATE tasks SET state = 'queued', attempt = 0, expiries = 0, available_at = p_now, lease_owner = NULL,
             charged = 0, output = NULL, result = NULL, error = NULL, settled_at = NULL
       WHERE parse_id = p_parse AND task_id = 'assemble' AND state = 'succeeded'
      RETURNING lane INTO v_lane;
      IF FOUND THEN
        PERFORM lectio_count(v_p.group_id, v_p.project_id, v_p.class, v_lane, 1, 0);
      ELSE
        PERFORM lectio_enqueue(p_parse, 'assemble', 'assemble', NULL, p_now);
      END IF;
    END IF;
    RETURN;
  END IF;

  -- assemble succeeded: the parse ends here, and there is no task after it.
  -- Everything that was read is kept whichever way it ends, for the parse's
  -- retention from now, and the pages that failed go back to their day. A
  -- parse that fails says why with the code of its failed pages.
  UPDATE parses SET
         state = CASE WHEN pages_failed <= allow_failed_pages THEN 'succeeded' ELSE 'failed' END,
         error = CASE WHEN pages_failed <= allow_failed_pages THEN NULL ELSE jsonb_build_object(
                   'code', lectio_failure(p_parse),
                   'detail', pages_failed || ' of ' || pages_total || ' pages could not be read') END,
         index_key = p_settle->'assemble'->>'index', finished_at = p_now, expires_at = p_now + retention
   WHERE parse_id = p_parse
  RETURNING * INTO STRICT v_p;
  PERFORM lectio_refund(p_parse);
  -- A parse with every page read keeps its counters on its own row and its
  -- output keys in the document index, and no task row. One that ended with
  -- a failed page keeps every row: they are what a retry queues again, and
  -- what the assemble after it finds the pages that were read through.
  IF v_p.pages_failed = 0 THEN
    -- The change that settled each page outlives the page's row, so the
    -- events of the parse are the same before and after.
    WITH gone AS (
      DELETE FROM tasks WHERE parse_id = p_parse AND state = 'succeeded' RETURNING kind, task_id, event
    )
    INSERT INTO page_events (parse_id, settled)
    SELECT p_parse, jsonb_agg(jsonb_build_array(substr(task_id, 6)::integer, event) ORDER BY event)
      FROM gone WHERE kind = 'page' AND event IS NOT NULL
    HAVING count(*) > 0
    ON CONFLICT (parse_id) DO UPDATE SET settled = EXCLUDED.settled;
  END IF;
END $$;

-- lectio_settle, as 000005 wrote it.
--
-- lectio_settle ends one attempt at a task under the fence: it matches only
-- a task that is leased to this worker under the token the worker was given.
-- A task that was canceled, reissued after its worker was taken for dead, or
-- settled before matches no row, and nothing is recorded for it. It reports
-- whether the settle was accepted.
--
-- What the attempt used is metered here, whatever its outcome, under the
-- reader it was claimed for: a page that moves down the policy's chain is
-- metered under each reader that was called for it, and a call that failed
-- or was told to wait is counted as the call it was. A page counts as read
-- when its task succeeds.
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
  v_read      integer;
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

  -- What an attempt spent is recorded whatever its outcome: on its parse,
  -- and in the meter with the page it read, when it read one.
  IF v_calls <> 0 OR v_in <> 0 OR v_out <> 0 THEN
    UPDATE parses SET calls = calls + v_calls, input_tokens = input_tokens + v_in,
           output_tokens = output_tokens + v_out
     WHERE parse_id = v_t.parse_id;
  END IF;
  v_read := CASE WHEN v_t.kind = 'page' AND v_state = 'succeeded' THEN 1 ELSE 0 END;
  IF v_calls <> 0 OR v_in <> 0 OR v_out <> 0 OR v_read <> 0 THEN
    PERFORM lectio_meter(v_t.group_id, (SELECT owner FROM parses WHERE parse_id = v_t.parse_id), v_t.kind, v_t.reader,
                         v_read, v_calls, v_in, v_out, p_now);
  END IF;

  IF v_state <> 'queued' THEN
    PERFORM lectio_settled(v_t.parse_id, v_t.kind, v_state, v_error, p_s, p_now);
  END IF;
  RETURN true;
END $$;

-- lectio_reap, as 000001 wrote it.
--
-- lectio_reap returns the tasks of every worker whose lease ran out to the
-- queue and removes those workers. Each task comes back with expiries + 1
-- and its attempts unchanged: at most one of them killed the worker, so none
-- is charged an attempt, and each runs alone from here on. A task at the
-- bound of expiries died while it ran alone and is the cause: it is failed.
-- The fairness charge made at the claim stands: the call the task was claimed
-- for may have been made, and no one is left to say what it used.
CREATE OR REPLACE FUNCTION lectio_reap(p_worker text, p_cfg settings, p_now timestamptz)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_t     tasks%ROWTYPE;
  v_error jsonb;
BEGIN
  FOR v_t IN
    SELECT t.* FROM tasks t JOIN workers w ON w.worker_id = t.lease_owner
     WHERE t.state = 'leased' AND w.expires_at <= p_now AND w.worker_id <> p_worker
     ORDER BY t.parse_id, t.task_id
  LOOP
    IF v_t.expiries + 1 >= p_cfg.expiries THEN
      -- The code is the task's own: an input that kills its worker is an
      -- unreadable page, a file intake cannot open, or a fault of ours.
      v_error := jsonb_build_object(
        'code', CASE v_t.kind WHEN 'page' THEN 'page_unreadable' WHEN 'prepare' THEN 'document_corrupt' ELSE 'internal' END,
        'detail', 'the task ended ' || (v_t.expiries + 1) || ' worker processes');
      UPDATE tasks SET state = 'failed', expiries = expiries + 1, calling = false, error = v_error, settled_at = p_now
       WHERE parse_id = v_t.parse_id AND task_id = v_t.task_id;
      PERFORM lectio_count(v_t.group_id, v_t.project_id, v_t.class, v_t.lane, 0, -1);
      PERFORM lectio_settled(v_t.parse_id, v_t.kind, 'failed', v_error, NULL, p_now);
    ELSE
      UPDATE tasks SET state = 'queued', expiries = expiries + 1, calling = false,
             lease_owner = NULL, reader = NULL, scope = NULL, charged = 0
       WHERE parse_id = v_t.parse_id AND task_id = v_t.task_id;
      PERFORM lectio_count(v_t.group_id, v_t.project_id, v_t.class, v_t.lane, 1, -1);
    END IF;
  END LOOP;
  DELETE FROM workers WHERE expires_at <= p_now AND worker_id <> p_worker;
END $$;

-- lectio_claim, as 000002 wrote it.
--
-- lectio_claim hands the worker up to p_free tasks and answers them as a
-- JSON array. Each task is the dispatch decision of
-- specs/006-fairness-and-priority.md: the class, then the group, then the
-- project, then the project's first task, each among those that hold a task
-- that can run now, and each by start-time fair queuing with a clock of its
-- own. A task that calls a model can run only when the reader it would be
-- read by has room (specs/007-model-capacity.md), so a task with no room is
-- never read, never written and never charged. p_sleep answers when the
-- earliest pause ends among the scopes that held a reader back, when the
-- claim ended with a slot still free.
CREATE FUNCTION lectio_claim(p_worker text, p_free integer, p_idle boolean, p_cfg settings, p_now timestamptz,
                                        OUT p_claims jsonb, OUT p_sleep timestamptz)
LANGUAGE plpgsql AS $$
DECLARE
  v_free   integer := p_free;
  v_held   integer;
  v_alone  boolean;
  v_solo   boolean;
  v_clock  bigint;
  v_shut   text[];
  v_open   text[];
  v_room   text[];
  v_paused text[];
  v_lanes  text[];
  v_among  text[];
  v_wake   timestamptz;
  v_scope  text;
  v_reader text;
  v_cost   integer;
  v_token  bigint;
  v_start  bigint;
  v_c      record;
  v_g      record;
  v_p      record;
  v_t      tasks%ROWTYPE;
  v_found  boolean;
BEGIN
  p_claims := '[]'::jsonb;
  -- A task whose worker died before runs alone: only a worker that runs
  -- nothing takes it, and a worker that holds one takes nothing more.
  SELECT count(*), coalesce(bool_or(expiries > 0), false) INTO v_held, v_alone
    FROM tasks WHERE state = 'leased' AND lease_owner = p_worker;

  WHILE v_free > 0 AND NOT v_alone LOOP
    v_solo  := p_idle AND v_held = 0;
    v_found := false;
    p_sleep := NULL;
    SELECT clock INTO STRICT v_clock FROM dispatch;

    -- The readers whose breaker admits no call now: it is open and its open
    -- period is not over, or its one trial is in flight. A page passes over
    -- such a reader. A slot is a leased task that is calling, so a trial in
    -- flight is a row.
    SELECT coalesce(array_agg(p.reader), '{}') INTO v_shut
      FROM pools p
     WHERE p.opened_at IS NOT NULL
       AND (p_now < p.opened_at + p_cfg.breaker_open
            OR EXISTS (SELECT 1 FROM tasks t
                        WHERE t.state = 'leased' AND t.calling AND t.reader = p.reader
                          AND t.leased_at > p.opened_at));
    -- The readers whose pool admits a call now, whatever the key: the
    -- breaker admits one and the calls in flight are below the pool's bound.
    -- A reader that is not shut and not here is full, and is waited for.
    SELECT coalesce(array_agg(p.reader), '{}') INTO v_open
      FROM pools p
     WHERE NOT (p.reader = ANY (v_shut))
       AND (SELECT count(*) FROM tasks t
             WHERE t.state = 'leased' AND t.calling AND t.reader = p.reader) < p.max_in_flight;
    -- The lanes whose tasks can run. With one key for every group the
    -- readers with room and the readers passed over are the same for all of
    -- them, and so are the lanes. With a key per group they are the group's
    -- own, read when the group is looked at; the groups are then chosen
    -- among those that hold a task in a lane some key could run: one with a
    -- reader, from its position on, whose pool admits a call.
    IF p_cfg.scope_by_group THEN
      v_among := lectio_lanes(v_open, ARRAY(SELECT reader FROM pools WHERE NOT (reader = ANY (v_open))), p_cfg);
    ELSE
      SELECT r.p_room, r.p_wake, r.p_paused INTO v_room, p_sleep, v_paused FROM lectio_room(v_open, '', p_cfg, p_now) r;
      v_lanes := lectio_lanes(v_room, v_shut || v_paused, p_cfg);
      v_among := v_lanes;
    END IF;

    <<decision>>
    FOR v_c IN
      SELECT class, weight, vtime, clock, greatest(vtime, v_clock) AS start
        FROM class_service ORDER BY start, class
    LOOP
      -- A group is looked at only when it holds a queued task in a lane that
      -- can run, which lane_service answers without reading a task. A group
      -- whose every task waits for a reader with no room is passed over
      -- here, at no cost that grows with its queue.
      FOR v_g IN
        SELECT gs.group_id, gs.vtime, gs.clock, g.weight, greatest(gs.vtime, v_c.clock) AS start
          FROM group_service gs JOIN groups g USING (group_id)
         WHERE gs.class = v_c.class AND gs.queued > 0
           AND (g.max_running = 0
                OR (SELECT sum(r.running) FROM group_service r WHERE r.group_id = gs.group_id) < g.max_running)
           AND EXISTS (SELECT 1 FROM lane_service l
                        WHERE l.group_id = gs.group_id AND l.class = gs.class AND l.queued > 0
                          AND l.lane = ANY (v_among))
         ORDER BY start, gs.group_id
      LOOP
        v_scope := CASE WHEN p_cfg.scope_by_group THEN v_g.group_id ELSE '' END;
        IF p_cfg.scope_by_group THEN
          SELECT r.p_room, r.p_wake, r.p_paused INTO v_room, v_wake, v_paused FROM lectio_room(v_open, v_scope, p_cfg, p_now) r;
          v_lanes := lectio_lanes(v_room, v_shut || v_paused, p_cfg);
          p_sleep := least(p_sleep, v_wake);
        END IF;

        FOR v_p IN
          SELECT ps.project_id, ps.vtime, pr.weight, greatest(ps.vtime, v_g.clock) AS start
            FROM project_service ps JOIN projects pr USING (group_id, project_id)
           WHERE ps.group_id = v_g.group_id AND ps.class = v_c.class AND ps.queued > 0
             AND EXISTS (SELECT 1 FROM lane_service l
                          WHERE l.group_id = ps.group_id AND l.project_id = ps.project_id AND l.class = ps.class
                            AND l.queued > 0 AND l.lane = ANY (v_lanes))
           ORDER BY start, ps.project_id
        LOOP
          -- The project's first task that can run: the best of the first
          -- task of each of its lanes that can run, each one probe of
          -- tasks_runnable.
          SELECT t.* INTO v_t
            FROM lane_service l
           CROSS JOIN LATERAL (
                 SELECT q.* FROM tasks q
                  WHERE q.state = 'queued' AND q.group_id = v_g.group_id AND q.project_id = v_p.project_id
                    AND q.class = v_c.class AND q.lane = l.lane
                    AND q.available_at <= p_now AND (q.expiries = 0 OR v_solo)
                  ORDER BY q.priority DESC, q.seq, q.created_at, q.parse_id, q.task_id
                  LIMIT 1) t
           WHERE l.group_id = v_g.group_id AND l.project_id = v_p.project_id AND l.class = v_c.class
             AND l.queued > 0 AND l.lane = ANY (v_lanes)
           ORDER BY t.priority DESC, t.seq, t.created_at, t.parse_id, t.task_id
           LIMIT 1;
          IF FOUND THEN
            v_found := true;
            EXIT decision;
          END IF;
        END LOOP;
      END LOOP;
    END LOOP;
    EXIT WHEN NOT v_found;
    p_sleep := NULL;

    -- The slot: the pinned reader, or the first reader of the policy's chain
    -- from the task's position on that is not passed over. The task's lane
    -- could run, so that reader has room. A task that calls no model holds
    -- no slot and costs 1.
    v_reader := NULL;
    v_cost   := 1;
    IF v_t.kind IN ('page', 'extract') THEN
      IF v_t.pin IS NOT NULL THEN
        v_reader := v_t.pin;
      ELSE
        v_reader := lectio_reader(
          CASE WHEN v_t.kind = 'page' THEN p_cfg.read_chain ELSE p_cfg.extract_chain END,
          v_t.chain_at, v_shut || v_paused);
      END IF;
      SELECT cost INTO STRICT v_cost FROM pools WHERE reader = v_reader;
      -- A claim admitted while the breaker is open is its one trial.
      UPDATE pools SET trial_at = p_now WHERE reader = v_reader AND opened_at IS NOT NULL;
      -- The ceiling the scope recovered since it was stored is written with
      -- the claim that uses it, so waiting writes nothing. The instant moves
      -- by whole recovery intervals, so the part of one that has passed still
      -- counts toward the next.
      UPDATE pool_scopes s SET
             ceiling = lectio_ceiling(s.ceiling, s.raised_at, p.max_in_flight, p_cfg.pool_recovery, p_now),
             raised_at = s.raised_at + p_cfg.pool_recovery * floor(
               extract(epoch FROM p_now - s.raised_at) / extract(epoch FROM p_cfg.pool_recovery))
        FROM pools p
       WHERE p.reader = s.reader AND s.reader = v_reader AND s.scope = v_scope
         AND lectio_ceiling(s.ceiling, s.raised_at, p.max_in_flight, p_cfg.pool_recovery, p_now) <> s.ceiling;
    END IF;

    UPDATE tasks SET state = 'leased', lease_owner = p_worker, leased_at = p_now, lease_token = lease_token + 1,
           reader = v_reader, scope = CASE WHEN v_reader IS NULL THEN NULL ELSE v_scope END,
           calling = v_reader IS NOT NULL, charged = v_cost
     WHERE parse_id = v_t.parse_id AND task_id = v_t.task_id
    RETURNING lease_token INTO STRICT v_token;
    UPDATE parses SET state = 'running', started_at = p_now
     WHERE parse_id = v_t.parse_id AND state = 'queued';

    -- The charge, the same rule at each level from the inside out: the level
    -- starts at the later of its own virtual time and the clock of the level
    -- above, the clock moves to that start, and the level's virtual time to
    -- the start plus cost over its weight.
    v_start := greatest(v_g.vtime, v_c.clock);
    UPDATE group_service SET clock = greatest(v_p.vtime, v_g.clock),
           vtime = v_start + v_cost::bigint * 1000000 / v_g.weight,
           queued = queued - 1, running = running + 1
     WHERE group_id = v_g.group_id AND class = v_c.class;
    UPDATE project_service SET vtime = greatest(v_p.vtime, v_g.clock) + v_cost::bigint * 1000000 / v_p.weight,
           queued = queued - 1, running = running + 1
     WHERE group_id = v_g.group_id AND project_id = v_p.project_id AND class = v_c.class;
    UPDATE lane_service SET queued = queued - 1
     WHERE group_id = v_g.group_id AND project_id = v_p.project_id AND class = v_c.class AND lane = v_t.lane;
    UPDATE class_service SET clock = v_start,
           vtime = greatest(v_c.vtime, v_clock) + v_cost::bigint * 1000000 / v_c.weight
     WHERE class = v_c.class;
    UPDATE dispatch SET clock = greatest(v_c.vtime, v_clock);

    p_claims := p_claims || jsonb_build_object(
      'parse', v_t.parse_id, 'task', v_t.task_id, 'kind', v_t.kind, 'token', v_token,
      'group', v_t.group_id, 'project', v_t.project_id, 'attempt', v_t.attempt, 'expiries', v_t.expiries,
      'pin', v_t.pin, 'reader', v_reader, 'scope', CASE WHEN v_reader IS NULL THEN NULL ELSE v_scope END,
      'alone', v_t.expiries > 0);
    v_free  := v_free - 1;
    v_held  := v_held + 1;
    v_alone := v_t.expiries > 0;
  END LOOP;
END $$;

-- lectio_context, as 000003 wrote it.
--
-- lectio_context is what a claim carries of its parse, by the kind of its
-- task: prepare is told the file and the caller's selection; a page the
-- manifest without its list of pages, the languages hinted, and, when the
-- parse takes earlier reads, where the result of the same read is kept; and
-- assemble the manifest whole.
CREATE OR REPLACE FUNCTION lectio_context(p_claim jsonb)
RETURNS jsonb LANGUAGE sql AS $$
  SELECT jsonb_strip_nulls(jsonb_build_object(
           'owner', p.owner,
           'file', CASE WHEN p_claim->>'kind' = 'prepare' AND f.file_id IS NOT NULL THEN jsonb_build_object(
                     'key', f.object_key, 'name', f.name, 'media_type', f.media_type) END,
           'pages', CASE WHEN p_claim->>'kind' = 'prepare' THEN p.options->>'pages' END,
           'languages', CASE WHEN p_claim->>'kind' = 'page' THEN p.options->'languages' END,
           'manifest', CASE p_claim->>'kind' WHEN 'page' THEN p.manifest - 'selected' WHEN 'assemble' THEN p.manifest END,
           'reuse', CASE WHEN p_claim->>'kind' = 'page' AND coalesce((p.options->>'reuse')::boolean, false) THEN
                      (SELECT r.output FROM reads r
                        WHERE r.owner = p.owner AND r.read_key = p.read_base || ':' || substr(p_claim->>'task', 6)) END))
    FROM parses p LEFT JOIN files f ON f.file_id = p.file_id
   WHERE p.parse_id = p_claim->>'parse';
$$;

-- lectio_retry, as 000005 wrote it.
--
-- lectio_retry queues again the pages of an owner's parse that failed
-- (specs/004-durable-tasks.md). It is for a parse that assemble ended with a
-- failed page, whether the parse failed or allowed them: the failed page
-- rows go back to queued with their attempts, their expiries and their place
-- in the policy's chain as a new task has them, their tokens kept, and the
-- parse is running again with only those pages open. The pages that were
-- read are not read again: their rows stayed, and the assemble that follows
-- reads them where they are.
--
-- The parse stays in its group and is held to that group's bounds as they
-- stand: the group's row is locked first, as a submit locks it, so a retry
-- and a submit count the group's parses one after the other, and the failed
-- pages are reserved again, of the day of the retry. The parse has as long
-- from the retry as its submit gave it.
--
-- It answers retried, missing, not_terminal, queue_full, budget_exhausted,
-- or why there is nothing to read again: nothing for a parse with no failed
-- page, unassembled for one that ended before assemble did, by a cancel, its
-- deadline or a task of its own, gone for one whose task rows are no longer
-- all there, and expired for one whose retention has ended.
CREATE OR REPLACE FUNCTION lectio_retry(p_owner text, p_parse text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_now    timestamptz := coalesce(p_now, now());
  v_day    date := (v_now AT TIME ZONE 'UTC')::date;
  v_group  text;
  v_p      parses%ROWTYPE;
  v_max    integer;
  v_budget integer;
  v_failed integer;
  v_rows   integer;
  v_l      record;
BEGIN
  SELECT group_id INTO v_group FROM parses WHERE parse_id = p_parse AND owner = p_owner;
  IF NOT FOUND THEN
    RETURN 'missing';
  END IF;
  SELECT max_queued, pages_per_day INTO v_max, v_budget FROM groups WHERE group_id = v_group FOR UPDATE;
  SELECT * INTO v_p FROM parses WHERE parse_id = p_parse AND owner = p_owner FOR UPDATE;
  IF NOT FOUND THEN
    RETURN 'missing';
  END IF;
  IF v_p.state IN ('queued', 'running') THEN
    RETURN 'not_terminal';
  END IF;
  IF v_p.expires_at <= v_now THEN
    -- The retention sweep may be removing what the parse wrote.
    RETURN 'expired';
  END IF;
  IF v_p.index_key IS NULL THEN
    RETURN 'unassembled';
  END IF;
  IF v_p.pages_failed = 0 THEN
    RETURN 'nothing';
  END IF;
  SELECT count(*) FILTER (WHERE state = 'failed'), count(*) INTO v_failed, v_rows
    FROM tasks WHERE parse_id = p_parse AND kind = 'page';
  IF v_failed <> v_p.pages_failed OR v_rows <> v_p.pages_total THEN
    RETURN 'gone';
  END IF;

  IF coalesce(v_max, 0) > 0
     AND (SELECT count(*) FROM parses WHERE group_id = v_group AND state IN ('queued', 'running')) >= v_max THEN
    RETURN 'queue_full';
  END IF;
  IF coalesce(v_budget, 0) > 0 AND NOT lectio_reserve(v_group, v_day, v_failed, v_budget) THEN
    RETURN 'budget_exhausted';
  END IF;

  FOR v_l IN
    WITH back AS (
      UPDATE tasks SET state = 'queued', attempt = 0, expiries = 0, chain_at = 0, invalid = 0, escalated = false,
             available_at = v_now, lease_owner = NULL, reader = NULL, scope = NULL, calling = false, charged = 0,
             error = NULL, settled_at = NULL, event = NULL
       WHERE parse_id = p_parse AND kind = 'page' AND state = 'failed'
      RETURNING lane
    )
    SELECT lane, count(*)::integer AS n FROM back GROUP BY lane ORDER BY lane
  LOOP
    PERFORM lectio_count(v_p.group_id, v_p.project_id, v_p.class, v_l.lane, v_l.n, 0);
  END LOOP;

  -- The pages the parse read before stay counted on the day they were
  -- reserved on. What it holds from here on is those and the pages queued
  -- again, so what it gives back when it ends is the pages of this retry it
  -- did not read, to the day of this retry.
  UPDATE parses SET state = 'running', error = NULL, finished_at = NULL, expires_at = NULL, index_key = NULL,
         pages_open = v_failed, pages_failed = 0,
         deadline_at = v_now + (deadline_at - coalesce(retried_at, created_at)), retried_at = v_now,
         reserved = CASE WHEN coalesce(v_budget, 0) > 0 THEN pages_done + v_failed ELSE reserved END,
         reserved_day = CASE WHEN coalesce(v_budget, 0) > 0 THEN v_day ELSE reserved_day END
   WHERE parse_id = p_parse;
  RETURN 'retried';
END $$;

-- lectio_exchange, as 000003 wrote it.
--
-- lectio_exchange is the one call a worker makes. In order it renews the
-- worker's lease, settles what finished under the fence, reports which of
-- the tasks the worker still runs are no longer its own, claims new tasks,
-- and runs the sweeps that are due. p_request and the answer are the JSON
-- documents of internal/tasks, bound and returned as text.
CREATE OR REPLACE FUNCTION lectio_exchange(p_worker text, p_request text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_now      timestamptz := coalesce(p_now, now());
  v_req      jsonb := p_request::jsonb;
  v_settles  jsonb := coalesce(nullif(v_req->'settles', 'null'::jsonb), '[]'::jsonb);
  v_holds    jsonb := coalesce(nullif(v_req->'held', 'null'::jsonb), '[]'::jsonb);
  v_cfg      settings%ROWTYPE;
  v_seen     timestamptz;
  v_refused  jsonb := '[]'::jsonb;
  v_lost     jsonb;
  v_claims   jsonb := '[]'::jsonb;
  v_sleep    timestamptz;
  v_s        jsonb;
  v_t        tasks%ROWTYPE;
  v_parse    text;
BEGIN
  PERFORM lectio_lock();
  SELECT * INTO STRICT v_cfg FROM settings;

  -- 1. Renew. A worker whose row is gone was taken for dead by another
  -- worker, which returned its tasks to the queue: nothing it sent is
  -- recorded. A row that is past its expiry and still there was reaped by no
  -- one, so its tasks are still its own and it renews: that is what makes a
  -- stall of the database itself expire nothing.
  SELECT seen_at INTO v_seen FROM workers WHERE worker_id = p_worker;
  IF NOT FOUND THEN
    RETURN jsonb_build_object(
      'gone', true,
      'refused', (SELECT coalesce(jsonb_agg(jsonb_build_object('parse', e->'parse', 'task', e->'task')), '[]'::jsonb)
                    FROM jsonb_array_elements(v_settles) e),
      'lost', (SELECT coalesce(jsonb_agg(jsonb_build_object('parse', e->'parse', 'task', e->'task')), '[]'::jsonb)
                 FROM jsonb_array_elements(v_holds) e),
      'claims', '[]'::jsonb, 'sleep_until', NULL)::text;
  END IF;
  UPDATE workers SET seen_at = v_now, expires_at = v_now + v_cfg.lease WHERE worker_id = p_worker;

  -- 2. Settle.
  FOR v_s IN SELECT e FROM jsonb_array_elements(v_settles) e LOOP
    IF NOT lectio_settle(p_worker, v_s, v_cfg, v_now) THEN
      v_refused := v_refused || jsonb_build_object('parse', v_s->'parse', 'task', v_s->'task');
    END IF;
  END LOOP;

  -- 3. Report the tasks the worker still runs that are no longer its own:
  -- canceled, or reissued. The worker stops them.
  SELECT coalesce(jsonb_agg(jsonb_build_object('parse', e->'parse', 'task', e->'task')), '[]'::jsonb) INTO v_lost
    FROM jsonb_array_elements(v_holds) e
   WHERE NOT EXISTS (
         SELECT 1 FROM tasks t
          WHERE t.parse_id = e->>'parse' AND t.task_id = e->>'task' AND t.state = 'leased'
            AND t.lease_owner = p_worker AND t.lease_token = (e->>'token')::bigint);

  -- A task that is leased to the worker and that it neither settled nor
  -- holds was handed to it in a reply it never read. It goes back to the
  -- queue with no attempt and no expiry counted, so an answer that was lost
  -- on its way strands no task behind a worker that keeps renewing.
  FOR v_t IN
    SELECT t.* FROM tasks t
     WHERE t.state = 'leased' AND t.lease_owner = p_worker
       AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(v_holds) e
                        WHERE e->>'parse' = t.parse_id AND e->>'task' = t.task_id
                          AND (e->>'token')::bigint = t.lease_token)
     ORDER BY t.parse_id, t.task_id
  LOOP
    UPDATE tasks SET state = 'queued', calling = false, lease_owner = NULL, reader = NULL, scope = NULL, charged = 0
     WHERE parse_id = v_t.parse_id AND task_id = v_t.task_id;
    PERFORM lectio_count(v_t.group_id, v_t.project_id, v_t.class, v_t.lane, 1, -1);
  END LOOP;

  IF coalesce((v_req->>'shutdown')::boolean, false) THEN
    -- The worker's last exchange: what it still holds goes back to the queue
    -- with no attempt and no expiry counted, and its row is removed, so a
    -- rolling restart costs neither an attempt nor a lease period. A task the
    -- worker says nothing about keeps the charge of its claim; one it gives
    -- back with a returned settle is corrected to what it used.
    FOR v_t IN
      SELECT * FROM tasks WHERE state = 'leased' AND lease_owner = p_worker ORDER BY parse_id, task_id
    LOOP
      UPDATE tasks SET state = 'queued', calling = false, lease_owner = NULL, reader = NULL, scope = NULL, charged = 0
       WHERE parse_id = v_t.parse_id AND task_id = v_t.task_id;
      PERFORM lectio_count(v_t.group_id, v_t.project_id, v_t.class, v_t.lane, 1, -1);
    END LOOP;
    DELETE FROM workers WHERE worker_id = p_worker;
  ELSE
    -- 4. Claim.
    SELECT p_claims, p_sleep INTO v_claims, v_sleep
      FROM lectio_claim(p_worker, coalesce((v_req->>'free')::integer, 0),
                        coalesce((v_req->>'idle')::boolean, false), v_cfg, v_now);
    -- Each claim carries what its task needs of its parse, so a worker makes
    -- no second statement to run it.
    SELECT coalesce(jsonb_agg(c.claim || jsonb_build_object('context', lectio_context(c.claim)) ORDER BY c.at), '[]'::jsonb)
      INTO v_claims FROM jsonb_array_elements(v_claims) WITH ORDINALITY AS c(claim, at);

    -- 5. The sweeps that are due. A worker returns the tasks of dead
    -- workers only when its own previous round trip was within the last
    -- third of a lease: one that was away longer cannot tell a dead worker
    -- from a database nobody could reach, and reaps no one.
    IF v_seen >= v_now - v_cfg.lease / 3 THEN
      UPDATE sweeps SET ran_at = v_now WHERE name = 'workers' AND ran_at <= v_now - v_cfg.sweep_interval;
      IF FOUND THEN
        PERFORM lectio_reap(p_worker, v_cfg, v_now);
      END IF;
    END IF;
    UPDATE sweeps SET ran_at = v_now WHERE name = 'deadlines' AND ran_at <= v_now - v_cfg.sweep_interval;
    IF FOUND THEN
      FOR v_parse IN
        SELECT parse_id FROM parses WHERE state IN ('queued', 'running') AND deadline_at <= v_now ORDER BY parse_id
      LOOP
        PERFORM lectio_stop(v_parse, 'failed', jsonb_build_object(
          'code', 'deadline_exceeded', 'detail', 'the parse did not end by its deadline'), v_now);
      END LOOP;
    END IF;
  END IF;

  RETURN jsonb_build_object('gone', false, 'refused', v_refused, 'lost', v_lost,
                            'claims', v_claims, 'sleep_until', v_sleep)::text;
END $$;

-- lectio_parse_delete, as 000003 wrote it.
--
-- lectio_parse_delete removes an owner's parse that has ended, with its
-- tasks and the reads kept from it. The caller removes the parse's objects
-- first. It answers deleted, missing, or not_terminal.
CREATE FUNCTION lectio_parse_delete(p_owner text, p_parse text)
RETURNS text LANGUAGE plpgsql AS $$
BEGIN
  DELETE FROM parses WHERE parse_id = p_parse AND owner = p_owner AND state IN ('succeeded', 'failed', 'canceled');
  IF FOUND THEN
    RETURN 'deleted';
  END IF;
  IF EXISTS (SELECT 1 FROM parses WHERE parse_id = p_parse AND owner = p_owner) THEN
    RETURN 'not_terminal';
  END IF;
  RETURN 'missing';
END $$;

-- lectio_parse_expire, as 000004 wrote it.
--
-- lectio_parse_expire removes a parse whose retention has ended, with its
-- tasks and the reads kept from it, whoever owns it. The caller removes the
-- parse's objects first. It reports whether a row was removed: a parse that
-- is not there, or whose retention has not ended, is left alone.
CREATE OR REPLACE FUNCTION lectio_parse_expire(p_parse text, p_now timestamptz DEFAULT NULL)
RETURNS boolean LANGUAGE plpgsql AS $$
BEGIN
  DELETE FROM parses
   WHERE parse_id = p_parse AND expires_at <= coalesce(p_now, now()) AND state IN ('succeeded', 'failed', 'canceled');
  RETURN FOUND;
END $$;

DROP FUNCTION IF EXISTS lectio_chain(text, settings);

DROP TABLE IF EXISTS descriptions;
DROP TABLE IF EXISTS figures;
DROP TABLE IF EXISTS figure_runs;
DROP TABLE IF EXISTS fields;
ALTER TABLE parses DROP COLUMN IF EXISTS described, DROP COLUMN IF EXISTS fields;
ALTER TABLE tasks DROP COLUMN IF EXISTS stuck;
ALTER TABLE settings DROP COLUMN IF EXISTS describe_chain;

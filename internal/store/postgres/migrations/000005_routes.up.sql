-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- What a parse says of itself when pages of it failed, reading those pages
-- again, following a parse as it changes, the meters, and the view of the
-- queue (specs/003-api.md, specs/004-durable-tasks.md,
-- specs/005-parse-graph.md, specs/006-fairness-and-priority.md,
-- specs/007-model-capacity.md, specs/013-limits-and-usage.md).

-- A parse that was read again has until its deadline from the retry, for as
-- long as its submit gave it, so the time it was given is counted from here.
ALTER TABLE parses ADD COLUMN retried_at timestamptz;   -- when its failed pages were last queued again

-- The events of a parse are read from its rows (specs/003-api.md). events
-- counts the changes of a parse that a stream reports: its state and its
-- progress. A parse is written at 1, and a page's row holds the change that
-- settled it, so the rows say in which order what happened and a stream on
-- any replica numbers the same event the same.
ALTER TABLE parses ADD COLUMN events bigint NOT NULL DEFAULT 1;
ALTER TABLE tasks  ADD COLUMN event  bigint;            -- the change of its parse that settled the page
CREATE INDEX tasks_events ON tasks (parse_id, event) WHERE event IS NOT NULL;

-- page_events holds the same for the pages that have no task row: the pages
-- of a format prepare wrote itself, and the pages of a parse that ended
-- with every page read, whose rows are deleted. It is one row per parse and
-- a few bytes per page, so a stream opened at any time tells every page of
-- a parse under the id it had, and the task table keeps no settled page.
CREATE TABLE page_events (
  parse_id text  PRIMARY KEY REFERENCES parses ON DELETE CASCADE,
  settled  jsonb NOT NULL                 -- [[page, change], ...] in the order the pages settled
);

-- lectio_changed counts a change of a parse. It is the one place the count
-- is raised: a trigger on the row, so no function that moves a parse can
-- leave a change a stream would then never report.
CREATE FUNCTION lectio_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.events := OLD.events + 1;
  RETURN NEW;
END $$;
CREATE TRIGGER parses_changed BEFORE UPDATE ON parses FOR EACH ROW
  WHEN (OLD.state IS DISTINCT FROM NEW.state
        OR OLD.pages_total  IS DISTINCT FROM NEW.pages_total  OR OLD.pages_open   IS DISTINCT FROM NEW.pages_open
        OR OLD.pages_done   IS DISTINCT FROM NEW.pages_done   OR OLD.pages_failed IS DISTINCT FROM NEW.pages_failed
        OR OLD.pages_reused IS DISTINCT FROM NEW.pages_reused OR (OLD.manifest IS NULL) <> (NEW.manifest IS NULL))
  EXECUTE FUNCTION lectio_changed();

-- usage is the meter (specs/013-limits-and-usage.md): what was read and
-- what it cost in model calls and tokens, summed by the hour a task settled
-- in, the group and the owner of its parse, the kind of the task and the
-- reader the attempt was claimed for. It is written with each settle, so a
-- row is the sum of an hour and the table grows with the hours a group, an
-- owner and a reader were active in, and never with pages or parses. A row
-- holds counts and names, and no content.
CREATE TABLE usage (
  hour          timestamptz NOT NULL,           -- the start of the hour, in UTC, the work settled in
  group_id      text   NOT NULL,
  owner         text   NOT NULL,
  kind          text   NOT NULL,                -- the kind of task: page
  reader        text   NOT NULL DEFAULT '',     -- the reader the attempt was claimed for; '' for pages no reader read
  pages         bigint NOT NULL DEFAULT 0,      -- pages that were read
  calls         bigint NOT NULL DEFAULT 0,      -- model calls, the ones that failed or were told to wait included
  input_tokens  bigint NOT NULL DEFAULT 0,
  output_tokens bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (hour, group_id, owner, kind, reader)
);

-- lectio_meter adds what one settle used to the meter's row of its hour. It
-- is the meter's one writer.
CREATE FUNCTION lectio_meter(p_group text, p_owner text, p_kind text, p_reader text,
                             p_pages integer, p_calls integer, p_in bigint, p_out bigint, p_now timestamptz)
RETURNS void LANGUAGE sql AS $$
  INSERT INTO usage AS u (hour, group_id, owner, kind, reader, pages, calls, input_tokens, output_tokens)
  VALUES (date_trunc('hour', p_now, 'UTC'), p_group, p_owner, p_kind, coalesce(p_reader, ''), p_pages, p_calls, p_in, p_out)
  ON CONFLICT (hour, group_id, owner, kind, reader) DO UPDATE SET
    pages = u.pages + EXCLUDED.pages, calls = u.calls + EXCLUDED.calls,
    input_tokens = u.input_tokens + EXCLUDED.input_tokens, output_tokens = u.output_tokens + EXCLUDED.output_tokens;
$$;

-- lectio_failure is the code a parse fails with when more of its pages
-- failed than it allows: the code its failed pages carry when they all carry
-- one, and page_unreadable when they differ. A parse whose every failed page
-- was refused for budget fails with budget_exhausted, and one whose reader
-- could not be reached with reader_unavailable, so a caller reads from the
-- parse alone whether reading it again can help.
CREATE FUNCTION lectio_failure(p_parse text)
RETURNS text LANGUAGE sql STABLE AS $$
  SELECT CASE WHEN count(DISTINCT error->>'code') = 1 THEN min(error->>'code') ELSE 'page_unreadable' END
    FROM tasks WHERE parse_id = p_parse AND kind = 'page' AND state = 'failed';
$$;

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
CREATE FUNCTION lectio_retry(p_owner text, p_parse text, p_now timestamptz DEFAULT NULL)
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

-- lectio_events answers what a stream of a parse's events is told
-- (specs/003-api.md): the parse as it stands, with the count of its changes,
-- and the pages that settled after the change p_after, oldest first and at
-- most p_limit of them. A page is read from its task's row while it has one
-- and from page_events after. It is one statement, so the parse and its
-- pages are read from one snapshot: every page that settled up to the
-- parse's count is in it and none after. It answers the JSON null for a
-- parse that is not there.
CREATE FUNCTION lectio_events(p_parse text, p_after bigint, p_limit integer)
RETURNS text LANGUAGE sql STABLE AS $$
  SELECT coalesce((
    SELECT jsonb_build_object(
             'parse', to_jsonb(p),
             'pages', (SELECT coalesce(jsonb_agg(jsonb_build_object(
                                'page', e.page, 'state', e.state, 'event', e.event, 'error', e.error)
                              ORDER BY e.event), '[]'::jsonb)
                         FROM (SELECT substr(t.task_id, 6)::integer AS page, t.state, t.event, t.error
                                 FROM tasks t
                                WHERE t.parse_id = p.parse_id AND t.event > p_after
                               UNION ALL
                               SELECT (s.pair->>0)::integer, 'succeeded', (s.pair->>1)::bigint, NULL::jsonb
                                 FROM page_events k CROSS JOIN LATERAL jsonb_array_elements(k.settled) AS s(pair)
                                WHERE k.parse_id = p.parse_id AND (s.pair->>1)::bigint > p_after
                               ORDER BY event LIMIT p_limit) e))::text
      FROM parses p WHERE p.parse_id = p_parse), 'null');
$$;

-- lectio_usage reads the meter (specs/013-limits-and-usage.md): its sums by
-- one key, the group, the owner or the reader, over fixed intervals of an
-- hour or a day in UTC, for the intervals that begin in [from, to). owners
-- and groups narrow the read to those owners and those groups, and a JSON
-- null or no member is every one. p_query is a JSON document bound as text,
-- and the answer a JSON array ordered by interval and key.
CREATE FUNCTION lectio_usage(p_query text)
RETURNS text LANGUAGE sql STABLE AS $$
  WITH q AS (
    SELECT j->>'by' AS by, j->>'interval' AS step, (j->>'from')::timestamptz AS since, (j->>'to')::timestamptz AS until,
           CASE WHEN jsonb_typeof(j->'owners') = 'array' THEN ARRAY(SELECT jsonb_array_elements_text(j->'owners')) END AS owners,
           CASE WHEN jsonb_typeof(j->'groups') = 'array' THEN ARRAY(SELECT jsonb_array_elements_text(j->'groups')) END AS groups
      FROM (SELECT p_query::jsonb AS j) doc
  ), sums AS (
    SELECT CASE q.by WHEN 'owner' THEN u.owner WHEN 'reader' THEN u.reader ELSE u.group_id END AS key,
           date_trunc(q.step, u.hour, 'UTC') AS start,
           sum(u.pages) AS pages, sum(u.calls) AS calls,
           sum(u.input_tokens) AS input_tokens, sum(u.output_tokens) AS output_tokens
      FROM usage u CROSS JOIN q
     WHERE u.hour >= q.since AND u.hour < q.until
       AND (q.owners IS NULL OR u.owner = ANY (q.owners))
       AND (q.groups IS NULL OR u.group_id = ANY (q.groups))
     GROUP BY 1, 2
  )
  SELECT coalesce(jsonb_agg(jsonb_build_object(
           'key', key, 'start', start, 'pages', pages, 'calls', calls,
           'input_tokens', input_tokens, 'output_tokens', output_tokens) ORDER BY start, key), '[]'::jsonb)::text
    FROM sums;
$$;

-- lectio_queue answers the view of the queue
-- (specs/006-fairness-and-priority.md, specs/007-model-capacity.md): each
-- group with its weight, its bounds, its parses that have not ended and its
-- queued and running tasks per class, the same for each of its projects,
-- and each reader's pool with its calls in flight, its breaker and the key
-- scopes that were limited. p_groups is a JSON array of group ids bound as
-- text, or the JSON null for every group that holds a parse that has not
-- ended. A read of some groups counts the calls of those groups alone and
-- lists their scopes, and the scope of the key every group shares. It is
-- one statement, so every number in it is of one instant, and it reads the
-- counters the claim keeps and no queued task.
CREATE FUNCTION lectio_queue(p_groups text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE sql STABLE AS $$
  WITH q AS (
    SELECT CASE WHEN jsonb_typeof(p_groups::jsonb) = 'array'
                THEN ARRAY(SELECT jsonb_array_elements_text(p_groups::jsonb)) END AS groups,
           coalesce(p_now, now()) AS at
  )
  SELECT jsonb_build_object(
    'groups', (
      SELECT coalesce(jsonb_agg(jsonb_build_object(
               'group', g.group_id, 'weight', g.weight, 'max_running', g.max_running, 'max_queued', g.max_queued,
               'parses', (SELECT count(*) FROM parses p WHERE p.group_id = g.group_id AND p.state IN ('queued', 'running')),
               'classes', (SELECT coalesce(jsonb_agg(jsonb_build_object(
                             'class', s.class, 'queued', s.queued, 'running', s.running, 'vtime', s.vtime) ORDER BY s.class), '[]'::jsonb)
                             FROM group_service s WHERE s.group_id = g.group_id),
               'projects', (SELECT coalesce(jsonb_agg(jsonb_build_object(
                              'project', pr.project_id, 'weight', pr.weight,
                              'parses', (SELECT count(*) FROM parses p
                                          WHERE p.group_id = pr.group_id AND p.project_id = pr.project_id
                                            AND p.state IN ('queued', 'running')),
                              'classes', (SELECT coalesce(jsonb_agg(jsonb_build_object(
                                            'class', s.class, 'queued', s.queued, 'running', s.running, 'vtime', s.vtime) ORDER BY s.class), '[]'::jsonb)
                                            FROM project_service s
                                           WHERE s.group_id = pr.group_id AND s.project_id = pr.project_id)) ORDER BY pr.project_id), '[]'::jsonb)
                              FROM projects pr WHERE pr.group_id = g.group_id))
             ORDER BY g.group_id), '[]'::jsonb)
        FROM groups g CROSS JOIN q
       WHERE g.group_id = ANY (q.groups)
          OR (q.groups IS NULL AND EXISTS (SELECT 1 FROM parses p WHERE p.group_id = g.group_id AND p.state IN ('queued', 'running')))),
    'pools', (
      SELECT coalesce(jsonb_agg(jsonb_build_object(
               'reader', p.reader, 'max_in_flight', p.max_in_flight,
               'in_flight', (SELECT count(*) FROM tasks t
                              WHERE t.state = 'leased' AND t.calling AND t.reader = p.reader
                                AND (q.groups IS NULL OR t.group_id = ANY (q.groups))),
               'breaker', CASE WHEN p.opened_at IS NULL THEN 'closed'
                               WHEN q.at < p.opened_at + cfg.breaker_open THEN 'open'
                               ELSE 'trial' END,
               'scopes', (SELECT coalesce(jsonb_agg(jsonb_build_object(
                            'scope', s.scope,
                            'ceiling', lectio_ceiling(s.ceiling, s.raised_at, p.max_in_flight, cfg.pool_recovery, q.at),
                            'paused_until', CASE WHEN s.paused_until > q.at THEN s.paused_until END,
                            'in_flight', (SELECT count(*) FROM tasks t
                                           WHERE t.state = 'leased' AND t.calling AND t.reader = s.reader AND t.scope = s.scope
                                             AND (q.groups IS NULL OR t.group_id = ANY (q.groups)))) ORDER BY s.scope), '[]'::jsonb)
                            FROM pool_scopes s
                           WHERE s.reader = p.reader AND (q.groups IS NULL OR s.scope = '' OR s.scope = ANY (q.groups))))
             ORDER BY p.reader), '[]'::jsonb)
        FROM pools p CROSS JOIN q CROSS JOIN settings cfg))::text;
$$;

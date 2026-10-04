-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- What a parse says of itself when pages of it failed, reading those pages
-- again, and following a parse as it changes (specs/003-api.md,
-- specs/004-durable-tasks.md, specs/005-parse-graph.md,
-- specs/007-model-capacity.md).

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

-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- Back to a parse that fails with page_unreadable whatever its pages failed
-- with, keeps the rows of its failed tasks alone, cannot be read again, and
-- counts none of its changes. lectio_settled is the one of 000004 again.

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

DROP TABLE IF EXISTS page_events;
DROP INDEX IF EXISTS tasks_events;
ALTER TABLE tasks DROP COLUMN event;
ALTER TABLE parses
  DROP COLUMN events,
  DROP COLUMN retried_at;

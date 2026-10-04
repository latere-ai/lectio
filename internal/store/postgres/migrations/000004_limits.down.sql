-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- Back to a parse held to the fair queue's bounds alone: no ceiling of its
-- own on its pages, no pages of a day, and nothing that expires. The 3
-- functions that held them are the ones of 000001 and 000003 again.

DROP FUNCTION IF EXISTS lectio_parse_expire(text, timestamptz);
DROP FUNCTION IF EXISTS lectio_expired(integer, timestamptz);
DROP FUNCTION IF EXISTS lectio_file_until(files);

-- lectio_submit as 000003 wrote it: a parse and its prepare task, with what
-- its caller chose and the bounds of the fair queue.
CREATE OR REPLACE FUNCTION lectio_submit(p_submit text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  s         jsonb := p_submit::jsonb;
  v_now     timestamptz := coalesce(p_now, now());
  v_parse   text := s->>'parse';
  v_owner   text := s->>'owner';
  v_group   text := s->>'group';
  v_project text := coalesce(s->>'project', '');
  v_class   smallint := (s->>'class')::smallint;
  v_file    text := nullif(s->>'file', '');
  v_key     text := nullif(s->>'idempotency_key', '');
  v_max     integer;
  v_open    integer;
  v_seen    record;
BEGIN
  INSERT INTO groups AS g (group_id, weight, max_running, max_queued, max_priority, updated_at)
  VALUES (v_group, greatest(1, least(1000, coalesce((s->>'weight')::integer, 1))),
          coalesce((s->>'max_running')::integer, 0), coalesce((s->>'max_queued')::integer, 0),
          coalesce((s->>'max_priority')::integer, 0), v_now)
  ON CONFLICT (group_id) DO UPDATE
    SET weight = EXCLUDED.weight, max_running = EXCLUDED.max_running, max_queued = EXCLUDED.max_queued,
        max_priority = EXCLUDED.max_priority, updated_at = EXCLUDED.updated_at
  RETURNING g.max_queued INTO v_max;

  INSERT INTO projects (group_id, project_id, weight, updated_at)
  VALUES (v_group, v_project, greatest(1, least(1000, coalesce((s->>'project_weight')::integer, 1))), v_now)
  ON CONFLICT (group_id, project_id) DO UPDATE
    SET weight = EXCLUDED.weight, updated_at = EXCLUDED.updated_at;

  IF EXISTS (SELECT 1 FROM parses WHERE parse_id = v_parse) THEN
    RETURN jsonb_build_object('result', 'exists', 'parse', v_parse)::text;
  END IF;
  IF v_key IS NOT NULL THEN
    SELECT parse_id, body_digest, created_at INTO v_seen FROM parses WHERE owner = v_owner AND idempotency_key = v_key;
    IF FOUND AND v_seen.created_at > v_now - interval '24 hours' THEN
      RETURN jsonb_build_object(
        'result', CASE WHEN v_seen.body_digest IS NOT DISTINCT FROM s->>'body_digest' THEN 'exists' ELSE 'conflict' END,
        'parse', v_seen.parse_id)::text;
    ELSIF FOUND THEN
      -- The key's 24 hours are over: it is free for this submit.
      UPDATE parses SET idempotency_key = NULL WHERE parse_id = v_seen.parse_id;
    END IF;
  END IF;
  -- The lock on the file's row holds a delete of the file back until this
  -- parse is written, and a delete that came first is seen here.
  IF v_file IS NOT NULL THEN
    PERFORM 1 FROM files WHERE file_id = v_file AND owner = v_owner AND deleted_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
      RETURN jsonb_build_object('result', 'file_not_found', 'parse', v_parse)::text;
    END IF;
  END IF;
  IF v_max > 0 THEN
    SELECT count(*) INTO v_open FROM parses WHERE group_id = v_group AND state IN ('queued', 'running');
    IF v_open >= v_max THEN
      RETURN jsonb_build_object('result', 'queue_full', 'parse', v_parse)::text;
    END IF;
  END IF;

  BEGIN
    INSERT INTO parses (parse_id, owner, group_id, project_id, class, priority, pin, allow_failed_pages, deadline_at, created_at,
                        file_id, options, labels, origin, idempotency_key, body_digest, read_base)
    VALUES (v_parse, v_owner, v_group, v_project, v_class, coalesce((s->>'priority')::integer, 0),
            nullif(s->>'pin', ''), coalesce((s->>'allow_failed_pages')::integer, 0),
            v_now + (s->>'deadline_ms')::bigint * interval '1 millisecond', v_now,
            v_file, coalesce(nullif(s->'options', 'null'::jsonb), '{}'::jsonb),
            coalesce(nullif(s->'labels', 'null'::jsonb), '{}'::jsonb), nullif(s->'origin', 'null'::jsonb),
            v_key, CASE WHEN v_key IS NOT NULL THEN s->>'body_digest' END, nullif(s->>'read_base', ''));
  EXCEPTION WHEN unique_violation THEN
    -- Two submits of one owner with one key, in two groups, met here: the
    -- other was written first, and this one answers with it.
    SELECT parse_id, body_digest INTO STRICT v_seen FROM parses WHERE owner = v_owner AND idempotency_key = v_key;
    RETURN jsonb_build_object(
      'result', CASE WHEN v_seen.body_digest IS NOT DISTINCT FROM s->>'body_digest' THEN 'exists' ELSE 'conflict' END,
      'parse', v_seen.parse_id)::text;
  END;

  -- A group or a project with no row for the class starts at virtual time 0,
  -- which is never above the clock: it is dispatched from the clock on.
  INSERT INTO group_service (group_id, class) VALUES (v_group, v_class) ON CONFLICT DO NOTHING;
  INSERT INTO project_service (group_id, project_id, class) VALUES (v_group, v_project, v_class) ON CONFLICT DO NOTHING;
  PERFORM lectio_enqueue(v_parse, 'prepare', 'prepare', NULL, v_now);
  RETURN jsonb_build_object('result', 'created', 'parse', v_parse)::text;
END $$;

-- lectio_stop ends a parse that has not ended, as canceled or as failed with
-- an error, and cancels its queued and leased tasks in the same transaction.
-- A canceled task holds no slot from here on, and a settle for it matches no
-- lease. It reports whether the parse was still open.
CREATE OR REPLACE FUNCTION lectio_stop(p_parse text, p_state text, p_error jsonb, p_now timestamptz)
RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE
  v_p parses%ROWTYPE;
  v_l record;
BEGIN
  UPDATE parses SET state = p_state, error = p_error, finished_at = p_now
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
  RETURN true;
END $$;

-- lectio_settled moves a parse forward when one of its tasks reaches
-- succeeded or failed, in the transaction that settled the task. The graph of
-- a parse is fixed, so there is no edge to follow: prepare writes the page
-- tasks, the page that takes pages_open to 0 writes assemble, and assemble
-- ends the parse. p_settle is the settle the worker sent, or NULL when the
-- store failed the task itself.
CREATE OR REPLACE FUNCTION lectio_settled(p_parse text, p_kind text, p_state text, p_error jsonb, p_settle jsonb, p_now timestamptz)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_p      parses%ROWTYPE;
  v_pages  jsonb;
  v_native boolean;
  v_n      integer;
  v_open   integer;
  v_lane   text;
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
    UPDATE parses SET manifest = p_settle->'prepare'->'manifest', pages_total = v_n,
           pages_open = CASE WHEN v_native THEN 0 ELSE v_n END,
           pages_done = CASE WHEN v_native THEN v_n ELSE 0 END
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
  -- Everything that was read is kept whichever way it ends.
  UPDATE parses SET
         state = CASE WHEN pages_failed <= allow_failed_pages THEN 'succeeded' ELSE 'failed' END,
         error = CASE WHEN pages_failed <= allow_failed_pages THEN NULL ELSE jsonb_build_object(
                   'code', 'page_unreadable',
                   'detail', pages_failed || ' of ' || pages_total || ' pages could not be read') END,
         index_key = p_settle->'assemble'->>'index', finished_at = p_now
   WHERE parse_id = p_parse;
  -- The parse row keeps the counters and the document index the output keys.
  DELETE FROM tasks WHERE parse_id = p_parse AND state = 'succeeded';
END $$;

DROP FUNCTION IF EXISTS lectio_refund(text);
DROP FUNCTION IF EXISTS lectio_reserve(text, date, integer, integer);

DELETE FROM sweeps WHERE name = 'retention';
DROP INDEX IF EXISTS files_deleted;
DROP INDEX IF EXISTS files_kept;
ALTER TABLE files
  DROP COLUMN retention,
  DROP COLUMN kept_until;
DROP INDEX IF EXISTS parses_by_file;
DROP INDEX IF EXISTS parses_expiry;
ALTER TABLE parses
  DROP COLUMN max_pages,
  DROP COLUMN reserved,
  DROP COLUMN reserved_day,
  DROP COLUMN retention,
  DROP COLUMN expires_at;
DROP TABLE IF EXISTS group_days;
ALTER TABLE groups DROP COLUMN pages_per_day;

-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- What an allow holds a parse and its group to beyond the fair queue's
-- bounds, and how long what is stored is kept
-- (specs/012-identity-and-authorization.md, specs/013-limits-and-usage.md,
-- specs/014-sources-and-retention.md): the most pages one parse may select,
-- the pages a group may have read in a day, and the retention of a file and
-- of a parse.

-- A group's pages for a day are a setting of the group, refreshed by every
-- submit as its other bounds are. 0 is no budget.
ALTER TABLE groups ADD COLUMN pages_per_day integer NOT NULL DEFAULT 0;

-- group_days holds what a group's parses were promised of a day. A page is
-- counted when its parse counts its pages, before any is read, and given
-- back when the parse ends without having read it.
CREATE TABLE group_days (
  group_id text    NOT NULL,
  day      date    NOT NULL,               -- in UTC
  reserved integer NOT NULL DEFAULT 0,     -- pages promised to parses that began this day
  PRIMARY KEY (group_id, day)
);

ALTER TABLE parses
  ADD COLUMN max_pages    integer NOT NULL DEFAULT 0,  -- most pages the parse may select; 0 is no bound of its own
  ADD COLUMN reserved     integer NOT NULL DEFAULT 0,  -- pages it holds of its group's day
  ADD COLUMN reserved_day date,                        -- the day they were reserved on
  ADD COLUMN retention    interval,                    -- how long it is kept after it ended; NULL keeps it
  ADD COLUMN expires_at   timestamptz;                 -- when it is removed, known once it has ended
CREATE INDEX parses_expiry   ON parses (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX parses_by_file  ON parses (file_id);

-- A file is kept until kept_until, and after that for its retention past
-- the end of the last parse that read it. Nothing is written to a file's
-- row when a parse ends: what a file is kept until is read from its parses.
ALTER TABLE files
  ADD COLUMN retention  interval,                      -- how long it is kept; NULL keeps it
  ADD COLUMN kept_until timestamptz;                   -- its last upload plus its retention
CREATE INDEX files_kept    ON files (kept_until) WHERE kept_until IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX files_deleted ON files (deleted_at) WHERE deleted_at IS NOT NULL;

INSERT INTO sweeps (name) VALUES ('retention');

-- lectio_reserve promises p_pages of a group's day to a parse, unless the
-- day would then hold more than p_limit. It is one statement: the row of
-- the day is written or locked by it, so of 2 reservations that would
-- together pass the limit the second waits for the first and then sees its
-- number. It reports whether the pages were reserved.
CREATE FUNCTION lectio_reserve(p_group text, p_day date, p_pages integer, p_limit integer)
RETURNS boolean LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO group_days AS d (group_id, day, reserved)
  SELECT p_group, p_day, p_pages WHERE p_pages <= p_limit
  ON CONFLICT (group_id, day) DO UPDATE SET reserved = d.reserved + EXCLUDED.reserved
   WHERE d.reserved + EXCLUDED.reserved <= p_limit;
  RETURN FOUND;
END $$;

-- lectio_refund gives back the pages a parse reserved and did not read, to
-- the day it reserved them on. It runs when the parse ends, and a second
-- run gives nothing back.
CREATE FUNCTION lectio_refund(p_parse text)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_p parses%ROWTYPE;
BEGIN
  SELECT * INTO v_p FROM parses WHERE parse_id = p_parse AND reserved > pages_done;
  IF NOT FOUND THEN
    RETURN;
  END IF;
  UPDATE group_days SET reserved = greatest(0, reserved - (v_p.reserved - v_p.pages_done))
   WHERE group_id = v_p.group_id AND day = v_p.reserved_day;
  UPDATE parses SET reserved = pages_done WHERE parse_id = p_parse;
END $$;

-- lectio_submit writes a parse and its prepare task, as in 000003, with the
-- limits its allow named: the group's pages for a day, the most pages the
-- parse may select, and how long it is kept. A group whose day holds nothing
-- more is answered budget_exhausted at once, before a parse is written.
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
  v_keep    bigint := coalesce((s->>'retention_ms')::bigint, 0);
  v_max     integer;
  v_budget  integer;
  v_open    integer;
  v_seen    record;
BEGIN
  INSERT INTO groups AS g (group_id, weight, max_running, max_queued, max_priority, pages_per_day, updated_at)
  VALUES (v_group, greatest(1, least(1000, coalesce((s->>'weight')::integer, 1))),
          coalesce((s->>'max_running')::integer, 0), coalesce((s->>'max_queued')::integer, 0),
          coalesce((s->>'max_priority')::integer, 0), coalesce((s->>'pages_per_day')::integer, 0), v_now)
  ON CONFLICT (group_id) DO UPDATE
    SET weight = EXCLUDED.weight, max_running = EXCLUDED.max_running, max_queued = EXCLUDED.max_queued,
        max_priority = EXCLUDED.max_priority, pages_per_day = EXCLUDED.pages_per_day, updated_at = EXCLUDED.updated_at
  RETURNING g.max_queued, g.pages_per_day INTO v_max, v_budget;

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
  -- A group with nothing left of its day is refused here. One with pages
  -- left is held to them in prepare, where the parse's pages are counted.
  IF v_budget > 0 AND v_budget <= coalesce((
       SELECT reserved FROM group_days WHERE group_id = v_group AND day = (v_now AT TIME ZONE 'UTC')::date), 0) THEN
    RETURN jsonb_build_object('result', 'budget_exhausted', 'parse', v_parse)::text;
  END IF;

  BEGIN
    INSERT INTO parses (parse_id, owner, group_id, project_id, class, priority, pin, allow_failed_pages, deadline_at, created_at,
                        file_id, options, labels, origin, idempotency_key, body_digest, read_base, max_pages, retention)
    VALUES (v_parse, v_owner, v_group, v_project, v_class, coalesce((s->>'priority')::integer, 0),
            nullif(s->>'pin', ''), coalesce((s->>'allow_failed_pages')::integer, 0),
            v_now + (s->>'deadline_ms')::bigint * interval '1 millisecond', v_now,
            v_file, coalesce(nullif(s->'options', 'null'::jsonb), '{}'::jsonb),
            coalesce(nullif(s->'labels', 'null'::jsonb), '{}'::jsonb), nullif(s->'origin', 'null'::jsonb),
            v_key, CASE WHEN v_key IS NOT NULL THEN s->>'body_digest' END, nullif(s->>'read_base', ''),
            coalesce((s->>'max_pages')::integer, 0),
            CASE WHEN v_keep > 0 THEN v_keep * interval '1 millisecond' END);
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

-- lectio_file_until is when a file may be removed: its last upload plus its
-- retention, or its retention past the end of the last parse that read it,
-- whichever is later. NULL is a file that is kept.
CREATE FUNCTION lectio_file_until(p_file files)
RETURNS timestamptz LANGUAGE sql STABLE AS $$
  SELECT greatest(p_file.kept_until,
                  (SELECT max(p.finished_at) FROM parses p WHERE p.file_id = p_file.file_id) + p_file.retention);
$$;

-- lectio_expired claims the retention sweep when it is due and answers what
-- is to be removed: the parses whose retention has ended, and the files
-- whose delete it begins here. due is false when another process ran the
-- sweep within the sweep interval, and then nothing is listed.
--
-- A file's delete begins as lectio_file_delete begins one: the row is
-- locked before its parses are looked at, so a submit that names the file is
-- wholly before the mark or wholly after it, and the row is marked, so the
-- file is gone for every caller. The caller removes the object and then the
-- row. A file whose delete began and did not finish, in a sweep or in a
-- request, is listed again, so a process that stopped between the object
-- and the row leaves nothing behind for good. A file a parse still reads,
-- or read within the file's retention, is kept.
--
-- A parse is listed and not changed: the caller removes the objects under
-- its prefix and then calls lectio_parse_expire.
CREATE FUNCTION lectio_expired(p_limit integer, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_now    timestamptz := coalesce(p_now, now());
  v_cfg    settings%ROWTYPE;
  v_file   text;
  v_one    jsonb;
  v_parses jsonb;
  v_files  jsonb;
BEGIN
  SELECT * INTO STRICT v_cfg FROM settings;
  UPDATE sweeps SET ran_at = v_now WHERE name = 'retention' AND ran_at <= v_now - v_cfg.sweep_interval;
  IF NOT FOUND THEN
    RETURN jsonb_build_object('due', false, 'parses', '[]'::jsonb, 'files', '[]'::jsonb)::text;
  END IF;

  SELECT coalesce(jsonb_agg(e.parse_id ORDER BY e.expires_at, e.parse_id), '[]'::jsonb) INTO v_parses
    FROM (SELECT parse_id, expires_at FROM parses
           WHERE expires_at <= v_now AND state IN ('succeeded', 'failed', 'canceled')
           ORDER BY expires_at, parse_id LIMIT p_limit) e;

  -- The deletes that began earlier and did not finish. One that began within
  -- the sweep interval is still the request's or the sweep's own.
  SELECT coalesce(jsonb_agg(jsonb_build_object('file', d.file_id, 'key', d.object_key) ORDER BY d.deleted_at, d.file_id), '[]'::jsonb)
    INTO v_files
    FROM (SELECT file_id, object_key, deleted_at FROM files
           WHERE deleted_at <= v_now - v_cfg.sweep_interval
           ORDER BY deleted_at, file_id LIMIT p_limit) d;

  FOR v_file IN
    SELECT f.file_id FROM files f
     WHERE f.deleted_at IS NULL AND f.kept_until <= v_now
       AND NOT EXISTS (SELECT 1 FROM parses p WHERE p.file_id = f.file_id AND p.state IN ('queued', 'running'))
       AND lectio_file_until(f) <= v_now
     ORDER BY f.kept_until, f.file_id LIMIT p_limit
  LOOP
    PERFORM 1 FROM files WHERE file_id = v_file AND deleted_at IS NULL FOR UPDATE;
    CONTINUE WHEN NOT FOUND;
    UPDATE files f SET deleted_at = v_now
     WHERE f.file_id = v_file
       AND NOT EXISTS (SELECT 1 FROM parses p WHERE p.file_id = f.file_id AND p.state IN ('queued', 'running'))
       AND lectio_file_until(f) <= v_now
    RETURNING jsonb_build_object('file', f.file_id, 'key', f.object_key) INTO v_one;
    IF FOUND THEN
      v_files := v_files || v_one;
    END IF;
  END LOOP;

  RETURN jsonb_build_object('due', true, 'parses', v_parses, 'files', v_files)::text;
END $$;

-- lectio_parse_expire removes a parse whose retention has ended, with its
-- tasks and the reads kept from it, whoever owns it. The caller removes the
-- parse's objects first. It reports whether a row was removed: a parse that
-- is not there, or whose retention has not ended, is left alone.
CREATE FUNCTION lectio_parse_expire(p_parse text, p_now timestamptz DEFAULT NULL)
RETURNS boolean LANGUAGE plpgsql AS $$
BEGIN
  DELETE FROM parses
   WHERE parse_id = p_parse AND expires_at <= coalesce(p_now, now()) AND state IN ('succeeded', 'failed', 'canceled');
  RETURN FOUND;
END $$;

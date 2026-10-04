-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- Back to the control plane's own rows: no file, no member of a parse its
-- caller chose, no kept read, and a claim that carries only its task.

DROP FUNCTION IF EXISTS lectio_exchange(text, text, timestamptz);
DROP FUNCTION IF EXISTS lectio_settled(text, text, text, jsonb, jsonb, timestamptz);
DROP FUNCTION IF EXISTS lectio_context(jsonb);
DROP FUNCTION IF EXISTS lectio_parse_delete(text, text);
DROP FUNCTION IF EXISTS lectio_file_delete(text, text, timestamptz);
DROP FUNCTION IF EXISTS lectio_submit(text, timestamptz);

DROP TABLE IF EXISTS reads;
DROP INDEX IF EXISTS parses_file;
DROP INDEX IF EXISTS parses_owner;
DROP INDEX IF EXISTS parses_idempotency;
ALTER TABLE parses
  DROP COLUMN file_id,
  DROP COLUMN options,
  DROP COLUMN labels,
  DROP COLUMN origin,
  DROP COLUMN idempotency_key,
  DROP COLUMN body_digest,
  DROP COLUMN read_base,
  DROP COLUMN pages_reused;
DROP TABLE IF EXISTS files;

CREATE FUNCTION lectio_submit(p_submit text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  s         jsonb := p_submit::jsonb;
  v_now     timestamptz := coalesce(p_now, now());
  v_parse   text := s->>'parse';
  v_group   text := s->>'group';
  v_project text := coalesce(s->>'project', '');
  v_class   smallint := (s->>'class')::smallint;
  v_max     integer;
  v_open    integer;
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
    RETURN 'exists';
  END IF;
  IF v_max > 0 THEN
    SELECT count(*) INTO v_open FROM parses WHERE group_id = v_group AND state IN ('queued', 'running');
    IF v_open >= v_max THEN
      RETURN 'queue_full';
    END IF;
  END IF;

  INSERT INTO parses (parse_id, owner, group_id, project_id, class, priority, pin, allow_failed_pages, deadline_at, created_at)
  VALUES (v_parse, s->>'owner', v_group, v_project, v_class, coalesce((s->>'priority')::integer, 0),
          nullif(s->>'pin', ''), coalesce((s->>'allow_failed_pages')::integer, 0),
          v_now + (s->>'deadline_ms')::bigint * interval '1 millisecond', v_now);

  -- A group or a project with no row for the class starts at virtual time 0,
  -- which is never above the clock: it is dispatched from the clock on.
  INSERT INTO group_service (group_id, class) VALUES (v_group, v_class) ON CONFLICT DO NOTHING;
  INSERT INTO project_service (group_id, project_id, class) VALUES (v_group, v_project, v_class) ON CONFLICT DO NOTHING;
  PERFORM lectio_enqueue(v_parse, 'prepare', 'prepare', NULL, v_now);
  RETURN 'created';
END $$;

CREATE FUNCTION lectio_settled(p_parse text, p_kind text, p_state text, p_error jsonb, p_settle jsonb, p_now timestamptz)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_p      parses%ROWTYPE;
  v_pages  jsonb;
  v_native boolean;
  v_n      integer;
  v_open   integer;
  v_lane   text;
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
           pages_failed = pages_failed + CASE WHEN p_state = 'failed' THEN 1 ELSE 0 END
     WHERE parse_id = p_parse
    RETURNING pages_open INTO STRICT v_open;
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

CREATE FUNCTION lectio_exchange(p_worker text, p_request text, p_now timestamptz DEFAULT NULL)
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

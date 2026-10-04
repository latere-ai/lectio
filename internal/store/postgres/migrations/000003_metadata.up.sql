-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- What the API serves from and a worker runs a task with: the files, the
-- members of a parse its caller chose, the page results kept for reuse, and
-- the part of a parse a claim carries (specs/002-object-model.md,
-- specs/003-api.md, specs/005-parse-graph.md,
-- specs/014-sources-and-retention.md). Bytes are in the object store: a row
-- here holds a key and never a page.

-- files holds a source snapshot's row. The same bytes are one file per
-- owner. A delete marks the row, removes the object and then the row, so a
-- delete that stops halfway leaves a row that still names its object and
-- never an object nothing names.
CREATE TABLE files (
  file_id    text PRIMARY KEY,
  owner      text   NOT NULL,
  name       text   NOT NULL DEFAULT '',
  size       bigint NOT NULL,
  sha256     text   NOT NULL,               -- hex digest of the bytes
  media_type text   NOT NULL,               -- as detected at upload
  object_key text   NOT NULL,               -- where the snapshot is in the object store
  created_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz                    -- a delete began; the file is gone for every caller
);
CREATE UNIQUE INDEX files_content ON files (owner, sha256) WHERE deleted_at IS NULL;

ALTER TABLE parses
  ADD COLUMN file_id         text,                          -- the file the parse reads
  ADD COLUMN options         jsonb NOT NULL DEFAULT '{}',   -- pages, languages and reuse, as submitted
  ADD COLUMN labels          jsonb NOT NULL DEFAULT '{}',   -- the caller's own keys and values
  ADD COLUMN origin          jsonb,                         -- where the file lives for the caller; never interpreted
  ADD COLUMN idempotency_key text,                          -- makes the submit safe to repeat
  ADD COLUMN body_digest     text,                          -- digest of the body the key came with
  ADD COLUMN read_base       text,                          -- names a read of this parse, less the page; NULL when its readers promise nothing
  ADD COLUMN pages_reused    integer NOT NULL DEFAULT 0;    -- pages done that were taken from an earlier read
CREATE UNIQUE INDEX parses_idempotency ON parses (owner, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX parses_owner ON parses (owner, parse_id DESC);
CREATE INDEX parses_file  ON parses (file_id) WHERE state IN ('queued', 'running');

-- reads keeps, per owner, where the result of a page read is: the key names
-- the file's bytes, the languages hinted, every reader that may come to read
-- the page with its version, and the page. Only a page that was read whole
-- has a row. The row goes with the parse that wrote the result, since the
-- parse's delete removes the object.
CREATE TABLE reads (
  owner      text NOT NULL,
  read_key   text NOT NULL,
  parse_id   text NOT NULL REFERENCES parses ON DELETE CASCADE,
  output     text NOT NULL,                 -- object key of the stored page result
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner, read_key)
);
CREATE INDEX reads_parse ON reads (parse_id);

-- lectio_submit writes a parse and its prepare task, as in 000001, with what
-- its caller chose. It answers a JSON document: result is created, exists
-- for a parse id or an idempotency key that is already there, conflict for
-- a key that came with another body, file_not_found, or queue_full; parse is
-- the id of the parse that was written or found. A key is held for 24 hours
-- from its parse's submit. p_submit is a JSON document bound as text.
DROP FUNCTION lectio_submit(text, timestamptz);
CREATE FUNCTION lectio_submit(p_submit text, p_now timestamptz DEFAULT NULL)
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

-- lectio_file_delete begins the delete of an owner's file: it marks the row
-- and answers the object's key, so the caller removes the object and then
-- the row. It answers missing for a file that is not there, and not_terminal
-- for one that a parse which has not ended reads. The row is locked before
-- the parses are counted, so a submit that names the file is wholly before
-- the delete or wholly after it.
CREATE FUNCTION lectio_file_delete(p_owner text, p_file text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_key text;
BEGIN
  SELECT object_key INTO v_key FROM files WHERE file_id = p_file AND owner = p_owner AND deleted_at IS NULL FOR UPDATE;
  IF NOT FOUND THEN
    RETURN jsonb_build_object('result', 'missing')::text;
  END IF;
  IF EXISTS (SELECT 1 FROM parses WHERE file_id = p_file AND state IN ('queued', 'running')) THEN
    RETURN jsonb_build_object('result', 'not_terminal')::text;
  END IF;
  UPDATE files SET deleted_at = coalesce(p_now, now()) WHERE file_id = p_file;
  RETURN jsonb_build_object('result', 'deleted', 'key', v_key)::text;
END $$;

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

-- lectio_context is what a claim carries of its parse, by the kind of its
-- task: prepare is told the file and the caller's selection; a page the
-- manifest without its list of pages, the languages hinted, and, when the
-- parse takes earlier reads, where the result of the same read is kept; and
-- assemble the manifest whole.
CREATE FUNCTION lectio_context(p_claim jsonb)
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

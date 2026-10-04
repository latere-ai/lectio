-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- Back to the dispatch of 000001: a page goes to the first reader with room,
-- and no task moves down the chain. A task that had moved waits again from
-- the head of the chain.

DROP FUNCTION IF EXISTS lectio_claim(text, integer, boolean, settings, timestamptz);
DROP FUNCTION IF EXISTS lectio_settle(text, jsonb, settings, timestamptz);
DROP FUNCTION IF EXISTS lectio_correct(tasks, integer, text);
DROP FUNCTION IF EXISTS lectio_lanes(text[], text[], settings);
DROP FUNCTION IF EXISTS lectio_room(text[], text, settings, timestamptz);
DROP FUNCTION IF EXISTS lectio_reader(text[], integer, text[]);

DROP INDEX IF EXISTS tasks_runnable;
ALTER TABLE tasks DROP COLUMN lane;
DROP FUNCTION IF EXISTS lectio_lane(text, text, integer);
ALTER TABLE tasks
  DROP COLUMN chain_at,
  DROP COLUMN invalid,
  DROP COLUMN escalated,
  DROP COLUMN result;
ALTER TABLE tasks ADD COLUMN lane text NOT NULL GENERATED ALWAYS AS (
  CASE WHEN kind IN ('prepare', 'assemble') THEN ''
       WHEN pin IS NULL THEN kind
       ELSE kind || ':' || pin END) STORED;
CREATE INDEX tasks_runnable ON tasks (group_id, project_id, class, lane, priority DESC, seq, created_at, parse_id, task_id)
  WHERE state = 'queued';

-- The counts of the lanes follow the rows again: a lane of a position down
-- the chain no longer exists.
DELETE FROM lane_service;
INSERT INTO lane_service (group_id, project_id, class, lane, queued)
SELECT group_id, project_id, class, lane, count(*) FROM tasks WHERE state = 'queued'
 GROUP BY group_id, project_id, class, lane;

CREATE FUNCTION lectio_correct(p_t tasks, p_used integer, p_requeued boolean)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_delta bigint := (p_used - p_t.charged)::bigint * 1000000;
  v_back  integer := CASE WHEN p_requeued THEN 1 ELSE 0 END;
BEGIN
  UPDATE group_service gs SET vtime = gs.vtime + v_delta / g.weight,
         queued = gs.queued + v_back, running = gs.running - 1
    FROM groups g
   WHERE g.group_id = gs.group_id AND gs.group_id = p_t.group_id AND gs.class = p_t.class;
  UPDATE project_service ps SET vtime = ps.vtime + v_delta / p.weight,
         queued = ps.queued + v_back, running = ps.running - 1
    FROM projects p
   WHERE p.group_id = ps.group_id AND p.project_id = ps.project_id
     AND ps.group_id = p_t.group_id AND ps.project_id = p_t.project_id AND ps.class = p_t.class;
  IF p_requeued THEN
    UPDATE lane_service SET queued = queued + 1
     WHERE group_id = p_t.group_id AND project_id = p_t.project_id AND class = p_t.class AND lane = p_t.lane;
  END IF;
  IF v_delta <> 0 THEN
    UPDATE class_service SET vtime = vtime + v_delta / weight WHERE class = p_t.class;
  END IF;
END $$;

CREATE FUNCTION lectio_settle(p_worker text, p_s jsonb, p_cfg settings, p_now timestamptz)
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
  CASE v_outcome
    WHEN 'succeeded' THEN
      v_state := 'succeeded';
      v_error := NULL;
    WHEN 'permanent' THEN
      v_state := 'failed';
    WHEN 'retryable' THEN
      v_attempt := v_t.attempt + 1;
      IF v_attempt >= p_cfg.attempts THEN
        v_state := 'failed';
      ELSE
        -- min(cap, base * 2^(attempt-1)), plus jitter uniform in half of it,
        -- so tasks that failed together do not come back together.
        v_state := 'queued';
        v_delay := least(p_cfg.backoff_cap, p_cfg.backoff_base * power(2, least(v_attempt - 1, 30)));
        v_available := p_now + v_delay + v_delay * (random() / 2);
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
  PERFORM lectio_correct(v_t, v_units, v_state = 'queued');

  UPDATE tasks SET
         state = v_state, attempt = v_attempt, available_at = v_available, calling = false,
         lease_owner = CASE WHEN v_state = 'queued' THEN NULL ELSE lease_owner END,
         reader      = CASE WHEN v_state = 'queued' THEN NULL ELSE reader END,
         scope       = CASE WHEN v_state = 'queued' THEN NULL ELSE scope END,
         charged     = CASE WHEN v_state = 'queued' THEN 0 ELSE v_units END,
         output      = CASE WHEN v_state = 'succeeded' THEN p_s->>'output' ELSE output END,
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

CREATE FUNCTION lectio_room(p_open text[], p_scope text, p_cfg settings, p_now timestamptz,
                            OUT p_room text[], OUT p_wake timestamptz)
LANGUAGE sql AS $$
  SELECT coalesce(array_agg(o.reader) FILTER (WHERE s.reader IS NULL
           OR ((s.paused_until IS NULL OR s.paused_until <= p_now)
               AND (SELECT count(*) FROM tasks t
                     WHERE t.state = 'leased' AND t.calling AND t.reader = o.reader AND t.scope = p_scope)
                   < lectio_admits(lectio_ceiling(s.ceiling, s.raised_at, p.max_in_flight, (p_cfg).pool_recovery, p_now),
                                   s.paused_until, (p_cfg).pool_resume, p_now))), '{}'),
         min(s.paused_until) FILTER (WHERE s.paused_until > p_now)
    FROM unnest(p_open) AS o(reader)
    JOIN pools p ON p.reader = o.reader
    LEFT JOIN pool_scopes s ON s.reader = o.reader AND s.scope = p_scope;
$$;

CREATE FUNCTION lectio_lanes(p_room text[], p_cfg settings)
RETURNS text[] LANGUAGE sql IMMUTABLE AS $$
  SELECT ARRAY['']
      || CASE WHEN (p_cfg).read_chain && p_room THEN ARRAY['page'] ELSE '{}'::text[] END
      || CASE WHEN (p_cfg).extract_chain && p_room THEN ARRAY['extract'] ELSE '{}'::text[] END
      || ARRAY(SELECT 'page:' || r FROM unnest(p_room) r)
      || ARRAY(SELECT 'extract:' || r FROM unnest(p_room) r);
$$;

CREATE FUNCTION lectio_claim(p_worker text, p_free integer, p_idle boolean, p_cfg settings, p_now timestamptz,
                             OUT p_claims jsonb, OUT p_sleep timestamptz)
LANGUAGE plpgsql AS $$
DECLARE
  v_free   integer := p_free;
  v_held   integer;
  v_alone  boolean;
  v_solo   boolean;
  v_clock  bigint;
  v_open   text[];
  v_room   text[];
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

    -- The readers whose pool admits a call now, whatever the key: the
    -- breaker is closed, or its open period is over and no trial is in
    -- flight, and the calls in flight are below the pool's bound. A slot is
    -- a leased task that is calling, so the count is the rows themselves.
    SELECT coalesce(array_agg(p.reader), '{}') INTO v_open
      FROM pools p
     WHERE (p.opened_at IS NULL
            OR (p_now >= p.opened_at + p_cfg.breaker_open
                AND NOT EXISTS (SELECT 1 FROM tasks t
                                 WHERE t.state = 'leased' AND t.calling AND t.reader = p.reader
                                   AND t.leased_at > p.opened_at)))
       AND (SELECT count(*) FROM tasks t
             WHERE t.state = 'leased' AND t.calling AND t.reader = p.reader) < p.max_in_flight;
    -- The lanes whose tasks can run. With one key for every group the
    -- readers with room are the same for all of them, and so are the lanes.
    -- With a key per group they are the group's own, read when the group is
    -- looked at; the groups are then chosen among those that hold a task in
    -- a lane whose pool admits a call at all.
    IF p_cfg.scope_by_group THEN
      v_among := lectio_lanes(v_open, p_cfg);
    ELSE
      SELECT r.p_room, r.p_wake INTO v_room, p_sleep FROM lectio_room(v_open, '', p_cfg, p_now) r;
      v_lanes := lectio_lanes(v_room, p_cfg);
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
          SELECT r.p_room, r.p_wake INTO v_room, v_wake FROM lectio_room(v_open, v_scope, p_cfg, p_now) r;
          v_lanes := lectio_lanes(v_room, p_cfg);
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

    -- The slot: the pinned reader, or the first of the policy's chain with
    -- room. A task that calls no model holds none and costs 1.
    v_reader := NULL;
    v_cost   := 1;
    IF v_t.kind IN ('page', 'extract') THEN
      IF v_t.pin IS NOT NULL THEN
        v_reader := v_t.pin;
      ELSE
        SELECT u.reader INTO STRICT v_reader
          FROM unnest(CASE WHEN v_t.kind = 'page' THEN p_cfg.read_chain ELSE p_cfg.extract_chain END)
               WITH ORDINALITY AS u(reader, at)
         WHERE u.reader = ANY (v_room) ORDER BY u.at LIMIT 1;
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

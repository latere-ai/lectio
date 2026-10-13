-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- Readers at runtime (specs/019-readers-at-runtime.md): the readers and the
-- routing policy an operator stores through the API, which every process
-- builds beside the documents of LECTIO_CONFIG. The version of what is
-- stored is 1 more at each change, and a process that sees it change reads
-- the store again. A change of the chains shortens one where it drops a
-- reader, and a task that waited at a place the chain no longer has is
-- moved to its last place when a process writes the new chains. The file
-- adds 2 tables and changes none in use.

-- readers holds one reader stored through the API: the spec of a Reader
-- document, kept as it was sent.
CREATE TABLE readers (
  name       text PRIMARY KEY,
  spec       text NOT NULL,                       -- the spec as one JSON object, kept byte for byte
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL
);

-- reader_policy is one row: the spec of the Policy stored through the API,
-- NULL while the configured one is in force, and the version of the
-- stored readers and policy, which each change of either adds 1 to.
CREATE TABLE reader_policy (
  one        boolean PRIMARY KEY DEFAULT true CHECK (one),
  spec       text,                                -- the spec as one JSON object, kept byte for byte
  version    bigint NOT NULL DEFAULT 0,
  updated_at timestamptz
);
INSERT INTO reader_policy (one) VALUES (true);

-- lectio_reader_state answers what is stored: the version, the policy's
-- spec or null, and the readers in the order of their names.
CREATE FUNCTION lectio_reader_state()
RETURNS text LANGUAGE sql STABLE AS $$
  SELECT jsonb_build_object('version', p.version, 'policy', p.spec,
           'readers', (SELECT coalesce(jsonb_agg(jsonb_build_object('name', r.name, 'spec', r.spec) ORDER BY r.name), '[]'::jsonb)
                         FROM readers r))::text
    FROM reader_policy p;
$$;

-- lectio_reader_put stores a reader, or replaces the one of the name, and
-- answers created or replaced. The row of the policy is taken first, so 2
-- changes are one after the other and each adds 1 to the version.
CREATE FUNCTION lectio_reader_put(p_name text, p_spec text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_now timestamptz := coalesce(p_now, now());
  v_new boolean;
BEGIN
  PERFORM 1 FROM reader_policy FOR UPDATE;
  INSERT INTO readers (name, spec, created_at, updated_at) VALUES (p_name, p_spec, v_now, v_now)
  ON CONFLICT (name) DO UPDATE SET spec = EXCLUDED.spec, updated_at = EXCLUDED.updated_at
  RETURNING xmax = 0 INTO v_new;
  UPDATE reader_policy SET version = version + 1, updated_at = v_now;
  RETURN CASE WHEN v_new THEN 'created' ELSE 'replaced' END;
END $$;

-- lectio_reader_delete removes a stored reader. It takes the lock the
-- exchange takes, so no work names the reader between the check and the
-- delete, then the row of the policy. It answers missing for a name no
-- stored reader has; chained for a reader a chain in force or the stored
-- policy names; set for one a set's definition names; busy for one a
-- parse that has not ended, a task that has not ended or an extraction
-- that is pending names; or deleted.
CREATE FUNCTION lectio_reader_delete(p_name text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_now    timestamptz := coalesce(p_now, now());
  v_policy jsonb;
BEGIN
  PERFORM lectio_lock();
  SELECT spec::jsonb INTO v_policy FROM reader_policy FOR UPDATE;
  IF NOT EXISTS (SELECT 1 FROM readers WHERE name = p_name) THEN
    RETURN 'missing';
  END IF;
  IF EXISTS (SELECT 1 FROM settings
              WHERE p_name = ANY (read_chain) OR p_name = ANY (extract_chain) OR p_name = ANY (describe_chain))
     OR coalesce(v_policy->'read'->'chain', '[]') ? p_name
     OR coalesce(v_policy->'describe'->'chain', '[]') ? p_name
     OR coalesce(v_policy->'extract'->'chain', '[]') ? p_name THEN
    RETURN 'chained';
  END IF;
  IF EXISTS (SELECT 1 FROM sets WHERE pin = p_name) THEN
    RETURN 'set';
  END IF;
  IF EXISTS (SELECT 1 FROM parses WHERE pin = p_name AND state IN ('queued', 'running'))
     OR EXISTS (SELECT 1 FROM tasks WHERE (pin = p_name OR reader = p_name) AND state IN ('queued', 'leased'))
     OR EXISTS (SELECT 1 FROM fields WHERE pin = p_name AND state = 'pending') THEN
    RETURN 'busy';
  END IF;
  DELETE FROM readers WHERE name = p_name;
  UPDATE reader_policy SET version = version + 1, updated_at = v_now;
  RETURN 'deleted';
END $$;

-- lectio_policy_put stores the policy's spec, or with NULL removes the one
-- stored, so the configured one is in force again, and answers which is.
CREATE FUNCTION lectio_policy_put(p_spec text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE sql AS $$
  UPDATE reader_policy SET spec = p_spec, version = version + 1, updated_at = coalesce(p_now, now())
  RETURNING CASE WHEN spec IS NULL THEN 'configured' ELSE 'set' END;
$$;

-- lectio_configure writes the settings a process is configured with, as
-- 000006 has it, and moves a task that waits past the end of a chain that
-- is now shorter to the chain's last place.
CREATE OR REPLACE FUNCTION lectio_configure(p_config text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  c     jsonb := p_config::jsonb;
  v_cfg settings%ROWTYPE;
  v_t   tasks%ROWTYPE;
  v_at  integer;
BEGIN
  PERFORM lectio_lock();
  INSERT INTO settings (one, lease, sweep_interval, attempts, expiries, backoff_base, backoff_cap,
                        pool_recovery, pool_resume, pool_pause, breaker_failures, breaker_open,
                        scope_by_group, read_chain, extract_chain, describe_chain)
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
          ARRAY(SELECT jsonb_array_elements_text(c->'extract_chain')),
          ARRAY(SELECT jsonb_array_elements_text(c->'describe_chain')))
  ON CONFLICT (one) DO UPDATE SET
    lease = EXCLUDED.lease, sweep_interval = EXCLUDED.sweep_interval,
    attempts = EXCLUDED.attempts, expiries = EXCLUDED.expiries,
    backoff_base = EXCLUDED.backoff_base, backoff_cap = EXCLUDED.backoff_cap,
    pool_recovery = EXCLUDED.pool_recovery, pool_resume = EXCLUDED.pool_resume,
    pool_pause = EXCLUDED.pool_pause, breaker_failures = EXCLUDED.breaker_failures,
    breaker_open = EXCLUDED.breaker_open, scope_by_group = EXCLUDED.scope_by_group,
    read_chain = EXCLUDED.read_chain, extract_chain = EXCLUDED.extract_chain,
    describe_chain = EXCLUDED.describe_chain;

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

  -- A task that stays with a reader the configuration no longer holds has
  -- no reader to wait for. Its pin was the store's and no caller's, so it
  -- is released to the chain of its kind, from its place in it or from the
  -- start when the chain is now shorter, and the counter of its lane moves
  -- with it.
  SELECT * INTO STRICT v_cfg FROM settings;
  FOR v_t IN
    SELECT * FROM tasks t
     WHERE t.stuck AND NOT EXISTS (SELECT 1 FROM pools p WHERE p.reader = t.pin)
     ORDER BY t.parse_id, t.task_id FOR UPDATE
  LOOP
    v_at := CASE WHEN v_t.chain_at < coalesce(array_length(lectio_chain(v_t.kind, v_cfg), 1), 0)
                 THEN v_t.chain_at ELSE 0 END;
    IF v_t.state = 'queued' THEN
      PERFORM lectio_count(v_t.group_id, v_t.project_id, v_t.class, v_t.lane, -1, 0);
      PERFORM lectio_count(v_t.group_id, v_t.project_id, v_t.class, lectio_lane(v_t.kind, NULL, v_at), 1, 0);
    END IF;
    UPDATE tasks SET pin = NULL, stuck = false, chain_at = v_at
     WHERE parse_id = v_t.parse_id AND task_id = v_t.task_id;
  END LOOP;

  -- A task that waits at a place of its chain that a shorter chain no
  -- longer has waits at the chain's last place instead: the readers before
  -- it were tried for it, as a page a text reader declined goes on to the
  -- model. A chain that is now empty leaves it where it is, for the next
  -- configuration that names a reader.
  FOR v_t IN
    SELECT * FROM tasks t
     WHERE t.state = 'queued' AND t.pin IS NULL AND t.kind IN ('page', 'extract', 'figure') AND t.chain_at > 0
       AND t.chain_at >= coalesce(array_length(lectio_chain(t.kind, v_cfg), 1), 0)
       AND coalesce(array_length(lectio_chain(t.kind, v_cfg), 1), 0) > 0
     ORDER BY t.parse_id, t.task_id FOR UPDATE
  LOOP
    v_at := array_length(lectio_chain(v_t.kind, v_cfg), 1) - 1;
    PERFORM lectio_count(v_t.group_id, v_t.project_id, v_t.class, v_t.lane, -1, 0);
    PERFORM lectio_count(v_t.group_id, v_t.project_id, v_t.class, lectio_lane(v_t.kind, NULL, v_at), 1, 0);
    UPDATE tasks SET chain_at = v_at
     WHERE parse_id = v_t.parse_id AND task_id = v_t.task_id;
  END LOOP;
END $$;

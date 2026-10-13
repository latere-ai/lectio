-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- Back to the schema of 000009: readers and the policy are the
-- configuration's alone. What was stored through the API is dropped, and a
-- process of the version before reads the configuration it is given.
DROP FUNCTION lectio_policy_put(text, bigint, timestamptz);
DROP FUNCTION lectio_reader_delete(text, timestamptz);
DROP FUNCTION lectio_reader_put(text, text, bigint, timestamptz);
DROP FUNCTION lectio_reader_state();
DROP TABLE reader_policy;
DROP TABLE readers;

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
END $$;

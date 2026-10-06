-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- An extraction that has ended is asked again under its name, with the same
-- request or another (specs/011-structured-extraction.md): to fill a field
-- that failed, or to fill it with other members. Its row is reset to
-- pending and its task is queued again when its parse has ended, as for a
-- new one. An extraction that has not ended is not asked again: its task
-- may be running.
--
-- A field counts how many times it was asked, and the task of each asking
-- starts its lease tokens at that count shifted into the high 32 bits, as a
-- figure run does with its run. A worker that lost the task of an earlier
-- asking removes the objects it wrote under its own tokens, and no token of
-- the new asking is one of them.

-- The locks are taken as 000006 takes its own: the lock of the exchange
-- first, then the tables at once and without waiting, for 5 seconds. The
-- file is one transaction, and fails whole.
SET LOCAL lock_timeout = '5s';
SELECT lectio_lock();
DO $$
DECLARE
  v_until timestamptz := clock_timestamp() + interval '5 seconds';
BEGIN
  LOOP
    BEGIN
      LOCK TABLE fields IN ACCESS EXCLUSIVE MODE NOWAIT;
      RETURN;
    EXCEPTION WHEN lock_not_available THEN
      IF clock_timestamp() >= v_until THEN
        RAISE EXCEPTION 'lectio: the tables this migration changes were in use for 5 seconds'
          USING ERRCODE = 'lock_not_available';
      END IF;
      PERFORM pg_sleep(0.02);
    END;
  END LOOP;
END $$;

ALTER TABLE fields ADD COLUMN asks integer NOT NULL DEFAULT 1;

-- lectio_fields_release queues the task of each extraction of a parse that
-- waits for one, now that the parse has ended. Its first lease token is the
-- asking's count less 1, shifted into the high 32 bits: 0 for a first ask.
CREATE OR REPLACE FUNCTION lectio_fields_release(p_parse text, p_now timestamptz)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_p    parses%ROWTYPE;
  v_f    record;
  v_lane text;
BEGIN
  SELECT * INTO STRICT v_p FROM parses WHERE parse_id = p_parse;
  FOR v_f IN
    UPDATE fields SET deadline_at = p_now + wait
     WHERE parse_id = p_parse AND state = 'pending' AND deadline_at IS NULL
    RETURNING name, pin, asks
  LOOP
    INSERT INTO tasks (parse_id, task_id, kind, group_id, project_id, class, priority, seq, pin, state, available_at, created_at, lease_token)
    VALUES (p_parse, 'extract-' || v_f.name, 'extract', v_p.group_id, v_p.project_id, v_p.class, v_p.priority,
            0, v_f.pin, 'queued', p_now, p_now, (v_f.asks - 1)::bigint << 32)
    ON CONFLICT DO NOTHING
    RETURNING lane INTO v_lane;
    IF FOUND THEN
      PERFORM lectio_count(v_p.group_id, v_p.project_id, v_p.class, v_lane, 1, 0);
    END IF;
  END LOOP;
END $$;

-- lectio_field_ask asks an extraction of a parse again under its name. It
-- takes the lock the exchange takes and then the parse's row, as
-- lectio_field_create does. It answers a JSON document: answer missing for
-- a parse that is not there or whose retention has ended, absent for a name
-- the parse has no extraction of, busy for an extraction that has not ended,
-- or asked, with output, the object key of the result the asking replaced,
-- when it had one. An asked extraction is pending, with the new request and
-- pin, nothing of its last result, and its counts of calls and tokens at 0;
-- its task is queued at once when the parse has ended, and otherwise when
-- it ends. p_field is a JSON document bound as text, as lectio_field_create
-- takes one.
CREATE FUNCTION lectio_field_ask(p_field text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  f       jsonb := p_field::jsonb;
  v_now   timestamptz := coalesce(p_now, now());
  v_parse text := f->>'parse';
  v_name  text := f->>'name';
  v_p     parses%ROWTYPE;
  v_f     fields%ROWTYPE;
BEGIN
  PERFORM lectio_lock();
  SELECT * INTO v_p FROM parses WHERE parse_id = v_parse FOR UPDATE;
  IF NOT FOUND OR v_p.expires_at <= v_now THEN
    RETURN '{"answer":"missing"}';
  END IF;
  SELECT * INTO v_f FROM fields WHERE parse_id = v_parse AND name = v_name FOR UPDATE;
  IF NOT FOUND THEN
    RETURN '{"answer":"absent"}';
  END IF;
  IF v_f.state = 'pending' THEN
    RETURN '{"answer":"busy"}';
  END IF;
  UPDATE fields SET state = 'pending', request = f->>'request', pin = nullif(f->>'pin', ''),
         wait = (f->>'deadline_ms')::bigint * interval '1 millisecond', deadline_at = NULL,
         output = NULL, result = NULL, error = NULL, calls = 0, input_tokens = 0, output_tokens = 0,
         finished_at = NULL, asks = asks + 1
   WHERE parse_id = v_parse AND name = v_name;
  IF v_p.state IN ('succeeded', 'failed', 'canceled') THEN
    PERFORM lectio_fields_release(v_parse, v_now);
  END IF;
  RETURN jsonb_build_object('answer', 'asked', 'output', v_f.output)::text;
END $$;

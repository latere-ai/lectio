-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- Back to the schema of 000007: no set. An extraction a set asked stays on
-- its parse as an extraction of no set. The locks are taken as the up file
-- takes them.
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

DROP FUNCTION lectio_set_delete(text);
DROP FUNCTION lectio_set_update(text, timestamptz);
DROP FUNCTION lectio_set_join(text, timestamptz);
DROP FUNCTION lectio_set_parses(text, text, integer);
DROP FUNCTION lectio_sets(text, text, text, integer);
DROP FUNCTION lectio_set(text);
DROP FUNCTION lectio_set_create(text, timestamptz);
DROP FUNCTION lectio_set_view(sets);

-- lectio_field_ask as 000007 wrote it.
CREATE OR REPLACE FUNCTION lectio_field_ask(p_field text, p_now timestamptz DEFAULT NULL)
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

DROP INDEX fields_set;
ALTER TABLE fields
  DROP COLUMN set_version,
  DROP COLUMN set_id;
DROP TABLE sets;

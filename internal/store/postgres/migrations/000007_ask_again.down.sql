-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- Back to the schema of 000006: an extraction is asked once. The locks are
-- taken as the up file takes them.
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

DROP FUNCTION lectio_field_ask(text, timestamptz);

-- lectio_fields_release as 000006 wrote it.
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
    RETURNING name, pin
  LOOP
    INSERT INTO tasks (parse_id, task_id, kind, group_id, project_id, class, priority, seq, pin, state, available_at, created_at)
    VALUES (p_parse, 'extract-' || v_f.name, 'extract', v_p.group_id, v_p.project_id, v_p.class, v_p.priority,
            0, v_f.pin, 'queued', p_now, p_now)
    ON CONFLICT DO NOTHING
    RETURNING lane INTO v_lane;
    IF FOUND THEN
      PERFORM lectio_count(v_p.group_id, v_p.project_id, v_p.class, v_lane, 1, 0);
    END IF;
  END LOOP;
END $$;

ALTER TABLE fields DROP COLUMN asks;

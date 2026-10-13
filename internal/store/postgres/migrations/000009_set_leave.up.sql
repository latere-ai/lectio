-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- A parse leaves a set (specs/018-sets.md): its extraction under the set's
-- name stays on the parse, with its values, as an extraction of no set, as
-- one does when its set is deleted. It no longer counts among the set's
-- parses, and a change of the set no longer asks it. The file adds one
-- function and changes no table.

-- lectio_set_leave takes a parse out of a set. It takes the lock the
-- exchange takes, then the set's row and the parse's, as lectio_set_join
-- does, so it is wholly before or wholly after a join, a change of the set
-- and any settle of the parse. An extraction that has not ended is not
-- taken out: its task may be running, and it would fill an extraction the
-- set no longer holds. It answers no_set, missing for a parse that is not
-- there, whose retention has ended or that is another owner's, not_member
-- for a parse that holds no extraction of the set, busy, or left.
CREATE FUNCTION lectio_set_leave(p_set text, p_parse text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_now timestamptz := coalesce(p_now, now());
  v_s   sets%ROWTYPE;
  v_p   parses%ROWTYPE;
  v_f   fields%ROWTYPE;
BEGIN
  PERFORM lectio_lock();
  SELECT * INTO v_s FROM sets WHERE set_id = p_set FOR UPDATE;
  IF NOT FOUND THEN
    RETURN 'no_set';
  END IF;
  SELECT * INTO v_p FROM parses WHERE parse_id = p_parse FOR UPDATE;
  IF NOT FOUND OR v_p.expires_at <= v_now OR v_p.owner <> v_s.owner THEN
    RETURN 'missing';
  END IF;
  SELECT * INTO v_f FROM fields WHERE parse_id = p_parse AND name = v_s.name AND set_id = p_set FOR UPDATE;
  IF NOT FOUND THEN
    RETURN 'not_member';
  END IF;
  IF v_f.state = 'pending' THEN
    RETURN 'busy';
  END IF;
  UPDATE fields SET set_id = NULL, set_version = NULL
   WHERE parse_id = p_parse AND name = v_s.name;
  RETURN 'left';
END $$;

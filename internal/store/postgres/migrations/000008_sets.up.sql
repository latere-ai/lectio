-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- A set is a stored definition of an extraction, made before any parse is
-- read, and asked of each parse that joins it (specs/018-sets.md). A parse
-- is in a set when it holds an extraction under the set's name that the set
-- asked: the row of fields carries the set, and the version of the set's
-- definition it was asked with. A change of the definition asks every
-- extraction of the set again, and is refused while one of them has a task
-- that may be running, as asking one extraction again is.

-- The locks are taken as 000006 takes its own: the lock of the exchange
-- first, then the table at once and without waiting, for 5 seconds. The
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

-- sets holds one set of an owner: its name, which is the name of every
-- extraction it asks, and its definition, kept as the request of an
-- extraction is kept, so an extraction asked with it holds the same bytes.
CREATE TABLE sets (
  set_id     text    PRIMARY KEY,
  owner      text    NOT NULL,
  name       text    NOT NULL,                  -- unique among the owner's sets, and never changed
  request    text    NOT NULL,                  -- the schema, the instructions and the citations flag, as one JSON document kept byte for byte
  pin        text,                              -- the extractor the definition names, when it names one
  version    integer NOT NULL DEFAULT 1,        -- 1 more at each change of the definition
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  UNIQUE (owner, name)
);

ALTER TABLE fields
  ADD COLUMN set_id      text REFERENCES sets ON DELETE SET NULL, -- the set that asked it, when one did
  ADD COLUMN set_version integer;                                 -- the version of the set's definition it was asked with
CREATE INDEX fields_set ON fields (set_id, parse_id) WHERE set_id IS NOT NULL;

-- lectio_set_view is one set as a read answers it: its row, with how many
-- parses are in it and where their extractions stand.
CREATE FUNCTION lectio_set_view(p_s sets)
RETURNS jsonb LANGUAGE sql STABLE AS $$
  SELECT to_jsonb(p_s) || jsonb_build_object('parses', jsonb_build_object(
           'total',     count(*),
           'pending',   count(*) FILTER (WHERE f.state = 'pending'),
           'succeeded', count(*) FILTER (WHERE f.state = 'succeeded'),
           'failed',    count(*) FILTER (WHERE f.state = 'failed')))
    FROM fields f WHERE f.set_id = p_s.set_id;
$$;

-- lectio_set_create writes a set. It answers a JSON document: answer
-- created with the set, or conflict for a name the owner has. It touches no
-- parse, so it takes no lock but the row's. p_set is a JSON document bound
-- as text.
CREATE FUNCTION lectio_set_create(p_set text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  s     jsonb := p_set::jsonb;
  v_now timestamptz := coalesce(p_now, now());
  v_s   sets%ROWTYPE;
BEGIN
  INSERT INTO sets (set_id, owner, name, request, pin, created_at, updated_at)
  VALUES (s->>'id', s->>'owner', s->>'name', s->>'request', nullif(s->>'pin', ''), v_now, v_now)
  ON CONFLICT (owner, name) DO NOTHING
  RETURNING * INTO v_s;
  IF NOT FOUND THEN
    RETURN '{"answer":"conflict"}';
  END IF;
  RETURN jsonb_build_object('answer', 'created', 'set', lectio_set_view(v_s))::text;
END $$;

-- lectio_set answers one set, or the JSON null.
CREATE FUNCTION lectio_set(p_set text)
RETURNS text LANGUAGE sql STABLE AS $$
  SELECT coalesce((SELECT lectio_set_view(s) FROM sets s WHERE s.set_id = p_set), 'null'::jsonb)::text;
$$;

-- lectio_sets answers one page of the sets of some owners, newest first, as
-- a JSON array. The owners are a JSON array bound as text, or the JSON null
-- for every owner's. Ids sort by creation time, so the page after an id is
-- the ids below it. An empty name matches every set.
CREATE FUNCTION lectio_sets(p_owners text, p_name text, p_after text, p_limit integer)
RETURNS text LANGUAGE sql STABLE AS $$
  SELECT coalesce(jsonb_agg(lectio_set_view(s) ORDER BY s.set_id DESC), '[]'::jsonb)::text
    FROM (SELECT * FROM sets
           WHERE (p_owners::jsonb = 'null'::jsonb
                  OR owner = ANY (ARRAY(SELECT jsonb_array_elements_text(nullif(p_owners::jsonb, 'null'::jsonb)))))
             AND (p_name = '' OR name = p_name)
             AND (p_after = '' OR set_id < p_after)
           ORDER BY set_id DESC LIMIT p_limit) s;
$$;

-- lectio_set_parses answers one page of the parses of a set, newest first,
-- as a JSON array: each the parse's row and the row of its extraction under
-- the set's name.
CREATE FUNCTION lectio_set_parses(p_set text, p_after text, p_limit integer)
RETURNS text LANGUAGE sql STABLE AS $$
  SELECT coalesce(jsonb_agg(jsonb_build_object('parse', to_jsonb(m.p), 'field', lectio_field_view(m.f)) ORDER BY (m.p).parse_id DESC), '[]'::jsonb)::text
    FROM (SELECT p, f FROM fields f JOIN parses p USING (parse_id)
           WHERE f.set_id = p_set AND (p_after = '' OR f.parse_id < p_after)
           ORDER BY f.parse_id DESC LIMIT p_limit) m;
$$;

-- lectio_set_join adds a parse to a set. It takes the lock the exchange
-- takes, then the set's row and the parse's, so it is wholly before or
-- wholly after a change of the set and any settle, retry or delete of the
-- parse. What it does follows from what the parse holds under the set's
-- name:
--
--   nothing                       the extraction is asked with the set's definition
--   the set's, at its version     nothing is asked
--   one that ended, asked with    it joins as it stands, and nothing is asked
--     the set's request, equal
--     as a JSON value, and pin
--   another one that ended        it is asked again with the set's definition
--   another one, pending          busy: its task may be running
--
-- It answers a JSON document: answer no_set, missing for a parse that is not
-- there, whose retention has ended or that is another owner's, busy, full
-- for a parse that holds as many extractions as it may, full_set for a set
-- that holds as many parses as it may, joined when nothing was asked, or
-- asked, with output, the object key of the result an asking replaced. An
-- asked extraction's task is queued at once when the parse has ended, and
-- otherwise when it ends. p_join is a JSON document bound as text.
CREATE FUNCTION lectio_set_join(p_join text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  j       jsonb := p_join::jsonb;
  v_now   timestamptz := coalesce(p_now, now());
  v_set   text := j->>'set';
  v_parse text := j->>'parse';
  v_wait  interval := (j->>'deadline_ms')::bigint * interval '1 millisecond';
  v_s     sets%ROWTYPE;
  v_p     parses%ROWTYPE;
  v_f     fields%ROWTYPE;
  v_held  boolean;
BEGIN
  PERFORM lectio_lock();
  SELECT * INTO v_s FROM sets WHERE set_id = v_set FOR UPDATE;
  IF NOT FOUND THEN
    RETURN '{"answer":"no_set"}';
  END IF;
  SELECT * INTO v_p FROM parses WHERE parse_id = v_parse FOR UPDATE;
  IF NOT FOUND OR v_p.expires_at <= v_now OR v_p.owner <> v_s.owner THEN
    RETURN '{"answer":"missing"}';
  END IF;
  SELECT * INTO v_f FROM fields WHERE parse_id = v_parse AND name = v_s.name FOR UPDATE;
  v_held := FOUND;
  IF v_held AND v_f.set_id IS NOT DISTINCT FROM v_set AND v_f.set_version IS NOT DISTINCT FROM v_s.version THEN
    RETURN '{"answer":"joined"}';
  END IF;
  IF (NOT v_held OR v_f.set_id IS DISTINCT FROM v_set)
     AND (SELECT count(*) FROM fields WHERE set_id = v_set) >= (j->>'max_parses')::integer THEN
    RETURN '{"answer":"full_set"}';
  END IF;
  IF NOT v_held THEN
    IF v_p.fields >= (j->>'max_fields')::integer THEN
      RETURN '{"answer":"full"}';
    END IF;
    INSERT INTO fields (parse_id, name, request, pin, wait, created_at, set_id, set_version)
    VALUES (v_parse, v_s.name, v_s.request, v_s.pin, v_wait, v_now, v_set, v_s.version);
    UPDATE parses SET fields = fields + 1 WHERE parse_id = v_parse;
  ELSIF v_f.state = 'pending' THEN
    RETURN '{"answer":"busy"}';
  ELSIF v_f.request::jsonb = v_s.request::jsonb AND v_f.pin IS NOT DISTINCT FROM v_s.pin THEN
    UPDATE fields SET set_id = v_set, set_version = v_s.version
     WHERE parse_id = v_parse AND name = v_s.name;
    RETURN '{"answer":"joined"}';
  ELSE
    UPDATE fields SET state = 'pending', request = v_s.request, pin = v_s.pin, wait = v_wait, deadline_at = NULL,
           output = NULL, result = NULL, error = NULL, calls = 0, input_tokens = 0, output_tokens = 0,
           finished_at = NULL, asks = asks + 1, set_id = v_set, set_version = v_s.version
     WHERE parse_id = v_parse AND name = v_s.name;
  END IF;
  IF v_p.state IN ('succeeded', 'failed', 'canceled') THEN
    PERFORM lectio_fields_release(v_parse, v_now);
  END IF;
  RETURN jsonb_build_object('answer', 'asked', 'output', v_f.output)::text;
END $$;

-- lectio_set_update replaces a set's definition and asks every extraction of
-- the set again with it. The definition the set holds, byte for byte, with
-- the same pin, changes nothing and asks nothing; one that only orders the
-- members otherwise is a change, since the order is the order a model is
-- asked in and a table's columns are shown in. It takes the lock the
-- exchange takes, then the set's row and its parses' rows in the order of
-- their ids. A parse whose retention has ended is being removed: it is left
-- as it is, and none of its work is queued. Of the others, an extraction
-- whose parse is still read has no task yet, and its request is replaced
-- where it stands; one that has ended is asked again, as lectio_field_ask
-- asks it; and while one has a task, which may be running, nothing changes
-- and the answer is busy. It answers a JSON document: answer no_set, busy,
-- or updated, with the set and outputs, the object keys of the results the
-- askings replaced. p_update is a JSON document bound as text.
CREATE FUNCTION lectio_set_update(p_update text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  u         jsonb := p_update::jsonb;
  v_now     timestamptz := coalesce(p_now, now());
  v_set     text := u->>'set';
  v_request text := u->>'request';
  v_pin     text := nullif(u->>'pin', '');
  v_wait    interval := (u->>'deadline_ms')::bigint * interval '1 millisecond';
  v_s       sets%ROWTYPE;
  v_parses  text[];
  v_outputs jsonb;
  v_parse   text;
BEGIN
  PERFORM lectio_lock();
  SELECT * INTO v_s FROM sets WHERE set_id = v_set FOR UPDATE;
  IF NOT FOUND THEN
    RETURN '{"answer":"no_set"}';
  END IF;
  IF v_s.request = v_request AND v_s.pin IS NOT DISTINCT FROM v_pin THEN
    RETURN jsonb_build_object('answer', 'updated', 'set', lectio_set_view(v_s), 'outputs', '[]'::jsonb)::text;
  END IF;
  SELECT coalesce(array_agg(p.parse_id ORDER BY p.parse_id), '{}') INTO v_parses
    FROM fields f JOIN parses p USING (parse_id)
   WHERE f.set_id = v_set AND (p.expires_at IS NULL OR p.expires_at > v_now);
  PERFORM 1 FROM parses WHERE parse_id = ANY (v_parses) ORDER BY parse_id FOR UPDATE;
  IF EXISTS (SELECT 1 FROM fields WHERE set_id = v_set AND parse_id = ANY (v_parses)
                AND state = 'pending' AND deadline_at IS NOT NULL) THEN
    RETURN '{"answer":"busy"}';
  END IF;
  UPDATE sets SET request = v_request, pin = v_pin, version = version + 1, updated_at = v_now
   WHERE set_id = v_set
  RETURNING * INTO v_s;
  SELECT coalesce(jsonb_agg(output), '[]'::jsonb) INTO v_outputs
    FROM fields WHERE set_id = v_set AND parse_id = ANY (v_parses) AND state <> 'pending' AND output IS NOT NULL;
  UPDATE fields SET request = v_request, pin = v_pin, wait = v_wait, set_version = v_s.version
   WHERE set_id = v_set AND parse_id = ANY (v_parses) AND state = 'pending';
  UPDATE fields SET state = 'pending', request = v_request, pin = v_pin, wait = v_wait, deadline_at = NULL,
         output = NULL, result = NULL, error = NULL, calls = 0, input_tokens = 0, output_tokens = 0,
         finished_at = NULL, asks = asks + 1, set_version = v_s.version
   WHERE set_id = v_set AND parse_id = ANY (v_parses) AND state <> 'pending';
  FOR v_parse IN
    SELECT parse_id FROM parses
     WHERE parse_id = ANY (v_parses) AND state IN ('succeeded', 'failed', 'canceled')
     ORDER BY parse_id
  LOOP
    PERFORM lectio_fields_release(v_parse, v_now);
  END LOOP;
  RETURN jsonb_build_object('answer', 'updated', 'set', lectio_set_view(v_s), 'outputs', v_outputs)::text;
END $$;

-- lectio_set_delete removes a set. Its extractions stay on their parses as
-- extractions of no set. It takes the lock the exchange takes, as every
-- statement that changes an extraction's row does. It answers deleted or
-- missing.
CREATE FUNCTION lectio_set_delete(p_set text)
RETURNS text LANGUAGE plpgsql AS $$
BEGIN
  PERFORM lectio_lock();
  UPDATE fields SET set_id = NULL, set_version = NULL WHERE set_id = p_set;
  DELETE FROM sets WHERE set_id = p_set;
  IF FOUND THEN
    RETURN 'deleted';
  END IF;
  RETURN 'missing';
END $$;

-- lectio_field_ask as 000007 wrote it, and an extraction of a set is asked
-- again with the set's request, equal as a JSON value, and its pin alone:
-- answer set for another. It is asked with the set's own bytes, so a
-- schema reaches the model with its members in the order the set has them.
-- A set's table holds the rows the set asked.
CREATE OR REPLACE FUNCTION lectio_field_ask(p_field text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  f         jsonb := p_field::jsonb;
  v_now     timestamptz := coalesce(p_now, now());
  v_parse   text := f->>'parse';
  v_name    text := f->>'name';
  v_request text := f->>'request';
  v_p       parses%ROWTYPE;
  v_f       fields%ROWTYPE;
  v_s       sets%ROWTYPE;
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
  IF v_f.set_id IS NOT NULL THEN
    SELECT * INTO STRICT v_s FROM sets WHERE set_id = v_f.set_id;
    IF v_s.request::jsonb <> v_request::jsonb OR v_s.pin IS DISTINCT FROM nullif(f->>'pin', '') THEN
      RETURN '{"answer":"set"}';
    END IF;
    v_request := v_s.request;
  END IF;
  UPDATE fields SET state = 'pending', request = v_request, pin = nullif(f->>'pin', ''),
         wait = (f->>'deadline_ms')::bigint * interval '1 millisecond', deadline_at = NULL,
         output = NULL, result = NULL, error = NULL, calls = 0, input_tokens = 0, output_tokens = 0,
         finished_at = NULL, asks = asks + 1
   WHERE parse_id = v_parse AND name = v_name;
  IF v_p.state IN ('succeeded', 'failed', 'canceled') THEN
    PERFORM lectio_fields_release(v_parse, v_now);
  END IF;
  RETURN jsonb_build_object('answer', 'asked', 'output', v_f.output)::text;
END $$;

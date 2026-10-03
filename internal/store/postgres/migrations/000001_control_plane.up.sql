-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- The durable control plane: the task table and the worker lease of
-- specs/004-durable-tasks.md, the fair queue's accounting of
-- specs/006-fairness-and-priority.md, the reader pools of
-- specs/007-model-capacity.md, and the functions that are the only writers of
-- all three. A worker reaches the database through one statement,
-- lectio_exchange, and the API through lectio_submit and lectio_cancel, so
-- every write is one round trip and one transaction with no client time
-- inside it, and runs unchanged behind a transaction-mode pooler.
--
-- Time is the database's now(). Each function takes an optional p_now that
-- stands in for it, so a test drives leases, backoff, pauses and deadlines
-- with a virtual clock and never sleeps. Production code does not pass it.
--
-- Virtual time is an integer count of millionths of a unit of work. A charge
-- is cost * 1000000 / weight in integer division, so the same sequence of
-- claims gives the same order on every server and in every implementation.

-- settings is what the configuration of the process says about the queue. The
-- store writes the one row when it opens, so the functions below read the
-- lease, the bounds and the reader chains from the database and every replica
-- applies the same values. The defaults are the store's and are not repeated
-- here: the table is empty until a store opens.
CREATE TABLE settings (
  one              boolean  PRIMARY KEY DEFAULT true CHECK (one),
  lease            interval NOT NULL,  -- LECTIO_TASK_LEASE
  sweep_interval   interval NOT NULL,  -- LECTIO_SWEEP_INTERVAL
  attempts         integer  NOT NULL,  -- LECTIO_TASK_ATTEMPTS
  expiries         integer  NOT NULL,  -- LECTIO_TASK_EXPIRIES
  backoff_base     interval NOT NULL,
  backoff_cap      interval NOT NULL,
  pool_recovery    interval NOT NULL,  -- LECTIO_POOL_RECOVERY
  pool_resume      interval NOT NULL,  -- LECTIO_POOL_RESUME
  pool_pause       interval NOT NULL,  -- the pause of a rate-limit reply that names none
  breaker_failures integer  NOT NULL,
  breaker_open     interval NOT NULL,
  scope_by_group   boolean  NOT NULL,  -- true when each group reads with a key of its own
  read_chain       text[]   NOT NULL,  -- the policy's readers for a page, in order
  extract_chain    text[]   NOT NULL   -- the policy's readers for an extraction, in order
);

-- parses holds what the control plane needs of a parse: who it belongs to,
-- how it is dispatched, where it stands, and what it used. The request's
-- other members, the file and the labels among them, join the row with the
-- API that stores them.
CREATE TABLE parses (
  parse_id           text     PRIMARY KEY,
  owner              text     NOT NULL,
  group_id           text     NOT NULL,
  project_id         text     NOT NULL DEFAULT '',
  class              smallint NOT NULL,              -- 0 interactive, 1 batch
  priority           integer  NOT NULL DEFAULT 0,
  pin                text,                           -- the reader the parse named, when it named one
  allow_failed_pages integer  NOT NULL DEFAULT 0,
  state              text     NOT NULL DEFAULT 'queued', -- queued | running | succeeded | failed | canceled
  pages_total        integer  NOT NULL DEFAULT 0,
  pages_open         integer  NOT NULL DEFAULT 0,    -- page tasks not yet settled
  pages_done         integer  NOT NULL DEFAULT 0,
  pages_failed       integer  NOT NULL DEFAULT 0,
  calls              integer  NOT NULL DEFAULT 0,    -- model calls, over every task and attempt
  input_tokens       bigint   NOT NULL DEFAULT 0,
  output_tokens      bigint   NOT NULL DEFAULT 0,
  manifest           jsonb,
  index_key          text,                           -- object key of the document index
  error              jsonb,
  deadline_at        timestamptz NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  started_at         timestamptz,
  finished_at        timestamptz
);
CREATE INDEX parses_open     ON parses (group_id)    WHERE state IN ('queued', 'running');
CREATE INDEX parses_deadline ON parses (deadline_at) WHERE state IN ('queued', 'running');

CREATE TABLE workers (
  worker_id  text PRIMARY KEY,                 -- minted at process start, never reused
  seen_at    timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL              -- seen_at + the lease
);

CREATE TABLE tasks (
  parse_id      text     NOT NULL REFERENCES parses ON DELETE CASCADE,
  task_id       text     NOT NULL,             -- prepare | page-<n> | assemble | extract-<name>
  kind          text     NOT NULL,
  group_id      text     NOT NULL,
  project_id    text     NOT NULL DEFAULT '',  -- the group's project; '' is its own
  class         smallint NOT NULL,             -- 0 interactive, 1 batch
  priority      integer  NOT NULL DEFAULT 0,
  seq           integer  NOT NULL,             -- position within the parse; orders a project's queue
  pin           text,                          -- the reader the task is held to, when the parse named one
  -- lane names whose room the task waits for: nothing for a task that calls
  -- no model, the policy's chain of its kind, or the one reader it is pinned
  -- to. Every task of a lane has room or none together, so the claim decides
  -- per lane and never reads a row it cannot take.
  lane          text     NOT NULL GENERATED ALWAYS AS (
                  CASE WHEN kind IN ('prepare', 'assemble') THEN ''
                       WHEN pin IS NULL THEN kind
                       ELSE kind || ':' || pin END) STORED,
  state         text     NOT NULL,             -- queued | leased | succeeded | failed | canceled
  attempt       integer  NOT NULL DEFAULT 0,
  expiries      integer  NOT NULL DEFAULT 0,
  available_at  timestamptz NOT NULL DEFAULT now(),
  lease_owner   text,                          -- the worker running it
  lease_token   bigint   NOT NULL DEFAULT 0,   -- raised by one at every claim
  leased_at     timestamptz,
  reader        text,                          -- the pool the task holds a slot in
  scope         text,                          -- the key scope of that slot
  calling       boolean  NOT NULL DEFAULT false, -- holds the slot now
  charged       integer  NOT NULL DEFAULT 0,   -- fairness units charged at claim
  output        text,                          -- object key of the result that won
  calls         integer  NOT NULL DEFAULT 0,   -- model calls, over every attempt
  input_tokens  bigint   NOT NULL DEFAULT 0,
  output_tokens bigint   NOT NULL DEFAULT 0,
  error         jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  settled_at    timestamptz,
  PRIMARY KEY (parse_id, task_id)
);
-- The order inside a project is priority, then seq, then age. The parse and
-- the task end the key so that two tasks equal in all three, the same page of
-- two parses prepared at one instant, are still taken in one fixed order.
CREATE INDEX tasks_runnable ON tasks (group_id, project_id, class, lane, priority DESC, seq, created_at, parse_id, task_id)
  WHERE state = 'queued';
CREATE INDEX tasks_leased ON tasks (lease_owner) WHERE state = 'leased';
CREATE INDEX tasks_slots  ON tasks (reader, scope) WHERE state = 'leased' AND calling;

CREATE TABLE groups (
  group_id     text PRIMARY KEY,
  weight       integer NOT NULL DEFAULT 1,   -- share of service, 1..1000
  max_running  integer NOT NULL DEFAULT 0,   -- leased tasks at once; 0 is no cap
  max_queued   integer NOT NULL DEFAULT 0,   -- non-terminal parses; 0 is no cap
  max_priority integer NOT NULL DEFAULT 0,
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE group_service (
  group_id text NOT NULL, class smallint NOT NULL,
  vtime    bigint  NOT NULL DEFAULT 0,       -- virtual time, in millionths: units charged / weight
  clock    bigint  NOT NULL DEFAULT 0,       -- virtual time inside the group: the start of its last dispatch
  queued   integer NOT NULL DEFAULT 0,       -- queued tasks, maintained in the transaction of each transition
  running  integer NOT NULL DEFAULT 0,       -- leased tasks
  PRIMARY KEY (group_id, class)
);
CREATE TABLE class_service (
  class  smallint PRIMARY KEY,
  weight integer NOT NULL,                   -- LECTIO_CLASS_WEIGHTS, written when a store opens
  vtime  bigint  NOT NULL DEFAULT 0,         -- virtual time of the class, in millionths
  clock  bigint  NOT NULL DEFAULT 0          -- virtual time inside the class: the start of its last dispatch
);
CREATE TABLE dispatch (
  one   boolean PRIMARY KEY DEFAULT true CHECK (one),
  clock bigint NOT NULL DEFAULT 0            -- virtual time across the classes
);
INSERT INTO dispatch DEFAULT VALUES;

CREATE TABLE projects (
  group_id   text NOT NULL, project_id text NOT NULL,   -- '' is the group's own project
  weight     integer NOT NULL DEFAULT 1,                -- share of the group's service, 1..1000
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (group_id, project_id)
);
CREATE TABLE project_service (
  group_id text NOT NULL, project_id text NOT NULL, class smallint NOT NULL,
  vtime    bigint  NOT NULL DEFAULT 0,       -- virtual time, in millionths: units charged / weight
  queued   integer NOT NULL DEFAULT 0,       -- queued tasks
  running  integer NOT NULL DEFAULT 0,       -- leased tasks
  PRIMARY KEY (group_id, project_id, class)
);

-- lane_service counts the queued tasks of each lane of a project. The claim
-- reads it to find the groups and the projects that hold a task in a lane
-- with room, so a group whose every task waits for a reader that has none is
-- passed over without reading a task: a poll against a full pool costs the
-- same whether 10 tasks wait or a million.
CREATE TABLE lane_service (
  group_id text NOT NULL, project_id text NOT NULL, class smallint NOT NULL, lane text NOT NULL,
  queued   integer NOT NULL DEFAULT 0,       -- queued tasks of the lane
  PRIMARY KEY (group_id, project_id, class, lane)
);
CREATE INDEX lane_service_waiting ON lane_service (class, lane, group_id) WHERE queued > 0;

CREATE TABLE pools (
  reader        text PRIMARY KEY,
  max_in_flight integer NOT NULL,            -- from configuration; across every scope
  cost          integer NOT NULL DEFAULT 1,  -- the fairness charge of one call, from configuration
  failures      integer NOT NULL DEFAULT 0,
  opened_at     timestamptz,                 -- breaker open since
  trial_at      timestamptz,                 -- when the one trial call was admitted
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE pool_scopes (
  reader       text NOT NULL,
  scope        text NOT NULL,                -- '' when one key serves every group; the group id when keys are per tenant
  ceiling      integer NOT NULL,             -- allowed in flight for this scope, 1..max_in_flight
  paused_until timestamptz,                  -- set by a rate-limit reply made with this scope's key
  halved_at    timestamptz,                  -- when the ceiling was last halved
  raised_at    timestamptz,                  -- the instant the ceiling was current: recovery is counted from it
  PRIMARY KEY (reader, scope)
);

-- sweeps records when each sweep last ran, so the exchange that finds one due
-- runs it and the others do not.
CREATE TABLE sweeps (
  name   text PRIMARY KEY,
  ran_at timestamptz NOT NULL DEFAULT '-infinity'
);
INSERT INTO sweeps (name) VALUES ('workers'), ('deadlines');

-- lectio_lock serializes the exchanges, and a cancel, across the fleet for
-- the length of one transaction. Every count the claim reads, a pool's calls
-- in flight and a group's running tasks among them, is exact under it with
-- no further lock. The lock ends with the transaction, so it is safe behind
-- a pooler that hands the connection to another client afterwards.
CREATE FUNCTION lectio_lock() RETURNS void LANGUAGE sql AS $$
  SELECT pg_advisory_xact_lock(119182732982639);
$$;

-- lectio_configure writes what the process was configured with: the queue's
-- settings, the class weights, and one pool per reader. A pool that exists
-- keeps its breaker state, and a pool whose reader is gone from the
-- configuration is removed with its scopes. p_config is a JSON document
-- bound as text.
CREATE FUNCTION lectio_configure(p_config text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  c jsonb := p_config::jsonb;
BEGIN
  INSERT INTO settings (one, lease, sweep_interval, attempts, expiries, backoff_base, backoff_cap,
                        pool_recovery, pool_resume, pool_pause, breaker_failures, breaker_open,
                        scope_by_group, read_chain, extract_chain)
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
          ARRAY(SELECT jsonb_array_elements_text(c->'extract_chain')))
  ON CONFLICT (one) DO UPDATE SET
    lease = EXCLUDED.lease, sweep_interval = EXCLUDED.sweep_interval,
    attempts = EXCLUDED.attempts, expiries = EXCLUDED.expiries,
    backoff_base = EXCLUDED.backoff_base, backoff_cap = EXCLUDED.backoff_cap,
    pool_recovery = EXCLUDED.pool_recovery, pool_resume = EXCLUDED.pool_resume,
    pool_pause = EXCLUDED.pool_pause, breaker_failures = EXCLUDED.breaker_failures,
    breaker_open = EXCLUDED.breaker_open, scope_by_group = EXCLUDED.scope_by_group,
    read_chain = EXCLUDED.read_chain, extract_chain = EXCLUDED.extract_chain;

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
END $$;

-- lectio_count moves the counters of queued and running tasks: the group's,
-- the project's, and for a change in what is queued the lane's. The rows are
-- written in that order everywhere, the group's first, so two transactions
-- that touch the same group never wait on each other in a circle.
CREATE FUNCTION lectio_count(p_group text, p_project text, p_class smallint, p_lane text, p_queued integer, p_running integer)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  UPDATE group_service SET queued = queued + p_queued, running = running + p_running
   WHERE group_id = p_group AND class = p_class;
  UPDATE project_service SET queued = queued + p_queued, running = running + p_running
   WHERE group_id = p_group AND project_id = p_project AND class = p_class;
  IF p_queued <> 0 THEN
    INSERT INTO lane_service AS l (group_id, project_id, class, lane, queued)
    VALUES (p_group, p_project, p_class, p_lane, p_queued)
    ON CONFLICT (group_id, project_id, class, lane) DO UPDATE SET queued = l.queued + EXCLUDED.queued;
  END IF;
END $$;

-- lectio_enqueue writes one task of a parse as queued. The id is fixed by
-- the parse, so writing it twice writes nothing the second time. Tasks that
-- are not pages take a seq below 0 and are dispatched ahead of the same
-- project's pages of the same priority.
CREATE FUNCTION lectio_enqueue(p_parse text, p_task text, p_kind text, p_pin text, p_now timestamptz)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_p    parses%ROWTYPE;
  v_lane text;
BEGIN
  SELECT * INTO STRICT v_p FROM parses WHERE parse_id = p_parse;
  INSERT INTO tasks (parse_id, task_id, kind, group_id, project_id, class, priority, seq, pin, state, available_at, created_at)
  VALUES (p_parse, p_task, p_kind, v_p.group_id, v_p.project_id, v_p.class, v_p.priority, -1, p_pin, 'queued', p_now, p_now)
  ON CONFLICT DO NOTHING
  RETURNING lane INTO v_lane;
  IF FOUND THEN
    PERFORM lectio_count(v_p.group_id, v_p.project_id, v_p.class, v_lane, 1, 0);
  END IF;
END $$;

-- lectio_register records a worker process and starts its lease.
CREATE FUNCTION lectio_register(p_worker text, p_now timestamptz DEFAULT NULL)
RETURNS void LANGUAGE sql AS $$
  INSERT INTO workers (worker_id, seen_at, expires_at)
  SELECT p_worker, coalesce(p_now, now()), coalesce(p_now, now()) + lease FROM settings;
$$;

-- lectio_submit writes a parse and its prepare task. It refreshes the group
-- and the project from the limits the submit came with, and holds the
-- group's row lock from that write to the end of the transaction, so two
-- submits of one group count its parses one after the other and cannot both
-- pass at one below max_queued. It answers created, exists for a parse id
-- that is already there, or queue_full. p_submit is a JSON document bound as
-- text.
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

-- lectio_stop ends a parse that has not ended, as canceled or as failed with
-- an error, and cancels its queued and leased tasks in the same transaction.
-- A canceled task holds no slot from here on, and a settle for it matches no
-- lease. It reports whether the parse was still open.
CREATE FUNCTION lectio_stop(p_parse text, p_state text, p_error jsonb, p_now timestamptz)
RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE
  v_p parses%ROWTYPE;
  v_l record;
BEGIN
  UPDATE parses SET state = p_state, error = p_error, finished_at = p_now
   WHERE parse_id = p_parse AND state IN ('queued', 'running')
  RETURNING * INTO v_p;
  IF NOT FOUND THEN
    RETURN false;
  END IF;
  FOR v_l IN
    WITH hit AS (
      SELECT task_id, state, lane FROM tasks
       WHERE parse_id = p_parse AND state IN ('queued', 'leased') FOR UPDATE
    ), gone AS (
      UPDATE tasks t SET state = 'canceled', calling = false, settled_at = p_now
        FROM hit WHERE t.parse_id = p_parse AND t.task_id = hit.task_id
    )
    SELECT lane, count(*) FILTER (WHERE state = 'queued')::integer AS queued,
           count(*) FILTER (WHERE state = 'leased')::integer AS running
      FROM hit GROUP BY lane ORDER BY lane
  LOOP
    PERFORM lectio_count(v_p.group_id, v_p.project_id, v_p.class, v_l.lane, -v_l.queued, -v_l.running);
  END LOOP;
  RETURN true;
END $$;

-- lectio_cancel is the cancel of a parse. It takes the lock the exchange
-- takes, so a settle is wholly before the cancel or wholly after it. It
-- answers canceled, terminal for a parse that had ended, or missing.
CREATE FUNCTION lectio_cancel(p_parse text, p_now timestamptz DEFAULT NULL)
RETURNS text LANGUAGE plpgsql AS $$
BEGIN
  PERFORM lectio_lock();
  IF lectio_stop(p_parse, 'canceled', NULL, coalesce(p_now, now())) THEN
    RETURN 'canceled';
  END IF;
  IF EXISTS (SELECT 1 FROM parses WHERE parse_id = p_parse) THEN
    RETURN 'terminal';
  END IF;
  RETURN 'missing';
END $$;

-- lectio_settled moves a parse forward when one of its tasks reaches
-- succeeded or failed, in the transaction that settled the task. The graph of
-- a parse is fixed, so there is no edge to follow: prepare writes the page
-- tasks, the page that takes pages_open to 0 writes assemble, and assemble
-- ends the parse. p_settle is the settle the worker sent, or NULL when the
-- store failed the task itself.
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

-- lectio_ceiling is what a scope's ceiling is at p_now: the stored value,
-- raised by a tenth of the pool, at least one, for each whole recovery
-- interval since it was stored, and never above the pool.
CREATE FUNCTION lectio_ceiling(p_ceiling integer, p_since timestamptz, p_max integer, p_recovery interval, p_now timestamptz)
RETURNS integer LANGUAGE sql IMMUTABLE AS $$
  SELECT least(p_max, p_ceiling + greatest(0, floor(
           extract(epoch FROM p_now - coalesce(p_since, p_now)) / extract(epoch FROM p_recovery)))::integer
         * greatest(1, p_max / 10));
$$;

-- lectio_admits is how many calls a scope may have in flight at p_now. For
-- the resume period after a pause ends it is a share of the ceiling that
-- grows from one slot to all of it in proportion to the time passed, so a
-- scope that was limited does not send its whole ceiling at once. Nothing is
-- written: the share is read from the clock.
CREATE FUNCTION lectio_admits(p_ceiling integer, p_paused_until timestamptz, p_resume interval, p_now timestamptz)
RETURNS integer LANGUAGE sql IMMUTABLE AS $$
  SELECT CASE
    WHEN p_paused_until IS NOT NULL AND p_now >= p_paused_until AND p_now < p_paused_until + p_resume
    THEN greatest(1, floor(p_ceiling
           * extract(epoch FROM p_now - p_paused_until) / extract(epoch FROM p_resume))::integer)
    ELSE p_ceiling END;
$$;

-- lectio_limited records a rate-limit reply for the scope a call was made in
-- and answers when the pause ends. The reply describes the key, so only that
-- scope stops. The ceiling is halved once per pause: a call that was claimed
-- before the last halving came back refused too, and says nothing new.
CREATE FUNCTION lectio_limited(p_reader text, p_scope text, p_leased timestamptz, p_wait interval, p_cfg settings, p_now timestamptz)
RETURNS timestamptz LANGUAGE plpgsql AS $$
DECLARE
  v_max   integer;
  v_until timestamptz;
BEGIN
  SELECT max_in_flight INTO v_max FROM pools WHERE reader = p_reader;
  IF NOT FOUND THEN
    -- A task that held no slot, or whose reader left the configuration,
    -- waits by itself.
    RETURN p_now + p_wait;
  END IF;
  INSERT INTO pool_scopes AS s (reader, scope, ceiling, paused_until, halved_at, raised_at)
  VALUES (p_reader, p_scope, greatest(1, v_max / 2), p_now + p_wait, p_now, p_now)
  ON CONFLICT (reader, scope) DO UPDATE SET
    ceiling = CASE
      WHEN s.halved_at IS NULL OR p_leased > s.halved_at
      THEN greatest(1, lectio_ceiling(s.ceiling, s.raised_at, v_max, p_cfg.pool_recovery, p_now) / 2)
      ELSE lectio_ceiling(s.ceiling, s.raised_at, v_max, p_cfg.pool_recovery, p_now) END,
    halved_at = CASE WHEN s.halved_at IS NULL OR p_leased > s.halved_at THEN p_now ELSE s.halved_at END,
    raised_at = p_now,
    paused_until = greatest(s.paused_until, p_now + p_wait)
  RETURNING s.paused_until INTO v_until;
  RETURN v_until;
END $$;

-- lectio_correct moves a task's fairness charge from what was charged at the
-- claim to what the task used, on its project, its group and its class, each
-- by its own weight. It changes the counters of queued and running tasks in
-- the same writes: the task leaves the running count, and joins the queued
-- one when it goes back to the queue.
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

-- lectio_settle ends one attempt at a task under the fence: it matches only
-- a task that is leased to this worker under the token the worker was given.
-- A task that was canceled, reissued after its worker was taken for dead, or
-- settled before matches no row, and nothing is recorded for it. It reports
-- whether the settle was accepted.
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

-- lectio_reap returns the tasks of every worker whose lease ran out to the
-- queue and removes those workers. Each task comes back with expiries + 1
-- and its attempts unchanged: at most one of them killed the worker, so none
-- is charged an attempt, and each runs alone from here on. A task at the
-- bound of expiries died while it ran alone and is the cause: it is failed.
-- The fairness charge made at the claim stands: the call the task was claimed
-- for may have been made, and no one is left to say what it used.
CREATE FUNCTION lectio_reap(p_worker text, p_cfg settings, p_now timestamptz)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_t     tasks%ROWTYPE;
  v_error jsonb;
BEGIN
  FOR v_t IN
    SELECT t.* FROM tasks t JOIN workers w ON w.worker_id = t.lease_owner
     WHERE t.state = 'leased' AND w.expires_at <= p_now AND w.worker_id <> p_worker
     ORDER BY t.parse_id, t.task_id
  LOOP
    IF v_t.expiries + 1 >= p_cfg.expiries THEN
      -- The code is the task's own: an input that kills its worker is an
      -- unreadable page, a file intake cannot open, or a fault of ours.
      v_error := jsonb_build_object(
        'code', CASE v_t.kind WHEN 'page' THEN 'page_unreadable' WHEN 'prepare' THEN 'document_corrupt' ELSE 'internal' END,
        'detail', 'the task ended ' || (v_t.expiries + 1) || ' worker processes');
      UPDATE tasks SET state = 'failed', expiries = expiries + 1, calling = false, error = v_error, settled_at = p_now
       WHERE parse_id = v_t.parse_id AND task_id = v_t.task_id;
      PERFORM lectio_count(v_t.group_id, v_t.project_id, v_t.class, v_t.lane, 0, -1);
      PERFORM lectio_settled(v_t.parse_id, v_t.kind, 'failed', v_error, NULL, p_now);
    ELSE
      UPDATE tasks SET state = 'queued', expiries = expiries + 1, calling = false,
             lease_owner = NULL, reader = NULL, scope = NULL, charged = 0
       WHERE parse_id = v_t.parse_id AND task_id = v_t.task_id;
      PERFORM lectio_count(v_t.group_id, v_t.project_id, v_t.class, v_t.lane, 1, -1);
    END IF;
  END LOOP;
  DELETE FROM workers WHERE expires_at <= p_now AND worker_id <> p_worker;
END $$;

-- lectio_room answers which of the readers whose pool admits a call have
-- room for one scope, the key a call would be made with: the scope is not
-- paused and is below what it admits now. A scope with no row was never
-- limited and admits what the pool does. p_wake is when the earliest pause
-- of the scope ends, when one holds a reader back.
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

-- lectio_lanes answers the lanes whose tasks can run when the readers of
-- p_room have room: the lane of the tasks that call no model, the lane of
-- each kind whose chain names a reader with room, and the lanes pinned to a
-- reader with room.
CREATE FUNCTION lectio_lanes(p_room text[], p_cfg settings)
RETURNS text[] LANGUAGE sql IMMUTABLE AS $$
  SELECT ARRAY['']
      || CASE WHEN (p_cfg).read_chain && p_room THEN ARRAY['page'] ELSE '{}'::text[] END
      || CASE WHEN (p_cfg).extract_chain && p_room THEN ARRAY['extract'] ELSE '{}'::text[] END
      || ARRAY(SELECT 'page:' || r FROM unnest(p_room) r)
      || ARRAY(SELECT 'extract:' || r FROM unnest(p_room) r);
$$;

-- lectio_claim hands the worker up to p_free tasks and answers them as a
-- JSON array. Each task is the dispatch decision of
-- specs/006-fairness-and-priority.md: the class, then the group, then the
-- project, then the project's first task, each among those that hold a task
-- that can run now, and each by start-time fair queuing with a clock of its
-- own. A task that calls a model can run only when a reader it may use has
-- room (specs/007-model-capacity.md), so a task with no room is never read,
-- never written and never charged. p_sleep answers when the earliest pause
-- ends among the scopes that had no room, when the claim ended with a slot
-- still free.
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

-- lectio_exchange is the one call a worker makes. In order it renews the
-- worker's lease, settles what finished under the fence, reports which of
-- the tasks the worker still runs are no longer its own, claims new tasks,
-- and runs the sweeps that are due. p_request and the answer are the JSON
-- documents of internal/tasks, bound and returned as text.
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

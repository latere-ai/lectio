// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package postgres is the durable task store: parses, their tasks, the
// workers' leases, the fair queue's accounting and the reader pools, in
// Postgres. The design is specs/004-durable-tasks.md,
// specs/006-fairness-and-priority.md and specs/007-model-capacity.md.
//
// The logic lives in the database. Every write this package makes is one
// call of a function the migrations carry, lectio_exchange above all, so a
// write is one statement, one round trip and one transaction, with no client
// time inside it. That is what lets the store sit behind a transaction-mode
// pooler: no statement depends on the one before it reaching the same
// connection. The package opens no LISTEN, takes no session lock, prepares
// no named statement, and binds every JSON document as text.
//
// Two connections are named apart. Migrate takes the direct URL, because
// the migrator holds a session lock across its statements. Open takes the
// serving URL, which may be a pooler's.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // the pgx5:// driver pgxmigrate selects by scheme
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"latere.ai/x/pkg/pgxmigrate"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/id"
	"latere.ai/x/lectio/internal/store/postgres/migrations"
	"latere.ai/x/lectio/internal/tasks"
)

// Pool bounds. A worker makes one short statement per exchange and the API
// one per request, so a process holds few connections; the design holds
// behind a server-side pool of one.
const (
	// DefaultMaxConns is the pool a zero Options.MaxConns takes.
	DefaultMaxConns = 4
	// MaxMaxConns is the largest pool the store opens.
	MaxMaxConns = 32

	maxConnLifetime   = 30 * time.Minute
	maxConnIdleTime   = time.Minute
	healthCheckPeriod = time.Minute
)

// workerPrefix marks the id of a worker process.
const workerPrefix = "wrk"

// Options opens the store.
type Options struct {
	// MaxConns bounds the pool. Zero takes DefaultMaxConns, and a value
	// above MaxMaxConns takes MaxMaxConns.
	MaxConns int32

	// Settings are what the queue is run with. They are written to the
	// database when the store opens, so the last store to open decides, and
	// replicas of one deployment carry the same settings.
	Settings tasks.Settings
}

// Store is the task store over one connection pool.
type Store struct {
	pool *pgxpool.Pool
	ids  id.Generator

	// clock is a test's stand-in for the database's now(). It is nil in a
	// running server: no statement then passes a time, and the functions
	// read the database's own clock.
	clock atomic.Pointer[time.Time]
}

// The statements. Each function takes an optional last argument that stands
// in for now(); the second statement of a pair passes it and is used only
// when a test set the clock.
const (
	registerSQL   = `SELECT lectio_register($1)`
	registerAtSQL = `SELECT lectio_register($1, $2)`
	exchangeSQL   = `SELECT lectio_exchange($1, $2)`
	exchangeAtSQL = `SELECT lectio_exchange($1, $2, $3)`
	submitSQL     = `SELECT lectio_submit($1)`
	submitAtSQL   = `SELECT lectio_submit($1, $2)`
	cancelSQL     = `SELECT lectio_cancel($1)`
	cancelAtSQL   = `SELECT lectio_cancel($1, $2)`

	configureSQL = `SELECT lectio_configure($1)`
	versionSQL   = `SELECT version, dirty FROM schema_migrations`

	parseSQL = `SELECT to_jsonb(p)::text FROM parses p WHERE p.parse_id = $1`
	tasksSQL = `SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY t.seq, t.task_id), '[]'::jsonb)::text
	              FROM tasks t WHERE t.parse_id = $1`

	// queueSQL reads every group with its counters per class, its parses
	// that have not ended, and its projects, in one statement and so from
	// one snapshot.
	queueSQL = `
SELECT coalesce(jsonb_agg(jsonb_build_object(
         'group', g.group_id, 'weight', g.weight, 'max_running', g.max_running, 'max_queued', g.max_queued,
         'parses', (SELECT count(*) FROM parses p WHERE p.group_id = g.group_id AND p.state IN ('queued', 'running')),
         'classes', (SELECT coalesce(jsonb_agg(jsonb_build_object(
                       'class', s.class, 'queued', s.queued, 'running', s.running, 'vtime', s.vtime) ORDER BY s.class), '[]'::jsonb)
                       FROM group_service s WHERE s.group_id = g.group_id),
         'projects', (SELECT coalesce(jsonb_agg(jsonb_build_object(
                        'project', pr.project_id, 'weight', pr.weight,
                        'classes', (SELECT coalesce(jsonb_agg(jsonb_build_object(
                                      'class', s.class, 'queued', s.queued, 'running', s.running, 'vtime', s.vtime) ORDER BY s.class), '[]'::jsonb)
                                      FROM project_service s
                                     WHERE s.group_id = pr.group_id AND s.project_id = pr.project_id)) ORDER BY pr.project_id), '[]'::jsonb)
                        FROM projects pr WHERE pr.group_id = g.group_id))
       ORDER BY g.group_id), '[]'::jsonb)::text
  FROM groups g`
)

// Migrate applies the pending migrations over the direct connection. It
// refuses a schema this binary does not know: one a failed migration left
// halfway, and one a newer binary wrote. The migrator holds a session lock
// while it runs, which is what lets several replicas start at once and one
// of them apply the schema, and why the URL must not be a transaction-mode
// pooler's.
func Migrate(ctx context.Context, directURL string) error {
	dsn, err := migrationURL(directURL)
	if err != nil {
		return err
	}
	highest, err := migrations.Highest()
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	conn, err := pgx.Connect(ctx, directURL)
	if err != nil {
		return fmt.Errorf("store: connecting to migrate: %w", err)
	}
	version, dirty, err := schemaVersion(ctx, conn)
	closeErr := conn.Close(ctx)
	switch {
	case err != nil:
		return err
	case closeErr != nil:
		return fmt.Errorf("store: closing the migration check's connection: %w", closeErr)
	case dirty:
		return fmt.Errorf("store: the schema is dirty at version %d: a migration failed halfway and an operator repairs it before this process starts", version)
	case version > highest:
		return fmt.Errorf("store: the schema is at version %d and this binary carries %d: run the version that wrote the schema", version, highest)
	}
	if err := pgxmigrate.Up(dsn, migrations.FS, "."); err != nil {
		return fmt.Errorf("store: applying the schema: %w", err)
	}
	return nil
}

// migrationURL is the direct URL under the scheme golang-migrate's pgx/v5
// driver registers. pgxmigrate imports no driver, so the scheme is what
// selects one. The errors name no part of the URL: it holds a password.
func migrationURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("store: the direct database URL is not a URL")
	}
	if !strings.HasPrefix(u.Scheme, "postgres") {
		return "", errors.New("store: the direct database URL does not have the scheme postgres")
	}
	u.Scheme = "pgx5"
	return u.String(), nil
}

// querier is what reads the schema version: a connection or a pool.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// schemaVersion reads the version golang-migrate recorded. A database with
// no schema at all is at version 0.
func schemaVersion(ctx context.Context, q querier) (version uint, dirty bool, err error) {
	var v int64
	err = q.QueryRow(ctx, versionSQL).Scan(&v, &dirty)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil // migrated to nothing yet
	case errors.As(err, &pgErr) && pgErr.Code == "42P01":
		return 0, false, nil // undefined table: a fresh database
	case err != nil:
		return 0, false, fmt.Errorf("store: reading the schema version: %w", err)
	}
	return uint(v), dirty, nil
}

// Open connects over the serving URL, checks that the schema is the one this
// binary carries, and writes the settings. It does not migrate: Migrate does,
// over the direct connection, before the first Open.
func Open(ctx context.Context, poolURL string, o Options) (*Store, error) {
	settings := o.Settings.WithDefaults()
	if err := settings.Validate(); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	cfg, err := pgxpool.ParseConfig(poolURL)
	if err != nil {
		// The parse error is not kept: it may quote the URL.
		return nil, errors.New("store: the serving database URL is not a Postgres URL")
	}
	// The driver's default prepares a named statement per query and keeps
	// it for the life of the connection, which a transaction-mode pooler
	// hands to another client between two statements. Describing the
	// unnamed statement instead keeps nothing on the server. A URL that
	// names another mode keeps it.
	if cfg.ConnConfig.DefaultQueryExecMode == pgx.QueryExecModeCacheStatement {
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	}
	cfg.MaxConns = bound(o.MaxConns)
	cfg.MinConns = 0
	cfg.MaxConnLifetime = maxConnLifetime
	cfg.MaxConnIdleTime = maxConnIdleTime
	cfg.HealthCheckPeriod = healthCheckPeriod
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: opening the pool: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.prepare(ctx, settings); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// bound holds the pool inside its ceiling.
func bound(maxConns int32) int32 {
	switch {
	case maxConns <= 0:
		return DefaultMaxConns
	case maxConns > MaxMaxConns:
		return MaxMaxConns
	}
	return maxConns
}

// prepare refuses a schema at another version than this binary's and writes
// the settings.
func (s *Store) prepare(ctx context.Context, settings tasks.Settings) error {
	highest, err := migrations.Highest()
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	version, dirty, err := schemaVersion(ctx, s.pool)
	switch {
	case err != nil:
		return err
	case dirty || version != highest:
		return fmt.Errorf("store: the schema is at version %d (dirty: %t) and this binary needs %d: migrate over the direct connection first", version, dirty, highest)
	}
	doc, err := json.Marshal(configOf(settings))
	if err != nil {
		return fmt.Errorf("store: encoding the settings: %w", err)
	}
	if _, err := s.pool.Exec(ctx, configureSQL, string(doc)); err != nil {
		return fmt.Errorf("store: writing the settings: %w", err)
	}
	return nil
}

// config is the document lectio_configure reads. Durations travel as whole
// milliseconds.
type config struct {
	LeaseMS           int64    `json:"lease_ms"`
	SweepMS           int64    `json:"sweep_ms"`
	Attempts          int      `json:"attempts"`
	Expiries          int      `json:"expiries"`
	BackoffBaseMS     int64    `json:"backoff_base_ms"`
	BackoffCapMS      int64    `json:"backoff_cap_ms"`
	InteractiveWeight int      `json:"interactive_weight"`
	BatchWeight       int      `json:"batch_weight"`
	PoolRecoveryMS    int64    `json:"pool_recovery_ms"`
	PoolResumeMS      int64    `json:"pool_resume_ms"`
	PoolPauseMS       int64    `json:"pool_pause_ms"`
	BreakerFailures   int      `json:"breaker_failures"`
	BreakerOpenMS     int64    `json:"breaker_open_ms"`
	ScopeByGroup      bool     `json:"scope_by_group"`
	ReadChain         []string `json:"read_chain"`
	ExtractChain      []string `json:"extract_chain"`
	Pools             []pool   `json:"pools"`
}

type pool struct {
	Reader      string `json:"reader"`
	MaxInFlight int    `json:"max_in_flight"`
	Cost        int    `json:"cost"`
}

func configOf(s tasks.Settings) config {
	c := config{
		LeaseMS: s.Lease.Milliseconds(), SweepMS: s.SweepInterval.Milliseconds(),
		Attempts: s.Attempts, Expiries: s.Expiries,
		BackoffBaseMS: s.BackoffBase.Milliseconds(), BackoffCapMS: s.BackoffCap.Milliseconds(),
		InteractiveWeight: s.InteractiveWeight, BatchWeight: s.BatchWeight,
		PoolRecoveryMS: s.PoolRecovery.Milliseconds(), PoolResumeMS: s.PoolResume.Milliseconds(),
		PoolPauseMS:     s.PoolPause.Milliseconds(),
		BreakerFailures: s.BreakerFailures, BreakerOpenMS: s.BreakerOpen.Milliseconds(),
		ScopeByGroup: s.KeysPerGroup,
		// An empty chain is written as an empty array, never as null.
		ReadChain: append([]string{}, s.ReadChain...), ExtractChain: append([]string{}, s.ExtractChain...),
		Pools: []pool{},
	}
	for _, p := range s.Pools {
		c.Pools = append(c.Pools, pool{Reader: p.Reader, MaxInFlight: p.MaxInFlight, Cost: p.Cost})
	}
	return c
}

// Close closes the pool. Statements in flight end with an error.
func (s *Store) Close() { s.pool.Close() }

// stamp chooses between a statement and its twin that takes a time. A test's
// clock, when one is set, is passed as the last argument of the twin; a
// running server passes none, and the function reads the database's now().
func (s *Store) stamp(sql, sqlAt string, args []any) (string, []any) {
	if at := s.clock.Load(); at != nil && sqlAt != "" {
		return sqlAt, append(args, *at)
	}
	return sql, args
}

// text runs one statement that answers one text value.
func (s *Store) text(ctx context.Context, sql, sqlAt string, args ...any) (string, error) {
	sql, args = s.stamp(sql, sqlAt, args)
	var out string
	if err := s.pool.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
		return "", err
	}
	return out, nil
}

// decode runs one statement that answers one JSON document and reads it.
func (s *Store) decode(ctx context.Context, into any, sql, sqlAt string, args ...any) error {
	doc, err := s.text(ctx, sql, sqlAt, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(doc), into); err != nil {
		return fmt.Errorf("reading the database's answer: %w", err)
	}
	return nil
}

// Register records a worker process under a new id and starts its lease.
// A process registers once when it starts, and again, under another id,
// when an exchange answers that the fleet gave it up.
func (s *Store) Register(ctx context.Context) (worker string, err error) {
	worker = s.ids.New(workerPrefix)
	sql, args := s.stamp(registerSQL, registerAtSQL, []any{worker})
	if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
		return "", fmt.Errorf("store: registering a worker: %w", err)
	}
	return worker, nil
}

// Exchange is the one call a worker makes: it renews the worker's lease,
// settles what finished, reports what the worker lost, claims new tasks, and
// runs the sweeps that are due, in one statement.
func (s *Store) Exchange(ctx context.Context, worker string, req tasks.Request) (tasks.Reply, error) {
	if err := req.Validate(); err != nil {
		return tasks.Reply{}, fmt.Errorf("store: %w", err)
	}
	doc, err := json.Marshal(req)
	if err != nil {
		return tasks.Reply{}, fmt.Errorf("store: encoding the exchange: %w", err)
	}
	var reply tasks.Reply
	if err := s.decode(ctx, &reply, exchangeSQL, exchangeAtSQL, worker, string(doc)); err != nil {
		return tasks.Reply{}, fmt.Errorf("store: the exchange of %s: %w", worker, err)
	}
	return reply, nil
}

// Submission is a parse to queue, with the limits its submit came with.
type Submission struct {
	Parse string `json:"parse"`
	Owner string `json:"owner"`

	// Group is the fairness group the parse joins; empty is the owner.
	// Project is the group's project; empty is the group's own. The weights
	// and the bounds are the group's and the project's settings, refreshed
	// by every submit: a weight of zero takes 1, and a bound of zero is no
	// bound.
	Group         string `json:"group"`
	Project       string `json:"project"`
	Weight        int    `json:"weight"`
	ProjectWeight int    `json:"project_weight"`
	MaxRunning    int    `json:"max_running"`
	MaxQueued     int    `json:"max_queued"`
	MaxPriority   int    `json:"max_priority"`

	Class    tasks.Class `json:"class"`
	Priority int         `json:"priority"`

	// Pin is the reader the parse named, when it named one. Its pages wait
	// for that reader and are never read by another.
	Pin string `json:"pin"`

	AllowFailedPages int `json:"allow_failed_pages"`

	// Deadline is how long the parse has from its submit, on the database's
	// clock. Nothing waits without bound, so it is required.
	Deadline time.Duration `json:"-"`
}

// submission is a Submission with the deadline in milliseconds.
type submission struct {
	Submission
	DeadlineMS int64 `json:"deadline_ms"`
}

// Submit writes a parse and its prepare task in one transaction. created is
// false when a parse of that id is already there, which is what a submit
// repeated after an answer was lost finds. A group that already holds
// max_queued parses that have not ended is refused with queue_full; two
// submits of one group are serialized on the group's row, so they cannot
// both pass at one below the bound.
func (s *Store) Submit(ctx context.Context, sub Submission) (created bool, err error) {
	switch {
	case sub.Parse == "" || sub.Owner == "":
		return false, fault.New(fault.InvalidRequest, "a parse has an id and an owner")
	case sub.Class != tasks.Interactive && sub.Class != tasks.Batch:
		return false, fault.New(fault.InvalidRequest, "the class is %d", sub.Class)
	case sub.Deadline < time.Millisecond:
		return false, fault.New(fault.InvalidRequest, "a parse has a deadline")
	}
	if sub.Group == "" {
		sub.Group = sub.Owner
	}
	doc, err := json.Marshal(submission{Submission: sub, DeadlineMS: sub.Deadline.Milliseconds()})
	if err != nil {
		return false, fmt.Errorf("store: encoding the submit of %s: %w", sub.Parse, err)
	}
	answer, err := s.text(ctx, submitSQL, submitAtSQL, string(doc))
	switch {
	case err != nil:
		return false, fmt.Errorf("store: submitting %s: %w", sub.Parse, err)
	case answer == "queue_full":
		return false, fault.New(fault.QueueFull, "the group %s holds as many parses as it may", sub.Group)
	}
	return answer == "created", nil
}

// Cancel moves a parse and its queued and leased tasks to canceled in one
// transaction. A task that finishes afterwards settles against a row that is
// no longer leased, so nothing is recorded after the cancel returns.
func (s *Store) Cancel(ctx context.Context, parseID string) error {
	answer, err := s.text(ctx, cancelSQL, cancelAtSQL, parseID)
	switch {
	case err != nil:
		return fmt.Errorf("store: canceling %s: %w", parseID, err)
	case answer == "missing":
		return fault.New(fault.ParseNotFound, "no parse %s", parseID)
	case answer == "terminal":
		return fault.New(fault.AlreadyTerminal, "parse %s has ended", parseID)
	}
	return nil
}

// Parse is a parse as the control plane holds it.
type Parse struct {
	ID               string          `json:"parse_id"`
	Owner            string          `json:"owner"`
	Group            string          `json:"group_id"`
	Project          string          `json:"project_id"`
	Class            tasks.Class     `json:"class"`
	Priority         int             `json:"priority"`
	Pin              string          `json:"pin"`
	AllowFailedPages int             `json:"allow_failed_pages"`
	State            string          `json:"state"`
	PagesTotal       int             `json:"pages_total"`
	PagesOpen        int             `json:"pages_open"`
	PagesDone        int             `json:"pages_done"`
	PagesFailed      int             `json:"pages_failed"`
	Calls            int             `json:"calls"`
	InputTokens      int64           `json:"input_tokens"`
	OutputTokens     int64           `json:"output_tokens"`
	Manifest         json.RawMessage `json:"manifest"`
	Index            string          `json:"index_key"`
	Error            *tasks.Error    `json:"error"`
	DeadlineAt       time.Time       `json:"deadline_at"`
	CreatedAt        time.Time       `json:"created_at"`
	StartedAt        *time.Time      `json:"started_at"`
	FinishedAt       *time.Time      `json:"finished_at"`
}

// Parse returns a parse with its counters.
func (s *Store) Parse(ctx context.Context, parseID string) (Parse, error) {
	var p Parse
	err := s.decode(ctx, &p, parseSQL, "", parseID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Parse{}, fault.New(fault.ParseNotFound, "no parse %s", parseID)
	case err != nil:
		return Parse{}, fmt.Errorf("store: reading %s: %w", parseID, err)
	}
	return p, nil
}

// Task is a task row.
type Task struct {
	Parse        string       `json:"parse_id"`
	ID           string       `json:"task_id"`
	Kind         tasks.Kind   `json:"kind"`
	Group        string       `json:"group_id"`
	Project      string       `json:"project_id"`
	Class        tasks.Class  `json:"class"`
	Priority     int          `json:"priority"`
	Seq          int          `json:"seq"`
	Pin          string       `json:"pin"`
	State        tasks.State  `json:"state"`
	Attempt      int          `json:"attempt"`
	Expiries     int          `json:"expiries"`
	AvailableAt  time.Time    `json:"available_at"`
	LeaseOwner   string       `json:"lease_owner"`
	LeaseToken   int64        `json:"lease_token"`
	LeasedAt     *time.Time   `json:"leased_at"`
	Reader       string       `json:"reader"`
	Scope        string       `json:"scope"`
	Calling      bool         `json:"calling"`
	Charged      int          `json:"charged"`
	Output       string       `json:"output"`
	Calls        int          `json:"calls"`
	InputTokens  int64        `json:"input_tokens"`
	OutputTokens int64        `json:"output_tokens"`
	Error        *tasks.Error `json:"error"`
	CreatedAt    time.Time    `json:"created_at"`
	SettledAt    *time.Time   `json:"settled_at"`
}

// Tasks returns the task rows of a parse, in the order of its queue. The
// rows of tasks that succeeded are gone once the parse has ended; the parse
// keeps their counters.
func (s *Store) Tasks(ctx context.Context, parseID string) ([]Task, error) {
	var out []Task
	if err := s.decode(ctx, &out, tasksSQL, "", parseID); err != nil {
		return nil, fmt.Errorf("store: reading the tasks of %s: %w", parseID, err)
	}
	return out, nil
}

// ClassQueue is the queue of one class: the tasks waiting and the tasks
// leased, and the virtual time the fair queue holds for it, in millionths
// of a unit.
type ClassQueue struct {
	Class   tasks.Class `json:"class"`
	Queued  int         `json:"queued"`
	Running int         `json:"running"`
	VTime   int64       `json:"vtime"`
}

// ProjectQueue is the queue of one project of a group.
type ProjectQueue struct {
	Project string       `json:"project"`
	Weight  int          `json:"weight"`
	Classes []ClassQueue `json:"classes"`
}

// GroupQueue is the queue of one group, and of each of its projects.
type GroupQueue struct {
	Group      string `json:"group"`
	Weight     int    `json:"weight"`
	MaxRunning int    `json:"max_running"`
	MaxQueued  int    `json:"max_queued"`
	// Parses counts the group's parses that have not ended.
	Parses   int            `json:"parses"`
	Classes  []ClassQueue   `json:"classes"`
	Projects []ProjectQueue `json:"projects"`
}

// Queue returns the queue of every group, by group id.
func (s *Store) Queue(ctx context.Context) ([]GroupQueue, error) {
	var out []GroupQueue
	if err := s.decode(ctx, &out, queueSQL, ""); err != nil {
		return nil, fmt.Errorf("store: reading the queue: %w", err)
	}
	return out, nil
}

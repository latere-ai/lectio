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
	retrySQL      = `SELECT lectio_retry($1, $2)`
	retryAtSQL    = `SELECT lectio_retry($1, $2, $3)`
	eventsSQL     = `SELECT lectio_events($1, $2, $3)`
	usageSQL      = `SELECT lectio_usage($1)`
	queueSQL      = `SELECT lectio_queue($1)`
	queueAtSQL    = `SELECT lectio_queue($1, $2)`

	configureSQL = `SELECT lectio_configure($1)`
	versionSQL   = `SELECT version, dirty FROM schema_migrations`

	parseSQL = `SELECT to_jsonb(p)::text FROM parses p WHERE p.parse_id = $1`
	taskSQL  = `SELECT to_jsonb(t)::text FROM tasks t WHERE t.parse_id = $1 AND t.task_id = $2`
	pingSQL  = `SELECT 1`

	parseOfSQL     = `SELECT to_jsonb(p)::text FROM parses p WHERE p.parse_id = $1 AND p.owner = $2`
	parseDeleteSQL = `SELECT lectio_parse_delete($1, $2)`

	// parsesSQL reads one page of the parses of some owners, newest first.
	// The owners are a JSON array bound as text, or the JSON null for every
	// owner's. Ids sort by creation time, so the page after an id is the
	// ids below it. An empty filter member matches everything, and the
	// labels are matched by containment: every one named must be on the
	// parse.
	parsesSQL = `
SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY p.parse_id DESC), '[]'::jsonb)::text
  FROM (SELECT * FROM parses
         WHERE ($1::jsonb = 'null'::jsonb
                OR owner = ANY (ARRAY(SELECT jsonb_array_elements_text(nullif($1::jsonb, 'null'::jsonb)))))
           AND ($2 = '' OR state = $2)
           AND ($3 = '' OR file_id = $3)
           AND ($4 = '' OR origin->>'path' = $4)
           AND labels @> $5::jsonb
           AND ($6 = '' OR parse_id < $6)
         ORDER BY parse_id DESC LIMIT $7) p`

	// A file is read with when it may be removed, which follows from its
	// row and from the parses that read it.
	fileSQL = `SELECT (to_jsonb(f) || jsonb_build_object('expires_at', lectio_file_until(f)))::text
	             FROM files f WHERE f.file_id = $1 AND f.deleted_at IS NULL`
	fileByContentSQL = `SELECT (to_jsonb(f) || jsonb_build_object('expires_at', lectio_file_until(f)))::text
	                      FROM files f WHERE f.owner = $1 AND f.sha256 = $2 AND f.deleted_at IS NULL`
	fileDeleteSQL   = `SELECT lectio_file_delete($1, $2)`
	fileDeleteAtSQL = `SELECT lectio_file_delete($1, $2, $3)`
	fileForgetSQL   = `DELETE FROM files WHERE file_id = $1 AND deleted_at IS NOT NULL`

	// fileInsertSQL writes a file's row, or answers the row the owner
	// already has for the same bytes. Either way the file is kept for the
	// retention of this upload from now: an upload of bytes the owner has
	// is a use of the file. $8 is the retention in milliseconds, and 0
	// keeps the file. $9 stands in for now() when a test set the clock.
	// The row a conflict finds is locked and written, so 2 uploads of the
	// same bytes at one instant answer the same file.
	fileInsertSQL = `
WITH ins AS (
  INSERT INTO files AS f (file_id, owner, name, size, sha256, media_type, object_key, retention, kept_until)
  VALUES ($1, $2, $3, $4, $5, $6, $7,
          CASE WHEN $8::bigint > 0 THEN $8::bigint * interval '1 millisecond' END,
          CASE WHEN $8::bigint > 0 THEN coalesce($9::timestamptz, now()) + $8::bigint * interval '1 millisecond' END)
  ON CONFLICT (owner, sha256) WHERE deleted_at IS NULL
  DO UPDATE SET retention = EXCLUDED.retention, kept_until = EXCLUDED.kept_until
  RETURNING f.*, (f.xmax = 0) AS created)
SELECT jsonb_build_object('created', ins.created,
         'file', to_jsonb(ins) - 'created' || jsonb_build_object('expires_at', greatest(ins.kept_until,
                   (SELECT max(p.finished_at) FROM parses p WHERE p.file_id = ins.file_id) + ins.retention)))::text
  FROM ins`

	// fileKeepSQL keeps a file its owner uploaded again for the retention
	// of that upload from now.
	fileKeepSQL = `
UPDATE files SET retention = CASE WHEN $2::bigint > 0 THEN $2::bigint * interval '1 millisecond' END,
       kept_until = CASE WHEN $2::bigint > 0 THEN coalesce($3::timestamptz, now()) + $2::bigint * interval '1 millisecond' END
 WHERE file_id = $1 AND deleted_at IS NULL`

	expiredSQL       = `SELECT lectio_expired($1)`
	expiredAtSQL     = `SELECT lectio_expired($1, $2)`
	parseExpireSQL   = `SELECT lectio_parse_expire($1)`
	parseExpireAtSQL = `SELECT lectio_parse_expire($1, $2)`
	tasksSQL         = `SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY t.seq, t.task_id), '[]'::jsonb)::text
	              FROM tasks t WHERE t.parse_id = $1`
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
	DescribeChain     []string `json:"describe_chain"`
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
		DescribeChain: append([]string{}, s.DescribeChain...),
		Pools:         []pool{},
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

// at is a test's clock as a statement's parameter, and NULL in a running
// server, where the statement reads the database's now().
func (s *Store) at() *time.Time { return s.clock.Load() }

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

	// PagesPerDay is how many pages the group's parses may count in one
	// day, and is the group's setting as its bounds are. Zero is no budget.
	// MaxPages is the most pages this parse may select; zero leaves the
	// server's own bound alone. Retention is how long the parse is kept
	// after it ended; zero keeps it.
	PagesPerDay int           `json:"pages_per_day"`
	MaxPages    int           `json:"max_pages"`
	Retention   time.Duration `json:"-"`

	Class    tasks.Class `json:"class"`
	Priority int         `json:"priority"`

	// Pin is the reader the parse named, when it named one. Its pages wait
	// for that reader and are never read by another.
	Pin string `json:"pin"`

	AllowFailedPages int `json:"allow_failed_pages"`

	// Deadline is how long the parse has from its submit, on the database's
	// clock. Nothing waits without bound, so it is required.
	Deadline time.Duration `json:"-"`

	// File is the file the parse reads. A file the owner does not have is
	// refused with file_not_found. Empty is a parse with no file, which only
	// a test of the queue submits.
	File string `json:"file,omitempty"`

	// Options, Labels and Origin are what the caller chose, kept as they
	// came and returned with the parse.
	Options ParseOptions      `json:"options"`
	Labels  map[string]string `json:"labels,omitempty"`
	Origin  *Origin           `json:"origin,omitempty"`

	// IdempotencyKey makes the submit safe to repeat for 24 hours: a second
	// submit of the owner with the key answers the parse the first made,
	// when BodyDigest is the same, and is refused with idempotency_conflict
	// when it is not.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	BodyDigest     string `json:"body_digest,omitempty"`

	// ReadBase names what reading a page of this parse means, less the
	// page: the file's bytes, the languages hinted, and every reader that
	// may come to read it with its version. A page that was read whole is
	// kept under it, and a later parse of the owner with the same base
	// takes the page when its Options say so. Empty keeps and takes nothing.
	ReadBase string `json:"read_base,omitempty"`
}

// ParseOptions are the members of a submit that say what is read.
type ParseOptions struct {
	// Pages is the caller's selection, empty for every page.
	Pages string `json:"pages,omitempty"`
	// Languages are hints for the reader, most likely first.
	Languages []string `json:"languages,omitempty"`
	// Reuse says the parse takes a page an earlier parse of the same owner
	// read whole from the same bytes with the same readers.
	Reuse bool `json:"reuse"`
}

// Origin is where a file lives for the caller. It is stored and returned,
// and never interpreted.
type Origin struct {
	Store   string `json:"store,omitempty"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
}

// submission is a Submission with its durations in milliseconds.
type submission struct {
	Submission
	DeadlineMS  int64 `json:"deadline_ms"`
	RetentionMS int64 `json:"retention_ms"`
}

// Submit writes a parse and its prepare task in one transaction. parse is
// the id of the parse that is there afterwards, and created is false when it
// was there before: a parse of that id, which is what a submit repeated
// after an answer was lost finds, or the parse an earlier submit made with
// the same idempotency key. A group that already holds max_queued parses
// that have not ended is refused with queue_full; two submits of one group
// are serialized on the group's row, so they cannot both pass at one below
// the bound, and two with one idempotency key make one parse. A group whose
// day holds no page more is refused with budget_exhausted.
func (s *Store) Submit(ctx context.Context, sub Submission) (parse string, created bool, err error) {
	switch {
	case sub.Parse == "" || sub.Owner == "":
		return "", false, fault.New(fault.InvalidRequest, "a parse has an id and an owner")
	case sub.Class != tasks.Interactive && sub.Class != tasks.Batch:
		return "", false, fault.New(fault.InvalidRequest, "the class is %d", sub.Class)
	case sub.Deadline < time.Millisecond:
		return "", false, fault.New(fault.InvalidRequest, "a parse has a deadline")
	}
	if sub.Group == "" {
		sub.Group = sub.Owner
	}
	doc, err := json.Marshal(submission{
		Submission: sub, DeadlineMS: sub.Deadline.Milliseconds(), RetentionMS: sub.Retention.Milliseconds(),
	})
	if err != nil {
		return "", false, fmt.Errorf("store: encoding the submit of %s: %w", sub.Parse, err)
	}
	var answer struct {
		Result string `json:"result"`
		Parse  string `json:"parse"`
	}
	if err := s.decode(ctx, &answer, submitSQL, submitAtSQL, string(doc)); err != nil {
		return "", false, fmt.Errorf("store: submitting %s: %w", sub.Parse, err)
	}
	switch answer.Result {
	case "queue_full":
		return "", false, fault.New(fault.QueueFull, "the group %s holds as many parses as it may", sub.Group)
	case "budget_exhausted":
		return "", false, fault.New(fault.BudgetExhausted, "the group %s has no page left of its %d for the day", sub.Group, sub.PagesPerDay)
	case "file_not_found":
		return "", false, fault.New(fault.FileNotFound, "no file %s", sub.File)
	case "conflict":
		return "", false, fault.New(fault.IdempotencyConflict, "the idempotency key was used with another body")
	}
	return answer.Parse, answer.Result == "created", nil
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

// Retry queues again the pages of an owner's parse that failed, in one
// transaction: the parse is running again when it returns, with only those
// pages open, and the pages that were read are not read again. It is for a
// parse that assemble ended with a failed page. One that has not ended is
// refused with not_terminal, and one with nothing to read again with
// conflict: it has no failed page, it ended before its pages were all read,
// its task rows are no longer there, its retention has ended, or an
// extraction or a figure of it is queued or running. The parse stays in its
// group and is held to the group's bounds as they stand: a
// group that holds max_queued parses that have not ended is refused with
// queue_full, and one whose day does not hold the failed pages with
// budget_exhausted.
func (s *Store) Retry(ctx context.Context, owner, parseID string) error {
	answer, err := s.text(ctx, retrySQL, retryAtSQL, owner, parseID)
	if err != nil {
		return fmt.Errorf("store: retrying %s: %w", parseID, err)
	}
	switch answer {
	case "missing":
		return fault.New(fault.ParseNotFound, "no parse %s", parseID)
	case "not_terminal":
		return fault.New(fault.NotTerminal, "parse %s has not ended", parseID)
	case "busy":
		return fault.New(fault.Conflict, "an extraction or a figure of parse %s is queued or running, and a retry writes again the pages it reads: retry once it has ended", parseID)
	case "nothing":
		return fault.New(fault.Conflict, "parse %s has no failed page to read again", parseID)
	case "unassembled":
		return fault.New(fault.Conflict, "parse %s ended before its pages were all read, so none of them is read again: submit it again", parseID)
	case "gone":
		return fault.New(fault.Conflict, "the task rows of parse %s are no longer kept, so its pages cannot be read again: submit it again", parseID)
	case "expired":
		return fault.New(fault.Conflict, "the retention of parse %s has ended", parseID)
	case "queue_full":
		return fault.New(fault.QueueFull, "the group of parse %s holds as many parses as it may", parseID)
	case "budget_exhausted":
		return fault.New(fault.BudgetExhausted, "the group of parse %s has too few pages left of its day for the pages that failed", parseID)
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

	// What the caller chose, and how many of the pages done were taken from
	// an earlier read.
	File        string            `json:"file_id"`
	Options     ParseOptions      `json:"options"`
	Labels      map[string]string `json:"labels"`
	Origin      *Origin           `json:"origin"`
	PagesReused int               `json:"pages_reused"`

	// MaxPages is the most pages the parse may select, when its allow
	// named a bound. Reserved is how many pages it holds of its group's
	// day, and ExpiresAt when it is removed, known once it has ended and
	// nil for a parse that is kept.
	MaxPages  int        `json:"max_pages"`
	Reserved  int        `json:"reserved"`
	ExpiresAt *time.Time `json:"expires_at"`

	// RetriedAt is when the parse's failed pages were last queued again,
	// and nil for a parse that never was retried.
	RetriedAt *time.Time `json:"retried_at"`

	// Events counts the changes of the parse's state and progress. It is
	// the sequence the events of the parse are numbered from.
	Events int64 `json:"events"`

	// Fields is how many extractions were asked of the parse, and Described
	// how many of its figures hold a description. A parse with neither has
	// nothing more to read than its pages.
	Fields    int `json:"fields"`
	Described int `json:"described"`
}

// Terminal reports whether the parse has ended.
func (p Parse) Terminal() bool {
	return p.State == "succeeded" || p.State == "failed" || p.State == "canceled"
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

// PageEvent is one page of a parse that settled: its number, how it ended,
// and the change of its parse that settled it.
type PageEvent struct {
	Page  int          `json:"page"`
	State tasks.State  `json:"state"`
	Event int64        `json:"event"`
	Error *tasks.Error `json:"error"`
}

// Events is what a stream of a parse's events is told at one instant, read
// from one snapshot: the parse as it stands, and the pages that settled
// after a change of it.
type Events struct {
	Parse Parse       `json:"parse"`
	Pages []PageEvent `json:"pages"`
}

// Events returns a parse as it stands and the pages of it that settled
// after its change after, oldest first and at most limit of them. Every
// page that settled up to the parse's own count of changes is among them or
// came before: the 2 are read in one statement. A page is there for as long
// as its task's row is, which is while its parse runs and after a parse
// that ended with a failed page.
func (s *Store) Events(ctx context.Context, parseID string, after int64, limit int) (Events, error) {
	var out *Events
	if err := s.decode(ctx, &out, eventsSQL, "", parseID, after, limit); err != nil {
		return Events{}, fmt.Errorf("store: reading the events of %s: %w", parseID, err)
	}
	if out == nil {
		return Events{}, fault.New(fault.ParseNotFound, "no parse %s", parseID)
	}
	return *out, nil
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

	// ChainAt is the position in the policy's chain the task's candidates
	// begin at, Invalid how many unusable replies the reader it is with
	// gave, and Escalated whether the one move for unusable replies was
	// made. Lane names whose room the task waits for.
	ChainAt   int    `json:"chain_at"`
	Invalid   int    `json:"invalid"`
	Escalated bool   `json:"escalated"`
	Lane      string `json:"lane"`

	// Result is what the task said of its output when it succeeded.
	Result json.RawMessage `json:"result"`

	// Event is the change of the parse that settled the task, for a page
	// that succeeded or failed, and 0 for a task that has not settled.
	Event int64 `json:"event"`
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
	Project string `json:"project"`
	Weight  int    `json:"weight"`
	// Parses counts the project's parses that have not ended.
	Parses  int          `json:"parses"`
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

// The states of a reader's breaker as a read of the queue names them.
const (
	// BreakerClosed admits calls.
	BreakerClosed = "closed"
	// BreakerOpen admits none: the reader failed too often in a row.
	BreakerOpen = "open"
	// BreakerTrial is past its open period: one call is admitted, and its
	// outcome closes the breaker or opens it again.
	BreakerTrial = "trial"
)

// ScopeQueue is one key scope of a pool that was limited: the scope of the
// key every group shares, which is empty, or of one group's key.
type ScopeQueue struct {
	Scope string `json:"scope"`
	// Ceiling is how many calls the scope may have in flight as it stands,
	// and InFlight how many it has.
	Ceiling  int `json:"ceiling"`
	InFlight int `json:"in_flight"`
	// PausedUntil is when a pause of the scope ends, and nil for a scope
	// that is not paused.
	PausedUntil *time.Time `json:"paused_until"`
}

// PoolQueue is the pool of one reader.
type PoolQueue struct {
	Reader      string `json:"reader"`
	MaxInFlight int    `json:"max_in_flight"`
	// InFlight counts the calls in flight, of every group or of the groups
	// the read names.
	InFlight int `json:"in_flight"`
	// Breaker is BreakerClosed, BreakerOpen or BreakerTrial.
	Breaker string       `json:"breaker"`
	Scopes  []ScopeQueue `json:"scopes"`
}

// Queue is the queue as it stands at one instant.
type Queue struct {
	Groups []GroupQueue `json:"groups"`
	Pools  []PoolQueue  `json:"pools"`
}

// Queue returns the queue: the groups with their projects, by group id, and
// the readers' pools, by reader. Nil groups is every group that holds a
// parse that has not ended. A list is those groups, whether they hold one
// or not, with the pools as those groups see them: their own calls in
// flight, their own key scopes, and the scope of the key every group
// shares. The read is of the counters the claim keeps, and of no queued
// task.
func (s *Store) Queue(ctx context.Context, groups []string) (Queue, error) {
	// A nil list encodes as the JSON null, which the function reads as
	// every group that holds work.
	scope, err := json.Marshal(groups)
	if err != nil {
		return Queue{}, fmt.Errorf("store: encoding the groups of a read of the queue: %w", err)
	}
	var out Queue
	if err := s.decode(ctx, &out, queueSQL, queueAtSQL, string(scope)); err != nil {
		return Queue{}, fmt.Errorf("store: reading the queue: %w", err)
	}
	return out, nil
}

// UsageQuery says which sums of the meter are read: by which key, over
// which intervals, in which span of time, and for whom.
type UsageQuery struct {
	// By is the key the sums are grouped by: group, owner or reader.
	By string `json:"by"`
	// Interval is the length of one interval: hour or day, in UTC.
	Interval string `json:"interval"`
	// From and To bound the read: an interval is in the answer when it
	// begins at or after From and before To.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Owners and Groups narrow the read to those owners and those groups.
	// Nil is every one, and an empty list is none.
	Owners []string `json:"owners"`
	Groups []string `json:"groups"`
}

// UsageSum is what one key used in one interval.
type UsageSum struct {
	Key   string    `json:"key"`
	Start time.Time `json:"start"`
	// Pages are the pages read, and Calls the model calls made for them,
	// the ones that failed or were told to wait included.
	Pages        int64 `json:"pages"`
	Calls        int64 `json:"calls"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Usage reads the meter: what was read and what it cost, summed by a key
// over fixed intervals, ordered by interval and then by key. The meter is
// written with each settle, so the read is of one small table and of no
// task.
func (s *Store) Usage(ctx context.Context, q UsageQuery) ([]UsageSum, error) {
	doc, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("store: encoding a read of the meter: %w", err)
	}
	var out []UsageSum
	if err := s.decode(ctx, &out, usageSQL, "", string(doc)); err != nil {
		return nil, fmt.Errorf("store: reading the meter: %w", err)
	}
	return out, nil
}

// Task returns one task row of a parse. ok is false when the parse has no
// such row: the task was never written, or it succeeded and its parse ended.
func (s *Store) Task(ctx context.Context, parseID, taskID string) (task Task, ok bool, err error) {
	err = s.decode(ctx, &task, taskSQL, "", parseID, taskID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Task{}, false, nil
	case err != nil:
		return Task{}, false, fmt.Errorf("store: reading %s of %s: %w", taskID, parseID, err)
	}
	return task, true, nil
}

// Ping reports whether the database answers.
func (s *Store) Ping(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, pingSQL); err != nil {
		return fmt.Errorf("store: the database does not answer: %w", err)
	}
	return nil
}

// ParseOf returns an owner's parse. Another owner's parse is not found.
func (s *Store) ParseOf(ctx context.Context, owner, parseID string) (Parse, error) {
	var p Parse
	err := s.decode(ctx, &p, parseOfSQL, "", parseID, owner)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Parse{}, fault.New(fault.ParseNotFound, "no parse %s", parseID)
	case err != nil:
		return Parse{}, fmt.Errorf("store: reading %s: %w", parseID, err)
	}
	return p, nil
}

// Filter narrows a list of parses. A zero member matches everything.
type Filter struct {
	State      string
	File       string
	OriginPath string
	// Labels must each be on a parse with the same value.
	Labels map[string]string
}

// Parses returns the parses of the owners that match, newest first: at most
// limit of them, starting after the parse whose id is after. Nil owners is
// every owner's, and an empty list is nobody's. more reports whether others
// follow.
func (s *Store) Parses(ctx context.Context, owners []string, f Filter, after string, limit int) (out []Parse, more bool, err error) {
	labels, err := json.Marshal(f.Labels)
	if err != nil || f.Labels == nil {
		labels = []byte("{}")
	}
	// A nil list encodes as the JSON null, which the statement reads as
	// every owner's.
	scope, err := json.Marshal(owners)
	if err != nil {
		return nil, false, fmt.Errorf("store: encoding the owners of a list: %w", err)
	}
	// One row past the limit says whether a next page exists.
	if err := s.decode(ctx, &out, parsesSQL, "", string(scope), f.State, f.File, f.OriginPath, string(labels), after, limit+1); err != nil {
		return nil, false, fmt.Errorf("store: listing parses: %w", err)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// DeleteParse removes an owner's parse that has ended, with its task rows
// and the reads kept from it. The caller removes the parse's objects first:
// a delete that stops between the two leaves a row to delete again, and
// never an object nothing names.
func (s *Store) DeleteParse(ctx context.Context, owner, parseID string) error {
	answer, err := s.text(ctx, parseDeleteSQL, "", owner, parseID)
	switch {
	case err != nil:
		return fmt.Errorf("store: deleting %s: %w", parseID, err)
	case answer == "missing":
		return fault.New(fault.ParseNotFound, "no parse %s", parseID)
	case answer == "not_terminal":
		return fault.New(fault.NotTerminal, "parse %s has not ended", parseID)
	}
	return nil
}

// File is the row of a source snapshot. The bytes are in the object store
// under Key.
type File struct {
	ID        string    `json:"file_id"`
	Owner     string    `json:"owner"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	MediaType string    `json:"media_type"`
	Key       string    `json:"object_key"`
	CreatedAt time.Time `json:"created_at"`

	// Retention is how long the file is kept from an upload of it, and
	// past the end of the last parse that read it. It is what InsertFile
	// and KeepFile are told; zero keeps the file. ExpiresAt is when the
	// file may be removed as its row and its parses stand, and nil for a
	// file that is kept.
	Retention time.Duration `json:"-"`
	ExpiresAt *time.Time    `json:"expires_at"`
}

// File returns a file whoever owns it. The API reads it before it asks
// whether its caller may act on it: the file's owner is part of the
// question.
func (s *Store) File(ctx context.Context, fileID string) (File, error) {
	var f File
	err := s.decode(ctx, &f, fileSQL, "", fileID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return File{}, fault.New(fault.FileNotFound, "no file %s", fileID)
	case err != nil:
		return File{}, fmt.Errorf("store: reading %s: %w", fileID, err)
	}
	return f, nil
}

// FileByContent returns the file an owner has for the bytes of a digest. ok
// is false when the owner has none.
func (s *Store) FileByContent(ctx context.Context, owner, sha256 string) (f File, ok bool, err error) {
	err = s.decode(ctx, &f, fileByContentSQL, "", owner, sha256)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return File{}, false, nil
	case err != nil:
		return File{}, false, fmt.Errorf("store: reading a file by its content: %w", err)
	}
	return f, true, nil
}

// InsertFile writes a file's row. The same bytes are one file per owner:
// when the owner already has a file with the digest, that file is returned
// and created is false, and the caller removes the object it wrote for f.
// Either way the file is kept for f.Retention from now.
func (s *Store) InsertFile(ctx context.Context, f File) (stored File, created bool, err error) {
	var answer struct {
		Created bool `json:"created"`
		File    File `json:"file"`
	}
	err = s.decode(ctx, &answer, fileInsertSQL, "", f.ID, f.Owner, f.Name, f.Size, f.SHA256, f.MediaType, f.Key, f.Retention.Milliseconds(), s.at())
	if err != nil {
		return File{}, false, fmt.Errorf("store: writing %s: %w", f.ID, err)
	}
	return answer.File, answer.Created, nil
}

// KeepFile keeps a file for retention from now: what an upload of bytes
// the owner already has does to the file that holds them. Zero keeps the
// file for good.
func (s *Store) KeepFile(ctx context.Context, fileID string, retention time.Duration) error {
	if _, err := s.pool.Exec(ctx, fileKeepSQL, fileID, retention.Milliseconds(), s.at()); err != nil {
		return fmt.Errorf("store: keeping %s: %w", fileID, err)
	}
	return nil
}

// DeleteFile begins the delete of an owner's file and returns the key of
// its object: the file is gone for every caller from here on. The caller
// removes the object and then calls ForgetFile. A file that a parse which
// has not ended reads is refused with not_terminal.
func (s *Store) DeleteFile(ctx context.Context, owner, fileID string) (key string, err error) {
	var answer struct {
		Result string `json:"result"`
		Key    string `json:"key"`
	}
	if err := s.decode(ctx, &answer, fileDeleteSQL, fileDeleteAtSQL, owner, fileID); err != nil {
		return "", fmt.Errorf("store: deleting %s: %w", fileID, err)
	}
	switch answer.Result {
	case "missing":
		return "", fault.New(fault.FileNotFound, "no file %s", fileID)
	case "not_terminal":
		return "", fault.New(fault.NotTerminal, "a parse that has not ended reads file %s", fileID)
	}
	return answer.Key, nil
}

// ForgetFile removes the row of a file whose delete began, once its object
// is gone.
func (s *Store) ForgetFile(ctx context.Context, fileID string) error {
	if _, err := s.pool.Exec(ctx, fileForgetSQL, fileID); err != nil {
		return fmt.Errorf("store: removing the row of %s: %w", fileID, err)
	}
	return nil
}

// ExpiredFile is a file whose delete the retention sweep began: its row is
// marked, and its object is under Key.
type ExpiredFile struct {
	ID  string `json:"file"`
	Key string `json:"key"`
}

// Expired is what one run of the retention sweep is to remove.
type Expired struct {
	// Due is false when another process ran the sweep within the sweep
	// interval. Nothing is listed then.
	Due bool `json:"due"`
	// Parses are the parses whose retention has ended. The caller removes
	// the objects under each one's prefix and then calls ExpireParse.
	Parses []string `json:"parses"`
	// Files are the files whose delete has begun. The caller removes each
	// one's object and then calls ForgetFile.
	Files []ExpiredFile `json:"files"`
}

// Expired claims the retention sweep when it is due and returns what it is
// to remove, at most limit parses and limit files. A file is marked here,
// in the statement that lists it, so it is gone for every caller before its
// object is removed; a parse is removed by ExpireParse, after its objects.
// A file whose delete began earlier and did not finish is listed again.
func (s *Store) Expired(ctx context.Context, limit int) (Expired, error) {
	var out Expired
	if err := s.decode(ctx, &out, expiredSQL, expiredAtSQL, limit); err != nil {
		return Expired{}, fmt.Errorf("store: the retention sweep: %w", err)
	}
	return out, nil
}

// ExpireParse removes the rows of a parse whose retention has ended, once
// its objects are gone. It reports false for a parse that is not there or
// has not expired, which is left alone.
func (s *Store) ExpireParse(ctx context.Context, parseID string) (removed bool, err error) {
	sql, args := s.stamp(parseExpireSQL, parseExpireAtSQL, []any{parseID})
	if err := s.pool.QueryRow(ctx, sql, args...).Scan(&removed); err != nil {
		return false, fmt.Errorf("store: expiring %s: %w", parseID, err)
	}
	return removed, nil
}

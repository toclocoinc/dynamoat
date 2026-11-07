// Package postgres is the Postgres implementation of storage.Store. One
// server process sits in front of one Postgres database; every DynamoDB table
// is a row in ddb_tables and every item of every table lives in items.
package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/metrics"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationLockKey is the advisory lock held while a migration file is applied,
// so two servers starting at once each apply the file exactly once.
const migrationLockKey = 7343

// Options configures OpenWithOptions. The zero value is valid.
type Options struct {
	// LockTimeout is how long a write waits for an item lock before it is
	// reported as storage.ErrThrottled. Zero means 5s.
	LockTimeout time.Duration
	// StatementTimeout is the statement_timeout applied to every new
	// connection as a runtime parameter. Zero means 30s.
	StatementTimeout time.Duration
	// IdleTxTimeout is the idle_in_transaction_session_timeout applied to
	// every new connection as a runtime parameter. Zero means 10s.
	IdleTxTimeout time.Duration
	// MaxConns is pgxpool's MaxConns. Zero means 10.
	MaxConns int32
	// MinConns is pgxpool's MinConns. Zero means 0 (no minimum).
	MinConns int32
	// TokenSource, when non-nil, supplies the password for every new
	// connection (Microsoft Entra Workload Identity). nil means use the
	// connection string's password as given.
	TokenSource TokenSource
	// Log receives errors from the background delete and backfill workers.
	// nil means slog.Default().
	Log *slog.Logger
	// Metrics, when set, records runs and items for the backfill and
	// table-delete workers.
	Metrics *metrics.Registry
}

// Store is a storage.Store backed by Postgres.
type Store struct {
	pool        *pgxpool.Pool
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup // background workers (deletes, backfills)
	lockTimeout time.Duration
	log         *slog.Logger
	metrics     *metrics.Registry
}

// lockTimeoutMs is lock_timeout in milliseconds, at least 1ms.
func (s *Store) lockTimeoutMs() int64 {
	ms := s.lockTimeout.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return ms
}

// setLockTimeoutSQL is the statement that bounds a write transaction's lock
// wait. lock_timeout takes a literal rather than a bind parameter, so the
// numeric millisecond value is inlined.
func (s *Store) setLockTimeoutSQL() string {
	return fmt.Sprintf(`SET LOCAL lock_timeout = %d`, s.lockTimeoutMs())
}

// Open connects to connString with a 5s lock timeout and slog.Default(), checks
// the Postgres version, applies pending migrations and resumes any table left
// DELETING and any GSI left CREATING by a previous run.
func Open(ctx context.Context, connString string) (*Store, error) {
	return OpenWithOptions(ctx, connString, Options{LockTimeout: 5 * time.Second, Log: slog.Default()})
}

// OpenWithOptions is Open with the given options.
func OpenWithOptions(ctx context.Context, connString string, opt Options) (s *Store, err error) {
	if opt.LockTimeout <= 0 {
		opt.LockTimeout = 5 * time.Second
	}
	if opt.StatementTimeout <= 0 {
		opt.StatementTimeout = 30 * time.Second
	}
	if opt.IdleTxTimeout <= 0 {
		opt.IdleTxTimeout = 10 * time.Second
	}
	if opt.MaxConns <= 0 {
		opt.MaxConns = 10
	}
	if opt.MinConns < 0 {
		opt.MinConns = 0
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}

	// The pool outlives the caller's ctx, so it is tied to its own context that
	// Close cancels.
	rootCtx, cancel := context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = opt.MaxConns
	cfg.MinConns = opt.MinConns
	// statement_timeout and idle_in_transaction_session_timeout are set as
	// connection runtime parameters so they apply to every statement on every
	// new connection without a round trip per query. Postgres takes them in
	// milliseconds, so inlining the millisecond value avoids any ambiguity
	// over Go duration strings.
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = make(map[string]string)
	}
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(opt.StatementTimeout.Milliseconds(), 10)
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = strconv.FormatInt(opt.IdleTxTimeout.Milliseconds(), 10)
	if opt.TokenSource != nil {
		cfg.BeforeConnect = func(ctx context.Context, cc *pgx.ConnConfig) error {
			token, err := opt.TokenSource.Token(ctx)
			if err != nil {
				return err
			}
			cc.Config.Password = token
			return nil
		}
	}
	pool, err := pgxpool.NewWithConfig(rootCtx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			pool.Close()
		}
	}()

	if err := pool.Ping(ctx); err != nil {
		return nil, err
	}

	var versionNum int
	var serverVersion string
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int, current_setting('server_version')`).Scan(&versionNum, &serverVersion); err != nil {
		return nil, err
	}
	if err := serverVersionTooOld(versionNum, serverVersion); err != nil {
		return nil, err
	}

	if err := migrate(ctx, pool); err != nil {
		return nil, err
	}

	s = &Store{pool: pool, ctx: rootCtx, cancel: cancel, lockTimeout: opt.LockTimeout, log: opt.Log, metrics: opt.Metrics}
	if err := s.backfillIndexPKHash(ctx); err != nil {
		return nil, err
	}
	opt.Log.Info("postgres pool", "maxConns", cfg.MaxConns, "minConns", cfg.MinConns)
	s.resumeDeletes()
	s.resumeBackfills()
	s.resumePITR()
	s.resumeBackups()
	return s, nil
}

// backfillIndexPKHash fills index_entries.pk_hash for rows written before the
// column existed (the part B1 schema). pk_hash is SHA-256's first four bytes
// over pk, which SQL cannot compute, so it is filled here once after migrate.
// The partial index on pk_hash IS NULL makes the probe cheap once filled.
func (s *Store) backfillIndexPKHash(ctx context.Context) error {
	for {
		rows, err := s.pool.Query(ctx, `SELECT table_id, index_id, ipk_hash, ipk, isk, pk, sk FROM index_entries WHERE pk_hash IS NULL LIMIT 1000`)
		if err != nil {
			return err
		}
		b := &pgx.Batch{}
		n := 0
		for rows.Next() {
			var tableID int32
			var indexID int16
			var ipkHash int32
			var ipk, isk, pk, sk []byte
			if err := rows.Scan(&tableID, &indexID, &ipkHash, &ipk, &isk, &pk, &sk); err != nil {
				rows.Close()
				return err
			}
			n++
			b.Queue(`UPDATE index_entries SET pk_hash = $1 WHERE table_id = $2 AND index_id = $3 AND ipk_hash = $4 AND ipk = $5 AND isk = $6 AND pk = $7 AND sk = $8`,
				attr.KeyHash(pk), tableID, indexID, ipkHash, ipk, isk, pk, sk)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if b.Len() > 0 {
			br := s.pool.SendBatch(ctx, b)
			for i := 0; i < b.Len(); i++ {
				if _, err := br.Exec(); err != nil {
					br.Close()
					return err
				}
			}
			br.Close()
		}
		if n < 1000 {
			return nil
		}
	}
}

// Close stops the background workers and closes the connection pool.
func (s *Store) Close() {
	s.cancel()
	s.wg.Wait()
	s.pool.Close()
}

// Ready reports whether the backend can serve requests.
func (s *Store) Ready(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// PoolStats reports the connection pool's state for the metrics registry,
// read from pgxpool at scrape time.
func (s *Store) PoolStats() metrics.PoolStats {
	st := s.pool.Stat()
	return metrics.PoolStats{
		Idle:  st.IdleConns(),
		InUse: st.AcquiredConns(),
		Total: st.TotalConns(),
		Max:   st.MaxConns(),
	}
}

// serverVersionTooOld reports an error when the Postgres major version (the
// numeric server_version_num, e.g. 140011) is older than 15.
func serverVersionTooOld(versionNum int, serverVersion string) error {
	if versionNum < 150000 {
		return fmt.Errorf("dynamoat needs PostgreSQL 15 or newer; this server is %s", serverVersion)
	}
	return nil
}

// migrate applies every migration file in name order. Each file runs inside a
// transaction holding pg_advisory_xact_lock(7343) and is recorded in
// schema_migrations, so two servers starting at once apply each file once.
func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if err := createSchemaMigrations(ctx, pool); err != nil {
		return err
	}
	names, err := migrationNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		if err := applyMigration(ctx, tx, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// createSchemaMigrations creates the migration bookkeeping table under the
// migration advisory lock; CREATE TABLE IF NOT EXISTS races on the type-name
// index when two servers create it at once.
func createSchemaMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, int64(migrationLockKey)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func applyMigration(ctx context.Context, tx pgx.Tx, name string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, int64(migrationLockKey)); err != nil {
		return err
	}
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&applied); err != nil {
		return err
	}
	if applied {
		return nil
	}
	sql, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
		return err
	}
	return nil
}

func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

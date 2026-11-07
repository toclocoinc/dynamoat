package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// Backup statuses, as Backup.Status documents them.
const (
	statusBackupAvailable = "AVAILABLE"
	statusBackupDeleted   = "DELETED"
)

// newBackupID builds an ID from the current millisecond timestamp (0-padded,
// so IDs sort by creation time) plus 8 random hex bytes, and the CreatedAt the
// ID encodes.
func newBackupID() (string, time.Time) {
	ms := time.Now().UnixMilli()
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%013d-%s", ms, hex.EncodeToString(b[:])), time.UnixMilli(ms)
}

// CreateBackup records a CREATING backup of t, fixes a REPEATABLE READ snapshot
// before returning, and copies the items into backup_items in the background.
func (s *Store) CreateBackup(ctx context.Context, t *storage.Table, name string) (*storage.Backup, error) {
	id, createdAt := newBackupID()

	// Begin the copy transaction and take its snapshot now, so every write that
	// commits after CreateBackup returns is outside the snapshot.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}

	defJSON, err := json.Marshal(t)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	// The CREATING row is committed separately so GetBackup sees it during the
	// copy, which runs inside tx.
	if _, err := s.pool.Exec(ctx, `INSERT INTO backups (backup_id, name, table_name, def, status, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, name, t.Name, defJSON, storage.StatusCreating, createdAt); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}

	s.wg.Add(1)
	go s.copyBackup(s.ctx, id, t.ID, tx)

	return &storage.Backup{ID: id, Name: name, Table: *cloneTable(t), Status: storage.StatusCreating, CreatedAt: createdAt}, nil
}

// copyBackup copies a table's items into backup_items and marks the backup
// AVAILABLE, retrying on error until the store is closed. first, when non-nil,
// is the snapshot-fixed transaction CreateBackup handed over; a retry begins a
// fresh transaction (and so a fresh snapshot).
func (s *Store) copyBackup(ctx context.Context, id string, tableID int32, first pgx.Tx) {
	defer s.wg.Done()
	tx := first
	delay := time.Second
	for {
		if ctx.Err() != nil {
			if tx != nil {
				_ = tx.Rollback(context.Background())
			}
			return
		}
		if tx == nil {
			var err error
			tx, err = s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
			if err != nil {
				s.log.Error("backup: begin copy", "backup_id", id, "err", err)
				delay = backoff(ctx, delay)
				continue
			}
			if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
				_ = tx.Rollback(ctx)
				tx = nil
				s.log.Error("backup: take snapshot", "backup_id", id, "err", err)
				delay = backoff(ctx, delay)
				continue
			}
		}
		if err := s.finishBackupCopy(ctx, tx, id, tableID); err != nil {
			s.log.Error("backup: copying items", "backup_id", id, "err", err)
			_ = tx.Rollback(ctx)
			tx = nil
			delay = backoff(ctx, delay)
			continue
		}
		return
	}
}

// finishBackupCopy copies every item of tableID into backup_items in one
// statement, computes the counts and marks the backup AVAILABLE. The metadata
// update runs on a separate connection after tx commits: tx's snapshot was
// taken before the CREATING row was inserted, so tx cannot see or update that
// row.
func (s *Store) finishBackupCopy(ctx context.Context, tx pgx.Tx, id string, tableID int32) error {
	// The copy is one statement over the whole table, so it must not be cut
	// off by the connection's statement_timeout (DYNAMOAT_PG_STATEMENT_TIMEOUT).
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = 0`); err != nil {
		return err
	}
	// The delete makes a retry after an ambiguous commit idempotent.
	if _, err := tx.Exec(ctx, `DELETE FROM backup_items WHERE backup_id = $1`, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO backup_items (backup_id, pk_hash, pk, sk, item)
		 SELECT $1, pk_hash, pk, sk, item FROM items WHERE table_id = $2`, id, tableID)
	if err != nil {
		return err
	}
	items := tag.RowsAffected()

	var bytes int64
	rows, err := tx.Query(ctx, `SELECT item FROM backup_items WHERE backup_id = $1`, id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			rows.Close()
			return err
		}
		var it attr.Item
		if err := json.Unmarshal(b, &it); err != nil {
			rows.Close()
			return err
		}
		bytes += int64(attr.ItemSize(it))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE backups SET status = $1, items = $2, bytes = $3 WHERE backup_id = $4`,
		statusBackupAvailable, items, bytes, id)
	return err
}

// GetBackup returns the backup metadata, or ErrBackupNotFound for an unknown or
// deleted ID.
func (s *Store) GetBackup(ctx context.Context, id string) (*storage.Backup, error) {
	var name string
	var def []byte
	var status string
	var createdAt time.Time
	var items, bytes int64
	err := s.pool.QueryRow(ctx,
		`SELECT name, def, status, created_at, COALESCE(items, 0), COALESCE(bytes, 0)
		 FROM backups WHERE backup_id = $1 AND status <> $2`, id, statusBackupDeleted).
		Scan(&name, &def, &status, &createdAt, &items, &bytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, storage.ErrBackupNotFound
	}
	if err != nil {
		return nil, err
	}
	t, err := unmarshalTable(def)
	if err != nil {
		return nil, err
	}
	return &storage.Backup{ID: id, Name: name, Table: *t, Status: status, CreatedAt: createdAt, Items: items, Bytes: bytes}, nil
}

// ListBackups returns backups of table ("" for all) created in [from, to)
// (zero times are open bounds), oldest first, after startAfter, at most limit
// (0 for no limit). IDs encode creation time, so oldest first is backup_id
// order.
func (s *Store) ListBackups(ctx context.Context, table string, from, to time.Time, startAfter string, limit int) ([]storage.Backup, error) {
	sql := `SELECT backup_id, name, table_name, def, status, created_at, COALESCE(items, 0), COALESCE(bytes, 0)
	        FROM backups WHERE status <> $1`
	args := []any{statusBackupDeleted}
	if table != "" {
		args = append(args, table)
		sql += fmt.Sprintf(" AND table_name = $%d", len(args))
	}
	if !from.IsZero() {
		args = append(args, from)
		sql += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to)
		sql += fmt.Sprintf(" AND created_at < $%d", len(args))
	}
	args = append(args, startAfter)
	sql += fmt.Sprintf(" AND backup_id > $%d", len(args))
	sql += " ORDER BY backup_id"
	if limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.Backup
	for rows.Next() {
		var b storage.Backup
		var def []byte
		var tableName string
		if err := rows.Scan(&b.ID, &b.Name, &tableName, &def, &b.Status, &b.CreatedAt, &b.Items, &b.Bytes); err != nil {
			return nil, err
		}
		t, err := unmarshalTable(def)
		if err != nil {
			return nil, err
		}
		b.Table = *t
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteBackup marks a backup DELETED and removes its rows in the background,
// returning the backup with Status DELETED.
func (s *Store) DeleteBackup(ctx context.Context, id string) (*storage.Backup, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var name string
	var def []byte
	var createdAt time.Time
	var items, bytes int64
	err = tx.QueryRow(ctx,
		`UPDATE backups SET status = $2 WHERE backup_id = $1 AND status <> $2
		 RETURNING name, def, created_at, COALESCE(items, 0), COALESCE(bytes, 0)`,
		id, statusBackupDeleted).Scan(&name, &def, &createdAt, &items, &bytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, storage.ErrBackupNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	t, err := unmarshalTable(def)
	if err != nil {
		return nil, err
	}

	s.wg.Add(1)
	go s.deleteBackup(s.ctx, id)
	return &storage.Backup{ID: id, Name: name, Table: *t, Status: statusBackupDeleted, CreatedAt: createdAt, Items: items, Bytes: bytes}, nil
}

// deleteBackup removes a DELETED backup's rows in batches, then the catalog
// row, retrying on error until the store is closed.
func (s *Store) deleteBackup(ctx context.Context, id string) {
	defer s.wg.Done()
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		done, err := s.deleteBackupStep(ctx, id)
		if err != nil {
			s.log.Error("delete backup: removing rows", "backup_id", id, "err", err)
			delay = backoff(ctx, delay)
			continue
		}
		if done {
			return
		}
		delay = time.Second
	}
}

func (s *Store) deleteBackupStep(ctx context.Context, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM backup_items WHERE ctid IN (SELECT ctid FROM backup_items WHERE backup_id = $1 LIMIT 1000)`, id)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.pool.Exec(ctx, `DELETE FROM backups WHERE backup_id = $1`, id); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// RestoreBackup creates the target table (CREATING) and fills it from the
// backup in the background. Only an AVAILABLE backup can be restored.
func (s *Store) RestoreBackup(ctx context.Context, id string, target *storage.Table) (*storage.Table, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM backups WHERE backup_id = $1 AND status = $2)`,
		id, statusBackupAvailable).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, storage.ErrBackupNotFound
	}
	t, err := s.createTable(ctx, target, storage.StatusCreating, storage.StatusCreating, id)
	if err != nil {
		return nil, err
	}
	s.wg.Add(1)
	go s.restoreBackup(s.ctx, id, t.ID)
	return t, nil
}

// restoreBackup copies backup_items into a restored table's items and index
// entries in batches, then marks the table and its GSIs ACTIVE, retrying on
// error until the store is closed.
func (s *Store) restoreBackup(ctx context.Context, backupID string, tableID int32) {
	defer s.wg.Done()
	t, err := s.tableByID(ctx, tableID)
	if err != nil {
		s.log.Error("restore: loading table", "table_id", tableID, "err", err)
		return
	}
	cur := backfillCursor{hash: math.MinInt32, pk: []byte{}, sk: []byte{}}
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		n, last, err := s.restoreBackupBatch(ctx, backupID, tableID, t, cur)
		if err != nil {
			s.log.Error("restore: copying items", "table_id", tableID, "backup_id", backupID, "err", err)
			delay = backoff(ctx, delay)
			continue
		}
		cur = last
		if n < deleteBatchSize {
			break
		}
		delay = time.Second
	}

	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.markTableActive(ctx, tableID); err != nil {
			s.log.Error("restore: marking table active", "table_id", tableID, "err", err)
			delay = backoff(ctx, delay)
			continue
		}
		return
	}
}

// restoreBackupBatch copies one batch of backup_items into items and builds the
// index entries for every GSI in the same pass. The upserts make a resume after
// a partial copy safe.
func (s *Store) restoreBackupBatch(ctx context.Context, backupID string, tableID int32, t *storage.Table, cur backfillCursor) (int, backfillCursor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pk_hash, pk, sk, item
		FROM backup_items
		WHERE backup_id = $1 AND (pk_hash, pk, sk) > ($2::int, $3::bytea, $4::bytea)
		ORDER BY pk_hash, pk, sk
		LIMIT 1000`, backupID, cur.hash, cur.pk, cur.sk)
	if err != nil {
		return 0, cur, err
	}
	defer rows.Close()

	type row struct {
		h        int32
		pk, sk   []byte
		itemJSON []byte
	}
	var batch []row
	var last backfillCursor
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.h, &r.pk, &r.sk, &r.itemJSON); err != nil {
			return 0, cur, err
		}
		batch = append(batch, r)
		last = backfillCursor{hash: r.h, pk: r.pk, sk: r.sk}
	}
	if err := rows.Err(); err != nil {
		return 0, cur, err
	}
	if len(batch) == 0 {
		return 0, cur, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, cur, err
	}
	defer tx.Rollback(ctx)

	b := &pgx.Batch{}
	for _, r := range batch {
		var it attr.Item
		if err := json.Unmarshal(r.itemJSON, &it); err != nil {
			return 0, cur, err
		}
		lk := lockKey{tableID: tableID, pkHash: r.h, pk: r.pk, sk: r.sk}
		b.Queue(`INSERT INTO items (table_id, pk_hash, pk, sk, item, ttl_at)
		         VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
			tableID, r.h, r.pk, r.sk, r.itemJSON, ttlAt(t, it))
		buildGSI(b, t, lk, nil, it)
	}
	if err := execBatch(ctx, tx, b); err != nil {
		return 0, cur, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, cur, err
	}
	return len(batch), last, nil
}

// markTableActive sets a restored table and all its GSIs ACTIVE once its items
// are copied. It reports nil when the table is gone, so a restore racing a
// DeleteTable finishes cleanly.
func (s *Store) markTableActive(ctx context.Context, tableID int32) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var def []byte
	err = tx.QueryRow(ctx, `SELECT def FROM ddb_tables WHERE table_id = $1 FOR UPDATE`, tableID).Scan(&def)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	t, err := unmarshalTable(def)
	if err != nil {
		return err
	}
	t.ID = tableID
	t.Status = storage.StatusActive
	for i := range t.GSIs {
		t.GSIs[i].Status = storage.StatusActive
	}
	defJSON, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE ddb_tables SET def = $1::jsonb, status = $2 WHERE table_id = $3`,
		string(defJSON), storage.StatusActive, tableID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// resumeBackups restarts the background jobs a previous run left unfinished:
// backups still CREATING, restores still CREATING and backups left DELETED.
func (s *Store) resumeBackups() {
	rows, err := s.pool.Query(s.ctx, `SELECT backup_id, table_name FROM backups WHERE status = $1`, storage.StatusCreating)
	if err != nil {
		s.log.Error("resume backups: listing backups", "err", err)
		return
	}
	type pendingBackup struct {
		id        string
		tableName string
	}
	var pbs []pendingBackup
	for rows.Next() {
		var p pendingBackup
		if err := rows.Scan(&p.id, &p.tableName); err != nil {
			s.log.Error("resume backups: scanning backup", "err", err)
			rows.Close()
			return
		}
		pbs = append(pbs, p)
	}
	rows.Close()
	for _, p := range pbs {
		var tableID int32
		err := s.pool.QueryRow(s.ctx, `SELECT table_id FROM ddb_tables WHERE name = $1`, p.tableName).Scan(&tableID)
		if err != nil {
			s.log.Error("resume backups: source table gone", "backup_id", p.id, "table", p.tableName, "err", err)
			continue
		}
		s.wg.Add(1)
		go s.copyBackup(s.ctx, p.id, tableID, nil)
	}

	rows, err = s.pool.Query(s.ctx, `SELECT table_id, restore_backup_id FROM ddb_tables WHERE status = $1 AND restore_backup_id IS NOT NULL`, storage.StatusCreating)
	if err != nil {
		s.log.Error("resume restores: listing tables", "err", err)
		return
	}
	type pendingRestore struct {
		tableID  int32
		backupID string
	}
	var prs []pendingRestore
	for rows.Next() {
		var p pendingRestore
		if err := rows.Scan(&p.tableID, &p.backupID); err != nil {
			s.log.Error("resume restores: scanning table", "err", err)
			rows.Close()
			return
		}
		prs = append(prs, p)
	}
	rows.Close()
	for _, p := range prs {
		s.wg.Add(1)
		go s.restoreBackup(s.ctx, p.backupID, p.tableID)
	}

	rows, err = s.pool.Query(s.ctx, `SELECT backup_id FROM backups WHERE status = $1`, statusBackupDeleted)
	if err != nil {
		s.log.Error("resume delete backups: listing backups", "err", err)
		return
	}
	var dels []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			s.log.Error("resume delete backups: scanning backup", "err", err)
			rows.Close()
			return
		}
		dels = append(dels, id)
	}
	rows.Close()
	for _, id := range dels {
		s.wg.Add(1)
		go s.deleteBackup(s.ctx, id)
	}
}

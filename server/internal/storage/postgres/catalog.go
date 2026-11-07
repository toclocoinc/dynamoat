package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// CreateTable stores a new table, assigning its ID and the GSI IDs.
func (s *Store) CreateTable(ctx context.Context, t *storage.Table) (*storage.Table, error) {
	return s.createTable(ctx, t, storage.StatusActive, storage.StatusActive, "")
}

// createTable stores a new table with the given statuses. restoreBackupID, when
// non-empty, records the backup the table is being restored from so a restore
// left CREATING by a previous run can be resumed.
func (s *Store) createTable(ctx context.Context, t *storage.Table, tableStatus, gsiStatus, restoreBackupID string) (*storage.Table, error) {
	c := cloneTable(t)
	assignGSIIDs(c)
	c.Status = tableStatus
	for i := range c.GSIs {
		c.GSIs[i].Status = gsiStatus
	}

	keyJSON, err := json.Marshal(c.Key)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var restoreBackup any
	if restoreBackupID != "" {
		restoreBackup = restoreBackupID
	}
	var id int32
	err = tx.QueryRow(ctx,
		`INSERT INTO ddb_tables (name, key_schema, status, def, restore_backup_id, deletion_protection) VALUES ($1, $2::jsonb, $3, '{}'::jsonb, $4, $5) RETURNING table_id`,
		c.Name, string(keyJSON), c.Status, restoreBackup, c.DeletionProtection).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, storage.ErrTableExists
		}
		return nil, err
	}
	c.ID = id

	defJSON, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE ddb_tables SET def = $1::jsonb WHERE table_id = $2`, string(defJSON), id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// GetTable returns the table definition, or ErrTableNotFound.
func (s *Store) GetTable(ctx context.Context, name string) (*storage.Table, error) {
	var id int32
	var def []byte
	var deletionProtection bool
	err := s.pool.QueryRow(ctx, `SELECT table_id, def, deletion_protection FROM ddb_tables WHERE name = $1`, name).Scan(&id, &def, &deletionProtection)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, storage.ErrTableNotFound
	}
	if err != nil {
		return nil, err
	}
	t, err := unmarshalTable(def)
	if err != nil {
		return nil, err
	}
	t.ID = id
	t.DeletionProtection = deletionProtection
	return t, nil
}

// ListTables returns up to limit names in byte order, after startAfter.
// Names compare with the C collation: DynamoDB orders them by bytes, and the
// database default (often en_US.UTF-8) would not.
// Tables still DELETING are hidden, as DynamoDB does.
func (s *Store) ListTables(ctx context.Context, startAfter string, limit int) ([]string, error) {
	var rows pgx.Rows
	var err error
	if limit > 0 {
		rows, err = s.pool.Query(ctx, `SELECT name FROM ddb_tables WHERE name COLLATE "C" > $1 AND status <> $2 ORDER BY name COLLATE "C" LIMIT $3`, startAfter, storage.StatusDeleting, limit)
	} else {
		rows, err = s.pool.Query(ctx, `SELECT name FROM ddb_tables WHERE name COLLATE "C" > $1 AND status <> $2 ORDER BY name COLLATE "C"`, startAfter, storage.StatusDeleting)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UpdateTable applies fn to the current definition under a row lock and stores
// the result. A new GSI (ID 0) gets the next ID and is backfilled in the
// background, staying CREATING until that finishes.
func (s *Store) UpdateTable(ctx context.Context, name string, fn func(*storage.Table) error) (*storage.Table, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var id int32
	var def []byte
	err = tx.QueryRow(ctx, `SELECT table_id, def FROM ddb_tables WHERE name = $1 FOR UPDATE`, name).Scan(&id, &def)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, storage.ErrTableNotFound
	}
	if err != nil {
		return nil, err
	}
	draft, err := unmarshalTable(def)
	if err != nil {
		return nil, err
	}
	draft.ID = id
	prevTTLAttr, prevTTLEnabled := draft.TTLAttr, draft.TTLEnabled
	prevPITR := draft.PITREnabled

	if err := fn(draft); err != nil {
		return nil, err
	}

	newIdx := assignGSIIDs(draft)
	for _, i := range newIdx {
		draft.GSIs[i].Status = storage.StatusCreating
	}

	defJSON, err := json.Marshal(draft)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE ddb_tables SET def = $1::jsonb, status = $2, deletion_protection = $3 WHERE table_id = $4`, string(defJSON), draft.Status, draft.DeletionProtection, id); err != nil {
		return nil, err
	}
	if !prevPITR && draft.PITREnabled {
		if err := queuePITREnable(ctx, tx, id, draft.PITRSince); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	for _, i := range newIdx {
		s.wg.Add(1)
		go s.backfill(s.ctx, id, draft.GSIs[i])
	}
	if draft.TTLAttr != prevTTLAttr || draft.TTLEnabled != prevTTLEnabled {
		s.wg.Add(1)
		go s.recomputeTTL(s.ctx, id)
	}
	if !prevPITR && draft.PITREnabled {
		s.wg.Add(1)
		go s.pitrCopy(s.ctx, id, draft.PITRSince)
	}
	return draft, nil
}

// DeleteTable marks the table DELETING and removes its rows in the background.
func (s *Store) DeleteTable(ctx context.Context, name string) (*storage.Table, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var id int32
	var def []byte
	err = tx.QueryRow(ctx, `SELECT table_id, def FROM ddb_tables WHERE name = $1 FOR UPDATE`, name).Scan(&id, &def)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, storage.ErrTableNotFound
	}
	if err != nil {
		return nil, err
	}
	draft, err := unmarshalTable(def)
	if err != nil {
		return nil, err
	}
	draft.ID = id
	draft.Status = storage.StatusDeleting

	defJSON, err := json.Marshal(draft)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE ddb_tables SET def = $1::jsonb, status = $2 WHERE table_id = $3`, string(defJSON), draft.Status, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	s.wg.Add(1)
	go s.deleteTable(s.ctx, id)
	return draft, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

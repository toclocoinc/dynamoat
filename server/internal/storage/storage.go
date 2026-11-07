// Package storage is the contract between the DynamoDB operations
// (internal/ops) and a storage backend (internal/storage/postgres today, an
// embedded engine later).
//
// The split of work: operations parse and validate requests, evaluate
// expressions, apply projections and filters, and shape responses. The store
// owns durability and atomicity: the table catalog, item reads and writes
// under a per-item lock, GSI entries, TTL timestamps and stream records, all
// written in the same transaction as the item.
package storage

import (
	"context"
	"errors"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
)

// Errors a store returns. Operations turn them into DynamoDB errors.
var (
	ErrTableNotFound = errors.New("storage: table not found")
	ErrTableExists   = errors.New("storage: table already exists")
	ErrTableNotReady = errors.New("storage: table is not ACTIVE")
	// ErrThrottled means the store gave up waiting for an item lock. Operations
	// report it as ProvisionedThroughputExceededException, so SDK clients back
	// off and retry.
	ErrThrottled = errors.New("storage: timed out waiting for an item lock")
	// ErrBackupNotFound is returned by BackupStore for an unknown backup ID.
	ErrBackupNotFound = errors.New("storage: backup not found")
	// ErrOutsideRecoveryWindow is returned by PITRStore.RestoreToTime for a
	// time before the earliest or after the latest restorable time.
	ErrOutsideRecoveryWindow = errors.New("storage: time is outside the recovery window")
)

// KeySchema is the key of a table or a GSI. Range is "" for a hash-only key.
type KeySchema struct {
	Hash      string
	HashType  attr.Type // S, N or B
	Range     string
	RangeType attr.Type
}

// Projection types of a GSI.
const (
	ProjectAll      = "ALL"
	ProjectKeysOnly = "KEYS_ONLY"
	ProjectInclude  = "INCLUDE"
)

// Table and index statuses, as DescribeTable reports them.
const (
	StatusCreating = "CREATING"
	StatusActive   = "ACTIVE"
	StatusUpdating = "UPDATING"
	StatusDeleting = "DELETING"
)

// Stream view types; "" means streams are off.
const (
	StreamKeysOnly  = "KEYS_ONLY"
	StreamNewImage  = "NEW_IMAGE"
	StreamOldImage  = "OLD_IMAGE"
	StreamNewAndOld = "NEW_AND_OLD_IMAGES"
)

// Index is a global secondary index. ID is assigned by the store and is
// unique within the table; it never changes and is never reused.
type Index struct {
	Name        string
	ID          int16
	Key         KeySchema
	Projection  string   // ProjectAll, ProjectKeysOnly or ProjectInclude
	NonKeyAttrs []string // for ProjectInclude
	Status      string   // CREATING while backfilling, then ACTIVE
	// Local marks a local secondary index: its Key.Hash is the table's hash
	// key, it is created with the table and never added or removed later, and
	// it supports consistent reads. Stores keep its entries exactly as a GSI's.
	// Table.GSIs holds both kinds; operations tell them apart by Local.
	Local bool
}

// Table is a catalog entry. The store persists every field; operations
// render DescribeTable from it.
type Table struct {
	ID        int32 // assigned by the store
	Name      string
	Key       KeySchema
	AttrDefs  map[string]attr.Type // AttributeDefinitions
	GSIs      []Index
	Billing   string // PAY_PER_REQUEST or PROVISIONED
	ReadCU    int64  // provisioned throughput as declared (not enforced)
	WriteCU   int64
	Status    string
	CreatedAt time.Time

	// DeletionProtection is DynamoDB's DeletionProtectionEnabled: while true,
	// DeleteTable rejects the table until protection is disabled.
	DeletionProtection bool

	StreamView  string // "" when streams are off
	StreamLabel string // set when a stream is enabled (DynamoDB's timestamp label)

	TTLAttr    string // "" when TTL is off
	TTLEnabled bool

	Tags map[string]string

	// Point-in-time recovery. While PITREnabled the store keeps every version
	// of every item (see PITRStore) for PITRDays days.
	PITREnabled bool
	PITRDays    int       // RecoveryPeriodInDays, 1-35 (default 35)
	PITRSince   time.Time // when it was last enabled

	// Replicas are the regions a global table is served in. dynamoat serves
	// a table from one database in every region a request is signed for, so
	// replicas are bookkeeping that make DescribeTable and the global tables
	// API answer as DynamoDB does; there is no copying between sites.
	Replicas []Replica

	// Restore is set on a table created by RestoreBackup or RestoreToTime
	// (DescribeTable's RestoreSummary; it is in progress while Status is
	// CREATING).
	Restore *RestoreSummary
}

// RestoreSummary records where a restored table came from.
type RestoreSummary struct {
	SourceBackupID string    // "" for a point-in-time restore
	SourceTable    string    // the source table's name
	At             time.Time // the restore's point in time (the backup's time for a backup)
}

// Replica is one region of a global table.
type Replica struct {
	Region string
	Status string // CREATING, ACTIVE, DELETING
}

// GSI returns the index with the given name.
func (t *Table) GSI(name string) (*Index, bool) {
	for i := range t.GSIs {
		if t.GSIs[i].Name == name {
			return &t.GSIs[i], true
		}
	}
	return nil, false
}

// Key is a primary key: the hash value and, for a table with a range key,
// the range value (Range.Type is 0 when absent).
type Key struct {
	Hash  attr.Value
	Range attr.Value
}

// WriteOp says what a write does with the item.
type WriteOp int

const (
	Keep   WriteOp = iota // leave the item as it is (a condition check, or a no-op)
	Put                   // store Result.Item (insert or replace)
	Delete                // remove the item
)

// WriteResult is what a WriteFunc decides.
type WriteResult struct {
	Op   WriteOp
	Item attr.Item // the new item for Put; it must contain the key
}

// WriteFunc decides a write given the current item (nil when there is none).
// The store calls it while holding the item's lock, inside the transaction,
// so a condition evaluated here is atomic with the write. Returning an error
// aborts the write (and the whole transaction for Transact); the store
// returns that same error to the caller.
type WriteFunc func(old attr.Item) (WriteResult, error)

// Write is one item write, used by WriteItem and Transact.
type Write struct {
	Table *Table
	Key   Key
	Fn    WriteFunc
}

// Page limits for Query and Scan, which DynamoDB applies before any filter.
type PageLimit struct {
	Items int // the request's Limit; 0 means no limit
	Bytes int // stop after this many bytes of items (attr.ItemSize); DynamoDB uses 1 MB
}

// Page is a run of items in key order.
type Page struct {
	Items []attr.Item
	// LastKey is the key attributes of the last item read (the table key, plus
	// the index key for a GSI) when more items may follow; nil when the range
	// is exhausted. It is DynamoDB's LastEvaluatedKey.
	LastKey attr.Item
}

// RangeCond is a sort key condition: Op is "=", "<", "<=", ">", ">=",
// "BETWEEN" (two values) or "begins_with" (S or B only).
type RangeCond struct {
	Op     string
	Values []attr.Value
}

// QueryInput reads one partition of a table or GSI in sort key order.
type QueryInput struct {
	Table      *Table
	Index      string // "" for the base table
	Hash       attr.Value
	Range      *RangeCond // nil for the whole partition
	Forward    bool       // ScanIndexForward
	StartAfter attr.Item  // ExclusiveStartKey, or nil
	Limit      PageLimit
}

// ScanInput reads a table or GSI in storage order. Segment and Segments
// split the pk_hash space into equal ranges (Segments 0 means 1).
type ScanInput struct {
	Table      *Table
	Index      string
	Segment    int
	Segments   int
	StartAfter attr.Item
	Limit      PageLimit
}

// Store is a storage backend. Every method is safe for concurrent use.
//
// Items returned for a GSI are the full base items; operations apply the
// index projection. Reads return items in DynamoDB order: by sort key within
// a partition (encoded key bytes), and for Scan by pk_hash, then pk, then sk.
type Store interface {
	// Ready reports whether the backend can serve requests (/readyz).
	Ready(ctx context.Context) error

	// CreateTable stores a new table, assigning Table.ID and the GSI IDs, and
	// returns it. ErrTableExists if the name is taken (including a table that
	// is still DELETING).
	CreateTable(ctx context.Context, t *Table) (*Table, error)
	// GetTable returns ErrTableNotFound when there is no such table.
	GetTable(ctx context.Context, name string) (*Table, error)
	// ListTables returns up to limit names in byte order, after startAfter.
	ListTables(ctx context.Context, startAfter string, limit int) ([]string, error)
	// UpdateTable applies fn to the current definition under a lock and
	// stores the result. A new GSI in the result (one without an ID) is
	// assigned an ID and backfilled; it stays CREATING until that finishes.
	UpdateTable(ctx context.Context, name string, fn func(*Table) error) (*Table, error)
	// DeleteTable marks the table DELETING and returns its definition; the
	// items are removed in the background, after which the name is free.
	DeleteTable(ctx context.Context, name string) (*Table, error)

	// GetItem returns the item, or nil when it does not exist.
	GetItem(ctx context.Context, t *Table, k Key) (attr.Item, error)
	// WriteItem runs one write atomically and returns the item before and
	// after it (either may be nil).
	WriteItem(ctx context.Context, w Write) (old, new attr.Item, err error)
	// Transact runs every write in one transaction, locking items in a fixed
	// order so concurrent transactions cannot deadlock. If any WriteFunc
	// returns an error nothing is written; the error is returned wrapped in
	// a *TxError that says which write failed.
	Transact(ctx context.Context, writes []Write) error

	Query(ctx context.Context, in QueryInput) (Page, error)
	Scan(ctx context.Context, in ScanInput) (Page, error)
}

// TxError reports which write of a Transact call failed.
type TxError struct {
	Index int   // position in the writes slice
	Err   error // what the WriteFunc (or the store) returned
}

func (e *TxError) Error() string { return e.Err.Error() }
func (e *TxError) Unwrap() error { return e.Err }

// Optional capabilities. A Store implements these to support DynamoDB Streams
// and TTL; operations find them with a type assertion and report the feature
// as unsupported otherwise. Both stores in this repository implement both.

// StreamRecord is one change to an item of a table with a stream. Records of a
// table are read in Seq order, which is commit order for any one item.
type StreamRecord struct {
	Seq   int64     // increasing within the table; DynamoDB's SequenceNumber
	Event string    // "INSERT", "MODIFY" or "REMOVE"
	Keys  attr.Item // the item's key attributes
	Old   attr.Item // the old image when the table's StreamView includes it, else nil
	New   attr.Item // the new image when the table's StreamView includes it, else nil
	At    time.Time // ApproximateCreationDateTime
	TTL   bool      // a REMOVE made by TTL expiry (userIdentity is the DynamoDB service)
	Size  int       // SizeBytes: the attr.ItemSize of Keys, Old and New together
}

// StreamStore keeps the records WriteItem and Transact write for a table whose
// StreamView is set, a consumer checkpoint per (consumer, table), and leases
// so only one server process runs each background job.
type StreamStore interface {
	// ReadStream returns up to limit records of t with Seq > after, in Seq
	// order. An empty result means the reader is at the end of the stream.
	// Seq never goes backwards: once a reader has seen Seq n, no record with
	// Seq <= n appears later, even from a write that committed late. So a
	// consumer can checkpoint the last Seq it processed. (A store may hold a
	// committed record back briefly to guarantee this.)
	ReadStream(ctx context.Context, t *Table, after int64, limit int) ([]StreamRecord, error)
	// StreamEnd returns the highest Seq ReadStream can currently return for
	// t (0 when the stream is empty), without reading the records: the
	// starting point of a LATEST shard iterator.
	StreamEnd(ctx context.Context, t *Table) (int64, error)
	// TrimStreams deletes every record created before cutoff (DynamoDB keeps
	// 24 hours) and returns how many it deleted, at most limit per call.
	TrimStreams(ctx context.Context, before time.Time, limit int) (int, error)
	// Checkpoint returns the last Seq the consumer finished for table t (0 if
	// none); SaveCheckpoint records it.
	Checkpoint(ctx context.Context, consumer string, t *Table) (int64, error)
	SaveCheckpoint(ctx context.Context, consumer string, t *Table, seq int64) error
	// TryLease takes or renews the named lease for owner until ttl from now.
	// It returns false when another owner holds an unexpired lease.
	TryLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error)
}

// TTLStore deletes expired items. An item expires when its table has TTL
// enabled and the TTL attribute is a number of epoch seconds before now.
type TTLStore interface {
	// ExpireItems deletes up to limit expired items across all tables, each
	// exactly as a WriteItem Delete would (GSI entries, and a REMOVE stream
	// record with TTL set when the table has a stream), and returns how many.
	ExpireItems(ctx context.Context, now time.Time, limit int) (int, error)
}

// Backup is an on-demand backup: a table's definition and items as of one
// moment.
type Backup struct {
	ID        string // unique; operations build the backup ARN from it
	Name      string
	Table     Table  // the definition when the backup was taken
	Status    string // CREATING while copying, then AVAILABLE; DELETED once deleted
	CreatedAt time.Time
	Items     int64 // item count, once AVAILABLE
	Bytes     int64 // attr.ItemSize total, once AVAILABLE
}

// BackupStore keeps on-demand backups in the store itself.
type BackupStore interface {
	// CreateBackup records a backup of t and copies its items as of one
	// consistent snapshot, in the background for a large table. The backup is
	// CREATING until the copy is complete.
	CreateBackup(ctx context.Context, t *Table, name string) (*Backup, error)
	// GetBackup returns ErrBackupNotFound for an unknown or deleted ID.
	GetBackup(ctx context.Context, id string) (*Backup, error)
	// ListBackups returns backups of the named table ("" for all tables)
	// created in [from, to) (zero times are open bounds), oldest first, after
	// the ID startAfter, at most limit (0 for no limit).
	ListBackups(ctx context.Context, table string, from, to time.Time, startAfter string, limit int) ([]Backup, error)
	// DeleteBackup deletes a backup and returns it with Status DELETED.
	DeleteBackup(ctx context.Context, id string) (*Backup, error)
	// RestoreBackup creates the table target (as CreateTable does, ErrTableExists
	// if the name is taken) and fills it with the backup's items, building its
	// index entries; target is CREATING until that is done, then ACTIVE. The
	// operations derive target from the backup's definition plus any overrides.
	RestoreBackup(ctx context.Context, id string, target *Table) (*Table, error)
}

// PITRStore restores tables to a point in time. For every table with
// PITREnabled, the store records each item version as it is written (and, when
// recovery is enabled, the table's items at that moment).
type PITRStore interface {
	// RecoveryWindow returns the earliest and latest times t can be restored
	// to now, or ErrOutsideRecoveryWindow when recovery is off or the initial
	// copy taken when it was enabled has not finished.
	RecoveryWindow(ctx context.Context, t *Table) (earliest, latest time.Time, err error)
	// RestoreToTime creates the table target (as RestoreBackup does) from t's
	// items as they were at time at.
	RestoreToTime(ctx context.Context, t *Table, at time.Time, target *Table) (*Table, error)
	// TrimHistory deletes item versions no table's recovery window needs any
	// more, at most limit per call, and returns how many it deleted.
	TrimHistory(ctx context.Context, now time.Time, limit int) (int, error)
}

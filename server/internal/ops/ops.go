// Package ops implements the DynamoDB operations on top of a storage.Store.
//
// One file per operation (getitem.go, query.go, ...). Each file registers
// its handler in an init function, so cards that add operations never edit a
// shared list:
//
//	func init() { register("GetItem", (*Ops).getItem) }
//
//	func (o *Ops) getItem(ctx context.Context, body []byte) (any, error) {
//		var req getItemInput
//		if err := decode(body, &req); err != nil { return nil, err }
//		...
//	}
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/api"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// Ops holds what every operation needs.
type Ops struct {
	Store storage.Store
	Now   func() time.Time
	Log   *slog.Logger
	// Region is the AWS region used in table and stream ARNs. It defaults to
	// "us-east-1" when empty.
	Region string

	// TagPageSize is the number of tags ListTagsOfResource returns per page.
	// Zero means DynamoDB's default of 50; tests lower it to exercise
	// NextToken paging without exceeding the 50-tag per-resource limit.
	TagPageSize int

	// idem holds TransactWriteItems idempotency state, keyed by
	// ClientRequestToken. idemEntry is defined in transactwrite.go.
	idemMu sync.Mutex
	idem   map[string]idemEntry
}

// now returns the current time, falling back to time.Now.
func (o *Ops) now() time.Time {
	if o.Now == nil {
		return time.Now()
	}
	return o.Now()
}

// region returns the AWS region for ARNs, defaulting to us-east-1.
func (o *Ops) region() string {
	if o.Region == "" {
		return "us-east-1"
	}
	return o.Region
}

type handler func(o *Ops, ctx context.Context, body []byte) (any, error)

var registry = map[string]handler{}

func register(name string, h handler) {
	if _, dup := registry[name]; dup {
		panic("ops: " + name + " registered twice")
	}
	registry[name] = h
}

// Names lists the registered operations, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Register adds every operation to the HTTP handler.
func (o *Ops) Register(h *api.Handler) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	for name, f := range registry {
		h.Register(name, func(ctx context.Context, body []byte) (any, error) {
			out, err := f(o, ctx, body)
			if errors.Is(err, storage.ErrThrottled) {
				return nil, ddberr.ProvisionedThroughputExceeded()
			}
			return out, err
		})
	}
}

// decode unmarshals a request body. Errors from attribute values (already
// *ddberr.Error) pass through; anything else is a SerializationException.
func decode(body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		var de *ddberr.Error
		if errors.As(err, &de) {
			return de
		}
		return ddberr.Serialization("Start of structure or map found where not expected.")
	}
	return nil
}

var tableNameRE = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// checkTableName applies DynamoDB's TableName constraints. param is the
// field name as DynamoDB reports it, usually "tableName".
func checkTableName(name, param string) error {
	switch {
	case len(name) < 3:
		return ddberr.Validation("1 validation error detected: Value '%s' at '%s' failed to satisfy constraint: Member must have length greater than or equal to 3", name, param)
	case len(name) > 255:
		return ddberr.Validation("1 validation error detected: Value '%s' at '%s' failed to satisfy constraint: Member must have length less than or equal to 255", name, param)
	case !tableNameRE.MatchString(name):
		return ddberr.Validation("1 validation error detected: Value '%s' at '%s' failed to satisfy constraint: Member must satisfy regular expression pattern: [a-zA-Z0-9_.-]+", name, param)
	}
	return nil
}

// table loads a table for a data plane operation. A missing or not yet ACTIVE
// table is ResourceNotFoundException, as in DynamoDB.
func (o *Ops) table(ctx context.Context, name string) (*storage.Table, error) {
	if err := checkTableName(name, "tableName"); err != nil {
		return nil, err
	}
	t, err := o.Store.GetTable(ctx, name)
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.ResourceNotFound("Requested resource not found")
	}
	if err != nil {
		return nil, err
	}
	if t.Status == storage.StatusCreating || t.Status == storage.StatusDeleting {
		return nil, ddberr.ResourceNotFound("Requested resource not found")
	}
	return t, nil
}

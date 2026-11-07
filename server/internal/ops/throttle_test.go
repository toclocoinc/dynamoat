package ops

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/api"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
)

// throttleStore fails every item write the way a store does when an item
// lock wait times out.
type throttleStore struct{ *memstore.Store }

func (throttleStore) WriteItem(context.Context, storage.Write) (attr.Item, attr.Item, error) {
	return nil, nil, storage.ErrThrottled
}

func TestLockTimeoutIsProvisionedThroughputExceeded(t *testing.T) {
	mem := memstore.New()
	makeTable(t, mem, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	o := &Ops{Store: throttleStore{mem}}
	h := api.New(api.Options{})
	o.Register(h)

	rec := post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"a"}}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if got, want := bodyJSON(t, rec)["__type"], "com.amazonaws.dynamodb.v20120810#ProvisionedThroughputExceededException"; got != want {
		t.Fatalf("__type = %v, want %s (body %s)", got, want, rec.Body.String())
	}
}

// txFailStore fails every transaction with err at the first write.
type txFailStore struct {
	*memstore.Store
	err error
}

func (s txFailStore) Transact(context.Context, []storage.Write) error {
	return &storage.TxError{Index: 0, Err: s.err}
}

func TestTransactStoreFailures(t *testing.T) {
	body := `{"TransactItems":[{"Put":{"TableName":"test","Item":{"id":{"S":"a"}}}},{"Put":{"TableName":"test","Item":{"id":{"S":"b"}}}}]}`
	for _, tc := range []struct {
		name   string
		err    error
		status int
		typ    string
		reason string
	}{
		{"lock timeout is a transaction conflict", storage.ErrThrottled, http.StatusBadRequest, "com.amazonaws.dynamodb.v20120810#TransactionCanceledException", "TransactionConflict"},
		{"store failure is a server error", errors.New("connection reset"), http.StatusInternalServerError, "com.amazonaws.dynamodb.v20120810#InternalServerError", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := memstore.New()
			makeTable(t, mem, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
			o := &Ops{Store: txFailStore{mem, tc.err}}
			h := api.New(api.Options{})
			o.Register(h)
			rec := post(t, h, "TransactWriteItems", body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}
			m := bodyJSON(t, rec)
			if m["__type"] != tc.typ {
				t.Fatalf("__type = %v, want %s", m["__type"], tc.typ)
			}
			if tc.reason != "" {
				reasons, _ := m["CancellationReasons"].([]any)
				if len(reasons) != 2 || reasons[0].(map[string]any)["Code"] != tc.reason || reasons[1].(map[string]any)["Code"] != "None" {
					t.Fatalf("CancellationReasons = %v", m["CancellationReasons"])
				}
			}
		})
	}
}

func TestTransactGetLockTimeoutIsConflict(t *testing.T) {
	mem := memstore.New()
	makeTable(t, mem, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	o := &Ops{Store: txFailStore{mem, storage.ErrThrottled}}
	h := api.New(api.Options{})
	o.Register(h)
	rec := post(t, h, "TransactGetItems", `{"TransactItems":[{"Get":{"TableName":"test","Key":{"id":{"S":"a"}}}},{"Get":{"TableName":"test","Key":{"id":{"S":"b"}}}}]}`)
	m := bodyJSON(t, rec)
	if rec.Code != http.StatusBadRequest || m["__type"] != "com.amazonaws.dynamodb.v20120810#TransactionCanceledException" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	reasons, _ := m["CancellationReasons"].([]any)
	if len(reasons) != 2 || reasons[0].(map[string]any)["Code"] != "TransactionConflict" {
		t.Fatalf("CancellationReasons = %v", m["CancellationReasons"])
	}
}

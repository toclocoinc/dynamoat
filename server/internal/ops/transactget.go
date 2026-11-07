package ops

import (
	"context"
	"errors"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/expr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() { register("TransactGetItems", (*Ops).transactGetItems) }

// transactGetLimit is the largest aggregate size, in bytes, of the items a
// TransactGetItems response may return (DynamoDB's 4 MB limit).
const transactGetLimit = 4 << 20

// transactGetItem is one element of TransactItems: exactly one Get member.
type transactGetItem struct {
	Get *transactGetInput `json:"Get"`
}

// transactGetInput is the Get member of a TransactGetItem.
type transactGetInput struct {
	TableName                string            `json:"TableName"`
	Key                      attr.Item         `json:"Key"`
	ProjectionExpression     string            `json:"ProjectionExpression"`
	ExpressionAttributeNames map[string]string `json:"ExpressionAttributeNames"`
}

type transactGetItemsInput struct {
	TransactItems          []transactGetItem `json:"TransactItems"`
	ReturnConsumedCapacity string            `json:"ReturnConsumedCapacity"`
}

// transactGetEntry is one validated read: its table, key and projection, plus
// the item the WriteFunc recorded from the snapshot.
type transactGetEntry struct {
	table *storage.Table
	key   storage.Key
	proj  []expr.Path
	item  attr.Item
}

func (o *Ops) transactGetItems(ctx context.Context, body []byte) (any, error) {
	var req transactGetItemsInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}

	entries, tables, err := o.transactGetPlans(ctx, body, req.TransactItems)
	if err != nil {
		return nil, err
	}

	// Read every item in one snapshot: each WriteFunc records the item it sees
	// while holding that item's lock and keeps it unchanged, and Transact takes
	// every item's lock in order, so all the reads observe one point in time.
	writes := make([]storage.Write, len(entries))
	for i := range entries {
		e := &entries[i]
		writes[i] = storage.Write{Table: e.table, Key: e.key, Fn: func(cur attr.Item) (storage.WriteResult, error) {
			e.item = cur
			return storage.WriteResult{Op: storage.Keep}, nil
		}}
	}
	if err := o.Store.Transact(ctx, writes); err != nil {
		// A lock timeout is a conflict with a concurrent write, which DynamoDB
		// reports as a cancelled transaction the SDK retries.
		var tx *storage.TxError
		if errors.As(err, &tx) && errors.Is(tx.Err, storage.ErrThrottled) {
			reasons := make([]map[string]any, len(entries))
			for i := range reasons {
				reasons[i] = map[string]any{"Code": "None"}
			}
			reasons[tx.Index] = map[string]any{"Code": "TransactionConflict", "Message": "Transaction is ongoing for the item"}
			return nil, ddberr.TransactionCanceled(reasons)
		}
		return nil, err
	}

	responses := make([]map[string]any, len(entries))
	total := 0
	for i := range entries {
		it := entries[i].item
		if it == nil {
			responses[i] = map[string]any{}
			continue
		}
		if entries[i].proj != nil {
			it = expr.Project(it, entries[i].proj)
		}
		if total += attr.ItemSize(it); total > transactGetLimit {
			return nil, ddberr.Validation("Transaction request cannot include more than 4 MB of data")
		}
		responses[i] = map[string]any{"Item": it}
	}

	out := map[string]any{"Responses": responses}
	if cc := consumedCapacityList(tables, req.ReturnConsumedCapacity); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// transactGetPlans validates every read and resolves its table, key and
// projection before any read, so a bad entry fails the whole call with a plain
// ValidationException.
func (o *Ops) transactGetPlans(ctx context.Context, body []byte, items []transactGetItem) ([]transactGetEntry, []*storage.Table, error) {
	if len(items) < 1 || len(items) > 100 {
		raw := rawTransactItems(body)
		if len(items) < 1 {
			return nil, nil, ddberr.Validation("1 validation error detected: Value '%s' at 'transactItems' failed to satisfy constraint: Member must have length greater than or equal to 1", raw)
		}
		return nil, nil, ddberr.Validation("1 validation error detected: Value '%s' at 'transactItems' failed to satisfy constraint: Member must have length less than or equal to 100", raw)
	}

	entries := make([]transactGetEntry, 0, len(items))
	tables := make([]*storage.Table, 0, 4)
	seen := make(map[string]bool, len(items))
	seenTable := make(map[string]bool, len(items))
	for i := range items {
		req := items[i].Get
		if req == nil {
			return nil, nil, ddberr.Validation("1 validation error detected: Value null at 'transactItems.%d.member.get' failed to satisfy constraint: Member must not be null", i)
		}
		t, err := o.table(ctx, req.TableName)
		if err != nil {
			return nil, nil, err
		}
		key, err := storage.KeyOf(t.Key, req.Key, false)
		if err != nil {
			return nil, nil, err
		}
		ex, err := parseExpressions(expressionInput{
			ProjectionExpression: req.ProjectionExpression,
			Names:                req.ExpressionAttributeNames,
		})
		if err != nil {
			return nil, nil, err
		}
		enc := t.Name + "\x00" + encodeStorageKey(key)
		if seen[enc] {
			return nil, nil, ddberr.Validation("Transaction request cannot include multiple operations on one item")
		}
		seen[enc] = true
		if !seenTable[t.Name] {
			seenTable[t.Name] = true
			tables = append(tables, t)
		}
		entries = append(entries, transactGetEntry{table: t, key: key, proj: ex.proj})
	}
	return entries, tables, nil
}

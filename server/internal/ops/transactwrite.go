package ops

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/expr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() {
	register("TransactWriteItems", (*Ops).transactWriteItems)
}

// transactItem is one element of TransactItems: exactly one of Put, Update,
// Delete or ConditionCheck. The entry types reuse the single-item request
// structs, whose fields the transaction variant shares.
type transactItem struct {
	Put            *putItemInput     `json:"Put"`
	Update         *updateItemInput  `json:"Update"`
	Delete         *deleteItemInput  `json:"Delete"`
	ConditionCheck *conditionCheckIn `json:"ConditionCheck"`
}

// conditionCheckIn is a ConditionCheck entry: a condition on an item that
// writes nothing.
type conditionCheckIn struct {
	TableName                           string                `json:"TableName"`
	Key                                 attr.Item             `json:"Key"`
	ConditionExpression                 string                `json:"ConditionExpression"`
	ExpressionAttributeNames            map[string]string     `json:"ExpressionAttributeNames"`
	ExpressionAttributeValues           map[string]attr.Value `json:"ExpressionAttributeValues"`
	ReturnValuesOnConditionCheckFailure string                `json:"ReturnValuesOnConditionCheckFailure"`
}

type transactWriteItemsInput struct {
	TransactItems          []transactItem `json:"TransactItems"`
	ReturnConsumedCapacity string         `json:"ReturnConsumedCapacity"`
	ClientRequestToken     string         `json:"ClientRequestToken"`
}

// transactEntry is one validated transaction element, ready to run as a
// storage.Write. old records the item the write saw, so a failed condition can
// report it when ReturnValuesOnConditionCheckFailure is ALL_OLD.
type transactEntry struct {
	kind  string
	table *storage.Table
	key   storage.Key
	item  attr.Item // the Item of a Put
	ex    *expressions
	rvocf string
	old   attr.Item
}

// idemEntry is one ClientRequestToken's idempotency record.
type idemEntry struct {
	hash [32]byte
	at   time.Time
}

func (o *Ops) transactWriteItems(ctx context.Context, body []byte) (any, error) {
	var req transactWriteItemsInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if req.ClientRequestToken != "" && len(req.ClientRequestToken) > 36 {
		return nil, ddberr.Validation("1 validation error detected: Value '%s' at 'clientRequestToken' failed to satisfy constraint: Member must have length less than or equal to 36", req.ClientRequestToken)
	}

	entries, tables, err := o.transactPlans(ctx, body, req.TransactItems)
	if err != nil {
		return nil, err
	}

	if req.ClientRequestToken != "" {
		hit, err := o.transactIdempotent(req.ClientRequestToken, body)
		if err != nil {
			return nil, err
		}
		if hit {
			return transactResponse(tables, req.ReturnConsumedCapacity), nil
		}
	}

	writes := make([]storage.Write, len(entries))
	for i := range entries {
		writes[i] = entries[i].write()
	}
	if err := o.Store.Transact(ctx, writes); err != nil {
		if req.ClientRequestToken != "" {
			o.forgetIdempotent(req.ClientRequestToken)
		}
		var tx *storage.TxError
		if !errors.As(err, &tx) {
			return nil, err
		}
		// A store failure is a server error, not a reason for the client to
		// change its request; a lock timeout is a conflict the SDK retries.
		var de *ddberr.Error
		if !errors.As(tx.Err, &de) && !errors.Is(tx.Err, storage.ErrThrottled) {
			return nil, tx.Err
		}
		return nil, transactCanceled(entries, tx.Index, tx.Err)
	}
	return transactResponse(tables, req.ReturnConsumedCapacity), nil
}

// transactPlans validates every entry and resolves its table, key and
// expressions before any write, so a bad entry fails the whole call with a
// plain ValidationException and nothing is written.
func (o *Ops) transactPlans(ctx context.Context, body []byte, items []transactItem) ([]transactEntry, []*storage.Table, error) {
	if len(items) < 1 || len(items) > 100 {
		raw := rawTransactItems(body)
		if len(items) < 1 {
			return nil, nil, ddberr.Validation("1 validation error detected: Value '%s' at 'transactItems' failed to satisfy constraint: Member must have length greater than or equal to 1", raw)
		}
		return nil, nil, ddberr.Validation("1 validation error detected: Value '%s' at 'transactItems' failed to satisfy constraint: Member must have length less than or equal to 100", raw)
	}

	entries := make([]transactEntry, 0, len(items))
	seen := make(map[string]bool, len(items))
	tables := make([]*storage.Table, 0, 4)
	seenTable := make(map[string]bool, len(items))
	for i := range items {
		e, err := o.transactEntry(ctx, items[i], i)
		if err != nil {
			return nil, nil, err
		}
		enc := e.table.Name + "\x00" + encodeStorageKey(e.key)
		if seen[enc] {
			return nil, nil, ddberr.Validation("Transaction request cannot include multiple operations on one item")
		}
		seen[enc] = true
		if !seenTable[e.table.Name] {
			seenTable[e.table.Name] = true
			tables = append(tables, e.table)
		}
		entries = append(entries, e)
	}
	return entries, tables, nil
}

// transactEntry validates one element and returns its plan.
func (o *Ops) transactEntry(ctx context.Context, it transactItem, i int) (transactEntry, error) {
	var e transactEntry
	switch {
	case it.Put != nil && it.Update == nil && it.Delete == nil && it.ConditionCheck == nil:
		req := it.Put
		rvocf, err := checkReturnValuesOnConditionCheckFailure(req.ReturnValuesOnConditionCheckFailure)
		if err != nil {
			return e, err
		}
		t, err := o.table(ctx, req.TableName)
		if err != nil {
			return e, err
		}
		key, err := storage.KeyOf(t.Key, req.Item, true)
		if err != nil {
			return e, err
		}
		if err := attr.CheckItemSize(req.Item); err != nil {
			return e, err
		}
		ex, err := parseExpressions(expressionInput{
			ConditionExpression: req.ConditionExpression,
			Names:               req.ExpressionAttributeNames,
			Values:              req.ExpressionAttributeValues,
		})
		if err != nil {
			return e, err
		}
		return transactEntry{kind: "Put", table: t, key: key, item: req.Item, ex: ex, rvocf: rvocf}, nil

	case it.Update != nil && it.Put == nil && it.Delete == nil && it.ConditionCheck == nil:
		req := it.Update
		rvocf, err := checkReturnValuesOnConditionCheckFailure(req.ReturnValuesOnConditionCheckFailure)
		if err != nil {
			return e, err
		}
		t, err := o.table(ctx, req.TableName)
		if err != nil {
			return e, err
		}
		key, err := storage.KeyOf(t.Key, req.Key, false)
		if err != nil {
			return e, err
		}
		ex, err := parseExpressions(expressionInput{
			ConditionExpression: req.ConditionExpression,
			UpdateExpression:    req.UpdateExpression,
			Names:               req.ExpressionAttributeNames,
			Values:              req.ExpressionAttributeValues,
		})
		if err != nil {
			return e, err
		}
		if name, ok := keyPath(ex.update, t.Key); ok {
			return e, ddberr.Validation("One or more parameter values were invalid: Cannot update attribute %s. This attribute is part of the key", name)
		}
		return transactEntry{kind: "Update", table: t, key: key, ex: ex, rvocf: rvocf}, nil

	case it.Delete != nil && it.Put == nil && it.Update == nil && it.ConditionCheck == nil:
		req := it.Delete
		rvocf, err := checkReturnValuesOnConditionCheckFailure(req.ReturnValuesOnConditionCheckFailure)
		if err != nil {
			return e, err
		}
		t, err := o.table(ctx, req.TableName)
		if err != nil {
			return e, err
		}
		key, err := storage.KeyOf(t.Key, req.Key, false)
		if err != nil {
			return e, err
		}
		ex, err := parseExpressions(expressionInput{
			ConditionExpression: req.ConditionExpression,
			Names:               req.ExpressionAttributeNames,
			Values:              req.ExpressionAttributeValues,
		})
		if err != nil {
			return e, err
		}
		return transactEntry{kind: "Delete", table: t, key: key, ex: ex, rvocf: rvocf}, nil

	case it.ConditionCheck != nil && it.Put == nil && it.Update == nil && it.Delete == nil:
		req := it.ConditionCheck
		rvocf, err := checkReturnValuesOnConditionCheckFailure(req.ReturnValuesOnConditionCheckFailure)
		if err != nil {
			return e, err
		}
		t, err := o.table(ctx, req.TableName)
		if err != nil {
			return e, err
		}
		key, err := storage.KeyOf(t.Key, req.Key, false)
		if err != nil {
			return e, err
		}
		if req.ConditionExpression == "" {
			return e, ddberr.Validation("1 validation error detected: Value at 'transactItems.%d.member.conditionCheck.conditionExpression' failed to satisfy constraint: Member must not be null", i)
		}
		ex, err := parseExpressions(expressionInput{
			ConditionExpression: req.ConditionExpression,
			Names:               req.ExpressionAttributeNames,
			Values:              req.ExpressionAttributeValues,
		})
		if err != nil {
			return e, err
		}
		return transactEntry{kind: "ConditionCheck", table: t, key: key, ex: ex, rvocf: rvocf}, nil
	}
	return e, ddberr.Validation("1 validation error detected: Value at 'transactItems.%d.member' failed to satisfy constraint: Member must satisfy constraint: [Member must have exactly one of the fields Put, Update, Delete or ConditionCheck]", i)
}

// write builds the storage.Write for a validated entry. The WriteFunc decides
// the write against the current item inside the transaction, as the
// single-item operations do.
func (e *transactEntry) write() storage.Write {
	return storage.Write{Table: e.table, Key: e.key, Fn: func(cur attr.Item) (storage.WriteResult, error) {
		e.old = cur
		if e.ex.cond != nil {
			ok, err := expr.Eval(e.ex.cond, cur, e.ex.values, "ConditionExpression")
			if err != nil {
				return storage.WriteResult{}, err
			}
			if !ok {
				return storage.WriteResult{}, conditionalCheckFailed(cur, e.rvocf)
			}
		}
		switch e.kind {
		case "Put":
			if err := checkIndexKeys(e.table, e.item); err != nil {
				return storage.WriteResult{}, err
			}
			return storage.WriteResult{Op: storage.Put, Item: e.item}, nil
		case "Delete":
			return storage.WriteResult{Op: storage.Delete}, nil
		case "ConditionCheck":
			return storage.WriteResult{Op: storage.Keep}, nil
		case "Update":
			base := cur
			if base == nil {
				base = keyOnlyItem(e.table.Key, e.key)
			}
			updated, _, err := expr.Apply(e.ex.update, base, e.ex.values)
			if err != nil {
				return storage.WriteResult{}, err
			}
			if err := checkIndexKeys(e.table, updated); err != nil {
				return storage.WriteResult{}, err
			}
			if err := attr.CheckItemSize(updated); err != nil {
				return storage.WriteResult{}, err
			}
			return storage.WriteResult{Op: storage.Put, Item: updated}, nil
		}
		return storage.WriteResult{}, nil
	}}
}

// transactCanceled turns a *storage.TxError into TransactionCanceled with one
// reason per entry in request order.
func transactCanceled(entries []transactEntry, failed int, err error) error {
	reasons := make([]map[string]any, len(entries))
	for i := range entries {
		if i != failed {
			reasons[i] = map[string]any{"Code": "None"}
			continue
		}
		reasons[i] = transactReason(entries[i], err)
	}
	return ddberr.TransactionCanceled(reasons)
}

// transactReason reports why one entry failed. A failed condition is
// ConditionalCheckFailed (with the old item when the entry asked for ALL_OLD);
// any other error found while applying is a ValidationError.
func transactReason(e transactEntry, err error) map[string]any {
	if errors.Is(err, storage.ErrThrottled) {
		return map[string]any{"Code": "TransactionConflict", "Message": "Transaction is ongoing for the item"}
	}
	var de *ddberr.Error
	if errors.As(err, &de) && de.Code() == "ConditionalCheckFailedException" {
		r := map[string]any{"Code": "ConditionalCheckFailed", "Message": "The conditional request failed"}
		if e.rvocf == returnAllOld && e.old != nil {
			r["Item"] = itemJSON(e.old)
		}
		return r
	}
	msg := err.Error()
	if errors.As(err, &de) {
		msg = de.Message
	}
	return map[string]any{"Code": "ValidationError", "Message": msg}
}

// transactIdempotent checks and records a ClientRequestToken. hit reports a
// previously seen token with the same body, so the caller returns success
// without writing again. A token seen with a different body is
// IdempotentParameterMismatch. Entries older than ten minutes are purged.
func (o *Ops) transactIdempotent(token string, body []byte) (bool, error) {
	sum := sha256.Sum256(body)
	now := o.now()
	o.idemMu.Lock()
	defer o.idemMu.Unlock()
	for k, e := range o.idem {
		if now.Sub(e.at) > 10*time.Minute {
			delete(o.idem, k)
		}
	}
	if e, ok := o.idem[token]; ok {
		if e.hash == sum {
			return true, nil
		}
		return false, ddberr.IdempotentParameterMismatch()
	}
	if o.idem == nil {
		o.idem = map[string]idemEntry{}
	}
	o.idem[token] = idemEntry{hash: sum, at: now}
	return false, nil
}

// forgetIdempotent drops a token's record when its transaction failed, so a
// retry can attempt the write again.
func (o *Ops) forgetIdempotent(token string) {
	o.idemMu.Lock()
	defer o.idemMu.Unlock()
	delete(o.idem, token)
}

// transactResponse is the TransactWriteItems response body: empty, plus
// ConsumedCapacity when asked.
func transactResponse(tables []*storage.Table, rcc string) map[string]any {
	out := map[string]any{}
	if cc := consumedCapacityList(tables, rcc); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out
}

// rawTransactItems returns the TransactItems member verbatim, for validation
// messages that echo the offending value.
func rawTransactItems(body []byte) string {
	var raw struct {
		TransactItems json.RawMessage `json:"TransactItems"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || len(raw.TransactItems) == 0 {
		return "[]"
	}
	return string(raw.TransactItems)
}

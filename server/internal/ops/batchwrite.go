package ops

import (
	"context"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() { register("BatchWriteItem", (*Ops).batchWriteItem) }

// batchWriteRequest is one entry of a table's write list: exactly one of
// PutRequest or DeleteRequest.
type batchWriteRequest struct {
	PutRequest    *batchPutRequest    `json:"PutRequest"`
	DeleteRequest *batchDeleteRequest `json:"DeleteRequest"`
}

type batchPutRequest struct {
	Item attr.Item `json:"Item"`
}

type batchDeleteRequest struct {
	Key attr.Item `json:"Key"`
}

type batchWriteItemInput struct {
	RequestItems           map[string][]batchWriteRequest `json:"RequestItems"`
	ReturnConsumedCapacity string                         `json:"ReturnConsumedCapacity"`
}

func (o *Ops) batchWriteItem(ctx context.Context, body []byte) (any, error) {
	var req batchWriteItemInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}

	names := sortedKeys(req.RequestItems)
	total := 0
	for _, n := range names {
		total += len(req.RequestItems[n])
	}
	if total < 1 || total > 25 {
		return nil, ddberr.Validation("1 validation error detected: Value '%s' at 'requestItems' failed to satisfy constraint: Map value must satisfy constraint: [Member must have length less than or equal to 25, Member must have length greater than or equal to 1]", rawRequestItems(body))
	}

	// Validate every request before any write, so a bad one fails the call
	// without a partial write. BatchWriteItem is not atomic: once validated,
	// each request is written separately.
	var tables []*storage.Table
	var writes []storage.Write
	for _, n := range names {
		t, err := o.table(ctx, n)
		if err != nil {
			return nil, err
		}
		tables = append(tables, t)
		seen := make(map[string]bool, len(req.RequestItems[n]))
		for _, wr := range req.RequestItems[n] {
			switch {
			case wr.PutRequest != nil && wr.DeleteRequest == nil:
				item := wr.PutRequest.Item
				key, err := storage.KeyOf(t.Key, item, true)
				if err != nil {
					return nil, err
				}
				if err := checkIndexKeys(t, item); err != nil {
					return nil, err
				}
				if err := attr.CheckItemSize(item); err != nil {
					return nil, err
				}
				if err := markKey(seen, key); err != nil {
					return nil, err
				}
				writes = append(writes, storage.Write{Table: t, Key: key, Fn: putWrite(item)})
			case wr.DeleteRequest != nil && wr.PutRequest == nil:
				key, err := storage.KeyOf(t.Key, wr.DeleteRequest.Key, false)
				if err != nil {
					return nil, err
				}
				if err := markKey(seen, key); err != nil {
					return nil, err
				}
				writes = append(writes, storage.Write{Table: t, Key: key, Fn: deleteWrite()})
			default:
				return nil, ddberr.Validation("1 validation error detected: Value at 'requestItems.%s.member' failed to satisfy constraint: Member must satisfy constraint: [Member must have exactly one of the fields PutRequest or DeleteRequest]", n)
			}
		}
	}

	for _, w := range writes {
		if _, _, err := o.Store.WriteItem(ctx, w); err != nil {
			return nil, err
		}
	}

	out := map[string]any{"UnprocessedItems": map[string]any{}}
	if cc := consumedCapacityList(tables, req.ReturnConsumedCapacity); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// markKey records a key for duplicate detection within one table.
func markKey(seen map[string]bool, key storage.Key) error {
	enc := encodeStorageKey(key)
	if seen[enc] {
		return ddberr.Validation("Provided list of item keys contains duplicates")
	}
	seen[enc] = true
	return nil
}

// putWrite is a WriteFunc that stores item, replacing any current item.
func putWrite(item attr.Item) storage.WriteFunc {
	return func(attr.Item) (storage.WriteResult, error) {
		return storage.WriteResult{Op: storage.Put, Item: item}, nil
	}
}

// deleteWrite is a WriteFunc that removes the item.
func deleteWrite() storage.WriteFunc {
	return func(attr.Item) (storage.WriteResult, error) {
		return storage.WriteResult{Op: storage.Delete}, nil
	}
}

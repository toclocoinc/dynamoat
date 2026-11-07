package ops

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/expr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() { register("BatchGetItem", (*Ops).batchGetItem) }

// batchResponseLimit is the largest BatchGetItem response DynamoDB returns
// before the unread keys move to UnprocessedKeys.
const batchResponseLimit = 16 << 20

// batchGetKeysAndAttributes is one table's request in RequestItems.
type batchGetKeysAndAttributes struct {
	Keys                     []attr.Item       `json:"Keys"`
	ProjectionExpression     string            `json:"ProjectionExpression"`
	ExpressionAttributeNames map[string]string `json:"ExpressionAttributeNames"`
	ConsistentRead           bool              `json:"ConsistentRead"`
}

type batchGetItemInput struct {
	RequestItems           map[string]batchGetKeysAndAttributes `json:"RequestItems"`
	ReturnConsumedCapacity string                               `json:"ReturnConsumedCapacity"`
}

// batchGetTable is one validated table of a BatchGetItem request, with its keys
// already extracted and its projection parsed.
type batchGetTable struct {
	name string
	t    *storage.Table
	req  batchGetKeysAndAttributes
	keys []storage.Key
	proj []expr.Path
}

func (o *Ops) batchGetItem(ctx context.Context, body []byte) (any, error) {
	var req batchGetItemInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	plans, err := o.batchGetPlans(ctx, body, req)
	if err != nil {
		return nil, err
	}

	responses := map[string][]attr.Item{}
	unprocessed := map[string]batchGetKeysAndAttributes{}
	running := 0
	for _, p := range plans {
		var returned []attr.Item
		for i, key := range p.keys {
			item, err := o.Store.GetItem(ctx, p.t, key)
			if err != nil {
				return nil, err
			}
			if item == nil {
				continue
			}
			if p.proj != nil {
				item = expr.Project(item, p.proj)
			}
			if running+attr.ItemSize(item) > batchResponseLimit {
				rest := p.req
				rest.Keys = p.req.Keys[i:]
				unprocessed[p.name] = rest
				break
			}
			returned = append(returned, item)
			running += attr.ItemSize(item)
		}
		if len(returned) > 0 {
			responses[p.name] = returned
		}
	}

	out := map[string]any{
		"Responses":       responses,
		"UnprocessedKeys": unprocessed,
	}
	if cc := consumedCapacityList(planTables(plans), req.ReturnConsumedCapacity); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// batchGetPlans validates the request and resolves every table, key and
// projection before any item is read.
func (o *Ops) batchGetPlans(ctx context.Context, body []byte, req batchGetItemInput) ([]batchGetTable, error) {
	names := sortedKeys(req.RequestItems)
	total := 0
	for _, n := range names {
		total += len(req.RequestItems[n].Keys)
	}
	if total > 100 {
		return nil, ddberr.Validation("Too many items requested for the BatchGetItem call")
	}
	if total == 0 {
		return nil, ddberr.Validation("1 validation error detected: Value '%s' at 'requestItems' failed to satisfy constraint: Map value must satisfy constraint: [Member must have length greater than or equal to 1]", rawRequestItems(body))
	}

	plans := make([]batchGetTable, 0, len(names))
	for _, n := range names {
		r := req.RequestItems[n]
		t, err := o.table(ctx, n)
		if err != nil {
			return nil, err
		}
		keys := make([]storage.Key, len(r.Keys))
		seen := make(map[string]bool, len(r.Keys))
		for i, k := range r.Keys {
			key, err := storage.KeyOf(t.Key, k, false)
			if err != nil {
				return nil, err
			}
			enc := encodeStorageKey(key)
			if seen[enc] {
				return nil, ddberr.Validation("Provided list of item keys contains duplicates")
			}
			seen[enc] = true
			keys[i] = key
		}
		ex, err := parseExpressions(expressionInput{
			ProjectionExpression: r.ProjectionExpression,
			Names:                r.ExpressionAttributeNames,
		})
		if err != nil {
			return nil, err
		}
		plans = append(plans, batchGetTable{name: n, t: t, req: r, keys: keys, proj: ex.proj})
	}
	return plans, nil
}

// sortedKeys returns a map's keys in byte order, so validation and responses
// are deterministic.
func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// rawRequestItems returns the RequestItems member verbatim, for validation
// messages that echo the offending value.
func rawRequestItems(body []byte) string {
	var raw struct {
		RequestItems json.RawMessage `json:"RequestItems"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || len(raw.RequestItems) == 0 {
		return "{}"
	}
	return string(raw.RequestItems)
}

// encodeStorageKey renders a key for duplicate detection.
func encodeStorageKey(k storage.Key) string {
	h, _ := attr.EncodeKey(k.Hash)
	if k.Range.Type == 0 {
		return string(h)
	}
	r, _ := attr.EncodeKey(k.Range)
	return string(h) + "\x00" + string(r)
}

// planTables returns the tables of the plans, in plan order.
func planTables(plans []batchGetTable) []*storage.Table {
	out := make([]*storage.Table, len(plans))
	for i, p := range plans {
		out[i] = p.t
	}
	return out
}

// consumedCapacityList reports a fixed 1.0 CapacityUnits per table when
// ReturnConsumedCapacity is TOTAL or INDEXES, as DynamoDB's batch operations do.
func consumedCapacityList(tables []*storage.Table, rcc string) []map[string]any {
	if rcc != "TOTAL" && rcc != "INDEXES" {
		return nil
	}
	out := make([]map[string]any, 0, len(tables))
	for _, t := range tables {
		out = append(out, map[string]any{"TableName": t.Name, "CapacityUnits": json.Number("1.0")})
	}
	return out
}

package ops

import (
	"context"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/expr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() { register("UpdateItem", (*Ops).updateItem) }

type updateItemInput struct {
	TableName                           string                `json:"TableName"`
	Key                                 attr.Item             `json:"Key"`
	UpdateExpression                    string                `json:"UpdateExpression"`
	ConditionExpression                 string                `json:"ConditionExpression"`
	ExpressionAttributeNames            map[string]string     `json:"ExpressionAttributeNames"`
	ExpressionAttributeValues           map[string]attr.Value `json:"ExpressionAttributeValues"`
	ReturnValues                        string                `json:"ReturnValues"`
	ReturnConsumedCapacity              string                `json:"ReturnConsumedCapacity"`
	ReturnValuesOnConditionCheckFailure string                `json:"ReturnValuesOnConditionCheckFailure"`
}

func (o *Ops) updateItem(ctx context.Context, body []byte) (any, error) {
	var req updateItemInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	rv, err := checkUpdateReturnValues(req.ReturnValues)
	if err != nil {
		return nil, err
	}
	rvocf, err := checkReturnValuesOnConditionCheckFailure(req.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		return nil, err
	}
	t, err := o.table(ctx, req.TableName)
	if err != nil {
		return nil, err
	}
	key, err := storage.KeyOf(t.Key, req.Key, false)
	if err != nil {
		return nil, err
	}
	ex, err := parseExpressions(expressionInput{
		ConditionExpression: req.ConditionExpression,
		UpdateExpression:    req.UpdateExpression,
		Names:               req.ExpressionAttributeNames,
		Values:              req.ExpressionAttributeValues,
	})
	if err != nil {
		return nil, err
	}
	if name, ok := keyPath(ex.update, t.Key); ok {
		return nil, ddberr.Validation("One or more parameter values were invalid: Cannot update attribute %s. This attribute is part of the key", name)
	}

	var old, updated attr.Item
	var touched []expr.Path
	_, _, err = o.Store.WriteItem(ctx, storage.Write{
		Table: t,
		Key:   key,
		Fn: func(cur attr.Item) (storage.WriteResult, error) {
			old = cur
			if ex.cond != nil {
				ok, err := expr.Eval(ex.cond, cur, ex.values, "ConditionExpression")
				if err != nil {
					return storage.WriteResult{}, err
				}
				if !ok {
					return storage.WriteResult{}, conditionalCheckFailed(cur, rvocf)
				}
			}
			base := cur
			if base == nil {
				base = keyOnlyItem(t.Key, key)
			}
			var err error
			updated, touched, err = expr.Apply(ex.update, base, ex.values)
			if err != nil {
				return storage.WriteResult{}, err
			}
			if err := checkIndexKeys(t, updated); err != nil {
				return storage.WriteResult{}, err
			}
			if err := attr.CheckItemSize(updated); err != nil {
				return storage.WriteResult{}, err
			}
			return storage.WriteResult{Op: storage.Put, Item: updated}, nil
		},
	})
	if err != nil {
		return nil, err
	}

	out := map[string]any{}
	if cc := consumedCapacity(t, req.ReturnConsumedCapacity); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	switch rv {
	case returnAllOld:
		if old != nil {
			out["Attributes"] = old
		}
	case returnAllNew:
		out["Attributes"] = updated
	case returnUpdatedOld, returnUpdatedNew:
		// DynamoDB leaves Attributes out when none of the touched paths has a
		// value (a new item's UPDATED_OLD, or UPDATED_NEW after only REMOVEs).
		src := updated
		if rv == returnUpdatedOld {
			src = old
		}
		if attrs := expr.Project(src, touched); len(attrs) > 0 {
			out["Attributes"] = attrs
		}
	}
	return out, nil
}

// checkUpdateReturnValues validates ReturnValues for UpdateItem, which accepts
// every value unlike PutItem and DeleteItem.
func checkUpdateReturnValues(v string) (string, error) {
	switch v {
	case "", returnNone:
		return returnNone, nil
	case returnAllOld, returnAllNew, returnUpdatedOld, returnUpdatedNew:
		return v, nil
	default:
		return "", ddberr.Validation("1 validation error detected: Value '%s' at 'returnValues' failed to satisfy constraint: Member must satisfy enum value set: [ALL_NEW, UPDATED_OLD, ALL_OLD, NONE, UPDATED_NEW]", v)
	}
}

// keyPath reports the first path whose first element is a table key attribute,
// which an update must not touch.
func keyPath(u expr.Update, schema storage.KeySchema) (string, bool) {
	paths := make([]expr.Path, 0, len(u.Set)+len(u.Remove)+len(u.Add)+len(u.Delete))
	for _, a := range u.Set {
		paths = append(paths, a.Path)
	}
	paths = append(paths, u.Remove...)
	for _, a := range u.Add {
		paths = append(paths, a.Path)
	}
	for _, a := range u.Delete {
		paths = append(paths, a.Path)
	}
	for _, p := range paths {
		if len(p) == 0 {
			continue
		}
		name := p[0].Name
		if name == schema.Hash || (schema.Range != "" && name == schema.Range) {
			return name, true
		}
	}
	return "", false
}

// keyOnlyItem is the item an UpdateItem starts from when none exists: the key
// attributes alone.
func keyOnlyItem(schema storage.KeySchema, k storage.Key) attr.Item {
	it := attr.Item{schema.Hash: k.Hash}
	if schema.Range != "" {
		it[schema.Range] = k.Range
	}
	return it
}

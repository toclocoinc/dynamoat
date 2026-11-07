package ops

import (
	"context"
	"encoding/json"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/expr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() { register("PutItem", (*Ops).putItem) }

type putItemInput struct {
	TableName                           string                     `json:"TableName"`
	Item                                attr.Item                  `json:"Item"`
	ConditionExpression                 string                     `json:"ConditionExpression"`
	ExpressionAttributeNames            map[string]string          `json:"ExpressionAttributeNames"`
	ExpressionAttributeValues           map[string]attr.Value      `json:"ExpressionAttributeValues"`
	ReturnValues                        string                     `json:"ReturnValues"`
	ReturnConsumedCapacity              string                     `json:"ReturnConsumedCapacity"`
	ReturnValuesOnConditionCheckFailure string                     `json:"ReturnValuesOnConditionCheckFailure"`
	Expected                            map[string]json.RawMessage `json:"Expected"`
}

func (o *Ops) putItem(ctx context.Context, body []byte) (any, error) {
	var req putItemInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := rejectLegacy(map[string]bool{"Expected": req.Expected != nil}); err != nil {
		return nil, err
	}
	rv, err := checkReturnValues(req.ReturnValues)
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
	key, err := storage.KeyOf(t.Key, req.Item, true)
	if err != nil {
		return nil, err
	}
	if err := checkIndexKeys(t, req.Item); err != nil {
		return nil, err
	}
	if err := attr.CheckItemSize(req.Item); err != nil {
		return nil, err
	}
	ex, err := parseExpressions(expressionInput{
		ConditionExpression: req.ConditionExpression,
		Names:               req.ExpressionAttributeNames,
		Values:              req.ExpressionAttributeValues,
	})
	if err != nil {
		return nil, err
	}

	var old attr.Item
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
			return storage.WriteResult{Op: storage.Put, Item: req.Item}, nil
		},
	})
	if err != nil {
		return nil, err
	}

	out := map[string]any{}
	if cc := consumedCapacity(t, req.ReturnConsumedCapacity); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	if rv == returnAllOld && old != nil {
		out["Attributes"] = old
	}
	return out, nil
}

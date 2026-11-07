package ops

import (
	"context"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/expr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() { register("GetItem", (*Ops).getItem) }

type getItemInput struct {
	TableName                string            `json:"TableName"`
	Key                      attr.Item         `json:"Key"`
	ProjectionExpression     string            `json:"ProjectionExpression"`
	ExpressionAttributeNames map[string]string `json:"ExpressionAttributeNames"`
	// ConsistentRead is accepted and ignored: reads are always consistent.
	ConsistentRead         bool     `json:"ConsistentRead"`
	ReturnConsumedCapacity string   `json:"ReturnConsumedCapacity"`
	AttributesToGet        []string `json:"AttributesToGet"`
}

func (o *Ops) getItem(ctx context.Context, body []byte) (any, error) {
	var req getItemInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := rejectLegacy(map[string]bool{"AttributesToGet": req.AttributesToGet != nil}); err != nil {
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
		ProjectionExpression: req.ProjectionExpression,
		Names:                req.ExpressionAttributeNames,
	})
	if err != nil {
		return nil, err
	}
	item, err := o.Store.GetItem(ctx, t, key)
	if err != nil {
		return nil, err
	}

	out := map[string]any{}
	if cc := consumedCapacity(t, req.ReturnConsumedCapacity); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	if item != nil {
		if ex.proj != nil {
			item = expr.Project(item, ex.proj)
		}
		out["Item"] = item
	}
	return out, nil
}

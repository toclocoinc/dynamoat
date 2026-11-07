package ops

import (
	"context"
	"encoding/json"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/expr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() { register("Query", (*Ops).query) }

type queryInput struct {
	TableName                 string                     `json:"TableName"`
	IndexName                 string                     `json:"IndexName"`
	KeyConditionExpression    string                     `json:"KeyConditionExpression"`
	FilterExpression          string                     `json:"FilterExpression"`
	ProjectionExpression      string                     `json:"ProjectionExpression"`
	ExpressionAttributeNames  map[string]string          `json:"ExpressionAttributeNames"`
	ExpressionAttributeValues map[string]attr.Value      `json:"ExpressionAttributeValues"`
	ScanIndexForward          *bool                      `json:"ScanIndexForward"`
	Limit                     *int                       `json:"Limit"`
	ExclusiveStartKey         attr.Item                  `json:"ExclusiveStartKey"`
	Select                    string                     `json:"Select"`
	ConsistentRead            bool                       `json:"ConsistentRead"`
	ReturnConsumedCapacity    string                     `json:"ReturnConsumedCapacity"`
	KeyConditions             map[string]json.RawMessage `json:"KeyConditions"`
	QueryFilter               map[string]json.RawMessage `json:"QueryFilter"`
	AttributesToGet           []string                   `json:"AttributesToGet"`
	ConditionalOperator       string                     `json:"ConditionalOperator"`
}

func (o *Ops) query(ctx context.Context, body []byte) (any, error) {
	var req queryInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := rejectLegacy(map[string]bool{
		"KeyConditions":       req.KeyConditions != nil,
		"QueryFilter":         req.QueryFilter != nil,
		"AttributesToGet":     req.AttributesToGet != nil,
		"ConditionalOperator": req.ConditionalOperator != "",
	}); err != nil {
		return nil, err
	}
	if req.KeyConditionExpression == "" {
		return nil, ddberr.Validation("Either the KeyConditions or KeyConditionExpression parameter must be specified in the request.")
	}
	if req.Limit != nil && *req.Limit < 1 {
		return nil, ddberr.Validation("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value greater than or equal to 1", *req.Limit)
	}
	sel, err := checkSelect(req.Select, req.ProjectionExpression != "", req.IndexName != "")
	if err != nil {
		return nil, err
	}
	t, err := o.table(ctx, req.TableName)
	if err != nil {
		return nil, err
	}
	var idx *storage.Index
	if req.IndexName != "" {
		g, ok := t.GSI(req.IndexName)
		if !ok {
			return nil, ddberr.Validation("The table does not have the specified index: %s", req.IndexName)
		}
		idx = g
		// Consistent reads are allowed on local secondary indexes; they are
		// rejected only on global secondary indexes.
		if req.ConsistentRead && !idx.Local {
			return nil, ddberr.Validation("Consistent reads are not supported on global secondary indexes")
		}
	}
	// ALL_ATTRIBUTES on a local secondary index fetches the base item, so it is
	// allowed regardless of the index projection; GSIs must project ALL for it.
	if sel == selectAll && idx != nil && !idx.Local && idx.Projection != storage.ProjectAll {
		return nil, ddberr.Validation("One or more parameter values were invalid: Select type ALL_ATTRIBUTES is not supported for global secondary index %s because its projection type is not ALL", req.IndexName)
	}
	if sel == selectAllProjected && idx == nil {
		return nil, ddberr.Validation("One or more parameter values were invalid: Select type ALL_PROJECTED_ATTRIBUTES is not supported for the requested table")
	}

	ex, err := parseExpressions(expressionInput{
		KeyConditionExpression: req.KeyConditionExpression,
		FilterExpression:       req.FilterExpression,
		ProjectionExpression:   req.ProjectionExpression,
		Names:                  req.ExpressionAttributeNames,
		Values:                 req.ExpressionAttributeValues,
	})
	if err != nil {
		return nil, err
	}

	schema := t.Key
	if idx != nil {
		schema = idx.Key
	}
	kc, err := ex.keycond.ForSchema(schema.Hash, schema.Range)
	if err != nil {
		return nil, err
	}
	if kc.SKOp == "begins_with" && schema.RangeType != attr.TypeS && schema.RangeType != attr.TypeB {
		return nil, ddberr.Validation("One or more parameter values were invalid: Invalid operator used in KeyConditionExpression: begins_with")
	}
	if err := checkKeyTypes(kc, ex.values, schema); err != nil {
		return nil, err
	}
	if err := checkExclusiveStartKey(t, idx, req.ExclusiveStartKey); err != nil {
		return nil, err
	}
	if err := checkProjected(t, idx, ex.proj); err != nil {
		return nil, err
	}

	forward := true
	if req.ScanIndexForward != nil {
		forward = *req.ScanIndexForward
	}

	in := storage.QueryInput{
		Table:      t,
		Index:      req.IndexName,
		Hash:       ex.values[kc.PKValue],
		Forward:    forward,
		StartAfter: req.ExclusiveStartKey,
		Limit:      storage.PageLimit{Bytes: pageBytes},
	}
	if req.Limit != nil {
		in.Limit.Items = *req.Limit
	}
	if kc.SK != "" {
		vals := make([]attr.Value, len(kc.SKValues))
		for i, k := range kc.SKValues {
			vals[i] = ex.values[k]
		}
		in.Range = &storage.RangeCond{Op: kc.SKOp, Values: vals}
	}

	page, err := o.Store.Query(ctx, in)
	if err != nil {
		return nil, err
	}

	out, err := readResult(page, t, idx, ex, sel)
	if err != nil {
		return nil, err
	}
	if cc := consumedCapacity(t, req.ReturnConsumedCapacity); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// checkKeyTypes checks the key condition values against the key schema's
// attribute types.
func checkKeyTypes(kc expr.KeyCond, values map[string]attr.Value, schema storage.KeySchema) error {
	if v, ok := values[kc.PKValue]; !ok || v.Type != schema.HashType {
		return keyTypeMismatch()
	}
	if kc.SK != "" {
		for _, k := range kc.SKValues {
			if v, ok := values[k]; !ok || v.Type != schema.RangeType {
				return keyTypeMismatch()
			}
		}
	}
	return nil
}

func keyTypeMismatch() error {
	return ddberr.Validation("One or more parameter values were invalid: Condition parameter type does not match schema type")
}

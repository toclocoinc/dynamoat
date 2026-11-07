package ops

import (
	"context"
	"encoding/json"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() { register("Scan", (*Ops).scan) }

type scanInput struct {
	TableName                 string                     `json:"TableName"`
	IndexName                 string                     `json:"IndexName"`
	FilterExpression          string                     `json:"FilterExpression"`
	ProjectionExpression      string                     `json:"ProjectionExpression"`
	ExpressionAttributeNames  map[string]string          `json:"ExpressionAttributeNames"`
	ExpressionAttributeValues map[string]attr.Value      `json:"ExpressionAttributeValues"`
	Limit                     *int                       `json:"Limit"`
	ExclusiveStartKey         attr.Item                  `json:"ExclusiveStartKey"`
	Select                    string                     `json:"Select"`
	Segment                   *int                       `json:"Segment"`
	TotalSegments             *int                       `json:"TotalSegments"`
	ConsistentRead            bool                       `json:"ConsistentRead"`
	ReturnConsumedCapacity    string                     `json:"ReturnConsumedCapacity"`
	ScanFilter                map[string]json.RawMessage `json:"ScanFilter"`
	AttributesToGet           []string                   `json:"AttributesToGet"`
	ConditionalOperator       string                     `json:"ConditionalOperator"`
}

func (o *Ops) scan(ctx context.Context, body []byte) (any, error) {
	var req scanInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := rejectLegacy(map[string]bool{
		"ScanFilter":          req.ScanFilter != nil,
		"AttributesToGet":     req.AttributesToGet != nil,
		"ConditionalOperator": req.ConditionalOperator != "",
	}); err != nil {
		return nil, err
	}
	if req.Limit != nil && *req.Limit < 1 {
		return nil, ddberr.Validation("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value greater than or equal to 1", *req.Limit)
	}
	sel, err := checkSelect(req.Select, req.ProjectionExpression != "", req.IndexName != "")
	if err != nil {
		return nil, err
	}
	seg, segments, err := checkSegments(req.Segment, req.TotalSegments)
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
		FilterExpression:     req.FilterExpression,
		ProjectionExpression: req.ProjectionExpression,
		Names:                req.ExpressionAttributeNames,
		Values:               req.ExpressionAttributeValues,
	})
	if err != nil {
		return nil, err
	}
	if err := checkExclusiveStartKey(t, idx, req.ExclusiveStartKey); err != nil {
		return nil, err
	}
	if err := checkProjected(t, idx, ex.proj); err != nil {
		return nil, err
	}

	in := storage.ScanInput{
		Table:      t,
		Index:      req.IndexName,
		Segment:    seg,
		Segments:   segments,
		StartAfter: req.ExclusiveStartKey,
		Limit:      storage.PageLimit{Bytes: pageBytes},
	}
	if req.Limit != nil {
		in.Limit.Items = *req.Limit
	}

	page, err := o.Store.Scan(ctx, in)
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

// checkSegments validates the parallel Scan parameters. Segment and
// TotalSegments must be present together; TotalSegments is 1..1000000 and
// Segment is 0..TotalSegments-1.
func checkSegments(segment, totalSegments *int) (seg, segments int, err error) {
	if segment == nil && totalSegments == nil {
		return 0, 0, nil
	}
	if totalSegments != nil && segment == nil {
		return 0, 0, ddberr.Validation("The Segment parameter is required but was not present in the request when parameter TotalSegments is present")
	}
	if totalSegments == nil && segment != nil {
		return 0, 0, ddberr.Validation("The TotalSegments parameter is required but was not present in the request when Segment parameter is present")
	}
	t := *totalSegments
	if t < 1 {
		return 0, 0, ddberr.Validation("1 validation error detected: Value '%d' at 'totalSegments' failed to satisfy constraint: Member must have value greater than or equal to 1", t)
	}
	if t > 1000000 {
		return 0, 0, ddberr.Validation("1 validation error detected: Value '%d' at 'totalSegments' failed to satisfy constraint: Member must have value less than or equal to 1000000", t)
	}
	s := *segment
	if s < 0 || s >= t {
		return 0, 0, ddberr.Validation("The Segment parameter is zero-based and must be less than parameter TotalSegments: Segment: %d is not less than TotalSegments: %d", s, t)
	}
	return s, t, nil
}

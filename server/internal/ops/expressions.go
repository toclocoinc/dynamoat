package ops

import (
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/expr"
)

// expressions is the parsed form of the expression parameters one request
// carries. cond, filter and keycond are nil when the matching expression is
// absent; proj is nil when there is no ProjectionExpression.
type expressions struct {
	cond    expr.Cond
	filter  expr.Cond
	keycond expr.KeyCond
	update  expr.Update
	proj    []expr.Path
	values  map[string]attr.Value
}

// expressionInput carries the expression parameters the data plane requests
// share.
type expressionInput struct {
	ConditionExpression    string
	FilterExpression       string
	KeyConditionExpression string
	UpdateExpression       string
	ProjectionExpression   string
	Names                  map[string]string
	Values                 map[string]attr.Value
}

// parseExpressions builds one expr.Env shared by every expression of the
// request, parses whichever expressions the request carries, and reports
// placeholders provided but never used, across the whole request. An empty
// expression string means the request has none.
func parseExpressions(in expressionInput) (*expressions, error) {
	env := &expr.Env{
		Names:  in.Names,
		Values: make(map[string]bool, len(in.Values)),
	}
	for k := range in.Values {
		env.Values[k] = true
	}
	out := &expressions{values: in.Values}
	var err error
	if in.ConditionExpression != "" {
		if out.cond, err = expr.ParseCondition(in.ConditionExpression, env); err != nil {
			return nil, err
		}
	}
	if in.KeyConditionExpression != "" {
		if out.keycond, err = expr.ParseKeyCondition(in.KeyConditionExpression, env); err != nil {
			return nil, err
		}
	}
	if in.UpdateExpression != "" {
		if out.update, err = expr.ParseUpdate(in.UpdateExpression, env); err != nil {
			return nil, err
		}
	}
	if in.FilterExpression != "" {
		if out.filter, err = expr.ParseFilter(in.FilterExpression, env); err != nil {
			return nil, err
		}
	}
	if in.ProjectionExpression != "" {
		if out.proj, err = expr.ParseProjection(in.ProjectionExpression, env); err != nil {
			return nil, err
		}
	}
	if err := env.CheckUnused(); err != nil {
		return nil, err
	}
	return out, nil
}

// legacyParams are the request parameters DynamoDB replaced with expression
// parameters and that dynamoat does not implement.
var legacyParams = []string{"Expected", "AttributesToGet", "ConditionalOperator", "KeyConditions", "QueryFilter", "ScanFilter"}

// rejectLegacy returns a ValidationException naming the first legacy parameter
// that is present, pointing the client at expressions instead.
func rejectLegacy(present map[string]bool) error {
	for _, name := range legacyParams {
		if present[name] {
			return ddberr.Validation("dynamoat does not support the legacy parameter %s; use expressions", name)
		}
	}
	return nil
}

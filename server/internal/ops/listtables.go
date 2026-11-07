package ops

import (
	"context"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

func init() { register("ListTables", (*Ops).listTables) }

type listTablesInput struct {
	ExclusiveStartTableName string `json:"ExclusiveStartTableName"`
	Limit                   *int   `json:"Limit"`
}

func (o *Ops) listTables(ctx context.Context, body []byte) (any, error) {
	var req listTablesInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	limit := 100
	if req.Limit != nil {
		limit = *req.Limit
		if limit < 1 {
			return nil, ddberr.Validation("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value greater than or equal to 1", limit)
		}
		if limit > 100 {
			return nil, ddberr.Validation("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value less than or equal to 100", limit)
		}
	}

	// Read one extra name to learn whether another page follows.
	names, err := o.Store.ListTables(ctx, req.ExclusiveStartTableName, limit+1)
	if err != nil {
		return nil, err
	}
	if len(names) > limit {
		return map[string]any{
			"TableNames":             names[:limit],
			"LastEvaluatedTableName": names[limit-1],
		}, nil
	}
	if names == nil {
		names = []string{}
	}
	return map[string]any{"TableNames": names}, nil
}

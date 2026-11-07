package ops

import (
	"context"
	"errors"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() { register("DescribeTable", (*Ops).describeTable) }

type describeTableInput struct {
	TableName string `json:"TableName"`
}

func (o *Ops) describeTable(ctx context.Context, body []byte) (any, error) {
	var req describeTableInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkTableName(req.TableName, "tableName"); err != nil {
		return nil, err
	}
	t, err := o.Store.GetTable(ctx, req.TableName)
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.ResourceNotFound("Requested resource not found: Table: " + req.TableName + " not found")
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"Table": o.tableDescription(t, "", tableDescFlags{warmThroughput: true})}, nil
}

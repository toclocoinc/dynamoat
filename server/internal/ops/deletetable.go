package ops

import (
	"context"
	"errors"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() { register("DeleteTable", (*Ops).deleteTable) }

type deleteTableInput struct {
	TableName string `json:"TableName"`
}

func (o *Ops) deleteTable(ctx context.Context, body []byte) (any, error) {
	var req deleteTableInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkTableName(req.TableName, "tableName"); err != nil {
		return nil, err
	}
	cur, err := o.Store.GetTable(ctx, req.TableName)
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.ResourceNotFound("Requested resource not found: Table: " + req.TableName + " not found")
	}
	if err != nil {
		return nil, err
	}
	if cur.DeletionProtection {
		return nil, ddberr.Validation("Resource cannot be deleted as it is currently protected against deletion. Disable deletion protection first.")
	}
	t, err := o.Store.DeleteTable(ctx, req.TableName)
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.ResourceNotFound("Requested resource not found: Table: " + req.TableName + " not found")
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"TableDescription": o.tableDescription(t, storage.StatusDeleting, tableDescFlags{})}, nil
}

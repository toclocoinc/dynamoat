package ops

import (
	"context"
	"errors"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() {
	register("UpdateTimeToLive", (*Ops).updateTimeToLive)
	register("DescribeTimeToLive", (*Ops).describeTimeToLive)
}

type updateTimeToLiveInput struct {
	TableName               string            `json:"TableName"`
	TimeToLiveSpecification *ttlSpecification `json:"TimeToLiveSpecification"`
}

type ttlSpecification struct {
	Enabled       bool   `json:"Enabled"`
	AttributeName string `json:"AttributeName"`
}

type describeTimeToLiveInput struct {
	TableName string `json:"TableName"`
}

// ttlSupported reports whether the store can serve TTL operations; the
// capability is declared by implementing storage.TTLStore.
func (o *Ops) ttlSupported() error {
	if _, ok := o.Store.(storage.TTLStore); !ok {
		return ddberr.Validation("dynamoat: this store does not support TTL")
	}
	return nil
}

func (o *Ops) updateTimeToLive(ctx context.Context, body []byte) (any, error) {
	var req updateTimeToLiveInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := o.ttlSupported(); err != nil {
		return nil, err
	}
	if err := checkTableName(req.TableName, "tableName"); err != nil {
		return nil, err
	}
	spec := req.TimeToLiveSpecification
	if spec == nil {
		return nil, ddberr.Validation("1 validation error detected: Value null at 'timeToLiveSpecification' failed to satisfy constraint: Member must not be null")
	}
	if spec.AttributeName == "" {
		return nil, ddberr.Validation("1 validation error detected: Value '%s' at 'timeToLiveSpecification.attributeName' failed to satisfy constraint: Member must have length greater than or equal to 1", spec.AttributeName)
	}

	_, err := o.Store.UpdateTable(ctx, req.TableName, func(t *storage.Table) error {
		switch {
		case spec.Enabled && t.TTLEnabled:
			return ddberr.Validation("TimeToLive is already enabled")
		case !spec.Enabled && !t.TTLEnabled:
			return ddberr.Validation("TimeToLive is already disabled")
		case !spec.Enabled && t.TTLEnabled && t.TTLAttr != spec.AttributeName:
			return ddberr.Validation("TimeToLive is active on a different AttributeName: current AttributeName is %s", t.TTLAttr)
		}
		t.TTLAttr = spec.AttributeName
		t.TTLEnabled = spec.Enabled
		return nil
	})
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.ResourceNotFound("Requested resource not found: Table: " + req.TableName + " not found")
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"TimeToLiveSpecification": map[string]any{
			"Enabled":       spec.Enabled,
			"AttributeName": spec.AttributeName,
		},
	}, nil
}

func (o *Ops) describeTimeToLive(ctx context.Context, body []byte) (any, error) {
	var req describeTimeToLiveInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := o.ttlSupported(); err != nil {
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
	desc := map[string]any{"TimeToLiveStatus": "DISABLED"}
	if t.TTLEnabled {
		desc["TimeToLiveStatus"] = "ENABLED"
		desc["AttributeName"] = t.TTLAttr
	}
	return map[string]any{"TimeToLiveDescription": desc}, nil
}

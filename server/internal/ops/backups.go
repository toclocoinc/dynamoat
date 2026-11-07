package ops

import (
	"context"
	"errors"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() {
	register("DescribeContinuousBackups", (*Ops).describeContinuousBackups)
	register("UpdateContinuousBackups", (*Ops).updateContinuousBackups)
}

// pitrDefaultDays is RecoveryPeriodInDays when the request does not give one.
const pitrDefaultDays = 35

// pointInTimeRecoveryDescription renders PointInTimeRecoveryDescription.
// Continuous backups are always ENABLED (dynamoat's durability comes from the
// managed Postgres server); point-in-time recovery is per table. While the
// store's initial copy is still running the window is not open yet, but
// DynamoDB reports both restorable dates from the moment PITR is enabled
// (recorded: both equal to the enable time), so dynamoat does the same; a
// restore in that gap is still refused by the store.
func (o *Ops) pointInTimeRecoveryDescription(ctx context.Context, t *storage.Table) map[string]any {
	p := map[string]any{"PointInTimeRecoveryStatus": "DISABLED"}
	if !t.PITREnabled {
		return p
	}
	p["PointInTimeRecoveryStatus"] = "ENABLED"
	days := t.PITRDays
	if days <= 0 {
		days = pitrDefaultDays
	}
	p["RecoveryPeriodInDays"] = days
	if pitr, ok := o.Store.(storage.PITRStore); ok {
		earliest, latest, err := pitr.RecoveryWindow(ctx, t)
		switch {
		case err == nil:
			p["EarliestRestorableDateTime"] = epochSeconds(earliest)
			p["LatestRestorableDateTime"] = epochSeconds(latest)
		case errors.Is(err, storage.ErrOutsideRecoveryWindow) && !t.PITRSince.IsZero():
			p["EarliestRestorableDateTime"] = epochSeconds(t.PITRSince)
			p["LatestRestorableDateTime"] = epochSeconds(t.PITRSince)
		}
	}
	return p
}

// continuousBackupsDescription is the response both operations return.
func (o *Ops) continuousBackupsDescription(ctx context.Context, t *storage.Table) map[string]any {
	return map[string]any{
		"ContinuousBackupsDescription": map[string]any{
			"ContinuousBackupsStatus":        "ENABLED",
			"PointInTimeRecoveryDescription": o.pointInTimeRecoveryDescription(ctx, t),
		},
	}
}

type describeContinuousBackupsInput struct {
	TableName string `json:"TableName"`
}

func (o *Ops) describeContinuousBackups(ctx context.Context, body []byte) (any, error) {
	var req describeContinuousBackupsInput
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
	return o.continuousBackupsDescription(ctx, t), nil
}

type updateContinuousBackupsInput struct {
	TableName                        string    `json:"TableName"`
	PointInTimeRecoverySpecification *pitrSpec `json:"PointInTimeRecoverySpecification"`
}

type pitrSpec struct {
	PointInTimeRecoveryEnabled bool `json:"PointInTimeRecoveryEnabled"`
	RecoveryPeriodInDays       *int `json:"RecoveryPeriodInDays"`
}

func (o *Ops) updateContinuousBackups(ctx context.Context, body []byte) (any, error) {
	var req updateContinuousBackupsInput
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

	spec := req.PointInTimeRecoverySpecification
	if spec == nil {
		return o.continuousBackupsDescription(ctx, cur), nil
	}
	if spec.PointInTimeRecoveryEnabled {
		if _, ok := o.Store.(storage.PITRStore); !ok {
			return nil, ddberr.Validation("dynamoat: this store does not support point-in-time recovery")
		}
		if err := validateRecoveryPeriod(spec.RecoveryPeriodInDays); err != nil {
			return nil, err
		}
	}

	updated, err := o.Store.UpdateTable(ctx, req.TableName, func(t *storage.Table) error {
		wasEnabled := t.PITREnabled
		t.PITREnabled = spec.PointInTimeRecoveryEnabled
		if spec.PointInTimeRecoveryEnabled {
			t.PITRDays = recoveryPeriodDays(spec.RecoveryPeriodInDays)
			if !wasEnabled {
				t.PITRSince = o.now().UTC()
			}
		}
		return nil
	})
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.ResourceNotFound("Requested resource not found: Table: " + req.TableName + " not found")
	}
	if err != nil {
		return nil, err
	}
	return o.continuousBackupsDescription(ctx, updated), nil
}

func recoveryPeriodDays(days *int) int {
	if days == nil {
		return pitrDefaultDays
	}
	return *days
}

func validateRecoveryPeriod(days *int) error {
	if days == nil {
		return nil
	}
	switch {
	case *days < 1:
		return ddberr.Validation("1 validation error detected: Value '%d' at 'recoveryPeriodInDays' failed to satisfy constraint: Member must have value greater than or equal to 1", *days)
	case *days > 35:
		return ddberr.Validation("1 validation error detected: Value '%d' at 'recoveryPeriodInDays' failed to satisfy constraint: Member must have value less than or equal to 35", *days)
	}
	return nil
}

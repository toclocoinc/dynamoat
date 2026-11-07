package ops

import (
	"context"
	"errors"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() { register("RestoreTableToPointInTime", (*Ops).restoreTableToPointInTime) }

type restoreToPointInTimeInput struct {
	SourceTableName               string                 `json:"SourceTableName"`
	SourceTableArn                string                 `json:"SourceTableArn"`
	TargetTableName               string                 `json:"TargetTableName"`
	UseLatestRestorableTime       bool                   `json:"UseLatestRestorableTime"`
	RestoreDateTime               *float64               `json:"RestoreDateTime"`
	BillingModeOverride           string                 `json:"BillingModeOverride"`
	ProvisionedThroughputOverride *provisionedThroughput `json:"ProvisionedThroughputOverride"`
	GlobalSecondaryIndexOverride  []createGSI            `json:"GlobalSecondaryIndexOverride"`
	LocalSecondaryIndexOverride   []createGSI            `json:"LocalSecondaryIndexOverride"`
	SSESpecificationOverride      map[string]any         `json:"SSESpecificationOverride"` // accepted and ignored: tables have no SSE
}

func (o *Ops) restoreTableToPointInTime(ctx context.Context, body []byte) (any, error) {
	var req restoreToPointInTimeInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkTableName(req.TargetTableName, "targetTableName"); err != nil {
		return nil, err
	}

	sourceName, err := sourceTableName(req.SourceTableName, req.SourceTableArn)
	if err != nil {
		return nil, err
	}

	source, err := o.Store.GetTable(ctx, sourceName)
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.TableNotFound("Requested resource not found")
	}
	if err != nil {
		return nil, err
	}

	pitr, ok := o.Store.(storage.PITRStore)
	if !ok {
		return nil, ddberr.Validation("dynamoat: this store does not support point-in-time recovery")
	}

	earliest, latest, err := pitr.RecoveryWindow(ctx, source)
	if errors.Is(err, storage.ErrOutsideRecoveryWindow) {
		return nil, ddberr.PointInTimeRecoveryUnavailable("Point in time recovery has not yet been enabled for this table.")
	}
	if err != nil {
		return nil, err
	}

	at, err := restoreTime(req, latest)
	if err != nil {
		return nil, err
	}
	// An explicit RestoreDateTime must fall in the window; the latest
	// restorable time is by construction the window's end, and the store checks
	// it again anyway.
	if !req.UseLatestRestorableTime && (at.Before(earliest) || at.After(latest)) {
		return nil, ddberr.InvalidRestoreTime("The restore time is outside of the recovery window.")
	}

	target, err := o.restoreTarget(source, restoreTableFromBackupInput{
		TargetTableName:               req.TargetTableName,
		BillingModeOverride:           req.BillingModeOverride,
		GlobalSecondaryIndexOverride:  req.GlobalSecondaryIndexOverride,
		LocalSecondaryIndexOverride:   req.LocalSecondaryIndexOverride,
		ProvisionedThroughputOverride: req.ProvisionedThroughputOverride,
	})
	if err != nil {
		return nil, err
	}
	target.Restore = &storage.RestoreSummary{SourceTable: source.Name, At: at}

	restored, err := pitr.RestoreToTime(ctx, source, at, target)
	if errors.Is(err, storage.ErrTableExists) {
		return nil, ddberr.TableAlreadyExists("Table already exists: " + req.TargetTableName)
	}
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.TableNotFound("Requested resource not found")
	}
	if errors.Is(err, storage.ErrOutsideRecoveryWindow) {
		return nil, ddberr.InvalidRestoreTime("The restore time is outside of the recovery window.")
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"TableDescription": o.tableDescription(restored, storage.StatusCreating, restoreTableDescFlags)}, nil
}

// sourceTableName resolves the source table name, which DynamoDB requires as
// exactly one of SourceTableName or SourceTableArn.
func sourceTableName(name, arn string) (string, error) {
	switch {
	case name != "" && arn != "":
		return "", ddberr.Validation("Only one of SourceTableName or SourceTableArn can be specified")
	case name != "":
		if err := checkTableName(name, "sourceTableName"); err != nil {
			return "", err
		}
		return name, nil
	case arn != "":
		return tableNameFromArn(arn)
	default:
		return "", ddberr.Validation("You must specify either SourceTableName or SourceTableArn")
	}
}

// restoreTime resolves the restore point: the latest restorable time when
// UseLatestRestorableTime is set, otherwise RestoreDateTime, of which DynamoDB
// requires exactly one.
func restoreTime(req restoreToPointInTimeInput, latest time.Time) (time.Time, error) {
	if req.UseLatestRestorableTime {
		if req.RestoreDateTime != nil {
			return time.Time{}, ddberr.Validation("Only one of UseLatestRestorableTime or RestoreDateTime can be specified")
		}
		return latest, nil
	}
	if req.RestoreDateTime == nil {
		return time.Time{}, ddberr.Validation("You must specify either UseLatestRestorableTime or RestoreDateTime")
	}
	return epochToTime(*req.RestoreDateTime), nil
}

// epochToTime converts DynamoDB's epoch-seconds timestamp (a number with a
// fractional part) to a time.
func epochToTime(sec float64) time.Time {
	return time.UnixMilli(int64(sec * 1000)).UTC()
}

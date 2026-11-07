package ops

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

const (
	typTableNotFound  = "com.amazonaws.dynamodb.v20120810#TableNotFoundException"
	typPITRUnavail    = "com.amazonaws.dynamodb.v20120810#PointInTimeRecoveryUnavailableException"
	typInvalidRestore = "com.amazonaws.dynamodb.v20120810#InvalidRestoreTimeException"
)

// enablePITR turns point-in-time recovery on through the handler.
func enablePITR(t *testing.T, h http.Handler, table string) {
	t.Helper()
	rec := post(t, h, "UpdateContinuousBackups",
		`{"TableName":"`+table+`","PointInTimeRecoverySpecification":{"PointInTimeRecoveryEnabled":true}}`)
	if rec.Code != 200 {
		t.Fatalf("enable PITR: status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRestoreMissingSource(t *testing.T) {
	_, h, _ := newOps(t)
	rec := post(t, h, "RestoreTableToPointInTime",
		`{"SourceTableName":"nope","TargetTableName":"dst","UseLatestRestorableTime":true}`)
	wantErr(t, rec, typTableNotFound, "Requested resource not found")
}

func TestRestorePITRDisabled(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "src", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	rec := post(t, h, "RestoreTableToPointInTime",
		`{"SourceTableName":"src","TargetTableName":"dst","UseLatestRestorableTime":true}`)
	wantErr(t, rec, typPITRUnavail, "Point in time recovery has not yet been enabled for this table.")
}

func TestRestoreTargetExists(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "src", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	makeTable(t, store, "dst", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	enablePITR(t, h, "src")
	time.Sleep(6 * time.Second)

	rec := post(t, h, "RestoreTableToPointInTime",
		`{"SourceTableName":"src","TargetTableName":"dst","UseLatestRestorableTime":true}`)
	wantErr(t, rec, typTableExists, "Table already exists: dst")
}

func TestRestoreInvalidTime(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "src", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	enablePITR(t, h, "src")

	rec := post(t, h, "RestoreTableToPointInTime",
		`{"SourceTableName":"src","TargetTableName":"dst","RestoreDateTime":0}`)
	wantErr(t, rec, typInvalidRestore, "The restore time is outside of the recovery window.")
}

func TestRestoreTimeArgumentValidation(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "src", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	enablePITR(t, h, "src")

	rec := post(t, h, "RestoreTableToPointInTime", `{"SourceTableName":"src","TargetTableName":"dst"}`)
	wantErr(t, rec, typValidation, "You must specify either UseLatestRestorableTime or RestoreDateTime")

	rec = post(t, h, "RestoreTableToPointInTime",
		`{"SourceTableName":"src","TargetTableName":"dst","UseLatestRestorableTime":true,"RestoreDateTime":1234567890}`)
	wantErr(t, rec, typValidation, "Only one of UseLatestRestorableTime or RestoreDateTime can be specified")
}

func TestRestoreSourceNameAndArn(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "src", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	enablePITR(t, h, "src")

	rec := post(t, h, "RestoreTableToPointInTime",
		`{"SourceTableName":"src","SourceTableArn":"arn:aws:dynamodb:us-east-1:000000000000:table/src","TargetTableName":"dst","UseLatestRestorableTime":true}`)
	wantErr(t, rec, typValidation, "Only one of SourceTableName or SourceTableArn can be specified")

	rec = post(t, h, "RestoreTableToPointInTime", `{"TargetTableName":"dst","UseLatestRestorableTime":true}`)
	wantErr(t, rec, typValidation, "You must specify either SourceTableName or SourceTableArn")
}

func TestRestoreByArn(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "src", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	enablePITR(t, h, "src")

	wantStatus(t, post(t, h, "PutItem", `{"TableName":"src","Item":{"id":{"S":"r1"},"v":{"S":"x"}}}`), 200)
	time.Sleep(6 * time.Second)

	rec := post(t, h, "RestoreTableToPointInTime",
		`{"SourceTableArn":"arn:aws:dynamodb:us-east-1:000000000000:table/src","TargetTableName":"dst","UseLatestRestorableTime":true}`)
	wantStatus(t, rec, 200)
}

func TestRestoreTableToPointInTimeEndToEnd(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "src", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	enablePITR(t, h, "src")

	// Write the item, then change it after the window has caught up.
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"src","Item":{"id":{"S":"r1"},"v":{"S":"before"}}}`), 200)
	time.Sleep(6 * time.Second)
	restoreAt := time.Now().Add(500 * time.Millisecond)
	time.Sleep(time.Second)
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"src","Item":{"id":{"S":"r1"},"v":{"S":"after"}}}`), 200)
	time.Sleep(6 * time.Second)

	body := `{"SourceTableName":"src","TargetTableName":"dst","RestoreDateTime":` +
		fmt.Sprintf("%f", float64(restoreAt.UnixMilli())/1000) + `}`
	rec := post(t, h, "RestoreTableToPointInTime", body)
	m := wantStatus(t, rec, 200)
	desc := m["TableDescription"].(map[string]any)
	if desc["TableStatus"] != "CREATING" {
		t.Errorf("TableStatus = %v, want CREATING", desc["TableStatus"])
	}
	summary := desc["RestoreSummary"].(map[string]any)
	if summary["SourceTableArn"] != "arn:aws:dynamodb:us-east-1:000000000000:table/src" {
		t.Errorf("RestoreSummary.SourceTableArn = %v", summary["SourceTableArn"])
	}
	if summary["RestoreInProgress"] != true {
		t.Errorf("RestoreSummary.RestoreInProgress = %v, want true", summary["RestoreInProgress"])
	}
	if _, ok := summary["SourceBackupArn"]; ok {
		t.Errorf("PITR RestoreSummary has SourceBackupArn, want none: %#v", summary)
	}
	if _, ok := desc["DeletionProtectionEnabled"]; ok {
		t.Errorf("PITR restore has DeletionProtectionEnabled, want none: %#v", desc)
	}

	// The restored table holds the item as it was before the change.
	rec = post(t, h, "GetItem", `{"TableName":"dst","Key":{"id":{"S":"r1"}}}`)
	m = wantStatus(t, rec, 200)
	equalJSON(t, m["Item"], map[string]any{
		"id": map[string]any{"S": "r1"},
		"v":  map[string]any{"S": "before"},
	}, "restored item")

	// DescribeTable carries the RestoreSummary, no longer in progress.
	rec = post(t, h, "DescribeTable", `{"TableName":"dst"}`)
	m = wantStatus(t, rec, 200)
	table := m["Table"].(map[string]any)
	summary = table["RestoreSummary"].(map[string]any)
	if summary["RestoreInProgress"] != false {
		t.Errorf("DescribeTable RestoreSummary.RestoreInProgress = %v, want false", summary["RestoreInProgress"])
	}
	if _, ok := summary["RestoreDateTime"]; !ok {
		t.Errorf("DescribeTable RestoreSummary missing RestoreDateTime: %v", summary)
	}
}

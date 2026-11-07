package ops

import (
	"context"
	"testing"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/api"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
)

func TestDescribeContinuousBackupsDisabled(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "tags", storage.KeySchema{Hash: "id"})

	rec := post(t, h, "DescribeContinuousBackups", `{"TableName":"tags"}`)
	m := wantStatus(t, rec, 200)
	equalJSON(t, m, map[string]any{
		"ContinuousBackupsDescription": map[string]any{
			"ContinuousBackupsStatus": "ENABLED",
			"PointInTimeRecoveryDescription": map[string]any{
				"PointInTimeRecoveryStatus": "DISABLED",
			},
		},
	}, "DescribeContinuousBackups disabled")
}

func TestUpdateContinuousBackupsEnablesPITR(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "tags", storage.KeySchema{Hash: "id"})

	rec := post(t, h, "UpdateContinuousBackups",
		`{"TableName":"tags","PointInTimeRecoverySpecification":{"PointInTimeRecoveryEnabled":true}}`)
	m := wantStatus(t, rec, 200)
	pitr := m["ContinuousBackupsDescription"].(map[string]any)["PointInTimeRecoveryDescription"].(map[string]any)
	if pitr["PointInTimeRecoveryStatus"] != "ENABLED" {
		t.Errorf("PointInTimeRecoveryStatus = %v, want ENABLED", pitr["PointInTimeRecoveryStatus"])
	}
	if pitr["RecoveryPeriodInDays"] != float64(35) {
		t.Errorf("RecoveryPeriodInDays = %v, want 35", pitr["RecoveryPeriodInDays"])
	}
	if _, ok := pitr["EarliestRestorableDateTime"]; !ok {
		t.Errorf("missing EarliestRestorableDateTime in %v", pitr)
	}
	if _, ok := pitr["LatestRestorableDateTime"]; !ok {
		t.Errorf("missing LatestRestorableDateTime in %v", pitr)
	}

	got, err := store.GetTable(context.Background(), "tags")
	if err != nil {
		t.Fatal(err)
	}
	if !got.PITREnabled {
		t.Error("PITREnabled = false, want true")
	}
	if got.PITRDays != 35 {
		t.Errorf("PITRDays = %d, want 35", got.PITRDays)
	}
	if got.PITRSince.IsZero() {
		t.Error("PITRSince was not set")
	}
}

func TestUpdateContinuousBackupsRecoveryPeriod(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "tags", storage.KeySchema{Hash: "id"})

	rec := post(t, h, "UpdateContinuousBackups",
		`{"TableName":"tags","PointInTimeRecoverySpecification":{"PointInTimeRecoveryEnabled":true,"RecoveryPeriodInDays":7}}`)
	m := wantStatus(t, rec, 200)
	pitr := m["ContinuousBackupsDescription"].(map[string]any)["PointInTimeRecoveryDescription"].(map[string]any)
	if pitr["RecoveryPeriodInDays"] != float64(7) {
		t.Errorf("RecoveryPeriodInDays = %v, want 7", pitr["RecoveryPeriodInDays"])
	}

	got, err := store.GetTable(context.Background(), "tags")
	if err != nil {
		t.Fatal(err)
	}
	if got.PITRDays != 7 {
		t.Errorf("PITRDays = %d, want 7", got.PITRDays)
	}
}

func TestUpdateContinuousBackupsRecoveryPeriodValidation(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "tags", storage.KeySchema{Hash: "id"})

	rec := post(t, h, "UpdateContinuousBackups",
		`{"TableName":"tags","PointInTimeRecoverySpecification":{"PointInTimeRecoveryEnabled":true,"RecoveryPeriodInDays":0}}`)
	wantErr(t, rec, typValidation,
		"1 validation error detected: Value '0' at 'recoveryPeriodInDays' failed to satisfy constraint: Member must have value greater than or equal to 1")

	rec = post(t, h, "UpdateContinuousBackups",
		`{"TableName":"tags","PointInTimeRecoverySpecification":{"PointInTimeRecoveryEnabled":true,"RecoveryPeriodInDays":36}}`)
	wantErr(t, rec, typValidation,
		"1 validation error detected: Value '36' at 'recoveryPeriodInDays' failed to satisfy constraint: Member must have value less than or equal to 35")
}

func TestUpdateContinuousBackupsDisablesPITR(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "tags", storage.KeySchema{Hash: "id"})

	wantStatus(t, post(t, h, "UpdateContinuousBackups",
		`{"TableName":"tags","PointInTimeRecoverySpecification":{"PointInTimeRecoveryEnabled":true}}`), 200)

	rec := post(t, h, "UpdateContinuousBackups",
		`{"TableName":"tags","PointInTimeRecoverySpecification":{"PointInTimeRecoveryEnabled":false}}`)
	m := wantStatus(t, rec, 200)
	equalJSON(t, m, map[string]any{
		"ContinuousBackupsDescription": map[string]any{
			"ContinuousBackupsStatus": "ENABLED",
			"PointInTimeRecoveryDescription": map[string]any{
				"PointInTimeRecoveryStatus": "DISABLED",
			},
		},
	}, "UpdateContinuousBackups disabled")
}

// noPITRStore wraps a Store and hides PITRStore, as a backend without
// point-in-time recovery.
type noPITRStore struct{ storage.Store }

func TestUpdateContinuousBackupsUnsupportedStore(t *testing.T) {
	store := memstore.New()
	if _, err := store.CreateTable(context.Background(), &storage.Table{Name: "tags", Key: storage.KeySchema{Hash: "id"}}); err != nil {
		t.Fatal(err)
	}
	h := api.New(api.Options{})
	o := &Ops{Store: noPITRStore{store}}
	o.Register(h)

	rec := post(t, h, "UpdateContinuousBackups",
		`{"TableName":"tags","PointInTimeRecoverySpecification":{"PointInTimeRecoveryEnabled":true}}`)
	wantErr(t, rec, typValidation, "dynamoat: this store does not support point-in-time recovery")
}

func TestContinuousBackupsMissingTable(t *testing.T) {
	_, h, _ := newOps(t)

	rec := post(t, h, "DescribeContinuousBackups", `{"TableName":"nope"}`)
	wantErr(t, rec, typResourceMissing, "Requested resource not found: Table: nope not found")

	rec = post(t, h, "UpdateContinuousBackups",
		`{"TableName":"nope","PointInTimeRecoverySpecification":{"PointInTimeRecoveryEnabled":false}}`)
	wantErr(t, rec, typResourceMissing, "Requested resource not found: Table: nope not found")

	// A missing table is ResourceNotFound even when enabling PITR.
	rec = post(t, h, "UpdateContinuousBackups",
		`{"TableName":"nope","PointInTimeRecoverySpecification":{"PointInTimeRecoveryEnabled":true}}`)
	wantErr(t, rec, typResourceMissing, "Requested resource not found: Table: nope not found")
}

// copyingPITRStore is a PITR store whose initial copy has not finished, as the
// Postgres store is just after PITR is enabled.
type copyingPITRStore struct{ *memstore.Store }

func (copyingPITRStore) RecoveryWindow(context.Context, *storage.Table) (time.Time, time.Time, error) {
	return time.Time{}, time.Time{}, storage.ErrOutsideRecoveryWindow
}

func TestDescribeContinuousBackupsWhileCopyRuns(t *testing.T) {
	store := memstore.New()
	since := time.Date(2026, 11, 7, 10, 5, 54, 0, time.UTC)
	if _, err := store.CreateTable(context.Background(), &storage.Table{
		Name: "pitr", Key: storage.KeySchema{Hash: "id"}, PITREnabled: true, PITRDays: 35, PITRSince: since,
	}); err != nil {
		t.Fatal(err)
	}
	h := api.New(api.Options{})
	o := &Ops{Store: copyingPITRStore{store}}
	o.Register(h)

	m := wantStatus(t, post(t, h, "DescribeContinuousBackups", `{"TableName":"pitr"}`), 200)
	p := m["ContinuousBackupsDescription"].(map[string]any)["PointInTimeRecoveryDescription"].(map[string]any)
	want := float64(since.Unix())
	if p["EarliestRestorableDateTime"] != want || p["LatestRestorableDateTime"] != want {
		t.Fatalf("restorable dates = %v / %v, want both %v (the enable time)", p["EarliestRestorableDateTime"], p["LatestRestorableDateTime"], want)
	}
}

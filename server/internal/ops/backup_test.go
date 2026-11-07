package ops

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/api"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
)

const (
	typBackupNotFound = "com.amazonaws.dynamodb.v20120810#BackupNotFoundException"
	typTableExists    = "com.amazonaws.dynamodb.v20120810#TableAlreadyExistsException"
	typResourceInUse  = "com.amazonaws.dynamodb.v20120810#ResourceInUseException"
)

// storeNoBackup hides the BackupStore capability of an underlying store.
type storeNoBackup struct{ storage.Store }

func newBackupOps(t *testing.T) (*Ops, http.Handler, *memstore.Store) {
	t.Helper()
	o, h, store := newOps(t)
	makeTable(t, store, "tbl", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	return o, h, store
}

func createBackup(t *testing.T, h http.Handler, table, name string) string {
	t.Helper()
	m := wantStatus(t, post(t, h, "CreateBackup", `{"TableName":"`+table+`","BackupName":"`+name+`"}`), 200)
	bd := m["BackupDetails"].(map[string]any)
	arn, _ := bd["BackupArn"].(string)
	if arn == "" {
		t.Fatalf("CreateBackup returned no BackupArn: %#v", m)
	}
	return arn
}

func TestCreateBackup(t *testing.T) {
	_, h, _ := newBackupOps(t)
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"tbl","Item":{"id":{"S":"a"},"val":{"S":"1"}}}`), 200)

	m := wantStatus(t, post(t, h, "CreateBackup", `{"TableName":"tbl","BackupName":"bk1"}`), 200)
	bd := m["BackupDetails"].(map[string]any)
	arn := bd["BackupArn"].(string)
	if !strings.HasPrefix(arn, "arn:aws:dynamodb:us-east-1:000000000000:table/tbl/backup/") {
		t.Errorf("BackupArn = %q, want the backup ARN prefix", arn)
	}
	if bd["BackupName"] != "bk1" {
		t.Errorf("BackupName = %v, want bk1", bd["BackupName"])
	}
	if bd["BackupStatus"] != "CREATING" {
		t.Errorf("BackupStatus = %v, want CREATING", bd["BackupStatus"])
	}
	if bd["BackupType"] != "USER" {
		t.Errorf("BackupType = %v, want USER", bd["BackupType"])
	}
	if bd["BackupSizeBytes"] != float64(0) {
		t.Errorf("BackupSizeBytes = %v, want 0", bd["BackupSizeBytes"])
	}
	if _, ok := bd["BackupCreationDateTime"].(float64); !ok {
		t.Errorf("BackupCreationDateTime = %v, want a number", bd["BackupCreationDateTime"])
	}

	// DescribeBackup reports the real status and size once the copy is done.
	d := wantStatus(t, post(t, h, "DescribeBackup", `{"BackupArn":"`+arn+`"}`), 200)
	details := d["BackupDescription"].(map[string]any)["BackupDetails"].(map[string]any)
	if details["BackupStatus"] != "AVAILABLE" {
		t.Errorf("DescribeBackup BackupStatus = %v, want AVAILABLE", details["BackupStatus"])
	}
	if details["BackupSizeBytes"] == float64(0) {
		t.Errorf("DescribeBackup BackupSizeBytes = %v, want a nonzero size", details["BackupSizeBytes"])
	}
}

func TestCreateBackupMissingTable(t *testing.T) {
	_, h, _ := newOps(t)
	rec := post(t, h, "CreateBackup", `{"TableName":"nope","BackupName":"bk1"}`)
	wantErr(t, rec, typResourceMissing, "Requested resource not found: Table: nope not found")
}

func TestCreateBackupNonActiveTable(t *testing.T) {
	_, h, store := newBackupOps(t)
	if _, err := store.UpdateTable(context.Background(), "tbl", func(t *storage.Table) error {
		t.Status = storage.StatusDeleting
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := post(t, h, "CreateBackup", `{"TableName":"tbl","BackupName":"bk1"}`)
	wantErr(t, rec, typResourceInUse, "Backups are not available for the table while it is in the DELETING state")
}

func TestCreateBackupBadName(t *testing.T) {
	_, h, _ := newBackupOps(t)
	rec := post(t, h, "CreateBackup", `{"TableName":"tbl","BackupName":"x"}`)
	wantErr(t, rec, typValidation, "1 validation error detected: Value 'x' at 'backupName' failed to satisfy constraint: Member must have length greater than or equal to 3")
	rec = post(t, h, "CreateBackup", `{"TableName":"tbl","BackupName":"bad name"}`)
	wantErr(t, rec, typValidation, "1 validation error detected: Value 'bad name' at 'backupName' failed to satisfy constraint: Member must satisfy regular expression pattern: [a-zA-Z0-9_.-]+")
}

func TestDescribeBackup(t *testing.T) {
	_, h, _ := newBackupOps(t)
	arn := createBackup(t, h, "tbl", "bk1")

	m := wantStatus(t, post(t, h, "DescribeBackup", `{"BackupArn":"`+arn+`"}`), 200)
	bd := m["BackupDescription"].(map[string]any)
	details := bd["BackupDetails"].(map[string]any)
	if details["BackupArn"] != arn {
		t.Errorf("BackupDetails.BackupArn = %v, want %s", details["BackupArn"], arn)
	}
	src := bd["SourceTableDetails"].(map[string]any)
	if src["TableName"] != "tbl" {
		t.Errorf("SourceTableDetails.TableName = %v, want t", src["TableName"])
	}
	if src["BillingMode"] != "PROVISIONED" {
		t.Errorf("SourceTableDetails.BillingMode = %v, want PROVISIONED", src["BillingMode"])
	}
	if _, ok := bd["SourceTableFeatureDetails"]; !ok {
		t.Errorf("SourceTableFeatureDetails missing from %#v", bd)
	}
}

func TestDescribeBackupUnknown(t *testing.T) {
	_, h, _ := newBackupOps(t)
	arn := "arn:aws:dynamodb:us-east-1:000000000000:table/tbl/backup/nope"
	rec := post(t, h, "DescribeBackup", `{"BackupArn":"`+arn+`"}`)
	wantErr(t, rec, typBackupNotFound, "Backup not found: "+arn)
}

func TestListBackups(t *testing.T) {
	_, h, store := newBackupOps(t)
	makeTable(t, store, "other", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	arnT := createBackup(t, h, "tbl", "bk1")
	createBackup(t, h, "other", "bk2")

	// Unfiltered: both backups, oldest first.
	m := wantStatus(t, post(t, h, "ListBackups", `{}`), 200)
	sums := m["BackupSummaries"].([]any)
	if len(sums) != 2 {
		t.Fatalf("ListBackups returned %d summaries, want 2: %#v", len(sums), m)
	}

	// TableName filter.
	m = wantStatus(t, post(t, h, "ListBackups", `{"TableName":"tbl"}`), 200)
	sums = m["BackupSummaries"].([]any)
	if len(sums) != 1 {
		t.Fatalf("ListBackups(t) returned %d summaries, want 1", len(sums))
	}
	first := sums[0].(map[string]any)
	if first["BackupArn"] != arnT {
		t.Errorf("BackupArn = %v, want %s", first["BackupArn"], arnT)
	}
	if first["TableName"] != "tbl" {
		t.Errorf("TableName = %v, want t", first["TableName"])
	}
	if first["BackupType"] != "USER" {
		t.Errorf("BackupType = %v, want USER", first["BackupType"])
	}

	// A kind dynamoat never creates returns nothing.
	m = wantStatus(t, post(t, h, "ListBackups", `{"BackupType":"SYSTEM"}`), 200)
	if sums, _ := m["BackupSummaries"].([]any); len(sums) != 0 {
		t.Errorf("ListBackups(SYSTEM) = %#v, want empty", m)
	}
}

func TestListBackupsPaging(t *testing.T) {
	_, h, _ := newBackupOps(t)
	a := createBackup(t, h, "tbl", "bk1")
	b := createBackup(t, h, "tbl", "bk2")
	c := createBackup(t, h, "tbl", "bk3")

	m := wantStatus(t, post(t, h, "ListBackups", `{"Limit":2}`), 200)
	sums := m["BackupSummaries"].([]any)
	if len(sums) != 2 {
		t.Fatalf("page size = %d, want 2", len(sums))
	}
	last := m["LastEvaluatedBackupArn"].(string)
	if last == "" {
		t.Fatal("missing LastEvaluatedBackupArn")
	}
	// last is the ARN of the second returned backup.
	page2 := wantStatus(t, post(t, h, "ListBackups", `{"Limit":2,"ExclusiveStartBackupArn":"`+last+`"}`), 200)
	sums2 := page2["BackupSummaries"].([]any)
	if len(sums2) != 1 {
		t.Fatalf("second page size = %d, want 1", len(sums2))
	}
	if _, hasLast := page2["LastEvaluatedBackupArn"]; hasLast {
		t.Errorf("second page has LastEvaluatedBackupArn, want none: %#v", page2)
	}

	// Collect every ARN across pages and check they are the three we made.
	seen := map[string]bool{}
	for _, s := range []string{a, b, c} {
		seen[s] = false
	}
	all := append([]any{}, sums...)
	all = append(all, sums2...)
	for _, s := range all {
		arn := s.(map[string]any)["BackupArn"].(string)
		if _, ok := seen[arn]; !ok {
			t.Errorf("unexpected backup ARN %s", arn)
		}
		seen[arn] = true
	}
	for arn, ok := range seen {
		if !ok {
			t.Errorf("backup %s missing from paged results", arn)
		}
	}
}

func TestListBackupsTimeRangeAndLimit(t *testing.T) {
	_, h, _ := newBackupOps(t)
	createBackup(t, h, "tbl", "bk1")

	// An upper bound before the backup's creation excludes it.
	m := wantStatus(t, post(t, h, "ListBackups", `{"TimeRangeUpperBound":0}`), 200)
	if sums, _ := m["BackupSummaries"].([]any); len(sums) != 0 {
		t.Errorf("ListBackups(range before) = %#v, want empty", m)
	}

	rec := post(t, h, "ListBackups", `{"Limit":0}`)
	wantErr(t, rec, typValidation, "1 validation error detected: Value '0' at 'limit' failed to satisfy constraint: Member must have value greater than or equal to 1")
	rec = post(t, h, "ListBackups", `{"Limit":101}`)
	wantErr(t, rec, typValidation, "1 validation error detected: Value '101' at 'limit' failed to satisfy constraint: Member must have value less than or equal to 100")
	rec = post(t, h, "ListBackups", `{"BackupType":"BOGUS"}`)
	wantErr(t, rec, typValidation, "1 validation error detected: Value 'BOGUS' at 'backupType' failed to satisfy constraint: Member must satisfy enum value set: [USER, SYSTEM, AWS_BACKUP, ALL]")
}

func TestDeleteBackup(t *testing.T) {
	_, h, _ := newBackupOps(t)
	arn := createBackup(t, h, "tbl", "bk1")

	m := wantStatus(t, post(t, h, "DeleteBackup", `{"BackupArn":"`+arn+`"}`), 200)
	details := m["BackupDescription"].(map[string]any)["BackupDetails"].(map[string]any)
	if details["BackupStatus"] != "DELETED" {
		t.Errorf("BackupStatus = %v, want DELETED", details["BackupStatus"])
	}
	if details["BackupArn"] != arn {
		t.Errorf("BackupArn = %v, want %s", details["BackupArn"], arn)
	}

	// Once deleted it is unknown.
	rec := post(t, h, "DescribeBackup", `{"BackupArn":"`+arn+`"}`)
	wantErr(t, rec, typBackupNotFound, "Backup not found: "+arn)
}

func TestDeleteBackupUnknown(t *testing.T) {
	_, h, _ := newBackupOps(t)
	arn := "arn:aws:dynamodb:us-east-1:000000000000:table/tbl/backup/nope"
	rec := post(t, h, "DeleteBackup", `{"BackupArn":"`+arn+`"}`)
	wantErr(t, rec, typBackupNotFound, "Backup not found: "+arn)
}

func TestRestoreTableFromBackup(t *testing.T) {
	_, h, _ := newBackupOps(t)
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"tbl","Item":{"id":{"S":"a"},"val":{"S":"1"}}}`), 200)
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"tbl","Item":{"id":{"S":"b"},"val":{"S":"2"}}}`), 200)
	arn := createBackup(t, h, "tbl", "bk1")

	m := wantStatus(t, post(t, h, "RestoreTableFromBackup", `{"TargetTableName":"dst","BackupArn":"`+arn+`"}`), 200)
	td := m["TableDescription"].(map[string]any)
	if td["TableStatus"] != "CREATING" {
		t.Errorf("TableStatus = %v, want CREATING", td["TableStatus"])
	}
	rs := td["RestoreSummary"].(map[string]any)
	if rs["SourceBackupArn"] != arn {
		t.Errorf("RestoreSummary.SourceBackupArn = %v, want %s", rs["SourceBackupArn"], arn)
	}
	if rs["RestoreInProgress"] != true {
		t.Errorf("RestoreSummary.RestoreInProgress = %v, want true", rs["RestoreInProgress"])
	}
	if _, ok := rs["RestoreDateTime"].(float64); !ok {
		t.Errorf("RestoreSummary.RestoreDateTime = %v, want a number", rs["RestoreDateTime"])
	}

	// The memstore restores synchronously, so DescribeTable reports ACTIVE and
	// the restore is no longer in progress.
	desc := wantStatus(t, post(t, h, "DescribeTable", `{"TableName":"dst"}`), 200)
	dst := desc["Table"].(map[string]any)
	if dst["TableStatus"] != "ACTIVE" {
		t.Errorf("restored TableStatus = %v, want ACTIVE", dst["TableStatus"])
	}
	rs = dst["RestoreSummary"].(map[string]any)
	if rs["RestoreInProgress"] != false {
		t.Errorf("RestoreSummary.RestoreInProgress = %v, want false", rs["RestoreInProgress"])
	}
	if rs["SourceTableArn"] != "arn:aws:dynamodb:us-east-1:000000000000:table/tbl" {
		t.Errorf("RestoreSummary.SourceTableArn = %v", rs["SourceTableArn"])
	}

	// The items were copied.
	g := wantStatus(t, post(t, h, "GetItem", `{"TableName":"dst","Key":{"id":{"S":"a"}},"ConsistentRead":true}`), 200)
	equalJSON(t, g["Item"], map[string]any{
		"id":  map[string]any{"S": "a"},
		"val": map[string]any{"S": "1"},
	}, "restored item a")
}

func TestRestoreTableDescriptionShape(t *testing.T) {
	_, h, store := newOps(t)
	if _, err := store.CreateTable(context.Background(), &storage.Table{
		Name:     "src",
		Key:      storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs: map[string]attr.Type{"id": attr.TypeS},
		Billing:  "PAY_PER_REQUEST",
	}); err != nil {
		t.Fatal(err)
	}
	arn := createBackup(t, h, "src", "bk1")

	m := wantStatus(t, post(t, h, "RestoreTableFromBackup", `{"TargetTableName":"dst","BackupArn":"`+arn+`"}`), 200)
	td := m["TableDescription"].(map[string]any)
	if td["TableStatus"] != "CREATING" {
		t.Errorf("TableStatus = %v, want CREATING", td["TableStatus"])
	}
	if _, ok := td["DeletionProtectionEnabled"]; ok {
		t.Errorf("restore has DeletionProtectionEnabled, want none: %#v", td)
	}
	pt := td["ProvisionedThroughput"].(map[string]any)
	if _, ok := pt["NumberOfDecreasesToday"]; ok {
		t.Errorf("restore ProvisionedThroughput has NumberOfDecreasesToday, want none: %#v", pt)
	}
	if pt["ReadCapacityUnits"] != float64(0) || pt["WriteCapacityUnits"] != float64(0) {
		t.Errorf("ProvisionedThroughput = %#v, want 0/0", pt)
	}
	bs := td["BillingModeSummary"].(map[string]any)
	if bs["BillingMode"] != "PAY_PER_REQUEST" {
		t.Errorf("BillingModeSummary.BillingMode = %v, want PAY_PER_REQUEST", bs["BillingMode"])
	}
	if _, ok := bs["LastUpdateToPayPerRequestDateTime"]; ok {
		t.Errorf("BillingModeSummary has LastUpdateToPayPerRequestDateTime, want none: %#v", bs)
	}
	rs := td["RestoreSummary"].(map[string]any)
	if rs["SourceBackupArn"] != arn {
		t.Errorf("RestoreSummary.SourceBackupArn = %v, want %s", rs["SourceBackupArn"], arn)
	}
	if rs["RestoreInProgress"] != true {
		t.Errorf("RestoreSummary.RestoreInProgress = %v, want true", rs["RestoreInProgress"])
	}
	if _, ok := rs["RestoreDateTime"].(float64); !ok {
		t.Errorf("RestoreSummary.RestoreDateTime = %v, want a number", rs["RestoreDateTime"])
	}
	if rs["SourceTableArn"] != "arn:aws:dynamodb:us-east-1:000000000000:table/src" {
		t.Errorf("RestoreSummary.SourceTableArn = %v", rs["SourceTableArn"])
	}
}

func TestRestoreTableFromBackupExistingTarget(t *testing.T) {
	_, h, store := newBackupOps(t)
	makeTable(t, store, "dst", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	arn := createBackup(t, h, "tbl", "bk1")

	rec := post(t, h, "RestoreTableFromBackup", `{"TargetTableName":"dst","BackupArn":"`+arn+`"}`)
	wantErr(t, rec, typTableExists, "Table already exists: dst")
}

func TestRestoreTableFromBackupUnknown(t *testing.T) {
	_, h, _ := newBackupOps(t)
	arn := "arn:aws:dynamodb:us-east-1:000000000000:table/tbl/backup/nope"
	rec := post(t, h, "RestoreTableFromBackup", `{"TargetTableName":"dst","BackupArn":"`+arn+`"}`)
	wantErr(t, rec, typBackupNotFound, "Backup not found: "+arn)
}

func TestRestoreTableFromBackupOverrides(t *testing.T) {
	_, h, store := newOps(t)
	if _, err := store.CreateTable(context.Background(), &storage.Table{
		Name: "src",
		Key:  storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs: map[string]attr.Type{
			"id": attr.TypeS, "entity": attr.TypeS,
		},
		Billing: "PAY_PER_REQUEST",
		GSIs: []storage.Index{
			{Name: "entity-index", Key: storage.KeySchema{Hash: "entity", HashType: attr.TypeS}, Projection: storage.ProjectAll},
		},
	}); err != nil {
		t.Fatal(err)
	}
	arn := createBackup(t, h, "src", "bk1")

	body := `{"TargetTableName":"dst","BackupArn":"` + arn + `",` +
		`"BillingModeOverride":"PROVISIONED",` +
		`"ProvisionedThroughputOverride":{"ReadCapacityUnits":5,"WriteCapacityUnits":6},` +
		`"GlobalSecondaryIndexOverride":[{"IndexName":"entity-index","KeySchema":[{"AttributeName":"entity","KeyType":"HASH"}],"Projection":{"ProjectionType":"KEYS_ONLY"}}],` +
		`"SSESpecificationOverride":{"Enabled":true,"SSEType":"KMS"}}`
	wantStatus(t, post(t, h, "RestoreTableFromBackup", body), 200)

	desc := wantStatus(t, post(t, h, "DescribeTable", `{"TableName":"dst"}`), 200)
	dst := desc["Table"].(map[string]any)
	pt := dst["ProvisionedThroughput"].(map[string]any)
	if pt["ReadCapacityUnits"] != float64(5) || pt["WriteCapacityUnits"] != float64(6) {
		t.Errorf("ProvisionedThroughput = %#v, want 5/6", pt)
	}
	if _, ok := dst["BillingModeSummary"]; ok {
		t.Errorf("restored table has BillingModeSummary, want none for PROVISIONED")
	}
	gsis := dst["GlobalSecondaryIndexes"].([]any)
	if len(gsis) != 1 {
		t.Fatalf("GlobalSecondaryIndexes = %#v, want one", dst["GlobalSecondaryIndexes"])
	}
	proj := gsis[0].(map[string]any)["Projection"].(map[string]any)
	if proj["ProjectionType"] != "KEYS_ONLY" {
		t.Errorf("ProjectionType = %v, want KEYS_ONLY", proj["ProjectionType"])
	}
}

func TestRestoreTableFromBackupLSIOverride(t *testing.T) {
	_, h, store := newOps(t)
	if _, err := store.CreateTable(context.Background(), &storage.Table{
		Name: "src",
		Key:  storage.KeySchema{Hash: "id", HashType: attr.TypeS, Range: "sk", RangeType: attr.TypeS},
		AttrDefs: map[string]attr.Type{
			"id": attr.TypeS, "sk": attr.TypeS, "year": attr.TypeN,
		},
		GSIs: []storage.Index{
			{Name: "year-index", Key: storage.KeySchema{Hash: "id", HashType: attr.TypeS, Range: "year", RangeType: attr.TypeN}, Projection: storage.ProjectAll, Local: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	arn := createBackup(t, h, "src", "bk1")

	body := `{"TargetTableName":"dst","BackupArn":"` + arn + `",` +
		`"LocalSecondaryIndexOverride":[{"IndexName":"year-index","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"year","KeyType":"RANGE"}],"Projection":{"ProjectionType":"KEYS_ONLY"}}]}`
	wantStatus(t, post(t, h, "RestoreTableFromBackup", body), 200)

	desc := wantStatus(t, post(t, h, "DescribeTable", `{"TableName":"dst"}`), 200)
	lsis := desc["Table"].(map[string]any)["LocalSecondaryIndexes"].([]any)
	if len(lsis) != 1 {
		t.Fatalf("LocalSecondaryIndexes = %#v, want one", desc)
	}
	proj := lsis[0].(map[string]any)["Projection"].(map[string]any)
	if proj["ProjectionType"] != "KEYS_ONLY" {
		t.Errorf("ProjectionType = %v, want KEYS_ONLY", proj["ProjectionType"])
	}
}

func TestRestoreTableFromBackupInvalidGSIOverride(t *testing.T) {
	_, h, store := newOps(t)
	if _, err := store.CreateTable(context.Background(), &storage.Table{
		Name:     "src",
		Key:      storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs: map[string]attr.Type{"id": attr.TypeS},
	}); err != nil {
		t.Fatal(err)
	}
	arn := createBackup(t, h, "src", "bk1")

	// A key attribute not present in the source's AttributeDefinitions.
	body := `{"TargetTableName":"dst","BackupArn":"` + arn + `",` +
		`"GlobalSecondaryIndexOverride":[{"IndexName":"bad-index","KeySchema":[{"AttributeName":"missing","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}}]}`
	rec := post(t, h, "RestoreTableFromBackup", body)
	wantErr(t, rec, typValidation, "One or more parameter values were invalid: Some index key attributes are not defined in AttributeDefinitions. Keys: [missing], AttributeDefinitions: [id]")

	// ProvisionedThroughputOverride with a PAY_PER_REQUEST override.
	body = `{"TargetTableName":"dst","BackupArn":"` + arn + `",` +
		`"BillingModeOverride":"PAY_PER_REQUEST","ProvisionedThroughputOverride":{"ReadCapacityUnits":5,"WriteCapacityUnits":6}}`
	rec = post(t, h, "RestoreTableFromBackup", body)
	wantErr(t, rec, typValidation, "One or more parameter values were invalid: Neither ReadCapacityUnits nor WriteCapacityUnits can be specified when BillingMode is PAY_PER_REQUEST")
}

func TestBackupsUnsupportedStore(t *testing.T) {
	o := &Ops{Store: storeNoBackup{memstore.New()}}
	h := api.New(api.Options{})
	o.Register(h)

	rec := post(t, h, "CreateBackup", `{"TableName":"tbl","BackupName":"bk1"}`)
	wantErr(t, rec, typValidation, "dynamoat: this store does not support backups")
}

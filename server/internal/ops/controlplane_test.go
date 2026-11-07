package ops

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/api"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
)

const (
	testRegion = "us-east-1"
	testTable  = "tenant-42"
	testARN    = "arn:aws:dynamodb:" + testRegion + ":000000000000:table/" + testTable
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func newTestHandler(t *testing.T, now time.Time) http.Handler {
	t.Helper()
	h := api.New(api.Options{})
	o := &Ops{Store: memstore.New(), Now: func() time.Time { return now }, Region: testRegion}
	o.Register(h)
	return h
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not JSON (%q): %v", rec.Body.String(), err)
	}
	return m
}

func checkError(t *testing.T, rec *httptest.ResponseRecorder, code int, typ, message string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, code, rec.Body.String())
	}
	m := decodeBody(t, rec)
	if m["__type"] != typ {
		t.Errorf("__type = %v, want %s", m["__type"], typ)
	}
	if message != "" && m["message"] != message {
		t.Errorf("message = %v, want %s", m["message"], message)
	}
}

func tableDesc(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	m := decodeBody(t, rec)
	desc, ok := m["TableDescription"].(map[string]any)
	if !ok {
		t.Fatalf("no TableDescription in %v", m)
	}
	return desc
}

func gsiByName(t *testing.T, desc map[string]any, name string) map[string]any {
	t.Helper()
	raw, ok := desc["GlobalSecondaryIndexes"].([]any)
	if !ok {
		t.Fatalf("no GlobalSecondaryIndexes in %v", desc)
	}
	for _, g := range raw {
		gm := g.(map[string]any)
		if gm["IndexName"] == name {
			return gm
		}
	}
	t.Fatalf("GSI %q not found in %v", name, raw)
	return nil
}

func TestTenantLifecycle(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 30, 45, 123000000, time.UTC)
	h := newTestHandler(t, now)

	body, err := os.ReadFile("testdata/tenant-createtable.json")
	if err != nil {
		t.Fatal(err)
	}

	rec := post(t, h, "CreateTable", string(body))
	if rec.Code != 200 {
		t.Fatalf("CreateTable status %d: %s", rec.Code, rec.Body.String())
	}
	desc := tableDesc(t, rec)

	if got := desc["TableName"]; got != testTable {
		t.Errorf("TableName = %v, want %s", got, testTable)
	}
	if got := desc["TableArn"]; got != testARN {
		t.Errorf("TableArn = %v, want %s", got, testARN)
	}
	if id, _ := desc["TableId"].(string); !uuidRE.MatchString(id) {
		t.Errorf("TableId = %v, want a UUID", desc["TableId"])
	}
	if got := desc["TableStatus"]; got != "CREATING" {
		t.Errorf("TableStatus = %v, want CREATING", got)
	}
	if got := desc["CreationDateTime"]; got != float64(now.UnixMilli())/1000 {
		t.Errorf("CreationDateTime = %v, want %v", got, float64(now.UnixMilli())/1000)
	}
	if got := desc["ItemCount"]; got != float64(0) {
		t.Errorf("ItemCount = %v, want 0", got)
	}
	if got := desc["TableSizeBytes"]; got != float64(0) {
		t.Errorf("TableSizeBytes = %v, want 0", got)
	}

	keySchema := desc["KeySchema"].([]any)
	if len(keySchema) != 1 {
		t.Fatalf("KeySchema = %v, want the id HASH key", keySchema)
	}
	if ks := keySchema[0].(map[string]any); ks["AttributeName"] != "id" || ks["KeyType"] != "HASH" {
		t.Errorf("KeySchema[0] = %v", keySchema[0])
	}

	defs := desc["AttributeDefinitions"].([]any)
	if len(defs) != 4 {
		t.Fatalf("AttributeDefinitions = %v, want 4 entries", defs)
	}
	for i, want := range []string{"comboKey", "entity", "id", "subscription"} {
		d := defs[i].(map[string]any)
		if d["AttributeName"] != want || d["AttributeType"] != "S" {
			t.Errorf("AttributeDefinitions[%d] = %v, want %s/S", i, d, want)
		}
	}

	pt := desc["ProvisionedThroughput"].(map[string]any)
	if pt["NumberOfDecreasesToday"] != float64(0) || pt["ReadCapacityUnits"] != float64(0) || pt["WriteCapacityUnits"] != float64(0) {
		t.Errorf("ProvisionedThroughput = %v", pt)
	}
	billing := desc["BillingModeSummary"].(map[string]any)
	if billing["BillingMode"] != "PAY_PER_REQUEST" {
		t.Errorf("BillingModeSummary.BillingMode = %v", billing["BillingMode"])
	}
	// CreateTable does not report when the table switched to pay-per-request.
	if _, ok := billing["LastUpdateToPayPerRequestDateTime"]; ok {
		t.Errorf("CreateTable BillingModeSummary unexpectedly has LastUpdateToPayPerRequestDateTime: %v", billing)
	}
	if got := desc["DeletionProtectionEnabled"]; got != false {
		t.Errorf("DeletionProtectionEnabled = %v, want false", got)
	}

	wantGSIs := map[string]struct {
		hash, rng   string
		projection  string
		nonKeyAttrs []string
	}{
		"entity-comboKey-index":      {"entity", "comboKey", "ALL", nil},
		"entity-comboKey-name-index": {"entity", "comboKey", "INCLUDE", []string{"name"}},
		"comboKey-index":             {"comboKey", "", "ALL", nil},
		"subscription-index":         {"subscription", "", "KEYS_ONLY", nil},
	}
	gsis := desc["GlobalSecondaryIndexes"].([]any)
	if len(gsis) != len(wantGSIs) {
		t.Fatalf("GlobalSecondaryIndexes has %d entries, want %d: %v", len(gsis), len(wantGSIs), gsis)
	}
	for name, want := range wantGSIs {
		g := gsiByName(t, desc, name)
		if g["IndexStatus"] != "CREATING" {
			t.Errorf("GSI %s IndexStatus = %v, want CREATING", name, g["IndexStatus"])
		}
		if g["IndexArn"] != testARN+"/index/"+name {
			t.Errorf("GSI %s IndexArn = %v", name, g["IndexArn"])
		}
		if g["ItemCount"] != float64(0) || g["IndexSizeBytes"] != float64(0) {
			t.Errorf("GSI %s sizes = %v", name, g)
		}
		ks := g["KeySchema"].([]any)
		if len(ks) != 1 && len(ks) != 2 {
			t.Fatalf("GSI %s KeySchema = %v", name, ks)
		}
		if ks[0].(map[string]any)["AttributeName"] != want.hash || ks[0].(map[string]any)["KeyType"] != "HASH" {
			t.Errorf("GSI %s hash = %v", name, ks[0])
		}
		if want.rng != "" {
			if ks[1].(map[string]any)["AttributeName"] != want.rng || ks[1].(map[string]any)["KeyType"] != "RANGE" {
				t.Errorf("GSI %s range = %v", name, ks[1])
			}
		}
		proj := g["Projection"].(map[string]any)
		if proj["ProjectionType"] != want.projection {
			t.Errorf("GSI %s ProjectionType = %v", name, proj["ProjectionType"])
		}
		if want.projection == "INCLUDE" {
			nka := proj["NonKeyAttributes"].([]any)
			if len(nka) != 1 || nka[0] != "name" {
				t.Errorf("GSI %s NonKeyAttributes = %v", name, proj["NonKeyAttributes"])
			}
		}
	}

	stream := desc["StreamSpecification"].(map[string]any)
	if stream["StreamEnabled"] != true || stream["StreamViewType"] != "OLD_IMAGE" {
		t.Errorf("StreamSpecification = %v", stream)
	}
	label := now.Format("2006-01-02T15:04:05.000")
	if desc["LatestStreamLabel"] != label {
		t.Errorf("LatestStreamLabel = %v, want %s", desc["LatestStreamLabel"], label)
	}
	if desc["LatestStreamArn"] != testARN+"/stream/"+label {
		t.Errorf("LatestStreamArn = %v", desc["LatestStreamArn"])
	}

	// DescribeTable shows the table ACTIVE right away.
	rec = post(t, h, "DescribeTable", `{"TableName":"tenant-42"}`)
	m := decodeBody(t, rec)
	table := m["Table"].(map[string]any)
	if table["TableStatus"] != "ACTIVE" {
		t.Errorf("DescribeTable TableStatus = %v, want ACTIVE", table["TableStatus"])
	}
	if table["TableId"] != desc["TableId"] {
		t.Errorf("DescribeTable TableId = %v, want %v (stable)", table["TableId"], desc["TableId"])
	}
	if got := gsiByName(t, table, "comboKey-index")["IndexStatus"]; got != "ACTIVE" {
		t.Errorf("DescribeTable GSI IndexStatus = %v, want ACTIVE", got)
	}
	if _, ok := table["BillingModeSummary"]; !ok {
		t.Errorf("DescribeTable missing BillingModeSummary")
	}
	describeBilling := table["BillingModeSummary"].(map[string]any)
	if describeBilling["LastUpdateToPayPerRequestDateTime"] != float64(now.UnixMilli())/1000 {
		t.Errorf("DescribeTable LastUpdateToPayPerRequestDateTime = %v", describeBilling["LastUpdateToPayPerRequestDateTime"])
	}
	if got := table["DeletionProtectionEnabled"]; got != false {
		t.Errorf("DescribeTable DeletionProtectionEnabled = %v, want false", got)
	}
	warm := table["WarmThroughput"].(map[string]any)
	if warm["ReadUnitsPerSecond"] != float64(12000) || warm["WriteUnitsPerSecond"] != float64(4000) || warm["Status"] != "ACTIVE" {
		t.Errorf("DescribeTable WarmThroughput = %v", warm)
	}
	gsiWarm := gsiByName(t, table, "comboKey-index")["WarmThroughput"].(map[string]any)
	if gsiWarm["ReadUnitsPerSecond"] != float64(12000) || gsiWarm["Status"] != "ACTIVE" {
		t.Errorf("DescribeTable GSI WarmThroughput = %v", gsiWarm)
	}

	// ListTables returns the single table with no continuation.
	rec = post(t, h, "ListTables", `{}`)
	m = decodeBody(t, rec)
	names := m["TableNames"].([]any)
	if len(names) != 1 || names[0] != testTable {
		t.Errorf("ListTables TableNames = %v", names)
	}
	if _, ok := m["LastEvaluatedTableName"]; ok {
		t.Errorf("ListTables unexpected LastEvaluatedTableName: %v", m)
	}

	// Add a GSI, then delete it.
	addGSI := `{
		"TableName":"tenant-42",
		"AttributeDefinitions":[{"AttributeName":"tenantId","AttributeType":"S"}],
		"GlobalSecondaryIndexUpdates":[{"Create":{
			"IndexName":"tenantId-index",
			"KeySchema":[{"AttributeName":"tenantId","KeyType":"HASH"}],
			"Projection":{"ProjectionType":"ALL"}
		}}]
	}`
	rec = post(t, h, "UpdateTable", addGSI)
	if rec.Code != 200 {
		t.Fatalf("UpdateTable add GSI status %d: %s", rec.Code, rec.Body.String())
	}
	updated := tableDesc(t, rec)
	gsiByName(t, updated, "tenantId-index")
	rec = post(t, h, "DescribeTable", `{"TableName":"tenant-42"}`)
	gsiByName(t, decodeBody(t, rec)["Table"].(map[string]any), "tenantId-index")

	delGSI := `{"TableName":"tenant-42","GlobalSecondaryIndexUpdates":[{"Delete":{"IndexName":"tenantId-index"}}]}`
	rec = post(t, h, "UpdateTable", delGSI)
	if rec.Code != 200 {
		t.Fatalf("UpdateTable delete GSI status %d: %s", rec.Code, rec.Body.String())
	}
	rec = post(t, h, "DescribeTable", `{"TableName":"tenant-42"}`)
	after := decodeBody(t, rec)["Table"].(map[string]any)
	gsis = after["GlobalSecondaryIndexes"].([]any)
	if len(gsis) != 4 {
		t.Errorf("after GSI delete, %d indexes remain, want 4", len(gsis))
	}

	// Delete the table.
	rec = post(t, h, "DeleteTable", `{"TableName":"tenant-42"}`)
	del := tableDesc(t, rec)
	if del["TableStatus"] != "DELETING" {
		t.Errorf("DeleteTable TableStatus = %v, want DELETING", del["TableStatus"])
	}
	rec = post(t, h, "DescribeTable", `{"TableName":"tenant-42"}`)
	checkError(t, rec, 400, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException", "Requested resource not found: Table: tenant-42 not found")
}

func TestListTablesPagination(t *testing.T) {
	h := newTestHandler(t, time.Now())
	for _, n := range []string{"aaa", "bbb", "ccc"} {
		body := `{"TableName":"` + n + `","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],` +
			`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],` +
			`"ProvisionedThroughput":{"ReadCapacityUnits":1,"WriteCapacityUnits":1}}`
		rec := post(t, h, "CreateTable", body)
		if rec.Code != 200 {
			t.Fatalf("CreateTable %s status %d: %s", n, rec.Code, rec.Body.String())
		}
	}

	rec := post(t, h, "ListTables", `{"Limit":2}`)
	m := decodeBody(t, rec)
	names := m["TableNames"].([]any)
	if len(names) != 2 || names[0] != "aaa" || names[1] != "bbb" {
		t.Errorf("first page = %v", names)
	}
	if m["LastEvaluatedTableName"] != "bbb" {
		t.Errorf("LastEvaluatedTableName = %v, want bbb", m["LastEvaluatedTableName"])
	}

	rec = post(t, h, "ListTables", `{"Limit":2,"ExclusiveStartTableName":"bbb"}`)
	m = decodeBody(t, rec)
	names = m["TableNames"].([]any)
	if len(names) != 1 || names[0] != "ccc" {
		t.Errorf("second page = %v", names)
	}
	if _, ok := m["LastEvaluatedTableName"]; ok {
		t.Errorf("second page has LastEvaluatedTableName: %v", m)
	}
}

func TestListTablesLimitValidation(t *testing.T) {
	h := newTestHandler(t, time.Now())
	rec := post(t, h, "ListTables", `{"Limit":0}`)
	checkError(t, rec, 400, "com.amazon.coral.validate#ValidationException",
		"1 validation error detected: Value '0' at 'limit' failed to satisfy constraint: Member must have value greater than or equal to 1")
	rec = post(t, h, "ListTables", `{"Limit":101}`)
	checkError(t, rec, 400, "com.amazon.coral.validate#ValidationException",
		"1 validation error detected: Value '101' at 'limit' failed to satisfy constraint: Member must have value less than or equal to 100")
}

func TestDescribeTableMissing(t *testing.T) {
	h := newTestHandler(t, time.Now())
	rec := post(t, h, "DescribeTable", `{"TableName":"nope"}`)
	checkError(t, rec, 400, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException",
		"Requested resource not found: Table: nope not found")
}

func TestDeleteTableMissing(t *testing.T) {
	h := newTestHandler(t, time.Now())
	rec := post(t, h, "DeleteTable", `{"TableName":"nope"}`)
	checkError(t, rec, 400, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException",
		"Requested resource not found: Table: nope not found")
}

func TestDeletionProtectionLifecycle(t *testing.T) {
	h := newTestHandler(t, time.Now())

	rec := post(t, h, "CreateTable", `{"TableName":"prot","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST","DeletionProtectionEnabled":true}`)
	if rec.Code != 200 {
		t.Fatalf("CreateTable status %d: %s", rec.Code, rec.Body.String())
	}
	if got := tableDesc(t, rec)["DeletionProtectionEnabled"]; got != true {
		t.Fatalf("CreateTable DeletionProtectionEnabled = %v, want true", got)
	}

	rec = post(t, h, "DeleteTable", `{"TableName":"prot"}`)
	checkError(t, rec, 400, "com.amazon.coral.validate#ValidationException",
		"Resource cannot be deleted as it is currently protected against deletion. Disable deletion protection first.")

	rec = post(t, h, "UpdateTable", `{"TableName":"prot","DeletionProtectionEnabled":false}`)
	if rec.Code != 200 {
		t.Fatalf("UpdateTable status %d: %s", rec.Code, rec.Body.String())
	}
	if got := tableDesc(t, rec)["DeletionProtectionEnabled"]; got != false {
		t.Fatalf("UpdateTable DeletionProtectionEnabled = %v, want false", got)
	}

	rec = post(t, h, "DeleteTable", `{"TableName":"prot"}`)
	if rec.Code != 200 {
		t.Fatalf("DeleteTable after disabling status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWarmThroughputProvisionedOmitted(t *testing.T) {
	h := newTestHandler(t, time.Now())
	rec := post(t, h, "CreateTable", `{"TableName":"prov","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"ProvisionedThroughput":{"ReadCapacityUnits":1,"WriteCapacityUnits":1}}`)
	if rec.Code != 200 {
		t.Fatalf("CreateTable status %d: %s", rec.Code, rec.Body.String())
	}
	rec = post(t, h, "DescribeTable", `{"TableName":"prov"}`)
	m := decodeBody(t, rec)
	table := m["Table"].(map[string]any)
	if _, ok := table["WarmThroughput"]; ok {
		t.Errorf("provisioned table has WarmThroughput: %v", table)
	}
}

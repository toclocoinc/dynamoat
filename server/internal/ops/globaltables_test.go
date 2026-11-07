package ops

import (
	"testing"
	"time"
)

const (
	typGlobalTableNotFound = "com.amazonaws.dynamodb.v20120810#GlobalTableNotFoundException"
	typGlobalTableExists   = "com.amazonaws.dynamodb.v20120810#GlobalTableAlreadyExistsException"
	typReplicaExists       = "com.amazonaws.dynamodb.v20120810#ReplicaAlreadyExistsException"
	typReplicaNotFound     = "com.amazonaws.dynamodb.v20120810#ReplicaNotFoundException"
)

// tableWithStream creates a hash-only PAY_PER_REQUEST table with a stream.
func tableWithStream(name, view string) string {
	return `{"TableName":"` + name + `","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST","StreamSpecification":{"StreamEnabled":true,"StreamViewType":"` + view + `"}}`
}

func replicaRegionNames(desc map[string]any) []string {
	raw, ok := desc["Replicas"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any)["RegionName"].(string))
	}
	return out
}

func replicaStatuses(desc map[string]any) []string {
	raw, ok := desc["Replicas"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any)["ReplicaStatus"].(string))
	}
	return out
}

func TestUpdateTableAddAndRemoveReplica(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 30, 45, 123000000, time.UTC)
	h := newTestHandler(t, now)
	if rec := post(t, h, "CreateTable", simpleTable("reptbl")); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}

	// Add a replica. Streams are off, so NEW_AND_OLD_IMAGES is enabled for us;
	// the table's own region (us-east-1) becomes the first replica.
	rec := post(t, h, "UpdateTable", `{"TableName":"reptbl","ReplicaUpdates":[{"Create":{"RegionName":"eu-west-1"}}]}`)
	if rec.Code != 200 {
		t.Fatalf("add replica: %d %s", rec.Code, rec.Body.String())
	}
	desc := tableDesc(t, rec)
	if desc["GlobalTableVersion"] != "2019.11.21" {
		t.Errorf("GlobalTableVersion = %v, want 2019.11.21", desc["GlobalTableVersion"])
	}
	if got, want := replicaRegionNames(desc), []string{"us-east-1", "eu-west-1"}; !equalStrings(got, want) {
		t.Errorf("Replicas regions = %v, want %v", got, want)
	}
	if got, want := replicaStatuses(desc), []string{"ACTIVE", "CREATING"}; !equalStrings(got, want) {
		t.Errorf("Replicas statuses = %v, want %v", got, want)
	}
	stream := desc["StreamSpecification"].(map[string]any)
	if stream["StreamViewType"] != "NEW_AND_OLD_IMAGES" {
		t.Errorf("StreamViewType = %v, want NEW_AND_OLD_IMAGES", stream["StreamViewType"])
	}

	// DescribeTable shows the replica ACTIVE now.
	rec = post(t, h, "DescribeTable", `{"TableName":"reptbl"}`)
	table := decodeBody(t, rec)["Table"].(map[string]any)
	if got, want := replicaStatuses(table), []string{"ACTIVE", "ACTIVE"}; !equalStrings(got, want) {
		t.Errorf("DescribeTable Replicas statuses = %v, want %v", got, want)
	}
	if table["GlobalTableVersion"] != "2019.11.21" {
		t.Errorf("DescribeTable GlobalTableVersion = %v", table["GlobalTableVersion"])
	}

	// Remove it; the table is no longer a global table.
	rec = post(t, h, "UpdateTable", `{"TableName":"reptbl","ReplicaUpdates":[{"Delete":{"RegionName":"eu-west-1"}}]}`)
	if rec.Code != 200 {
		t.Fatalf("remove replica: %d %s", rec.Code, rec.Body.String())
	}
	rec = post(t, h, "DescribeTable", `{"TableName":"reptbl"}`)
	table = decodeBody(t, rec)["Table"].(map[string]any)
	if _, ok := table["Replicas"]; ok {
		t.Errorf("DescribeTable still has Replicas after removing the last replica: %v", table)
	}
	if _, ok := table["GlobalTableVersion"]; ok {
		t.Errorf("DescribeTable still has GlobalTableVersion after removing the last replica: %v", table)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestUpdateTableReplicaErrors(t *testing.T) {
	h := newTestHandler(t, time.Now())
	if rec := post(t, h, "CreateTable", simpleTable("reptbl")); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	post(t, h, "UpdateTable", `{"TableName":"reptbl","ReplicaUpdates":[{"Create":{"RegionName":"eu-west-1"}}]}`)

	// Adding a region that exists is a ValidationException.
	rec := post(t, h, "UpdateTable", `{"TableName":"reptbl","ReplicaUpdates":[{"Create":{"RegionName":"eu-west-1"}}]}`)
	checkError(t, rec, 400, typValidation, "One or more parameter values were invalid: Replica already exists: eu-west-1")

	// Deleting a region that does not exist is a ValidationException.
	rec = post(t, h, "UpdateTable", `{"TableName":"reptbl","ReplicaUpdates":[{"Delete":{"RegionName":"eu-west-2"}}]}`)
	checkError(t, rec, 400, typValidation, "One or more parameter values were invalid: Replica not found: eu-west-2")

	// The home region cannot be deleted directly.
	rec = post(t, h, "UpdateTable", `{"TableName":"reptbl","ReplicaUpdates":[{"Delete":{"RegionName":"us-east-1"}}]}`)
	checkError(t, rec, 400, typValidation, "One or more parameter values were invalid: Cannot delete the table's home region: us-east-1")

	// Only one replica update per call.
	rec = post(t, h, "UpdateTable", `{"TableName":"reptbl","ReplicaUpdates":[{"Create":{"RegionName":"eu-west-2"}},{"Create":{"RegionName":"eu-west-3"}}]}`)
	checkError(t, rec, 400, typValidation, "Only one replica update can be performed at a time")
}

func TestUpdateTableReplicaRequiresNewAndOldStream(t *testing.T) {
	h := newTestHandler(t, time.Now())
	if rec := post(t, h, "CreateTable", tableWithStream("reptbl", "OLD_IMAGE")); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rec := post(t, h, "UpdateTable", `{"TableName":"reptbl","ReplicaUpdates":[{"Create":{"RegionName":"eu-west-1"}}]}`)
	checkError(t, rec, 400, typValidation, "One or more parameter values were invalid: Global Table creation requires Streams enabled with NEW_AND_OLD_IMAGES view type")
}

func TestCreateGlobalTable(t *testing.T) {
	h := newTestHandler(t, time.Now())
	if rec := post(t, h, "CreateTable", tableWithStream("gtab", "NEW_AND_OLD_IMAGES")); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rec := post(t, h, "CreateGlobalTable", `{"GlobalTableName":"gtab","ReplicationGroup":[{"RegionName":"eu-west-1"},{"RegionName":"eu-west-2"}]}`)
	if rec.Code != 200 {
		t.Fatalf("CreateGlobalTable: %d %s", rec.Code, rec.Body.String())
	}
	gd := decodeBody(t, rec)["GlobalTableDescription"].(map[string]any)
	if gd["GlobalTableName"] != "gtab" {
		t.Errorf("GlobalTableName = %v", gd["GlobalTableName"])
	}
	if gd["GlobalTableArn"] != "arn:aws:dynamodb::000000000000:global-table/gtab" {
		t.Errorf("GlobalTableArn = %v", gd["GlobalTableArn"])
	}
	if gd["GlobalTableStatus"] != "ACTIVE" {
		t.Errorf("GlobalTableStatus = %v", gd["GlobalTableStatus"])
	}
	group := gd["ReplicationGroup"].([]any)
	if len(group) != 3 {
		t.Fatalf("ReplicationGroup = %v, want 3 replicas", group)
	}
	if got := group[0].(map[string]any)["RegionName"]; got != "us-east-1" {
		t.Errorf("ReplicationGroup[0] = %v, want us-east-1 first", got)
	}
}

func TestCreateGlobalTableErrors(t *testing.T) {
	h := newTestHandler(t, time.Now())

	// Missing table.
	rec := post(t, h, "CreateGlobalTable", `{"GlobalTableName":"nope","ReplicationGroup":[{"RegionName":"eu-west-1"}]}`)
	checkError(t, rec, 400, typGlobalTableNotFound, "Global table not found: nope")

	// A table without streams NEW_AND_OLD_IMAGES.
	post(t, h, "CreateTable", simpleTable("gtab"))
	rec = post(t, h, "CreateGlobalTable", `{"GlobalTableName":"gtab","ReplicationGroup":[{"RegionName":"eu-west-1"}]}`)
	checkError(t, rec, 400, typValidation, "One or more parameter values were invalid: Global Table creation requires Streams enabled with NEW_AND_OLD_IMAGES view type")

	// Already a global table.
	post(t, h, "CreateTable", tableWithStream("gtab2", "NEW_AND_OLD_IMAGES"))
	post(t, h, "CreateGlobalTable", `{"GlobalTableName":"gtab2","ReplicationGroup":[{"RegionName":"eu-west-1"}]}`)
	rec = post(t, h, "CreateGlobalTable", `{"GlobalTableName":"gtab2","ReplicationGroup":[{"RegionName":"eu-west-1"}]}`)
	checkError(t, rec, 400, typGlobalTableExists, "Global table already exists: gtab2")

	// Duplicate region in the replication group.
	post(t, h, "CreateTable", tableWithStream("gtab3", "NEW_AND_OLD_IMAGES"))
	rec = post(t, h, "CreateGlobalTable", `{"GlobalTableName":"gtab3","ReplicationGroup":[{"RegionName":"eu-west-1"},{"RegionName":"eu-west-1"}]}`)
	checkError(t, rec, 400, typReplicaExists, "Replica already exists: eu-west-1")
}

func TestDescribeGlobalTableNotFound(t *testing.T) {
	h := newTestHandler(t, time.Now())
	post(t, h, "CreateTable", simpleTable("gtab"))
	rec := post(t, h, "DescribeGlobalTable", `{"GlobalTableName":"gtab"}`)
	checkError(t, rec, 400, typGlobalTableNotFound, "Global table not found: gtab")
}

func TestListGlobalTables(t *testing.T) {
	h := newTestHandler(t, time.Now())
	for _, n := range []string{"aaa", "bbb"} {
		post(t, h, "CreateTable", tableWithStream(n, "NEW_AND_OLD_IMAGES"))
	}
	post(t, h, "CreateGlobalTable", `{"GlobalTableName":"aaa","ReplicationGroup":[{"RegionName":"eu-west-1"}]}`)
	post(t, h, "CreateGlobalTable", `{"GlobalTableName":"bbb","ReplicationGroup":[{"RegionName":"eu-west-2"}]}`)

	rec := post(t, h, "ListGlobalTables", `{}`)
	m := decodeBody(t, rec)
	tables := m["GlobalTables"].([]any)
	if len(tables) != 2 {
		t.Fatalf("GlobalTables = %v, want 2", tables)
	}
	if tables[0].(map[string]any)["GlobalTableName"] != "aaa" {
		t.Errorf("GlobalTables[0] = %v", tables[0])
	}

	// Filter by region.
	rec = post(t, h, "ListGlobalTables", `{"RegionName":"eu-west-2"}`)
	tables = decodeBody(t, rec)["GlobalTables"].([]any)
	if len(tables) != 1 || tables[0].(map[string]any)["GlobalTableName"] != "bbb" {
		t.Errorf("ListGlobalTables RegionName filter = %v", tables)
	}

	// Pagination.
	rec = post(t, h, "ListGlobalTables", `{"Limit":1}`)
	m = decodeBody(t, rec)
	tables = m["GlobalTables"].([]any)
	if len(tables) != 1 || tables[0].(map[string]any)["GlobalTableName"] != "aaa" {
		t.Errorf("ListGlobalTables first page = %v", tables)
	}
	if m["LastEvaluatedGlobalTableName"] != "aaa" {
		t.Errorf("LastEvaluatedGlobalTableName = %v, want aaa", m["LastEvaluatedGlobalTableName"])
	}
}

func TestUpdateGlobalTable(t *testing.T) {
	h := newTestHandler(t, time.Now())
	post(t, h, "CreateTable", tableWithStream("gtab", "NEW_AND_OLD_IMAGES"))
	post(t, h, "CreateGlobalTable", `{"GlobalTableName":"gtab","ReplicationGroup":[{"RegionName":"eu-west-1"}]}`)

	// Add a replica.
	rec := post(t, h, "UpdateGlobalTable", `{"GlobalTableName":"gtab","ReplicaUpdates":[{"Create":{"RegionName":"eu-west-2"}}]}`)
	if rec.Code != 200 {
		t.Fatalf("UpdateGlobalTable create: %d %s", rec.Code, rec.Body.String())
	}
	gd := decodeBody(t, rec)["GlobalTableDescription"].(map[string]any)
	if len(gd["ReplicationGroup"].([]any)) != 3 {
		t.Errorf("ReplicationGroup = %v, want 3", gd["ReplicationGroup"])
	}

	// Add an existing replica.
	rec = post(t, h, "UpdateGlobalTable", `{"GlobalTableName":"gtab","ReplicaUpdates":[{"Create":{"RegionName":"eu-west-2"}}]}`)
	checkError(t, rec, 400, typReplicaExists, "Replica already exists: eu-west-2")

	// Delete a missing replica.
	rec = post(t, h, "UpdateGlobalTable", `{"GlobalTableName":"gtab","ReplicaUpdates":[{"Delete":{"RegionName":"eu-west-9"}}]}`)
	checkError(t, rec, 400, typReplicaNotFound, "Replica not found: eu-west-9")

	// Delete an existing replica.
	rec = post(t, h, "UpdateGlobalTable", `{"GlobalTableName":"gtab","ReplicaUpdates":[{"Delete":{"RegionName":"eu-west-2"}}]}`)
	if rec.Code != 200 {
		t.Fatalf("UpdateGlobalTable delete: %d %s", rec.Code, rec.Body.String())
	}
	gd = decodeBody(t, rec)["GlobalTableDescription"].(map[string]any)
	if len(gd["ReplicationGroup"].([]any)) != 2 {
		t.Errorf("ReplicationGroup = %v, want 2", gd["ReplicationGroup"])
	}
}

func TestGlobalTableSettings(t *testing.T) {
	h := newTestHandler(t, time.Now())
	post(t, h, "CreateTable", tableWithStream("gtab", "NEW_AND_OLD_IMAGES"))
	post(t, h, "CreateGlobalTable", `{"GlobalTableName":"gtab","ReplicationGroup":[{"RegionName":"eu-west-1"}]}`)

	rec := post(t, h, "DescribeGlobalTableSettings", `{"GlobalTableName":"gtab"}`)
	if rec.Code != 200 {
		t.Fatalf("DescribeGlobalTableSettings: %d %s", rec.Code, rec.Body.String())
	}
	m := decodeBody(t, rec)
	if m["GlobalTableName"] != "gtab" {
		t.Errorf("GlobalTableName = %v", m["GlobalTableName"])
	}
	settings := m["ReplicaSettings"].([]any)
	if len(settings) != 2 {
		t.Errorf("ReplicaSettings = %v, want 2", settings)
	}

	// Not a global table.
	post(t, h, "CreateTable", simpleTable("notglobal"))
	rec = post(t, h, "DescribeGlobalTableSettings", `{"GlobalTableName":"notglobal"}`)
	checkError(t, rec, 400, typGlobalTableNotFound, "Global table not found: notglobal")

	// Accept capacity updates; billing applies to the one table.
	rec = post(t, h, "UpdateGlobalTableSettings", `{"GlobalTableName":"gtab","GlobalTableBillingMode":"PROVISIONED","GlobalTableProvisionedWriteCapacityUnits":7}`)
	if rec.Code != 200 {
		t.Fatalf("UpdateGlobalTableSettings: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTableReplicaAutoScaling(t *testing.T) {
	h := newTestHandler(t, time.Now())
	post(t, h, "CreateTable", tableWithStream("gtab", "NEW_AND_OLD_IMAGES"))
	post(t, h, "CreateGlobalTable", `{"GlobalTableName":"gtab","ReplicationGroup":[{"RegionName":"eu-west-1"}]}`)

	rec := post(t, h, "DescribeTableReplicaAutoScaling", `{"TableName":"gtab"}`)
	if rec.Code != 200 {
		t.Fatalf("DescribeTableReplicaAutoScaling: %d %s", rec.Code, rec.Body.String())
	}
	m := decodeBody(t, rec)
	if m["TableName"] != "gtab" {
		t.Errorf("TableName = %v", m["TableName"])
	}
	if len(m["Replicas"].([]any)) != 2 {
		t.Errorf("Replicas = %v, want 2", m["Replicas"])
	}

	// Updates are accepted and ignored.
	rec = post(t, h, "UpdateTableReplicaAutoScaling", `{"TableName":"gtab","ReplicaUpdates":[{"Create":{"RegionName":"eu-west-2"}}]}`)
	if rec.Code != 200 {
		t.Fatalf("UpdateTableReplicaAutoScaling: %d %s", rec.Code, rec.Body.String())
	}
	m = decodeBody(t, rec)
	if len(m["Replicas"].([]any)) != 2 {
		t.Errorf("Replicas after ignored update = %v, want still 2", m["Replicas"])
	}
}

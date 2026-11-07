package ops

import (
	"testing"
	"time"
)

func simpleTable(name string) string {
	return `{"TableName":"` + name + `","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST"}`
}

func TestUpdateTableUnsupportedField(t *testing.T) {
	h := newTestHandler(t, time.Now())
	rec := post(t, h, "UpdateTable", `{"TableName":"foo","SSESpecification":{"Enabled":true}}`)
	checkError(t, rec, 400, "com.amazon.coral.validate#ValidationException", "dynamoat does not support SSESpecification")
}

func TestUpdateTableMissingTable(t *testing.T) {
	h := newTestHandler(t, time.Now())
	rec := post(t, h, "UpdateTable", `{"TableName":"nope","BillingMode":"PROVISIONED","ProvisionedThroughput":{"ReadCapacityUnits":1,"WriteCapacityUnits":1}}`)
	checkError(t, rec, 400, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException", "Requested resource not found: Table: nope not found")
}

func TestUpdateTableDeleteMissingIndex(t *testing.T) {
	h := newTestHandler(t, time.Now())
	if rec := post(t, h, "CreateTable", simpleTable("foo")); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rec := post(t, h, "UpdateTable", `{"TableName":"foo","GlobalSecondaryIndexUpdates":[{"Delete":{"IndexName":"nope"}}]}`)
	checkError(t, rec, 400, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException", "Requested resource not found: Index: nope not found")
}

func TestUpdateTableEnableStreamTwice(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 30, 45, 123000000, time.UTC)
	h := newTestHandler(t, now)
	body := `{"TableName":"streamy","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST","StreamSpecification":{"StreamEnabled":true,"StreamViewType":"OLD_IMAGE"}}`
	if rec := post(t, h, "CreateTable", body); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	label := now.Format("2006-01-02T15:04:05.000")
	arn := "arn:aws:dynamodb:us-east-1:000000000000:table/streamy/stream/" + label
	rec := post(t, h, "UpdateTable", `{"TableName":"streamy","StreamSpecification":{"StreamEnabled":true,"StreamViewType":"NEW_IMAGE"}}`)
	checkError(t, rec, 400, "com.amazon.coral.validate#ValidationException", "Table already has an enabled stream: "+arn)
}

func TestUpdateTableTwoGSIChanges(t *testing.T) {
	h := newTestHandler(t, time.Now())
	if rec := post(t, h, "CreateTable", simpleTable("foo")); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	body := `{"TableName":"foo","AttributeDefinitions":[{"AttributeName":"a","AttributeType":"S"},{"AttributeName":"b","AttributeType":"S"}],"GlobalSecondaryIndexUpdates":[{"Create":{"IndexName":"gsi-a","KeySchema":[{"AttributeName":"a","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}}},{"Create":{"IndexName":"gsi-b","KeySchema":[{"AttributeName":"b","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}}}]}`
	rec := post(t, h, "UpdateTable", body)
	checkError(t, rec, 400, "com.amazonaws.dynamodb.v20120810#LimitExceededException", "Subscriber limit exceeded: Only 1 online index can be created or deleted simultaneously per table")
}

func TestUpdateTableBillingModes(t *testing.T) {
	h := newTestHandler(t, time.Now())
	if rec := post(t, h, "CreateTable", simpleTable("foo")); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}

	rec := post(t, h, "UpdateTable", `{"TableName":"foo","BillingMode":"PROVISIONED","ProvisionedThroughput":{"ReadCapacityUnits":5,"WriteCapacityUnits":7}}`)
	if rec.Code != 200 {
		t.Fatalf("switch to PROVISIONED: %d %s", rec.Code, rec.Body.String())
	}
	pt := tableDesc(t, rec)["ProvisionedThroughput"].(map[string]any)
	if pt["ReadCapacityUnits"] != float64(5) || pt["WriteCapacityUnits"] != float64(7) {
		t.Errorf("ProvisionedThroughput = %v", pt)
	}

	rec = post(t, h, "UpdateTable", `{"TableName":"foo","BillingMode":"PAY_PER_REQUEST"}`)
	if rec.Code != 200 {
		t.Fatalf("switch to PAY_PER_REQUEST: %d %s", rec.Code, rec.Body.String())
	}
	if b := tableDesc(t, rec)["BillingModeSummary"].(map[string]any); b["BillingMode"] != "PAY_PER_REQUEST" {
		t.Errorf("BillingModeSummary = %v", b)
	}
}

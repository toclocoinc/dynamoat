package ops

import (
	"testing"
	"time"
)

func TestCreateTableValidation(t *testing.T) {
	h := newTestHandler(t, time.Now())
	cases := []struct {
		name    string
		body    string
		message string
	}{
		{
			"keySchema absent",
			`{"TableName":"foo","AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST"}`,
			"1 validation error detected: Value null at 'keySchema' failed to satisfy constraint: Member must not be null",
		},
		{
			"keySchema invalid",
			`{"TableName":"foo","KeySchema":[],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST"}`,
			"Invalid KeySchema: Some index key schema element is not valid",
		},
		{
			"key attribute not defined",
			`{"TableName":"foo","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[],"BillingMode":"PAY_PER_REQUEST"}`,
			"One or more parameter values were invalid: Some index key attributes are not defined in AttributeDefinitions. Keys: [id], AttributeDefinitions: []",
		},
		{
			"unused attribute definition",
			`{"TableName":"foo","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"extra","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST"}`,
			"One or more parameter values were invalid: Number of attributes in KeySchema does not exactly match number of attributes defined in AttributeDefinitions",
		},
		{
			"pay per request with provisioned throughput",
			`{"TableName":"foo","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST","ProvisionedThroughput":{"ReadCapacityUnits":1,"WriteCapacityUnits":1}}`,
			"One or more parameter values were invalid: Neither ReadCapacityUnits nor WriteCapacityUnits can be specified when BillingMode is PAY_PER_REQUEST",
		},
		{
			"provisioned without provisioned throughput",
			`{"TableName":"foo","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PROVISIONED"}`,
			"One or more parameter values were invalid: ReadCapacityUnits and WriteCapacityUnits must both be specified when BillingMode is PROVISIONED",
		},
		{
			"duplicate index name",
			`{"TableName":"foo","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"entity","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST","GlobalSecondaryIndexes":[{"IndexName":"dup-index","KeySchema":[{"AttributeName":"entity","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}},{"IndexName":"dup-index","KeySchema":[{"AttributeName":"entity","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}}]}`,
			"One or more parameter values were invalid: Duplicate index name: dup-index",
		},
		{
			"local secondary index without table range key",
			`{"TableName":"foo","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"year","AttributeType":"N"}],"BillingMode":"PAY_PER_REQUEST","LocalSecondaryIndexes":[{"IndexName":"year-index","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"year","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}}]}`,
			"One or more parameter values were invalid: Table KeySchema does not have a range key, which is required when specifying a LocalSecondaryIndex",
		},
		{
			"local secondary index hash key differs from table hash key",
			`{"TableName":"foo","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"S"},{"AttributeName":"other","AttributeType":"S"},{"AttributeName":"year","AttributeType":"N"}],"BillingMode":"PAY_PER_REQUEST","LocalSecondaryIndexes":[{"IndexName":"year-index","KeySchema":[{"AttributeName":"other","KeyType":"HASH"},{"AttributeName":"year","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}}]}`,
			"Index KeySchema does not have the same leading hash key as table KeySchema for index: year-index",
		},
		{
			"local secondary index range key same as table range key",
			`{"TableName":"foo","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST","LocalSecondaryIndexes":[{"IndexName":"year-index","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}}]}`,
			"One or more parameter values were invalid: Local Secondary index range key must be different from the table's range key",
		},
		{
			"local secondary index without range key",
			`{"TableName":"foo","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST","LocalSecondaryIndexes":[{"IndexName":"year-index","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}}]}`,
			"One or more parameter values were invalid: Local Secondary index must specify a range key",
		},
		{
			"local secondary index with provisioned throughput",
			`{"TableName":"foo","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"S"},{"AttributeName":"year","AttributeType":"N"}],"BillingMode":"PAY_PER_REQUEST","LocalSecondaryIndexes":[{"IndexName":"year-index","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"year","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"},"ProvisionedThroughput":{"ReadCapacityUnits":1,"WriteCapacityUnits":1}}]}`,
			"One or more parameter values were invalid: ProvisionedThroughput cannot be specified for LocalSecondaryIndex",
		},
		{
			"local secondary index name duplicates a gsi",
			`{"TableName":"foo","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"S"},{"AttributeName":"entity","AttributeType":"S"},{"AttributeName":"year","AttributeType":"N"}],"BillingMode":"PAY_PER_REQUEST","GlobalSecondaryIndexes":[{"IndexName":"dup-index","KeySchema":[{"AttributeName":"entity","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}}],"LocalSecondaryIndexes":[{"IndexName":"dup-index","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"year","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}}]}`,
			"One or more parameter values were invalid: Duplicate index name: dup-index",
		},
		{
			"too many local secondary indexes",
			`{"TableName":"foo","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"S"},{"AttributeName":"year","AttributeType":"N"}],"BillingMode":"PAY_PER_REQUEST","LocalSecondaryIndexes":[{"IndexName":"l1","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"year","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}},{"IndexName":"l2","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"year","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}},{"IndexName":"l3","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"year","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}},{"IndexName":"l4","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"year","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}},{"IndexName":"l5","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"year","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}},{"IndexName":"l6","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"year","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}}]}`,
			"One or more parameter values were invalid: Number of LocalSecondaryIndexes exceeds the limit of 5",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(t, h, "CreateTable", c.body)
			checkError(t, rec, 400, "com.amazon.coral.validate#ValidationException", c.message)
		})
	}
}

func TestCreateTableExistingName(t *testing.T) {
	h := newTestHandler(t, time.Now())
	body := `{"TableName":"foo","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST"}`
	if rec := post(t, h, "CreateTable", body); rec.Code != 200 {
		t.Fatalf("first create: %d %s", rec.Code, rec.Body.String())
	}
	rec := post(t, h, "CreateTable", body)
	checkError(t, rec, 400, "com.amazonaws.dynamodb.v20120810#ResourceInUseException", "Table already exists: foo")
}

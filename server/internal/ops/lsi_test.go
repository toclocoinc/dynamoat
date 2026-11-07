package ops

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// lsiTable creates a table with a sort key and one local secondary index over
// (id, year) with the given projection type.
func lsiTable(t *testing.T, h http.Handler, projection string) {
	t.Helper()
	body := fmt.Sprintf(`{"TableName":"lsitable","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"S"},{"AttributeName":"year","AttributeType":"N"}],"BillingMode":"PAY_PER_REQUEST","LocalSecondaryIndexes":[{"IndexName":"year-index","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"year","KeyType":"RANGE"}],"Projection":{"ProjectionType":%q}}]}`, projection)
	wantStatus(t, post(t, h, "CreateTable", body), 200)
}

// lsiYears extracts the year (N) of each returned item, in order.
func lsiYears(t *testing.T, m map[string]any) []string {
	t.Helper()
	var out []string
	items, _ := m["Items"].([]any)
	for _, it := range items {
		im, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("item %#v is not a map", it)
		}
		year, ok := im["year"].(map[string]any)
		if !ok {
			t.Fatalf("year %#v is not a map", im["year"])
		}
		out = append(out, year["N"].(string))
	}
	return out
}

func TestCreateTableWithLocalSecondaryIndex(t *testing.T) {
	h := newTestHandler(t, time.Now())
	body := `{"TableName":"lsi","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"S"},{"AttributeName":"year","AttributeType":"N"}],"BillingMode":"PAY_PER_REQUEST","LocalSecondaryIndexes":[{"IndexName":"year-index","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"year","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}}]}`
	rec := post(t, h, "CreateTable", body)
	desc := tableDesc(t, rec)

	if _, ok := desc["GlobalSecondaryIndexes"]; ok {
		t.Errorf("GlobalSecondaryIndexes present, want absent: %v", desc["GlobalSecondaryIndexes"])
	}
	raw, ok := desc["LocalSecondaryIndexes"].([]any)
	if !ok || len(raw) != 1 {
		t.Fatalf("LocalSecondaryIndexes = %v, want one entry", desc["LocalSecondaryIndexes"])
	}
	l := raw[0].(map[string]any)
	if l["IndexName"] != "year-index" {
		t.Errorf("IndexName = %v, want year-index", l["IndexName"])
	}
	wantArn := "arn:aws:dynamodb:" + testRegion + ":000000000000:table/lsi/index/year-index"
	if l["IndexArn"] != wantArn {
		t.Errorf("IndexArn = %v, want %v", l["IndexArn"], wantArn)
	}
	if l["ItemCount"] != float64(0) || l["IndexSizeBytes"] != float64(0) {
		t.Errorf("sizes = %v", l)
	}
	ks := l["KeySchema"].([]any)
	if len(ks) != 2 || ks[0].(map[string]any)["AttributeName"] != "id" || ks[1].(map[string]any)["AttributeName"] != "year" {
		t.Errorf("KeySchema = %v", ks)
	}
	proj := l["Projection"].(map[string]any)
	if proj["ProjectionType"] != "ALL" {
		t.Errorf("ProjectionType = %v", proj["ProjectionType"])
	}
	if _, ok := l["IndexStatus"]; ok {
		t.Errorf("LSI has IndexStatus, want absent: %v", l)
	}
	if _, ok := l["ProvisionedThroughput"]; ok {
		t.Errorf("LSI has ProvisionedThroughput, want absent: %v", l)
	}
}

func TestQueryLSIConsistentRead(t *testing.T) {
	_, h, _ := newOps(t)
	lsiTable(t, h, "ALL")
	for _, item := range []string{
		`{"id":{"S":"p1"},"sk":{"S":"a"},"year":{"N":"2001"},"name":{"S":"n1"}}`,
		`{"id":{"S":"p1"},"sk":{"S":"b"},"year":{"N":"2000"},"name":{"S":"n2"}}`,
		`{"id":{"S":"p2"},"sk":{"S":"a"},"year":{"N":"1999"},"name":{"S":"n3"}}`,
	} {
		wantStatus(t, post(t, h, "PutItem", `{"TableName":"lsitable","Item":`+item+`}`), 200)
	}

	body := `{"TableName":"lsitable","IndexName":"year-index","KeyConditionExpression":"id = :i","ExpressionAttributeValues":{":i":{"S":"p1"}},"ConsistentRead":true}`
	m := wantStatus(t, post(t, h, "Query", body), 200)

	got := lsiYears(t, m)
	want := []string{"2000", "2001"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %q, want %q", i, got[i], want[i])
		}
	}
	if countOf(t, m, "Count") != 2 {
		t.Errorf("Count = %v, want 2", m["Count"])
	}
}

func TestQueryLSIAllAttributesOnKeysOnly(t *testing.T) {
	_, h, _ := newOps(t)
	lsiTable(t, h, "KEYS_ONLY")
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"lsitable","Item":{"id":{"S":"p1"},"sk":{"S":"a"},"year":{"N":"2001"},"name":{"S":"n1"}}}`), 200)

	// Select ALL_ATTRIBUTES on an LSI fetches the base item even though the
	// projection is KEYS_ONLY. (Without Select an index read returns only the
	// projected attributes.)
	m := wantStatus(t, post(t, h, "Query", `{"TableName":"lsitable","IndexName":"year-index","Select":"ALL_ATTRIBUTES","KeyConditionExpression":"id = :i","ExpressionAttributeValues":{":i":{"S":"p1"}}}`), 200)
	items, _ := m["Items"].([]any)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	im := items[0].(map[string]any)
	for _, k := range []string{"id", "sk", "year", "name"} {
		if _, ok := im[k]; !ok {
			t.Errorf("item missing %q: %v", k, im)
		}
	}
}

func TestQueryLSIAllProjectedAttributes(t *testing.T) {
	_, h, _ := newOps(t)
	lsiTable(t, h, "KEYS_ONLY")
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"lsitable","Item":{"id":{"S":"p1"},"sk":{"S":"a"},"year":{"N":"2001"},"name":{"S":"n1"}}}`), 200)

	m := wantStatus(t, post(t, h, "Query", `{"TableName":"lsitable","IndexName":"year-index","KeyConditionExpression":"id = :i","ExpressionAttributeValues":{":i":{"S":"p1"}},"Select":"ALL_PROJECTED_ATTRIBUTES"}`), 200)
	items, _ := m["Items"].([]any)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	im := items[0].(map[string]any)
	for _, k := range []string{"id", "sk", "year"} {
		if _, ok := im[k]; !ok {
			t.Errorf("item missing %q: %v", k, im)
		}
	}
	if _, ok := im["name"]; ok {
		t.Errorf("item has non-projected attribute name: %v", im)
	}
}

func TestScanLSIConsistentRead(t *testing.T) {
	_, h, _ := newOps(t)
	lsiTable(t, h, "ALL")
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"lsitable","Item":{"id":{"S":"p1"},"sk":{"S":"a"},"year":{"N":"2001"}}}`), 200)

	m := wantStatus(t, post(t, h, "Scan", `{"TableName":"lsitable","IndexName":"year-index","ConsistentRead":true}`), 200)
	if countOf(t, m, "Count") != 1 {
		t.Errorf("Count = %v, want 1", m["Count"])
	}
}

func TestUpdateTableDeleteLSIRejected(t *testing.T) {
	_, h, _ := newOps(t)
	lsiTable(t, h, "ALL")

	rec := post(t, h, "UpdateTable", `{"TableName":"lsitable","GlobalSecondaryIndexUpdates":[{"Delete":{"IndexName":"year-index"}}]}`)
	wantErr(t, rec, typValidation, "One or more parameter values were invalid: Cannot delete a local secondary index: year-index")
}

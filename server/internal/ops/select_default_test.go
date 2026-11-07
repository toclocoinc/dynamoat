package ops

import (
	"net/http"
	"testing"
)

// With no Select, DynamoDB reads an index as ALL_PROJECTED_ATTRIBUTES (and a
// ProjectionExpression implies SPECIFIC_ATTRIBUTES), so a query on a KEYS_ONLY
// or INCLUDE index needs neither Select nor an ALL projection.
func TestIndexReadDefaultsToProjectedAttributes(t *testing.T) {
	_, h, _ := newOps(t)
	rec := post(t, h, "CreateTable", `{"TableName":"cards","BillingMode":"PAY_PER_REQUEST",
		"KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],
		"AttributeDefinitions":[{"AttributeName":"pk","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"S"},
			{"AttributeName":"entity","AttributeType":"S"},{"AttributeName":"at","AttributeType":"N"}],
		"GlobalSecondaryIndexes":[{"IndexName":"by-entity","KeySchema":[{"AttributeName":"entity","KeyType":"HASH"}],"Projection":{"ProjectionType":"KEYS_ONLY"}}],
		"LocalSecondaryIndexes":[{"IndexName":"by-at","KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},{"AttributeName":"at","KeyType":"RANGE"}],"Projection":{"ProjectionType":"INCLUDE","NonKeyAttributes":["name"]}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateTable: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(t, h, "PutItem", `{"TableName":"cards","Item":{"pk":{"S":"p"},"sk":{"S":"s"},"entity":{"S":"BACKLOG"},"at":{"N":"1"},"name":{"S":"n"},"secret":{"S":"x"}}}`); rec.Code != http.StatusOK {
		t.Fatalf("PutItem: %d %s", rec.Code, rec.Body.String())
	}
	for _, tc := range []struct {
		op, body string
		want     []string
	}{
		{"Query", `{"TableName":"cards","IndexName":"by-entity","KeyConditionExpression":"entity = :e","ExpressionAttributeValues":{":e":{"S":"BACKLOG"}}}`, []string{"entity", "pk", "sk"}},
		{"Query", `{"TableName":"cards","IndexName":"by-at","KeyConditionExpression":"pk = :p","ExpressionAttributeValues":{":p":{"S":"p"}}}`, []string{"at", "name", "pk", "sk"}},
		{"Query", `{"TableName":"cards","IndexName":"by-entity","KeyConditionExpression":"entity = :e","ProjectionExpression":"pk","ExpressionAttributeValues":{":e":{"S":"BACKLOG"}}}`, []string{"pk"}},
		{"Scan", `{"TableName":"cards","IndexName":"by-entity"}`, []string{"entity", "pk", "sk"}},
	} {
		rec := post(t, h, tc.op, tc.body)
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: %d %s", tc.op, tc.body, rec.Code, rec.Body.String())
			continue
		}
		items, _ := bodyJSON(t, rec)["Items"].([]any)
		if len(items) != 1 {
			t.Errorf("%s: %d items", tc.body, len(items))
			continue
		}
		got := items[0].(map[string]any)
		if len(got) != len(tc.want) {
			t.Errorf("%s: attributes %v, want %v", tc.body, got, tc.want)
			continue
		}
		for _, a := range tc.want {
			if _, ok := got[a]; !ok {
				t.Errorf("%s: missing %s in %v", tc.body, a, got)
			}
		}
	}
}

package ops

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func TestGetItemPutGetRoundTripT04(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "T04", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	const item = `{"id":{"S":"t04"},"n":{"N":"12345678901234567890123456789012345678"},"ss":{"SS":["alpha","beta","gamma"]},"b":{"B":"aGVsbG8="}}`
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"T04","Item":`+item+`}`), 200)

	m := wantStatus(t, post(t, h, "GetItem", `{"TableName":"T04","Key":{"id":{"S":"t04"}},"ConsistentRead":true}`), 200)
	want := map[string]any{
		"id": map[string]any{"S": "t04"},
		"n":  map[string]any{"N": "12345678901234567890123456789012345678"},
		"ss": map[string]any{"SS": []any{"alpha", "beta", "gamma"}},
		"b":  map[string]any{"B": "aGVsbG8="},
	}
	equalJSON(t, m["Item"], want, "Item")
}

func TestGetItemMissingReturnsEmptyObject(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	rec := post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"nope"}}}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "{}" {
		t.Errorf("body = %q, want {}", got)
	}
}

func TestGetItemProjection(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"},"a":{"S":"1"},"b":{"S":"2"},"c":{"S":"3"}}}`), 200)

	m := wantStatus(t, post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"x"}},"ProjectionExpression":"a, b"}`), 200)
	equalJSON(t, m["Item"], map[string]any{
		"a": map[string]any{"S": "1"},
		"b": map[string]any{"S": "2"},
	}, "Item")
}

func TestGetItemMissingTable(t *testing.T) {
	_, h, _ := newOps(t)
	rec := post(t, h, "GetItem", `{"TableName":"nosuchtable","Key":{"id":{"S":"x"}}}`)
	wantErr(t, rec, typResourceMissing, "Requested resource not found")
}

func TestGetItemKeyErrors(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	cases := []struct{ name, key, msg string }{
		{"missing", `{}`, "The provided key element does not match the schema"},
		{"wrong type", `{"id":{"N":"1"}}`, "The provided key element does not match the schema"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(t, h, "GetItem", `{"TableName":"test","Key":`+c.key+`}`)
			wantErr(t, rec, typValidation, c.msg)
		})
	}
}

func TestGetItemExpressionErrors(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	t.Run("unused name", func(t *testing.T) {
		rec := post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"x"}},"ExpressionAttributeNames":{"#unused":"a"},"ProjectionExpression":"id"}`)
		wantErr(t, rec, typValidation, "1 validation error detected: Value provided in ExpressionAttributeNames unused in expressions: keys: {#unused}")
	})
	t.Run("undefined name", func(t *testing.T) {
		rec := post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"x"}},"ProjectionExpression":"#n"}`)
		wantErr(t, rec, typValidation, "Invalid ProjectionExpression: An expression attribute name used in the document path is not defined; attribute name: #n")
	})
}

func TestGetItemLegacyAttributesToGet(t *testing.T) {
	_, h, _ := newOps(t)
	rec := post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"x"}},"AttributesToGet":["id"]}`)
	wantErr(t, rec, typValidation, "dynamoat does not support the legacy parameter AttributesToGet; use expressions")
}

func TestGetItemConsumedCapacity(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"}}}`), 200)

	m := wantStatus(t, post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"x"}},"ReturnConsumedCapacity":"TOTAL"}`), 200)
	equalJSON(t, m["ConsumedCapacity"], map[string]any{"TableName": "test", "CapacityUnits": float64(1)}, "ConsumedCapacity")

	m = wantStatus(t, post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"x"}}}`), 200)
	if _, ok := m["ConsumedCapacity"]; ok {
		t.Errorf("ConsumedCapacity present when ReturnConsumedCapacity omitted: %v", m["ConsumedCapacity"])
	}
}

func TestGetItemConsumedCapacityNumberIsFloat(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"}}}`), 200)

	rec := post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"x"}},"ReturnConsumedCapacity":"TOTAL"}`)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if got := string(raw["ConsumedCapacity"]); !strings.Contains(got, `"CapacityUnits":1.0`) {
		t.Errorf("ConsumedCapacity = %s, want CapacityUnits 1.0", got)
	}
}

func TestGetItemProjectionPresentWithEmptyResult(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"}}}`), 200)

	m := wantStatus(t, post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"x"}},"ProjectionExpression":"absent"}`), 200)
	if got, ok := m["Item"].(map[string]any); !ok || !reflect.DeepEqual(got, map[string]any{}) {
		t.Errorf("Item = %#v, want empty object", m["Item"])
	}
}

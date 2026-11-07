package ops

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func TestPutItemConditionalCreate(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	const body = `{"TableName":"test","Item":{"id":{"S":"x"},"v":{"S":"1"}},"ConditionExpression":"attribute_not_exists(id)"}`
	wantStatus(t, post(t, h, "PutItem", body), 200)
	rec := post(t, h, "PutItem", body)
	wantErr(t, rec, typConditional, "The conditional request failed")
}

func TestPutItemReturnValuesOnConditionCheckFailure(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"},"v":{"S":"old"}}}`), 200)

	rec := post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"},"v":{"S":"new"}},"ConditionExpression":"attribute_not_exists(id)","ReturnValuesOnConditionCheckFailure":"ALL_OLD"}`)
	wantErr(t, rec, typConditional, "The conditional request failed")
	m := bodyJSON(t, rec)
	equalJSON(t, m["Item"], map[string]any{
		"id": map[string]any{"S": "x"},
		"v":  map[string]any{"S": "old"},
	}, "Item on condition failure")
}

func TestPutItemReturnValuesAllOld(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"},"v":{"S":"old"}}}`), 200)

	rec := post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"},"v":{"S":"new"}},"ReturnValues":"ALL_OLD"}`)
	m := wantStatus(t, rec, 200)
	equalJSON(t, m["Attributes"], map[string]any{
		"id": map[string]any{"S": "x"},
		"v":  map[string]any{"S": "old"},
	}, "Attributes")

	// First write has no old item, so ALL_OLD returns nothing.
	rec = post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"y"}},"ReturnValues":"ALL_OLD"}`)
	m = wantStatus(t, rec, 200)
	if _, ok := m["Attributes"]; ok {
		t.Errorf("Attributes = %v, want absent", m["Attributes"])
	}
}

func TestPutItemKeyErrors(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	cases := []struct{ name, item, msg string }{
		{"missing key", `{"v":{"S":"1"}}`, "One or more parameter values were invalid: Missing the key id in the item"},
		{"wrong type", `{"id":{"N":"1"}}`, "One or more parameter values were invalid: Type mismatch for key id expected: S actual: N"},
		{"empty string key", `{"id":{"S":""}}`, "One or more parameter values are not valid. The AttributeValue for a key attribute cannot contain an empty string value. Key: id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(t, h, "PutItem", `{"TableName":"test","Item":`+c.item+`}`)
			wantErr(t, rec, typValidation, c.msg)
		})
	}
}

func TestPutItemEmptyBinaryKey(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "bin", storage.KeySchema{Hash: "id", HashType: attr.TypeB})

	rec := post(t, h, "PutItem", `{"TableName":"bin","Item":{"id":{"B":""}}}`)
	wantErr(t, rec, typValidation, "One or more parameter values are not valid. The AttributeValue for a key attribute cannot contain an empty binary value. Key: id")
}

func TestPutItemOver400KB(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	body, err := json.Marshal(map[string]any{
		"TableName": "test",
		"Item": map[string]any{
			"id":      map[string]any{"S": "big"},
			"payload": map[string]any{"S": strings.Repeat("a", 400*1024)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := post(t, h, "PutItem", string(body))
	wantErr(t, rec, typValidation, "Item size has exceeded the maximum allowed size")
}

func TestPutItemLegacyExpected(t *testing.T) {
	_, h, _ := newOps(t)
	rec := post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"}},"Expected":{"id":{"Value":{"S":"x"}}}}`)
	wantErr(t, rec, typValidation, "dynamoat does not support the legacy parameter Expected; use expressions")
}

func TestPutItemInvalidReturnValues(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	t.Run("unknown value", func(t *testing.T) {
		rec := post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"}},"ReturnValues":"BOGUS"}`)
		wantErr(t, rec, typValidation, "1 validation error detected: Value 'BOGUS' at 'returnValues' failed to satisfy constraint: Member must satisfy enum value set: [ALL_NEW, UPDATED_OLD, ALL_OLD, NONE, UPDATED_NEW]")
	})
	t.Run("known but not allowed", func(t *testing.T) {
		rec := post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"}},"ReturnValues":"ALL_NEW"}`)
		wantErr(t, rec, typValidation, "Return values set to invalid value")
	})
}

func TestPutItemExpressionErrors(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	t.Run("unused value", func(t *testing.T) {
		rec := post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"}},"ConditionExpression":"attribute_not_exists(id)","ExpressionAttributeValues":{":unused":{"S":"x"}}}`)
		wantErr(t, rec, typValidation, "1 validation error detected: Value provided in ExpressionAttributeValues unused in expressions: keys: {:unused}")
	})
	t.Run("undefined name", func(t *testing.T) {
		rec := post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"}},"ConditionExpression":"attribute_not_exists(#n)"}`)
		wantErr(t, rec, typValidation, "Invalid ConditionExpression: An expression attribute name used in the document path is not defined; attribute name: #n")
	})
}

func TestPutItemConcurrentConditionalCreate(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	const n = 20
	const body = `{"TableName":"test","Item":{"id":{"S":"same"},"v":{"S":"x"}},"ConditionExpression":"attribute_not_exists(id)"}`
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i] = post(t, h, "PutItem", body)
		}(i)
	}
	wg.Wait()

	ok, failed := 0, 0
	for _, rec := range recs {
		switch rec.Code {
		case 200:
			ok++
		case 400:
			m := bodyJSON(t, rec)
			if m["__type"] != typConditional {
				t.Errorf("__type = %v, want %s", m["__type"], typConditional)
			}
			failed++
		default:
			t.Errorf("status = %d; body: %s", rec.Code, rec.Body.String())
		}
	}
	if ok != 1 || failed != n-1 {
		t.Errorf("got %d success, %d conditional failures; want 1, %d", ok, failed, n-1)
	}
}

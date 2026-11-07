package ops

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// transactGetEntryJSON builds one TransactItems element: a Get for id on table.
func transactGetEntryJSON(table, id string) string {
	return `{"Get":{"TableName":"` + table + `","Key":{"id":{"S":"` + id + `"}}}}`
}

func TestTransactGetSuccessProjectionAndMissing(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	putItem(t, h, "test", `{"id":{"S":"a"},"name":{"S":"Alice"},"age":{"N":"30"}}`)
	putItem(t, h, "test", `{"id":{"S":"c"},"name":{"S":"Carol"},"age":{"N":"31"}}`)

	rec := post(t, h, "TransactGetItems", `{"TransactItems":[`+
		`{"Get":{"TableName":"test","Key":{"id":{"S":"a"}},"ProjectionExpression":"#n","ExpressionAttributeNames":{"#n":"name"}}},`+
		`{"Get":{"TableName":"test","Key":{"id":{"S":"missing"}}}},`+
		`{"Get":{"TableName":"test","Key":{"id":{"S":"c"}}}}`+
		`]}`)
	m := wantStatus(t, rec, 200)
	equalJSON(t, m["Responses"], []any{
		map[string]any{"Item": map[string]any{"name": map[string]any{"S": "Alice"}}},
		map[string]any{},
		map[string]any{"Item": map[string]any{
			"id":   map[string]any{"S": "c"},
			"name": map[string]any{"S": "Carol"},
			"age":  map[string]any{"N": "31"},
		}},
	}, "Responses")
}

func TestTransactGetConsumedCapacity(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	putItem(t, h, "test", `{"id":{"S":"a"}}`)

	m := wantStatus(t, post(t, h, "TransactGetItems", `{"TransactItems":[`+
		transactGetEntryJSON("test", "a")+
		`],"ReturnConsumedCapacity":"TOTAL"}`), 200)
	cc := m["ConsumedCapacity"].([]any)
	equalJSON(t, cc[0], map[string]any{"TableName": "test", "CapacityUnits": float64(1)}, "ConsumedCapacity[0]")

	// NONE omits the field.
	m = wantStatus(t, post(t, h, "TransactGetItems", `{"TransactItems":[`+
		transactGetEntryJSON("test", "a")+
		`]}`), 200)
	if _, ok := m["ConsumedCapacity"]; ok {
		t.Errorf("ConsumedCapacity present without ReturnConsumedCapacity")
	}
}

func TestTransactGetNoItems(t *testing.T) {
	_, h, _ := newOps(t)
	rec := post(t, h, "TransactGetItems", `{"TransactItems":[]}`)
	wantErr(t, rec, typValidation, "1 validation error detected: Value '[]' at 'transactItems' failed to satisfy constraint: Member must have length greater than or equal to 1")
}

func TestTransactGetTooManyItems(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	var b strings.Builder
	b.WriteString(`{"TransactItems":[`)
	for i := 0; i < 101; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(transactGetEntryJSON("test", fmt.Sprintf("i%d", i)))
	}
	b.WriteString(`]}`)

	rec := post(t, h, "TransactGetItems", b.String())
	wantErr(t, rec, typValidation, "")
}

func TestTransactGetDuplicateKeys(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	rec := post(t, h, "TransactGetItems", `{"TransactItems":[`+
		transactGetEntryJSON("test", "dup")+`,`+
		transactGetEntryJSON("test", "dup")+
		`]}`)
	wantErr(t, rec, typValidation, "Transaction request cannot include multiple operations on one item")
}

func TestTransactGetMissingTable(t *testing.T) {
	_, h, _ := newOps(t)
	rec := post(t, h, "TransactGetItems", `{"TransactItems":[`+
		transactGetEntryJSON("nosuchtable", "a")+
		`]}`)
	wantErr(t, rec, typResourceMissing, "Requested resource not found")
}

func TestTransactGetWrongKey(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	rec := post(t, h, "TransactGetItems", `{"TransactItems":[`+
		`{"Get":{"TableName":"test","Key":{"id":{"N":"1"}}}}`+
		`]}`)
	wantErr(t, rec, typValidation, "The provided key element does not match the schema")
}

func TestTransactGetFourMBAggregate(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "big", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	// Items near (but under) the 400 KB item limit: enough of them together
	// exceed the 4 MB TransactGetItems response limit.
	const n = 12
	payload := strings.Repeat("a", 390*1024)
	for i := 0; i < n; i++ {
		item := map[string]any{
			"id":      map[string]any{"S": fmt.Sprintf("k%d", i)},
			"payload": map[string]any{"S": payload},
		}
		b, err := json.Marshal(map[string]any{"TableName": "big", "Item": item})
		if err != nil {
			t.Fatal(err)
		}
		wantStatus(t, post(t, h, "PutItem", string(b)), 200)
	}

	var req strings.Builder
	req.WriteString(`{"TransactItems":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			req.WriteString(",")
		}
		req.WriteString(transactGetEntryJSON("big", fmt.Sprintf("k%d", i)))
	}
	req.WriteString(`]}`)

	rec := post(t, h, "TransactGetItems", req.String())
	wantErr(t, rec, typValidation, "Transaction request cannot include more than 4 MB of data")
}

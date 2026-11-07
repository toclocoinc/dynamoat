package ops

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func TestUpdateItemUpsertMissingItem(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	m := wantStatus(t, post(t, h, "UpdateItem",
		`{"TableName":"test","Key":{"id":{"S":"x"}},"UpdateExpression":"SET v = :v","ExpressionAttributeValues":{":v":{"S":"1"}},"ReturnValues":"ALL_NEW"}`), 200)
	equalJSON(t, m["Attributes"], map[string]any{
		"id": map[string]any{"S": "x"},
		"v":  map[string]any{"S": "1"},
	}, "Attributes")

	// With no UpdateExpression a missing item is created with only its key.
	wantStatus(t, post(t, h, "UpdateItem", `{"TableName":"test","Key":{"id":{"S":"y"}}}`), 200)
	g := wantStatus(t, post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"y"}}}`), 200)
	equalJSON(t, g["Item"], map[string]any{"id": map[string]any{"S": "y"}}, "key-only item")
}

func TestUpdateItemReturnValues(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	oldItem := map[string]any{
		"id": map[string]any{"S": ""},
		"a":  map[string]any{"S": "old"},
		"b":  map[string]any{"S": "2"},
	}
	newItem := map[string]any{
		"id": map[string]any{"S": ""},
		"a":  map[string]any{"S": "new"},
		"b":  map[string]any{"S": "3"},
	}

	cases := []struct {
		name string
		rv   string
	}{
		{"none", ""},
		{"all_old", "ALL_OLD"},
		{"all_new", "ALL_NEW"},
		{"updated_old", "UPDATED_OLD"},
		{"updated_new", "UPDATED_NEW"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := "x-" + c.name
			put := `{"TableName":"test","Item":{"id":{"S":"` + id + `"},"a":{"S":"old"},"b":{"S":"2"}}}`
			wantStatus(t, post(t, h, "PutItem", put), 200)

			rvField := ""
			if c.rv != "" {
				rvField = `,"ReturnValues":"` + c.rv + `"`
			}
			body := `{"TableName":"test","Key":{"id":{"S":"` + id + `"}},"UpdateExpression":"SET a = :a, b = :b","ExpressionAttributeValues":{":a":{"S":"new"},":b":{"S":"3"}}` + rvField + `}`
			m := wantStatus(t, post(t, h, "UpdateItem", body), 200)

			var want map[string]any
			switch c.rv {
			case "":
				if len(m) != 0 {
					t.Fatalf("body = %v, want {}", m)
				}
				return
			case "ALL_OLD":
				want = withID(oldItem, id)
			case "ALL_NEW":
				want = withID(newItem, id)
			case "UPDATED_OLD":
				want = map[string]any{
					"a": map[string]any{"S": "old"},
					"b": map[string]any{"S": "2"},
				}
			case "UPDATED_NEW":
				want = map[string]any{
					"a": map[string]any{"S": "new"},
					"b": map[string]any{"S": "3"},
				}
			}
			equalJSON(t, m["Attributes"], want, "Attributes")
		})
	}
}

func withID(m map[string]any, id string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	out["id"] = map[string]any{"S": id}
	return out
}

func TestUpdateItemConditionFailure(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"},"v":{"S":"1"}}}`), 200)

	// Without ALL_OLD the failure carries no Item.
	rec := post(t, h, "UpdateItem",
		`{"TableName":"test","Key":{"id":{"S":"x"}},"UpdateExpression":"SET v = :v","ExpressionAttributeValues":{":v":{"S":"2"},":want":{"S":"9"}},"ConditionExpression":"v = :want"}`)
	wantErr(t, rec, typConditional, "The conditional request failed")
	if _, ok := bodyJSON(t, rec)["Item"]; ok {
		t.Errorf("Item present without ALL_OLD")
	}

	// With ALL_OLD the failure carries the old item.
	rec = post(t, h, "UpdateItem",
		`{"TableName":"test","Key":{"id":{"S":"x"}},"UpdateExpression":"SET v = :v","ExpressionAttributeValues":{":v":{"S":"2"},":want":{"S":"9"}},"ConditionExpression":"v = :want","ReturnValuesOnConditionCheckFailure":"ALL_OLD"}`)
	wantErr(t, rec, typConditional, "The conditional request failed")
	equalJSON(t, bodyJSON(t, rec)["Item"], map[string]any{
		"id": map[string]any{"S": "x"},
		"v":  map[string]any{"S": "1"},
	}, "Item on condition failure")
}

func TestUpdateItemKeyAttribute(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	cases := []struct{ name, expr, values string }{
		{"set", `SET id = :v`, `,"ExpressionAttributeValues":{":v":{"S":"y"}}`},
		{"remove", `REMOVE id`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(t, h, "UpdateItem",
				`{"TableName":"test","Key":{"id":{"S":"x"}},"UpdateExpression":"`+c.expr+`"`+c.values+`}`)
			wantErr(t, rec, typValidation, "One or more parameter values were invalid: Cannot update attribute id. This attribute is part of the key")
		})
	}
}

func TestUpdateItemSetAdd(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"}}}`), 200)

	m := wantStatus(t, post(t, h, "UpdateItem",
		`{"TableName":"test","Key":{"id":{"S":"x"}},"UpdateExpression":"SET #name = :name, updated = :now ADD version :one","ExpressionAttributeNames":{"#name":"name"},"ExpressionAttributeValues":{":name":{"S":"alice"},":now":{"S":"2026-11-06"},":one":{"N":"1"}},"ReturnValues":"ALL_NEW"}`), 200)
	equalJSON(t, m["Attributes"], map[string]any{
		"id":      map[string]any{"S": "x"},
		"name":    map[string]any{"S": "alice"},
		"updated": map[string]any{"S": "2026-11-06"},
		"version": map[string]any{"N": "1"},
	}, "Attributes")
}

func TestUpdateItemCounter(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"}}}`), 200)

	const body = `{"TableName":"test","Key":{"id":{"S":"x"}},"UpdateExpression":"SET n = if_not_exists(n, :zero) + :inc","ExpressionAttributeValues":{":zero":{"N":"0"},":inc":{"N":"5"}}}`
	wantStatus(t, post(t, h, "UpdateItem", body), 200)
	wantStatus(t, post(t, h, "UpdateItem", body), 200)

	g := wantStatus(t, post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"x"}}}`), 200)
	equalJSON(t, g["Item"].(map[string]any)["n"], map[string]any{"N": "10"}, "n")
}

func TestUpdateItemSetRemove(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"},"s":{"S":"old"},"assignee":{"S":"bob"}}}`), 200)

	m := wantStatus(t, post(t, h, "UpdateItem",
		`{"TableName":"test","Key":{"id":{"S":"x"}},"UpdateExpression":"SET #s = :s REMOVE assignee","ExpressionAttributeNames":{"#s":"s"},"ExpressionAttributeValues":{":s":{"S":"new"}},"ReturnValues":"ALL_NEW"}`), 200)
	equalJSON(t, m["Attributes"], map[string]any{
		"id": map[string]any{"S": "x"},
		"s":  map[string]any{"S": "new"},
	}, "Attributes")
}

func TestUpdateItemOver400KB(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"}}}`), 200)

	body, err := json.Marshal(map[string]any{
		"TableName":        "test",
		"Key":              map[string]any{"id": map[string]any{"S": "x"}},
		"UpdateExpression": "SET payload = :p",
		"ExpressionAttributeValues": map[string]any{
			":p": map[string]any{"S": strings.Repeat("a", 400*1024)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := post(t, h, "UpdateItem", string(body))
	wantErr(t, rec, typValidation, "Item size has exceeded the maximum allowed size")
}

func TestUpdateItemGSIKeyTypeMismatch(t *testing.T) {
	_, h, store := newOps(t)
	if _, err := store.CreateTable(context.Background(), &storage.Table{
		Name: "gtest",
		Key:  storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		GSIs: []storage.Index{{Name: "gsi1", Key: storage.KeySchema{Hash: "gid", HashType: attr.TypeN}}},
	}); err != nil {
		t.Fatal(err)
	}

	rec := post(t, h, "UpdateItem",
		`{"TableName":"gtest","Key":{"id":{"S":"x"}},"UpdateExpression":"SET gid = :g","ExpressionAttributeValues":{":g":{"S":"wrong"}}}`)
	wantErr(t, rec, typValidation, "One or more parameter values were invalid: Type mismatch for Index Key gid Expected: N Actual: S IndexName: gsi1")
}

func TestUpdateItemConcurrentAdd(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"}}}`), 200)

	const n = 50
	const body = `{"TableName":"test","Key":{"id":{"S":"x"}},"UpdateExpression":"ADD n :one","ExpressionAttributeValues":{":one":{"N":"1"}}}`
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(t, h, "UpdateItem", body)
		}()
	}
	wg.Wait()

	g := wantStatus(t, post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"x"}}}`), 200)
	equalJSON(t, g["Item"].(map[string]any)["n"], map[string]any{"N": "50"}, "n")
}

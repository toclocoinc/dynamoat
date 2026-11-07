package ops

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func putReq(id, v string) map[string]any {
	return map[string]any{
		"PutRequest": map[string]any{
			"Item": map[string]any{
				"id": map[string]any{"S": id},
				"v":  map[string]any{"S": v},
			},
		},
	}
}

func deleteReq(id string) map[string]any {
	return map[string]any{
		"DeleteRequest": map[string]any{
			"Key": map[string]any{"id": map[string]any{"S": id}},
		},
	}
}

func TestBatchWriteItemMixedPutsAndDeletes(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "bat", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	// Seed the items the batch deletes.
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("d%d", i)
		wantStatus(t, post(t, h, "PutItem", fmt.Sprintf(`{"TableName":"bat","Item":{"id":{"S":"%s"},"v":{"S":"old"}}}`, id)), 200)
	}

	reqs := make([]any, 0, 25)
	for i := 0; i < 15; i++ {
		reqs = append(reqs, putReq(fmt.Sprintf("k%d", i), "new"))
	}
	for i := 0; i < 10; i++ {
		reqs = append(reqs, deleteReq(fmt.Sprintf("d%d", i)))
	}
	body, err := json.Marshal(map[string]any{"RequestItems": map[string]any{"bat": reqs}})
	if err != nil {
		t.Fatal(err)
	}
	m := wantStatus(t, post(t, h, "BatchWriteItem", string(body)), 200)
	equalJSON(t, m["UnprocessedItems"], map[string]any{}, "UnprocessedItems")

	for i := 0; i < 15; i++ {
		id := fmt.Sprintf("k%d", i)
		got := wantStatus(t, post(t, h, "GetItem", fmt.Sprintf(`{"TableName":"bat","Key":{"id":{"S":"%s"}}}`, id)), 200)
		equalJSON(t, got["Item"], map[string]any{
			"id": map[string]any{"S": id},
			"v":  map[string]any{"S": "new"},
		}, "put item "+id)
	}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("d%d", i)
		got := wantStatus(t, post(t, h, "GetItem", fmt.Sprintf(`{"TableName":"bat","Key":{"id":{"S":"%s"}}}`, id)), 200)
		if _, ok := got["Item"]; ok {
			t.Errorf("delete %s: Item = %v, want absent", id, got["Item"])
		}
	}
}

func TestBatchWriteItemTooManyRequests(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "bat", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	reqs := make([]any, 0, 26)
	for i := 0; i < 26; i++ {
		reqs = append(reqs, putReq(fmt.Sprintf("k%d", i), "v"))
	}
	body, err := json.Marshal(map[string]any{"RequestItems": map[string]any{"bat": reqs}})
	if err != nil {
		t.Fatal(err)
	}
	rec := post(t, h, "BatchWriteItem", string(body))
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	m := bodyJSON(t, rec)
	if m["__type"] != typValidation {
		t.Errorf("__type = %v, want %s", m["__type"], typValidation)
	}
	msg, _ := m["message"].(string)
	if !strings.HasPrefix(msg, "1 validation error detected: Value '") ||
		!strings.Contains(msg, " at 'requestItems' failed to satisfy constraint: Map value must satisfy constraint: [Member must have length less than or equal to 25, Member must have length greater than or equal to 1]") {
		t.Errorf("message = %q, want the requestItems length constraint", msg)
	}
}

func TestBatchWriteItemDuplicateKeys(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "bat", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	body, err := json.Marshal(map[string]any{
		"RequestItems": map[string]any{"bat": []any{putReq("dup", "1"), putReq("dup", "2")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := post(t, h, "BatchWriteItem", string(body))
	wantErr(t, rec, typValidation, "Provided list of item keys contains duplicates")
}

func TestBatchWriteItemInvalidItemWritesNothing(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "bat", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	big := map[string]any{
		"PutRequest": map[string]any{
			"Item": map[string]any{
				"id":      map[string]any{"S": "big"},
				"payload": map[string]any{"S": strings.Repeat("a", 400*1024)},
			},
		},
	}
	reqs := []any{putReq("k0", "v"), putReq("k1", "v"), big}
	body, err := json.Marshal(map[string]any{"RequestItems": map[string]any{"bat": reqs}})
	if err != nil {
		t.Fatal(err)
	}
	rec := post(t, h, "BatchWriteItem", string(body))
	wantErr(t, rec, typValidation, "Item size has exceeded the maximum allowed size")

	for _, id := range []string{"k0", "k1"} {
		got := wantStatus(t, post(t, h, "GetItem", fmt.Sprintf(`{"TableName":"bat","Key":{"id":{"S":"%s"}}}`, id)), 200)
		if _, ok := got["Item"]; ok {
			t.Errorf("%s: Item = %v, want absent (nothing should be written)", id, got["Item"])
		}
	}
}

func TestBatchWriteItemMissingTable(t *testing.T) {
	_, h, _ := newOps(t)
	body, err := json.Marshal(map[string]any{
		"RequestItems": map[string]any{"nosuchtable": []any{putReq("k0", "v")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := post(t, h, "BatchWriteItem", string(body))
	wantErr(t, rec, typResourceMissing, "Requested resource not found")
}

func TestBatchWriteItemConsumedCapacity(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "bat", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	body, err := json.Marshal(map[string]any{
		"RequestItems":           map[string]any{"bat": []any{putReq("k0", "v")}},
		"ReturnConsumedCapacity": "TOTAL",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := wantStatus(t, post(t, h, "BatchWriteItem", string(body)), 200)
	cc := m["ConsumedCapacity"].([]any)
	if len(cc) != 1 {
		t.Fatalf("ConsumedCapacity = %v, want 1 entry", cc)
	}
	equalJSON(t, cc[0], map[string]any{"TableName": "bat", "CapacityUnits": float64(1)}, "ConsumedCapacity[0]")
}

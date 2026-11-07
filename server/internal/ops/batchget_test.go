package ops

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// batchKeys builds n key objects {"id":{"S":"<prefix><i>"}}.
func batchKeys(prefix string, n int) []any {
	keys := make([]any, 0, n)
	for i := 0; i < n; i++ {
		keys = append(keys, map[string]any{"id": map[string]any{"S": fmt.Sprintf("%s%d", prefix, i)}})
	}
	return keys
}

func TestBatchGetItemHundredKeysTwoTablesProjection(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "bat1", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	makeTable(t, store, "bat2", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("k%d", i)
		wantStatus(t, post(t, h, "PutItem", fmt.Sprintf(`{"TableName":"bat1","Item":{"id":{"S":"%s"},"extra":{"S":"x"}}}`, id)), 200)
		wantStatus(t, post(t, h, "PutItem", fmt.Sprintf(`{"TableName":"bat2","Item":{"id":{"S":"%s"},"extra":{"S":"y"}}}`, id)), 200)
	}

	req := map[string]any{
		"RequestItems": map[string]any{
			"bat1": map[string]any{
				"Keys":                 batchKeys("k", 50),
				"ProjectionExpression": "id",
			},
			"bat2": map[string]any{
				"Keys": batchKeys("k", 50),
			},
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	m := wantStatus(t, post(t, h, "BatchGetItem", string(body)), 200)

	responses := m["Responses"].(map[string]any)
	t1 := responses["bat1"].([]any)
	t2 := responses["bat2"].([]any)
	if len(t1) != 50 || len(t2) != 50 {
		t.Fatalf("Responses counts = T1:%d T2:%d, want 50 each", len(t1), len(t2))
	}
	for _, it := range t1 {
		item := it.(map[string]any)
		if _, ok := item["id"]; !ok {
			t.Errorf("T1 projection dropped id: %v", item)
		}
		if _, ok := item["extra"]; ok {
			t.Errorf("T1 projection kept extra: %v", item)
		}
	}
	for _, it := range t2 {
		item := it.(map[string]any)
		if _, ok := item["id"]; !ok || item["id"] == nil {
			t.Errorf("T2 item missing id: %v", item)
		}
		if _, ok := item["extra"]; !ok {
			t.Errorf("T2 item missing extra (no projection expected): %v", item)
		}
	}
	equalJSON(t, m["UnprocessedKeys"], map[string]any{}, "UnprocessedKeys")
}

func TestBatchGetItemTooManyKeys(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "bat", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	req := map[string]any{
		"RequestItems": map[string]any{"bat": map[string]any{"Keys": batchKeys("k", 101)}},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := post(t, h, "BatchGetItem", string(body))
	wantErr(t, rec, typValidation, "Too many items requested for the BatchGetItem call")
}

func TestBatchGetItemDuplicateKeys(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "bat", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	req := map[string]any{
		"RequestItems": map[string]any{
			"bat": map[string]any{
				"Keys": []any{
					map[string]any{"id": map[string]any{"S": "dup"}},
					map[string]any{"id": map[string]any{"S": "dup"}},
				},
			},
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := post(t, h, "BatchGetItem", string(body))
	wantErr(t, rec, typValidation, "Provided list of item keys contains duplicates")
}

func TestBatchGetItemMissingTable(t *testing.T) {
	_, h, _ := newOps(t)
	req := map[string]any{
		"RequestItems": map[string]any{
			"nosuchtable": map[string]any{"Keys": batchKeys("k", 1)},
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := post(t, h, "BatchGetItem", string(body))
	wantErr(t, rec, typResourceMissing, "Requested resource not found")
}

func TestBatchGetItemConsumedCapacity(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "bat1", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	makeTable(t, store, "bat2", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"bat1","Item":{"id":{"S":"a"}}}`), 200)

	req := map[string]any{
		"RequestItems": map[string]any{
			"bat1": map[string]any{"Keys": []any{map[string]any{"id": map[string]any{"S": "a"}}}},
			"bat2": map[string]any{"Keys": []any{map[string]any{"id": map[string]any{"S": "a"}}}},
		},
		"ReturnConsumedCapacity": "TOTAL",
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	m := wantStatus(t, post(t, h, "BatchGetItem", string(body)), 200)
	cc := m["ConsumedCapacity"].([]any)
	if len(cc) != 2 {
		t.Fatalf("ConsumedCapacity = %v, want 2 entries", cc)
	}
	equalJSON(t, cc[0], map[string]any{"TableName": "bat1", "CapacityUnits": float64(1)}, "ConsumedCapacity[0]")
	equalJSON(t, cc[1], map[string]any{"TableName": "bat2", "CapacityUnits": float64(1)}, "ConsumedCapacity[1]")
}

func TestBatchGetItemSixteenMBSplits(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "big", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	// A payload near (but under) the 400 KB item limit: enough of these items
	// together exceed the 16 MB BatchGetItem response limit.
	const n = 45
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

	body, err := json.Marshal(map[string]any{
		"RequestItems": map[string]any{"big": map[string]any{"Keys": batchKeys("k", n)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := wantStatus(t, post(t, h, "BatchGetItem", string(body)), 200)

	returned := 0
	if arr, ok := m["Responses"].(map[string]any)["big"].([]any); ok {
		returned = len(arr)
	}
	remaining := 0
	if table, ok := m["UnprocessedKeys"].(map[string]any)["big"].(map[string]any); ok {
		if keys, ok := table["Keys"].([]any); ok {
			remaining = len(keys)
		}
	}
	if returned == 0 || remaining == 0 {
		t.Fatalf("returned=%d remaining=%d, want the response split across the 16 MB limit", returned, remaining)
	}
	if returned+remaining != n {
		t.Errorf("returned=%d + remaining=%d = %d, want %d", returned, remaining, returned+remaining, n)
	}
}

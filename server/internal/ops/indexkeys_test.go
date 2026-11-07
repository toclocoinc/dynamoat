package ops

import (
	"context"
	"errors"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
)

// makeGSITable creates a table whose base key is id (S) plus one GSI, gsi1,
// with the given key schema.
func makeGSITable(t *testing.T, store *memstore.Store, name string, gsi storage.KeySchema) {
	t.Helper()
	if _, err := store.CreateTable(context.Background(), &storage.Table{
		Name: name,
		Key:  storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		GSIs: []storage.Index{{Name: "gsi1", Key: gsi}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckIndexKeys(t *testing.T) {
	tbl := &storage.Table{GSIs: []storage.Index{
		{Name: "gsi1", Key: storage.KeySchema{Hash: "gid", HashType: attr.TypeN}},
		{Name: "gsi2", Key: storage.KeySchema{Hash: "h", HashType: attr.TypeS, Range: "r", RangeType: attr.TypeB}},
	}}

	cases := []struct {
		name string
		item attr.Item
		want string // empty means no error
	}{
		{
			name: "wrong hash type",
			item: attr.Item{"gid": {Type: attr.TypeS, S: "x"}},
			want: "One or more parameter values were invalid: Type mismatch for Index Key gid Expected: N Actual: S IndexName: gsi1",
		},
		{
			name: "wrong range type",
			item: attr.Item{"h": {Type: attr.TypeS, S: "x"}, "r": {Type: attr.TypeS, S: "y"}},
			want: "One or more parameter values were invalid: Type mismatch for Index Key r Expected: B Actual: S IndexName: gsi2",
		},
		{
			name: "empty string hash",
			item: attr.Item{"h": {Type: attr.TypeS, S: ""}, "r": {Type: attr.TypeB, B: []byte{1}}},
			want: "One or more parameter values are not valid. A value specified for a secondary index key is not supported. The AttributeValue for a member of the IndexKey attribute cannot be an empty string. IndexName: gsi2, IndexKey: h",
		},
		{
			name: "empty binary range",
			item: attr.Item{"h": {Type: attr.TypeS, S: "x"}, "r": {Type: attr.TypeB, B: []byte{}}},
			want: "One or more parameter values are not valid. A value specified for a secondary index key is not supported. The AttributeValue for a member of the IndexKey attribute cannot be an empty string. IndexName: gsi2, IndexKey: r",
		},
		{
			name: "missing index attributes",
			item: attr.Item{"id": {Type: attr.TypeS, S: "x"}},
		},
		{
			name: "valid",
			item: attr.Item{"gid": {Type: attr.TypeN, N: "5"}, "h": {Type: attr.TypeS, S: "x"}, "r": {Type: attr.TypeB, B: []byte{1}}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkIndexKeys(tbl, c.item)
			if c.want == "" {
				if err != nil {
					t.Fatalf("checkIndexKeys() = %v, want nil", err)
				}
				return
			}
			var de *ddberr.Error
			if !errors.As(err, &de) {
				t.Fatalf("checkIndexKeys() = %v, want *ddberr.Error", err)
			}
			if de.Message != c.want {
				t.Errorf("message = %q, want %q", de.Message, c.want)
			}
		})
	}
}

func TestPutItemIndexKey(t *testing.T) {
	_, h, store := newOps(t)
	makeGSITable(t, store, "gtest", storage.KeySchema{Hash: "gid", HashType: attr.TypeN})

	t.Run("wrong type rejected and nothing written", func(t *testing.T) {
		rec := post(t, h, "PutItem", `{"TableName":"gtest","Item":{"id":{"S":"x"},"gid":{"S":"wrong"}}}`)
		wantErr(t, rec, typValidation, "One or more parameter values were invalid: Type mismatch for Index Key gid Expected: N Actual: S IndexName: gsi1")
		got := wantStatus(t, post(t, h, "GetItem", `{"TableName":"gtest","Key":{"id":{"S":"x"}}}`), 200)
		if _, ok := got["Item"]; ok {
			t.Errorf("Item = %v, want absent", got["Item"])
		}
	})

	t.Run("empty string rejected", func(t *testing.T) {
		makeGSITable(t, store, "gtest-s", storage.KeySchema{Hash: "sidx", HashType: attr.TypeS})
		rec := post(t, h, "PutItem", `{"TableName":"gtest-s","Item":{"id":{"S":"x"},"sidx":{"S":""}}}`)
		wantErr(t, rec, typValidation, "One or more parameter values are not valid. A value specified for a secondary index key is not supported. The AttributeValue for a member of the IndexKey attribute cannot be an empty string. IndexName: gsi1, IndexKey: sidx")
	})

	t.Run("missing index attribute accepted", func(t *testing.T) {
		wantStatus(t, post(t, h, "PutItem", `{"TableName":"gtest","Item":{"id":{"S":"y"}}}`), 200)
	})
}

func TestUpdateItemIndexKey(t *testing.T) {
	_, h, store := newOps(t)
	makeGSITable(t, store, "gtest", storage.KeySchema{Hash: "gid", HashType: attr.TypeN})

	t.Run("wrong type rejected and nothing written", func(t *testing.T) {
		rec := post(t, h, "UpdateItem", `{"TableName":"gtest","Key":{"id":{"S":"x"}},"UpdateExpression":"SET gid = :g","ExpressionAttributeValues":{":g":{"S":"wrong"}}}`)
		wantErr(t, rec, typValidation, "One or more parameter values were invalid: Type mismatch for Index Key gid Expected: N Actual: S IndexName: gsi1")
		got := wantStatus(t, post(t, h, "GetItem", `{"TableName":"gtest","Key":{"id":{"S":"x"}}}`), 200)
		if _, ok := got["Item"]; ok {
			t.Errorf("Item = %v, want absent", got["Item"])
		}
	})

	t.Run("empty string rejected", func(t *testing.T) {
		makeGSITable(t, store, "gtest-s", storage.KeySchema{Hash: "sidx", HashType: attr.TypeS})
		rec := post(t, h, "UpdateItem", `{"TableName":"gtest-s","Key":{"id":{"S":"x"}},"UpdateExpression":"SET sidx = :g","ExpressionAttributeValues":{":g":{"S":""}}}`)
		wantErr(t, rec, typValidation, "One or more parameter values are not valid. A value specified for a secondary index key is not supported. The AttributeValue for a member of the IndexKey attribute cannot be an empty string. IndexName: gsi1, IndexKey: sidx")
	})

	t.Run("missing index attribute accepted", func(t *testing.T) {
		wantStatus(t, post(t, h, "UpdateItem", `{"TableName":"gtest","Key":{"id":{"S":"y"}},"UpdateExpression":"SET v = :v","ExpressionAttributeValues":{":v":{"S":"1"}}}`), 200)
		got := wantStatus(t, post(t, h, "GetItem", `{"TableName":"gtest","Key":{"id":{"S":"y"}}}`), 200)
		equalJSON(t, got["Item"].(map[string]any)["v"], map[string]any{"S": "1"}, "v")
	})
}

func TestBatchWriteItemIndexKey(t *testing.T) {
	_, h, store := newOps(t)
	makeGSITable(t, store, "gtest", storage.KeySchema{Hash: "gid", HashType: attr.TypeN})

	t.Run("wrong type rejected and nothing written", func(t *testing.T) {
		rec := post(t, h, "BatchWriteItem", `{"RequestItems":{"gtest":[`+
			`{"PutRequest":{"Item":{"id":{"S":"x"},"gid":{"S":"wrong"}}}},`+
			`{"PutRequest":{"Item":{"id":{"S":"ok"},"gid":{"N":"1"}}}}`+
			`]}}`)
		wantErr(t, rec, typValidation, "One or more parameter values were invalid: Type mismatch for Index Key gid Expected: N Actual: S IndexName: gsi1")
		for _, id := range []string{"x", "ok"} {
			got := wantStatus(t, post(t, h, "GetItem", `{"TableName":"gtest","Key":{"id":{"S":"`+id+`"}}}`), 200)
			if _, ok := got["Item"]; ok {
				t.Errorf("%s: Item = %v, want absent", id, got["Item"])
			}
		}
	})

	t.Run("missing index attribute accepted", func(t *testing.T) {
		wantStatus(t, post(t, h, "BatchWriteItem", `{"RequestItems":{"gtest":[{"PutRequest":{"Item":{"id":{"S":"y"}}}}]}}`), 200)
	})
}

func TestTransactPutIndexKey(t *testing.T) {
	_, h, store := newOps(t)
	makeGSITable(t, store, "gtest", storage.KeySchema{Hash: "gid", HashType: attr.TypeN})

	rec := post(t, h, "TransactWriteItems", `{"TransactItems":[`+
		`{"Put":{"TableName":"gtest","Item":{"id":{"S":"a"},"gid":{"S":"wrong"}}}},`+
		`{"Put":{"TableName":"gtest","Item":{"id":{"S":"b"},"gid":{"N":"1"}}}}`+
		`]}`)
	wantErr(t, rec, typTransactionCanceled, "Transaction cancelled, please refer cancellation reasons for specific reasons [ValidationError, None]")
	m := bodyJSON(t, rec)
	equalJSON(t, m["CancellationReasons"], []any{
		map[string]any{"Code": "ValidationError", "Message": "One or more parameter values were invalid: Type mismatch for Index Key gid Expected: N Actual: S IndexName: gsi1"},
		map[string]any{"Code": "None"},
	}, "CancellationReasons")
	for _, id := range []string{"a", "b"} {
		got := wantStatus(t, post(t, h, "GetItem", `{"TableName":"gtest","Key":{"id":{"S":"`+id+`"}}}`), 200)
		if _, ok := got["Item"]; ok {
			t.Errorf("%s: Item = %v, want absent", id, got["Item"])
		}
	}
}

func TestTransactUpdateIndexKey(t *testing.T) {
	_, h, store := newOps(t)
	makeGSITable(t, store, "gtest", storage.KeySchema{Hash: "gid", HashType: attr.TypeN})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"gtest","Item":{"id":{"S":"a"}}}`), 200)

	rec := post(t, h, "TransactWriteItems", `{"TransactItems":[`+
		`{"Update":{"TableName":"gtest","Key":{"id":{"S":"a"}},"UpdateExpression":"SET gid = :g","ExpressionAttributeValues":{":g":{"S":"wrong"}}}},`+
		`{"Put":{"TableName":"gtest","Item":{"id":{"S":"b"},"gid":{"N":"1"}}}}`+
		`]}`)
	wantErr(t, rec, typTransactionCanceled, "Transaction cancelled, please refer cancellation reasons for specific reasons [ValidationError, None]")
	m := bodyJSON(t, rec)
	equalJSON(t, m["CancellationReasons"], []any{
		map[string]any{"Code": "ValidationError", "Message": "One or more parameter values were invalid: Type mismatch for Index Key gid Expected: N Actual: S IndexName: gsi1"},
		map[string]any{"Code": "None"},
	}, "CancellationReasons")

	// The update on a was rolled back and b was never written.
	got := wantStatus(t, post(t, h, "GetItem", `{"TableName":"gtest","Key":{"id":{"S":"a"}}}`), 200)
	if item, _ := got["Item"].(map[string]any); item != nil {
		if _, ok := item["gid"]; ok {
			t.Errorf("a.gid = %v, want absent", item["gid"])
		}
	}
	got = wantStatus(t, post(t, h, "GetItem", `{"TableName":"gtest","Key":{"id":{"S":"b"}}}`), 200)
	if _, ok := got["Item"]; ok {
		t.Errorf("b: Item = %v, want absent", got["Item"])
	}
}

func TestTransactMissingIndexAttributeAccepted(t *testing.T) {
	_, h, store := newOps(t)
	makeGSITable(t, store, "gtest", storage.KeySchema{Hash: "gid", HashType: attr.TypeN})

	wantStatus(t, post(t, h, "TransactWriteItems", `{"TransactItems":[`+
		`{"Put":{"TableName":"gtest","Item":{"id":{"S":"a"}}}}`+
		`]}`), 200)
	got := wantStatus(t, post(t, h, "GetItem", `{"TableName":"gtest","Key":{"id":{"S":"a"}}}`), 200)
	equalJSON(t, got["Item"], map[string]any{"id": map[string]any{"S": "a"}}, "Item")
}

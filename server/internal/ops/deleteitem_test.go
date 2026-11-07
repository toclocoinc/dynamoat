package ops

import (
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func TestDeleteItemRemovesItem(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"},"v":{"S":"1"}}}`), 200)

	wantStatus(t, post(t, h, "DeleteItem", `{"TableName":"test","Key":{"id":{"S":"x"}}}`), 200)
	rec := post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"x"}}}`)
	m := wantStatus(t, rec, 200)
	if _, ok := m["Item"]; ok {
		t.Errorf("Item = %v, want absent", m["Item"])
	}
}

func TestDeleteItemAllOld(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"},"v":{"S":"old"}}}`), 200)

	m := wantStatus(t, post(t, h, "DeleteItem", `{"TableName":"test","Key":{"id":{"S":"x"}},"ReturnValues":"ALL_OLD"}`), 200)
	equalJSON(t, m["Attributes"], map[string]any{
		"id": map[string]any{"S": "x"},
		"v":  map[string]any{"S": "old"},
	}, "Attributes")
}

func TestDeleteItemCondition(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"id":{"S":"x"},"v":{"S":"1"}}}`), 200)

	// Condition fails, so the item stays.
	rec := post(t, h, "DeleteItem", `{"TableName":"test","Key":{"id":{"S":"x"}},"ConditionExpression":"v = :want","ExpressionAttributeValues":{":want":{"S":"2"}}}`)
	wantErr(t, rec, typConditional, "The conditional request failed")
	wantStatus(t, post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"x"}}}`), 200)

	// Condition passes, so the item is deleted.
	wantStatus(t, post(t, h, "DeleteItem", `{"TableName":"test","Key":{"id":{"S":"x"}},"ConditionExpression":"v = :want","ExpressionAttributeValues":{":want":{"S":"1"}}}`), 200)
}

func TestDeleteItemKeyErrors(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	t.Run("missing key", func(t *testing.T) {
		rec := post(t, h, "DeleteItem", `{"TableName":"test","Key":{}}`)
		wantErr(t, rec, typValidation, "The provided key element does not match the schema")
	})
	t.Run("wrong type", func(t *testing.T) {
		rec := post(t, h, "DeleteItem", `{"TableName":"test","Key":{"id":{"N":"1"}}}`)
		wantErr(t, rec, typValidation, "The provided key element does not match the schema")
	})
}

func TestDeleteItemMissingTable(t *testing.T) {
	_, h, _ := newOps(t)
	rec := post(t, h, "DeleteItem", `{"TableName":"nosuchtable","Key":{"id":{"S":"x"}}}`)
	wantErr(t, rec, typResourceMissing, "Requested resource not found")
}

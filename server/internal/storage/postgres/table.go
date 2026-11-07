package postgres

import (
	"encoding/json"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// cloneTable returns a deep copy so CreateTable can assign IDs and statuses
// without mutating the caller's definition.
func cloneTable(t *storage.Table) *storage.Table {
	if t == nil {
		return nil
	}
	out := *t
	out.AttrDefs = cloneTypes(t.AttrDefs)
	out.GSIs = make([]storage.Index, len(t.GSIs))
	for i, g := range t.GSIs {
		out.GSIs[i] = g
		out.GSIs[i].NonKeyAttrs = append([]string(nil), g.NonKeyAttrs...)
	}
	out.Tags = cloneTags(t.Tags)
	return &out
}

func cloneTypes(m map[string]attr.Type) map[string]attr.Type {
	if m == nil {
		return nil
	}
	out := make(map[string]attr.Type, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneTags(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// assignGSIIDs gives a distinct non-zero ID to every GSI whose ID is 0 and
// returns the positions it assigned.
func assignGSIIDs(t *storage.Table) []int {
	next := int16(1)
	for i := range t.GSIs {
		if t.GSIs[i].ID >= next {
			next = t.GSIs[i].ID + 1
		}
	}
	var assigned []int
	for i := range t.GSIs {
		if t.GSIs[i].ID == 0 {
			t.GSIs[i].ID = next
			next++
			assigned = append(assigned, i)
		}
	}
	return assigned
}

func marshalTable(t *storage.Table) ([]byte, error) {
	return json.Marshal(t)
}

func unmarshalTable(def []byte) (*storage.Table, error) {
	var t storage.Table
	if err := json.Unmarshal(def, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

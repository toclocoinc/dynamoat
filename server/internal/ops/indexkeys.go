package ops

import (
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// checkIndexKeys verifies that every present GSI key attribute has the type the
// index key schema expects, and that a string or binary GSI key value is not
// empty. A missing attribute is fine: the item is simply not in that sparse
// index. The base table key is checked by storage.KeyOf.
func checkIndexKeys(t *storage.Table, item attr.Item) error {
	for _, g := range t.GSIs {
		if v, ok := item[g.Key.Hash]; ok {
			if v.Type != g.Key.HashType {
				return gsiKeyTypeError(g.Name, g.Key.Hash, g.Key.HashType, v.Type)
			}
			if err := checkIndexKeyValue(g.Name, g.Key.Hash, v); err != nil {
				return err
			}
		}
		if g.Key.Range != "" {
			if v, ok := item[g.Key.Range]; ok {
				if v.Type != g.Key.RangeType {
					return gsiKeyTypeError(g.Name, g.Key.Range, g.Key.RangeType, v.Type)
				}
				if err := checkIndexKeyValue(g.Name, g.Key.Range, v); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkIndexKeyValue rejects an empty S or B index key value.
func checkIndexKeyValue(index, name string, v attr.Value) error {
	if (v.Type == attr.TypeS && v.S == "") || (v.Type == attr.TypeB && len(v.B) == 0) {
		return gsiKeyEmptyError(index, name)
	}
	return nil
}

func gsiKeyTypeError(index, name string, want, got attr.Type) error {
	return ddberr.Validation("One or more parameter values were invalid: Type mismatch for Index Key %s Expected: %s Actual: %s IndexName: %s",
		name, want.String(), got.String(), index)
}

func gsiKeyEmptyError(index, name string) error {
	return ddberr.Validation("One or more parameter values are not valid. A value specified for a secondary index key is not supported. The AttributeValue for a member of the IndexKey attribute cannot be an empty string. IndexName: %s, IndexKey: %s",
		index, name)
}

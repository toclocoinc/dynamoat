package storage

import (
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

// KeyOf extracts and checks the primary key of item (a full item or a Key
// map) against schema. extra says whether attributes other than the key are
// allowed: false for a Key parameter (GetItem, DeleteItem), true for an Item.
func KeyOf(schema KeySchema, item attr.Item, extra bool) (Key, error) {
	var k Key
	var err error
	if k.Hash, err = keyPart(schema.Hash, schema.HashType, item, extra); err != nil {
		return Key{}, err
	}
	n := 1
	if schema.Range != "" {
		if k.Range, err = keyPart(schema.Range, schema.RangeType, item, extra); err != nil {
			return Key{}, err
		}
		n = 2
	}
	if !extra && len(item) != n {
		return Key{}, ddberr.Validation("The provided key element does not match the schema")
	}
	return k, nil
}

func keyPart(name string, want attr.Type, item attr.Item, extra bool) (attr.Value, error) {
	v, ok := item[name]
	if !ok {
		if extra {
			return attr.Value{}, ddberr.Validation("One or more parameter values were invalid: Missing the key %s in the item", name)
		}
		return attr.Value{}, ddberr.Validation("The provided key element does not match the schema")
	}
	if v.Type != want {
		if extra {
			return attr.Value{}, ddberr.Validation("One or more parameter values were invalid: Type mismatch for key %s expected: %s actual: %s", name, want, v.Type)
		}
		return attr.Value{}, ddberr.Validation("The provided key element does not match the schema")
	}
	if (v.Type == attr.TypeS && v.S == "") || (v.Type == attr.TypeB && len(v.B) == 0) {
		return attr.Value{}, ddberr.Validation("One or more parameter values are not valid. The AttributeValue for a key attribute cannot contain an empty %s value. Key: %s", typeWord(v.Type), name)
	}
	return v, nil
}

func typeWord(t attr.Type) string {
	if t == attr.TypeB {
		return "binary"
	}
	return "string"
}

// KeyItem returns the key attributes of item for schema: the shape of
// LastEvaluatedKey and of a KEYS_ONLY projection.
func KeyItem(schema KeySchema, item attr.Item) attr.Item {
	out := attr.Item{schema.Hash: item[schema.Hash]}
	if schema.Range != "" {
		out[schema.Range] = item[schema.Range]
	}
	return out
}

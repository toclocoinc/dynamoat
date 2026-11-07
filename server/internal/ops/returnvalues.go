package ops

import (
	"encoding/json"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// ReturnValues accepted by the operations that write or delete an item.
const (
	returnNone       = "NONE"
	returnAllOld     = "ALL_OLD"
	returnAllNew     = "ALL_NEW"
	returnUpdatedOld = "UPDATED_OLD"
	returnUpdatedNew = "UPDATED_NEW"
)

// checkReturnValues validates ReturnValues for PutItem and DeleteItem, which
// accept only NONE (the default) and ALL_OLD. It returns the canonical value.
func checkReturnValues(v string) (string, error) {
	switch v {
	case "", returnNone:
		return returnNone, nil
	case returnAllOld:
		return returnAllOld, nil
	case returnAllNew, returnUpdatedOld, returnUpdatedNew:
		return "", ddberr.Validation("Return values set to invalid value")
	default:
		return "", ddberr.Validation("1 validation error detected: Value '%s' at 'returnValues' failed to satisfy constraint: Member must satisfy enum value set: [ALL_NEW, UPDATED_OLD, ALL_OLD, NONE, UPDATED_NEW]", v)
	}
}

// checkReturnValuesOnConditionCheckFailure validates the parameter of the same
// name, which accepts only NONE (the default) and ALL_OLD.
func checkReturnValuesOnConditionCheckFailure(v string) (string, error) {
	switch v {
	case "", returnNone:
		return returnNone, nil
	case returnAllOld:
		return returnAllOld, nil
	default:
		return "", ddberr.Validation("1 validation error detected: Value '%s' at 'returnValuesOnConditionCheckFailure' failed to satisfy constraint: Member must satisfy enum value set: [ALL_OLD, NONE]", v)
	}
}

// conditionalCheckFailed builds the ConditionalCheckFailed error for a write
// whose condition evaluated false, carrying the old item when the request asked
// for ALL_OLD and one existed.
func conditionalCheckFailed(old attr.Item, rvocf string) error {
	if rvocf == returnAllOld && old != nil {
		return ddberr.ConditionalCheckFailed(itemJSON(old))
	}
	return ddberr.ConditionalCheckFailed(nil)
}

// itemJSON is an item in the shape ddberr.Extra expects: a map that marshals
// to DynamoDB typed JSON.
func itemJSON(it attr.Item) map[string]any {
	out := make(map[string]any, len(it))
	for k, v := range it {
		out[k] = v
	}
	return out
}

// consumedCapacity reports a fixed 1.0 CapacityUnits when ReturnConsumedCapacity
// is TOTAL or INDEXES, so clients that read it keep working; dynamoat does not
// meter. It is nil (omitted) otherwise.
func consumedCapacity(t *storage.Table, rcc string) map[string]any {
	if rcc == "TOTAL" || rcc == "INDEXES" {
		return map[string]any{"TableName": t.Name, "CapacityUnits": json.Number("1.0")}
	}
	return nil
}

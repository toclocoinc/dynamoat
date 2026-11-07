// Package streams renders the DynamoDB Streams record and shard model shared by
// the streams operations (internal/ops/streams.go) and the stream dispatcher.
package streams

import (
	"crypto/sha256"
	"fmt"

	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// StreamARN is the ARN of a table's stream: the table ARN plus the stream
// label, exactly as DescribeTable reports LatestStreamArn.
func StreamARN(region, table, label string) string {
	return "arn:aws:dynamodb:" + region + ":000000000000:table/" + table + "/stream/" + label
}

// ShardID is the single shard of a table's stream. The trailing hex digits are
// the table ID, so the shard ID is stable for the life of the table.
func ShardID(tableID int32) string {
	return fmt.Sprintf("shardId-00000000000000000000-%08x", uint32(tableID))
}

// Record renders one storage.StreamRecord as DynamoDB's stream record shape.
func Record(t *storage.Table, region string, r storage.StreamRecord) map[string]any {
	ddb := map[string]any{
		"ApproximateCreationDateTime": float64(r.At.UnixMilli()) / 1000,
		"Keys":                        r.Keys,
		"SequenceNumber":              fmt.Sprintf("%021d", r.Seq),
		"SizeBytes":                   r.Size,
		"StreamViewType":              t.StreamView,
	}
	if r.New != nil {
		ddb["NewImage"] = r.New
	}
	if r.Old != nil {
		ddb["OldImage"] = r.Old
	}

	rec := map[string]any{
		"eventID":      eventID(t.ID, r.Seq),
		"eventName":    r.Event,
		"eventVersion": "1.1",
		"eventSource":  "aws:dynamodb",
		"awsRegion":    region,
		"dynamodb":     ddb,
	}
	if r.TTL {
		rec["userIdentity"] = map[string]any{
			"type":        "Service",
			"principalId": "dynamodb.amazonaws.com",
		}
	}
	return rec
}

// eventID derives the 32 hex character event ID from the table ID and the
// record's sequence number, so the same record always reports the same ID.
func eventID(tableID int32, seq int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", tableID, seq)))
	return fmt.Sprintf("%x", sum[:16])
}

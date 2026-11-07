package ops

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/streams"
)

// tableArn is the ARN dynamoat reports for a table. The account id is the
// fixed local placeholder 000000000000, as DynamoDB Local uses.
func (o *Ops) tableArn(t *storage.Table) string {
	return o.tableArnForName(t.Name)
}

// tableArnForName builds a table ARN from a table name.
func (o *Ops) tableArnForName(name string) string {
	return "arn:aws:dynamodb:" + o.region() + ":000000000000:table/" + name
}

// tableDescFlags selects the parts of a TableDescription that differ between
// the operations that return one. DynamoDB records DeletionProtectionEnabled in
// every TableDescription, but only DescribeTable carries WarmThroughput, and
// CreateTable omits BillingModeSummary's LastUpdateToPayPerRequestDateTime.
type tableDescFlags struct {
	warmThroughput          bool // DescribeTable only
	noPayPerRequestDateTime bool // CreateTable and restores: no LastUpdateToPayPerRequestDateTime
	// noDeletionProtection omits DeletionProtectionEnabled (restores only:
	// DynamoDB does not record it on a restored table).
	noDeletionProtection bool
	// provisionedThroughputShort renders ProvisionedThroughput without
	// NumberOfDecreasesToday (restores only).
	provisionedThroughputShort bool
}

// restoreTableDescFlags renders a restored table's TableDescription as
// DynamoDB records it: no DeletionProtectionEnabled, ProvisionedThroughput
// without NumberOfDecreasesToday, and BillingModeSummary without
// LastUpdateToPayPerRequestDateTime.
var restoreTableDescFlags = tableDescFlags{
	noPayPerRequestDateTime:    true,
	noDeletionProtection:       true,
	provisionedThroughputShort: true,
}

// tableDescription renders a storage.Table as DynamoDB's DescribeTable shape.
// status overrides the table status (and, for CREATING and DELETING, every
// index status); "" renders the stored statuses.
func (o *Ops) tableDescription(t *storage.Table, status string, flags tableDescFlags) map[string]any {
	arn := o.tableArn(t)
	pt := provisionedThroughputJSON(t.ReadCU, t.WriteCU)
	if flags.provisionedThroughputShort {
		pt = provisionedThroughputShortJSON(t.ReadCU, t.WriteCU)
	}
	desc := map[string]any{
		"TableName":             t.Name,
		"TableArn":              arn,
		"TableId":               tableID(t),
		"KeySchema":             keySchemaJSON(t.Key),
		"AttributeDefinitions":  attributeDefinitionsJSON(t.AttrDefs),
		"TableStatus":           orStatus(status, t.Status),
		"CreationDateTime":      epochSeconds(t.CreatedAt),
		"ItemCount":             0,
		"TableSizeBytes":        0,
		"ProvisionedThroughput": pt,
	}
	if !flags.noDeletionProtection {
		desc["DeletionProtectionEnabled"] = t.DeletionProtection
	}
	if t.Billing == "PAY_PER_REQUEST" {
		sum := map[string]any{"BillingMode": "PAY_PER_REQUEST"}
		if !flags.noPayPerRequestDateTime {
			sum["LastUpdateToPayPerRequestDateTime"] = epochSeconds(t.CreatedAt)
		}
		desc["BillingModeSummary"] = sum
		if flags.warmThroughput {
			desc["WarmThroughput"] = warmThroughputJSON()
		}
	}
	gsis, lsis := splitIndexes(t.GSIs)
	if len(gsis) > 0 {
		desc["GlobalSecondaryIndexes"] = gsiDescriptions(gsis, arn, status, flags.warmThroughput && t.Billing == "PAY_PER_REQUEST")
	}
	if len(lsis) > 0 {
		desc["LocalSecondaryIndexes"] = lsiDescriptions(lsis, arn)
	}
	if t.StreamView != "" {
		desc["StreamSpecification"] = map[string]any{
			"StreamEnabled":  true,
			"StreamViewType": t.StreamView,
		}
		desc["LatestStreamLabel"] = t.StreamLabel
		desc["LatestStreamArn"] = streams.StreamARN(o.region(), t.Name, t.StreamLabel)
	}
	if t.Restore != nil {
		desc["RestoreSummary"] = o.restoreSummaryJSON(t, status)
	}
	if len(t.Replicas) > 0 {
		desc["GlobalTableVersion"] = "2019.11.21"
		desc["Replicas"] = replicasJSON(t)
	}
	return desc
}

// replicasJSON renders the table's replicas as DynamoDB's ReplicaDescription
// list (bookkeeping: the same table serves every region).
func replicasJSON(t *storage.Table) []map[string]any {
	out := make([]map[string]any, 0, len(t.Replicas))
	for _, r := range t.Replicas {
		out = append(out, map[string]any{
			"RegionName":    r.Region,
			"ReplicaStatus": r.Status,
		})
	}
	return out
}

func orStatus(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}

// keySchemaJSON renders a table or index key schema.
func keySchemaJSON(k storage.KeySchema) []map[string]any {
	out := []map[string]any{{"AttributeName": k.Hash, "KeyType": "HASH"}}
	if k.Range != "" {
		out = append(out, map[string]any{"AttributeName": k.Range, "KeyType": "RANGE"})
	}
	return out
}

// attributeDefinitionsJSON renders the definitions in name order, which is the
// order DynamoDB returns them in.
func attributeDefinitionsJSON(defs map[string]attr.Type) []map[string]any {
	names := make([]string, 0, len(defs))
	for n := range defs {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"AttributeName": n, "AttributeType": defs[n].String()})
	}
	return out
}

func provisionedThroughputJSON(read, write int64) map[string]any {
	return map[string]any{
		"NumberOfDecreasesToday": 0,
		"ReadCapacityUnits":      read,
		"WriteCapacityUnits":     write,
	}
}

// splitIndexes partitions the table's indexes into global and local.
func splitIndexes(gsis []storage.Index) (global, local []storage.Index) {
	for _, g := range gsis {
		if g.Local {
			local = append(local, g)
		} else {
			global = append(global, g)
		}
	}
	return global, local
}

func gsiDescriptions(gsis []storage.Index, arn, status string, warm bool) []map[string]any {
	out := make([]map[string]any, 0, len(gsis))
	for _, g := range gsis {
		indexStatus := g.Status
		if status == storage.StatusCreating || status == storage.StatusDeleting {
			indexStatus = status
		}
		entry := map[string]any{
			"IndexName":             g.Name,
			"KeySchema":             keySchemaJSON(g.Key),
			"Projection":            projectionJSON(g),
			"IndexStatus":           indexStatus,
			"IndexArn":              arn + "/index/" + g.Name,
			"ItemCount":             0,
			"IndexSizeBytes":        0,
			"ProvisionedThroughput": provisionedThroughputJSON(0, 0),
		}
		if warm {
			entry["WarmThroughput"] = warmThroughputJSON()
		}
		out = append(out, entry)
	}
	return out
}

// warmThroughputJSON is DescribeTable's fixed on-demand warm throughput: the
// table and each GSI report these values while the service keeps capacity warm.
func warmThroughputJSON() map[string]any {
	return map[string]any{
		"ReadUnitsPerSecond":  12000,
		"WriteUnitsPerSecond": 4000,
		"Status":              "ACTIVE",
	}
}

// lsiDescriptions renders a local secondary index: the same shape as a GSI
// without IndexStatus and ProvisionedThroughput, which LSIs do not have.
func lsiDescriptions(lsis []storage.Index, arn string) []map[string]any {
	out := make([]map[string]any, 0, len(lsis))
	for _, g := range lsis {
		out = append(out, map[string]any{
			"IndexName":      g.Name,
			"KeySchema":      keySchemaJSON(g.Key),
			"Projection":     projectionJSON(g),
			"IndexSizeBytes": 0,
			"ItemCount":      0,
			"IndexArn":       arn + "/index/" + g.Name,
		})
	}
	return out
}

func projectionJSON(g storage.Index) map[string]any {
	p := map[string]any{"ProjectionType": g.Projection}
	if g.Projection == storage.ProjectInclude {
		p["NonKeyAttributes"] = g.NonKeyAttrs
	}
	return p
}

// epochSeconds is DynamoDB's timestamp form: epoch seconds as a number with a
// fractional (millisecond) part.
func epochSeconds(t time.Time) float64 {
	return float64(t.UnixMilli()) / 1000
}

// tableID derives a stable UUID from the table ID and creation time, so the
// same table always reports the same TableId without storing one.
func tableID(t *storage.Table) string {
	seed := fmt.Sprintf("%d:%d", t.ID, t.CreatedAt.UnixNano())
	sum := sha256.Sum256([]byte(seed))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50 // version 5
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

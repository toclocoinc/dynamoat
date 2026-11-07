package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/streams"
)

func init() { register("UpdateTable", (*Ops).updateTable) }

type updateTableInput struct {
	TableName                   string                 `json:"TableName"`
	BillingMode                 string                 `json:"BillingMode"`
	ProvisionedThroughput       *provisionedThroughput `json:"ProvisionedThroughput"`
	StreamSpecification         *streamSpecification   `json:"StreamSpecification"`
	GlobalSecondaryIndexUpdates []gsiUpdate            `json:"GlobalSecondaryIndexUpdates"`
	ReplicaUpdates              []replicaUpdate        `json:"ReplicaUpdates"`
	AttributeDefinitions        []attributeDefinition  `json:"AttributeDefinitions"`
	DeletionProtectionEnabled   *bool                  `json:"DeletionProtectionEnabled"`
}

// replicaUpdate is one UpdateTable ReplicaUpdates entry: Create adds a replica,
// Delete removes one. Update is not a DynamoDB member and is rejected.
type replicaUpdate struct {
	Create *createReplica `json:"Create"`
	Update *updateReplica `json:"Update"`
	Delete *deleteReplica `json:"Delete"`
}

// createReplica holds the fields DynamoDB accepts on a replica create; only
// RegionName is used, the rest are accepted and ignored.
type createReplica struct {
	RegionName string `json:"RegionName"`
}

type updateReplica struct {
	RegionName string `json:"RegionName"`
}

type deleteReplica struct {
	RegionName string `json:"RegionName"`
}

type gsiUpdate struct {
	Create *createGSI `json:"Create"`
	Delete *deleteGSI `json:"Delete"`
	Update *updateGSI `json:"Update"`
}

type deleteGSI struct {
	IndexName string `json:"IndexName"`
}

type updateGSI struct {
	IndexName             string                 `json:"IndexName"`
	ProvisionedThroughput *provisionedThroughput `json:"ProvisionedThroughput"`
}

// supportedUpdateFields are the UpdateTable fields dynamoat implements; every
// other present field is rejected as unsupported.
var supportedUpdateFields = map[string]bool{
	"TableName":                   true,
	"BillingMode":                 true,
	"ProvisionedThroughput":       true,
	"StreamSpecification":         true,
	"GlobalSecondaryIndexUpdates": true,
	"ReplicaUpdates":              true,
	"AttributeDefinitions":        true,
	"DeletionProtectionEnabled":   true,
}

func (o *Ops) updateTable(ctx context.Context, body []byte) (any, error) {
	var req updateTableInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}

	if err := checkUnsupportedFields(body); err != nil {
		return nil, err
	}
	if err := checkTableName(req.TableName, "tableName"); err != nil {
		return nil, err
	}

	updated, err := o.Store.UpdateTable(ctx, req.TableName, func(t *storage.Table) error {
		return applyUpdate(o, req, t)
	})
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.ResourceNotFound("Requested resource not found: Table: " + req.TableName + " not found")
	}
	if err != nil {
		return nil, err
	}
	// The replica just created is reported CREATING in this response only; it is
	// stored ACTIVE, so every later read shows it ACTIVE.
	if creating := replicaCreatingRegion(req); creating != "" {
		markReplicaCreating(updated, creating)
	}
	return map[string]any{"TableDescription": o.tableDescription(updated, "", tableDescFlags{})}, nil
}

func checkUnsupportedFields(body []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil // decode already rejected malformed bodies
	}
	var unsupported []string
	for k, raw := range fields {
		if supportedUpdateFields[k] {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		unsupported = append(unsupported, k)
	}
	if len(unsupported) == 0 {
		return nil
	}
	sort.Strings(unsupported)
	return ddberr.Validation("dynamoat does not support %s", unsupported[0])
}

func applyUpdate(o *Ops, req updateTableInput, t *storage.Table) error {
	if err := applyBilling(req, t); err != nil {
		return err
	}
	if err := applyStream(o, req, t); err != nil {
		return err
	}
	if err := applyReplicaUpdates(o, req, t); err != nil {
		return err
	}
	if req.DeletionProtectionEnabled != nil {
		t.DeletionProtection = *req.DeletionProtectionEnabled
	}
	return applyGSIUpdates(req, t)
}

func applyBilling(req updateTableInput, t *storage.Table) error {
	switch req.BillingMode {
	case "PAY_PER_REQUEST":
		if req.ProvisionedThroughput != nil {
			return ddberr.Validation("One or more parameter values were invalid: Neither ReadCapacityUnits nor WriteCapacityUnits can be specified when BillingMode is PAY_PER_REQUEST")
		}
		t.Billing = "PAY_PER_REQUEST"
		t.ReadCU = 0
		t.WriteCU = 0
	case "PROVISIONED":
		if req.ProvisionedThroughput == nil {
			return ddberr.Validation("One or more parameter values were invalid: ReadCapacityUnits and WriteCapacityUnits must both be specified when BillingMode is PROVISIONED")
		}
		t.Billing = "PROVISIONED"
		t.ReadCU = req.ProvisionedThroughput.ReadCapacityUnits
		t.WriteCU = req.ProvisionedThroughput.WriteCapacityUnits
	default:
		// BillingMode absent: a bare ProvisionedThroughput updates a provisioned
		// table's capacity.
		if req.ProvisionedThroughput != nil {
			if t.Billing == "PAY_PER_REQUEST" {
				return ddberr.Validation("One or more parameter values were invalid: Neither ReadCapacityUnits nor WriteCapacityUnits can be specified when BillingMode is PAY_PER_REQUEST")
			}
			t.ReadCU = req.ProvisionedThroughput.ReadCapacityUnits
			t.WriteCU = req.ProvisionedThroughput.WriteCapacityUnits
		}
	}
	return nil
}

func applyStream(o *Ops, req updateTableInput, t *storage.Table) error {
	if req.StreamSpecification == nil {
		return nil
	}
	spec := req.StreamSpecification
	if spec.StreamEnabled {
		if t.StreamView != "" {
			arn := streams.StreamARN(o.region(), t.Name, t.StreamLabel)
			return ddberr.Validation("Table already has an enabled stream: %s", arn)
		}
		if spec.StreamViewType == "" {
			return ddberr.Validation("One or more parameter values were invalid: StreamViewType is required when StreamEnabled is true")
		}
		switch spec.StreamViewType {
		case storage.StreamKeysOnly, storage.StreamNewImage, storage.StreamOldImage, storage.StreamNewAndOld:
		default:
			return ddberr.Validation("One or more parameter values were invalid: Invalid StreamViewType: %s", spec.StreamViewType)
		}
		t.StreamView = spec.StreamViewType
		t.StreamLabel = o.now().UTC().Format("2006-01-02T15:04:05.000")
	} else {
		t.StreamView = ""
		t.StreamLabel = ""
	}
	return nil
}

func applyGSIUpdates(req updateTableInput, t *storage.Table) error {
	if len(req.GlobalSecondaryIndexUpdates) == 0 {
		return nil
	}
	var creates []createGSI
	var deletes []deleteGSI
	for _, u := range req.GlobalSecondaryIndexUpdates {
		switch {
		case u.Create != nil:
			creates = append(creates, *u.Create)
		case u.Delete != nil:
			deletes = append(deletes, *u.Delete)
		case u.Update != nil:
			return ddberr.Validation("dynamoat does not support GlobalSecondaryIndexUpdate.Update")
		default:
			return ddberr.Validation("dynamoat does not support an empty GlobalSecondaryIndexUpdate")
		}
	}
	if len(creates)+len(deletes) > 1 {
		return ddberr.LimitExceeded("Subscriber limit exceeded: Only 1 online index can be created or deleted simultaneously per table")
	}
	if len(creates) == 1 {
		return createGSIUpdate(req, t, creates[0])
	}
	if len(deletes) == 1 {
		return deleteGSIUpdate(t, deletes[0])
	}
	return nil
}

func createGSIUpdate(req updateTableInput, t *storage.Table, g createGSI) error {
	if err := checkIndexName(g.IndexName); err != nil {
		return err
	}
	if _, exists := t.GSI(g.IndexName); exists {
		return ddberr.Validation("One or more parameter values were invalid: Duplicate index name: %s", g.IndexName)
	}
	ks, ok := parseKeySchema(g.KeySchema)
	if !ok {
		return ddberr.Validation("Invalid KeySchema: Some index key schema element is not valid")
	}
	proj, err := validateProjection(g.Projection)
	if err != nil {
		return err
	}

	reqDefs, err := parseAttributeDefinitions(req.AttributeDefinitions)
	if err != nil {
		return err
	}
	keys := []string{ks.Hash}
	if ks.Range != "" {
		keys = append(keys, ks.Range)
	}
	if err := checkKeyAttrsDefined(keys, reqDefs); err != nil {
		return err
	}
	ks.HashType = reqDefs[ks.Hash]
	if ks.Range != "" {
		ks.RangeType = reqDefs[ks.Range]
	}
	for name, ty := range reqDefs {
		t.AttrDefs[name] = ty
	}

	t.GSIs = append(t.GSIs, storage.Index{
		Name:        g.IndexName,
		Key:         ks,
		Projection:  proj.ProjectionType,
		NonKeyAttrs: proj.NonKeyAttributes,
		Status:      storage.StatusCreating,
	})
	return nil
}

func deleteGSIUpdate(t *storage.Table, d deleteGSI) error {
	idx := -1
	for i := range t.GSIs {
		if t.GSIs[i].Name == d.IndexName {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ddberr.ResourceNotFound("Requested resource not found: Index: " + d.IndexName + " not found")
	}
	if t.GSIs[idx].Local {
		return ddberr.Validation("One or more parameter values were invalid: Cannot delete a local secondary index: %s", d.IndexName)
	}
	t.GSIs = append(t.GSIs[:idx], t.GSIs[idx+1:]...)
	return nil
}

package ops

// Global tables. dynamoat serves a table from one database in every region a
// request is signed for, so replicas are bookkeeping: they make DescribeTable
// and the global tables API answer as DynamoDB does, without any copying
// between sites (see the Replicas comment in internal/storage/storage.go).
//
// Version 2019.11.21 (current) manages replicas through UpdateTable's
// ReplicaUpdates. Version 2017.11.29 (legacy) exposes the older
// CreateGlobalTable/DescribeGlobalTable/... operations over the same state.

import (
	"context"
	"errors"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// replicaCreatingRegion returns the region a Create replica update names, or ""
// when the request does not create one.
func replicaCreatingRegion(req updateTableInput) string {
	if len(req.ReplicaUpdates) == 1 && req.ReplicaUpdates[0].Create != nil {
		return req.ReplicaUpdates[0].Create.RegionName
	}
	return ""
}

// markReplicaCreating sets the named replica's status to CREATING, so the one
// UpdateTable response reports it CREATING even though it is stored ACTIVE.
func markReplicaCreating(t *storage.Table, region string) {
	reps := make([]storage.Replica, len(t.Replicas))
	copy(reps, t.Replicas)
	for i := range reps {
		if reps[i].Region == region {
			reps[i].Status = storage.StatusCreating
		}
	}
	t.Replicas = reps
}

func applyReplicaUpdates(o *Ops, req updateTableInput, t *storage.Table) error {
	if len(req.ReplicaUpdates) == 0 {
		return nil
	}
	if len(req.ReplicaUpdates) != 1 {
		return ddberr.Validation("Only one replica update can be performed at a time")
	}
	switch u := req.ReplicaUpdates[0]; {
	case u.Create != nil:
		return applyCreateReplica(o, t, u.Create.RegionName)
	case u.Delete != nil:
		return applyDeleteReplica(o, t, u.Delete.RegionName)
	case u.Update != nil:
		return ddberr.Validation("dynamoat does not support ReplicaUpdate.Update")
	default:
		return ddberr.Validation("dynamoat does not support an empty ReplicaUpdate")
	}
}

// applyCreateReplica adds a replica. Creating one requires a stream with the
// NEW_AND_OLD_IMAGES view, as DynamoDB does: enabled automatically when
// streams are off, a ValidationException when they are on with another view.
// The table's own region is always the first replica once any replica exists.
func applyCreateReplica(o *Ops, t *storage.Table, region string) error {
	if region == "" {
		return ddberr.Validation("1 validation error detected: Value null at 'regionName' failed to satisfy constraint: Member must not be null")
	}
	if replicaIndex(t.Replicas, region) >= 0 {
		return ddberr.Validation("One or more parameter values were invalid: Replica already exists: %s", region)
	}
	switch t.StreamView {
	case "":
		t.StreamView = storage.StreamNewAndOld
		t.StreamLabel = o.now().UTC().Format("2006-01-02T15:04:05.000")
	case storage.StreamNewAndOld:
	default:
		return ddberr.Validation("One or more parameter values were invalid: Global Table creation requires Streams enabled with NEW_AND_OLD_IMAGES view type")
	}

	own := o.region()
	reps := append([]storage.Replica(nil), t.Replicas...)
	if len(reps) == 0 && region != own {
		reps = append(reps, storage.Replica{Region: own, Status: storage.StatusActive})
	}
	reps = append(reps, storage.Replica{Region: region, Status: storage.StatusActive})
	t.Replicas = reps
	return nil
}

// applyDeleteReplica removes a replica. The table's own region cannot be
// removed directly; it disappears with the last replica, leaving the table
// with no replicas again.
func applyDeleteReplica(o *Ops, t *storage.Table, region string) error {
	if region == "" {
		return ddberr.Validation("1 validation error detected: Value null at 'regionName' failed to satisfy constraint: Member must not be null")
	}
	if region == o.region() {
		return ddberr.Validation("One or more parameter values were invalid: Cannot delete the table's home region: %s", region)
	}
	idx := replicaIndex(t.Replicas, region)
	if idx < 0 {
		return ddberr.Validation("One or more parameter values were invalid: Replica not found: %s", region)
	}
	reps := append(append([]storage.Replica(nil), t.Replicas[:idx]...), t.Replicas[idx+1:]...)
	if len(reps) == 1 && reps[0].Region == o.region() {
		reps = nil
	}
	t.Replicas = reps
	return nil
}

func replicaIndex(reps []storage.Replica, region string) int {
	for i, r := range reps {
		if r.Region == region {
			return i
		}
	}
	return -1
}

// globalTable loads a table that is a global table, or returns
// GlobalTableNotFound. The 2017 API reports a table without replicas the same
// way it reports a missing table.
func (o *Ops) globalTable(ctx context.Context, name string) (*storage.Table, error) {
	t, err := o.Store.GetTable(ctx, name)
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.GlobalTableNotFound("Global table not found: " + name)
	}
	if err != nil {
		return nil, err
	}
	if len(t.Replicas) == 0 {
		return nil, ddberr.GlobalTableNotFound("Global table not found: " + name)
	}
	return t, nil
}

// --- 2017.11.29 (legacy) operations ---

func init() {
	register("CreateGlobalTable", (*Ops).createGlobalTable)
	register("DescribeGlobalTable", (*Ops).describeGlobalTable)
	register("ListGlobalTables", (*Ops).listGlobalTables)
	register("UpdateGlobalTable", (*Ops).updateGlobalTable)
	register("DescribeGlobalTableSettings", (*Ops).describeGlobalTableSettings)
	register("UpdateGlobalTableSettings", (*Ops).updateGlobalTableSettings)
	register("DescribeTableReplicaAutoScaling", (*Ops).describeTableReplicaAutoScaling)
	register("UpdateTableReplicaAutoScaling", (*Ops).updateTableReplicaAutoScaling)
}

type replicaRegion struct {
	RegionName string `json:"RegionName"`
}

type createGlobalTableInput struct {
	GlobalTableName  string          `json:"GlobalTableName"`
	ReplicationGroup []replicaRegion `json:"ReplicationGroup"`
}

func (o *Ops) createGlobalTable(ctx context.Context, body []byte) (any, error) {
	var req createGlobalTableInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkTableName(req.GlobalTableName, "globalTableName"); err != nil {
		return nil, err
	}
	if len(req.ReplicationGroup) == 0 {
		return nil, ddberr.Validation("1 validation error detected: Value null at 'replicationGroup' failed to satisfy constraint: Member must not be null")
	}
	for _, r := range req.ReplicationGroup {
		if r.RegionName == "" {
			return nil, ddberr.Validation("1 validation error detected: Value null at 'regionName' failed to satisfy constraint: Member must not be null")
		}
	}

	updated, err := o.Store.UpdateTable(ctx, req.GlobalTableName, func(t *storage.Table) error {
		if len(t.Replicas) > 0 {
			return ddberr.GlobalTableAlreadyExists("Global table already exists: " + req.GlobalTableName)
		}
		if t.StreamView != storage.StreamNewAndOld {
			return ddberr.Validation("One or more parameter values were invalid: Global Table creation requires Streams enabled with NEW_AND_OLD_IMAGES view type")
		}
		own := o.region()
		seen := map[string]bool{own: true}
		reps := []storage.Replica{{Region: own, Status: storage.StatusActive}}
		for _, r := range req.ReplicationGroup {
			if seen[r.RegionName] {
				return ddberr.ReplicaAlreadyExists("Replica already exists: " + r.RegionName)
			}
			seen[r.RegionName] = true
			reps = append(reps, storage.Replica{Region: r.RegionName, Status: storage.StatusActive})
		}
		t.Replicas = reps
		return nil
	})
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.GlobalTableNotFound("Global table not found: " + req.GlobalTableName)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"GlobalTableDescription": o.globalTableDescription(updated)}, nil
}

type describeGlobalTableInput struct {
	GlobalTableName string `json:"GlobalTableName"`
}

func (o *Ops) describeGlobalTable(ctx context.Context, body []byte) (any, error) {
	var req describeGlobalTableInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkTableName(req.GlobalTableName, "globalTableName"); err != nil {
		return nil, err
	}
	t, err := o.globalTable(ctx, req.GlobalTableName)
	if err != nil {
		return nil, err
	}
	return map[string]any{"GlobalTableDescription": o.globalTableDescription(t)}, nil
}

type listGlobalTablesInput struct {
	RegionName                    string `json:"RegionName"`
	ExclusiveStartGlobalTableName string `json:"ExclusiveStartGlobalTableName"`
	Limit                         *int   `json:"Limit"`
}

func (o *Ops) listGlobalTables(ctx context.Context, body []byte) (any, error) {
	var req listGlobalTablesInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	limit := 100
	if req.Limit != nil {
		limit = *req.Limit
		if limit < 1 {
			return nil, ddberr.Validation("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value greater than or equal to 1", limit)
		}
		if limit > 100 {
			return nil, ddberr.Validation("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value less than or equal to 100", limit)
		}
	}

	// ListTables returns names in byte order with no limit when limit is 0.
	names, err := o.Store.ListTables(ctx, "", 0)
	if err != nil {
		return nil, err
	}
	var tables []map[string]any
	for _, n := range names {
		if n <= req.ExclusiveStartGlobalTableName {
			continue
		}
		t, err := o.Store.GetTable(ctx, n)
		if err != nil {
			return nil, err
		}
		if len(t.Replicas) == 0 {
			continue
		}
		if req.RegionName != "" && replicaIndex(t.Replicas, req.RegionName) < 0 {
			continue
		}
		tables = append(tables, map[string]any{
			"GlobalTableName":  t.Name,
			"ReplicationGroup": replicaRegionsJSON(t),
		})
		if len(tables) > limit {
			break
		}
	}
	if len(tables) > limit {
		return map[string]any{
			"GlobalTables":                 tables[:limit],
			"LastEvaluatedGlobalTableName": tables[limit-1]["GlobalTableName"],
		}, nil
	}
	if tables == nil {
		tables = []map[string]any{}
	}
	return map[string]any{"GlobalTables": tables}, nil
}

type updateGlobalTableInput struct {
	GlobalTableName string                `json:"GlobalTableName"`
	ReplicaUpdates  []globalReplicaUpdate `json:"ReplicaUpdates"`
}

type globalReplicaUpdate struct {
	Create *replicaRegion `json:"Create"`
	Delete *replicaRegion `json:"Delete"`
}

func (o *Ops) updateGlobalTable(ctx context.Context, body []byte) (any, error) {
	var req updateGlobalTableInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkTableName(req.GlobalTableName, "globalTableName"); err != nil {
		return nil, err
	}
	if len(req.ReplicaUpdates) != 1 {
		return nil, ddberr.Validation("Only one replica update can be performed at a time")
	}
	u := req.ReplicaUpdates[0]

	updated, err := o.Store.UpdateTable(ctx, req.GlobalTableName, func(t *storage.Table) error {
		if len(t.Replicas) == 0 {
			return ddberr.GlobalTableNotFound("Global table not found: " + req.GlobalTableName)
		}
		switch {
		case u.Create != nil:
			region := u.Create.RegionName
			if region == "" {
				return ddberr.Validation("1 validation error detected: Value null at 'regionName' failed to satisfy constraint: Member must not be null")
			}
			if replicaIndex(t.Replicas, region) >= 0 {
				return ddberr.ReplicaAlreadyExists("Replica already exists: " + region)
			}
			t.Replicas = append(append([]storage.Replica(nil), t.Replicas...), storage.Replica{Region: region, Status: storage.StatusActive})
		case u.Delete != nil:
			region := u.Delete.RegionName
			if region == "" {
				return ddberr.Validation("1 validation error detected: Value null at 'regionName' failed to satisfy constraint: Member must not be null")
			}
			if region == o.region() {
				return ddberr.Validation("One or more parameter values were invalid: Cannot delete the table's home region: %s", region)
			}
			idx := replicaIndex(t.Replicas, region)
			if idx < 0 {
				return ddberr.ReplicaNotFound("Replica not found: " + region)
			}
			reps := append(append([]storage.Replica(nil), t.Replicas[:idx]...), t.Replicas[idx+1:]...)
			if len(reps) == 1 && reps[0].Region == o.region() {
				reps = nil
			}
			t.Replicas = reps
		default:
			return ddberr.Validation("dynamoat does not support an empty ReplicaUpdate")
		}
		return nil
	})
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.GlobalTableNotFound("Global table not found: " + req.GlobalTableName)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"GlobalTableDescription": o.globalTableDescription(updated)}, nil
}

type describeGlobalTableSettingsInput struct {
	GlobalTableName string `json:"GlobalTableName"`
}

func (o *Ops) describeGlobalTableSettings(ctx context.Context, body []byte) (any, error) {
	var req describeGlobalTableSettingsInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkTableName(req.GlobalTableName, "globalTableName"); err != nil {
		return nil, err
	}
	t, err := o.globalTable(ctx, req.GlobalTableName)
	if err != nil {
		return nil, err
	}
	return o.globalTableSettings(t), nil
}

type updateGlobalTableSettingsInput struct {
	GlobalTableName                          string `json:"GlobalTableName"`
	GlobalTableBillingMode                   string `json:"GlobalTableBillingMode"`
	GlobalTableProvisionedWriteCapacityUnits *int64 `json:"GlobalTableProvisionedWriteCapacityUnits"`
}

func (o *Ops) updateGlobalTableSettings(ctx context.Context, body []byte) (any, error) {
	var req updateGlobalTableSettingsInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkTableName(req.GlobalTableName, "globalTableName"); err != nil {
		return nil, err
	}
	updated, err := o.Store.UpdateTable(ctx, req.GlobalTableName, func(t *storage.Table) error {
		if len(t.Replicas) == 0 {
			return ddberr.GlobalTableNotFound("Global table not found: " + req.GlobalTableName)
		}
		switch req.GlobalTableBillingMode {
		case "":
		case "PAY_PER_REQUEST":
			t.Billing = "PAY_PER_REQUEST"
			t.ReadCU = 0
			t.WriteCU = 0
		case "PROVISIONED":
			t.Billing = "PROVISIONED"
		default:
			return ddberr.Validation("One or more parameter values were invalid: Invalid BillingMode: %s", req.GlobalTableBillingMode)
		}
		if req.GlobalTableProvisionedWriteCapacityUnits != nil {
			t.WriteCU = *req.GlobalTableProvisionedWriteCapacityUnits
		}
		return nil
	})
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.GlobalTableNotFound("Global table not found: " + req.GlobalTableName)
	}
	if err != nil {
		return nil, err
	}
	return o.globalTableSettings(updated), nil
}

type describeTableReplicaAutoScalingInput struct {
	TableName string `json:"TableName"`
}

func (o *Ops) describeTableReplicaAutoScaling(ctx context.Context, body []byte) (any, error) {
	var req describeTableReplicaAutoScalingInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkTableName(req.TableName, "tableName"); err != nil {
		return nil, err
	}
	t, err := o.Store.GetTable(ctx, req.TableName)
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.ResourceNotFound("Requested resource not found: Table: " + req.TableName + " not found")
	}
	if err != nil {
		return nil, err
	}
	return o.tableReplicaAutoScaling(t), nil
}

func (o *Ops) updateTableReplicaAutoScaling(ctx context.Context, body []byte) (any, error) {
	var req describeTableReplicaAutoScalingInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkTableName(req.TableName, "tableName"); err != nil {
		return nil, err
	}
	// Updates are accepted and ignored: dynamoat reports no auto scaling.
	t, err := o.Store.GetTable(ctx, req.TableName)
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.ResourceNotFound("Requested resource not found: Table: " + req.TableName + " not found")
	}
	if err != nil {
		return nil, err
	}
	return o.tableReplicaAutoScaling(t), nil
}

// --- rendering ---

func (o *Ops) globalTableDescription(t *storage.Table) map[string]any {
	return map[string]any{
		"GlobalTableName":   t.Name,
		"ReplicationGroup":  replicationGroupJSON(t),
		"GlobalTableArn":    "arn:aws:dynamodb::000000000000:global-table/" + t.Name,
		"CreationDateTime":  epochSeconds(t.CreatedAt),
		"GlobalTableStatus": storage.StatusActive,
	}
}

func replicaRegionsJSON(t *storage.Table) []map[string]any {
	out := make([]map[string]any, 0, len(t.Replicas))
	for _, r := range t.Replicas {
		out = append(out, map[string]any{"RegionName": r.Region})
	}
	return out
}

func replicationGroupJSON(t *storage.Table) []map[string]any {
	out := make([]map[string]any, 0, len(t.Replicas))
	for _, r := range t.Replicas {
		out = append(out, map[string]any{"RegionName": r.Region, "ReplicaStatus": r.Status})
	}
	return out
}

func (o *Ops) globalTableSettings(t *storage.Table) map[string]any {
	return map[string]any{
		"GlobalTableName": t.Name,
		"ReplicaSettings": replicaSettingsJSON(t),
	}
}

func replicaSettingsJSON(t *storage.Table) []map[string]any {
	out := make([]map[string]any, 0, len(t.Replicas))
	for _, r := range t.Replicas {
		out = append(out, map[string]any{
			"RegionName":                           r.Region,
			"ReplicaStatus":                        r.Status,
			"ReplicaBillingModeSummary":            map[string]any{"BillingMode": t.Billing},
			"ReplicaProvisionedReadCapacityUnits":  t.ReadCU,
			"ReplicaProvisionedWriteCapacityUnits": t.WriteCU,
		})
	}
	return out
}

func (o *Ops) tableReplicaAutoScaling(t *storage.Table) map[string]any {
	reps := make([]map[string]any, 0, len(t.Replicas))
	for _, r := range t.Replicas {
		reps = append(reps, map[string]any{"RegionName": r.Region, "ReplicaStatus": r.Status})
	}
	return map[string]any{"TableName": t.Name, "Replicas": reps}
}

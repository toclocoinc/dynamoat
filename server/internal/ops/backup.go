package ops

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() {
	register("CreateBackup", (*Ops).createBackup)
	register("DescribeBackup", (*Ops).describeBackup)
	register("ListBackups", (*Ops).listBackups)
	register("DeleteBackup", (*Ops).deleteBackup)
	register("RestoreTableFromBackup", (*Ops).restoreTableFromBackup)
}

// unsupportedBackups is the error for a Store that does not implement
// BackupStore.
func unsupportedBackups() error {
	return ddberr.Validation("dynamoat: this store does not support backups")
}

// backupStore asserts that the store supports backups.
func (o *Ops) backupStore() (storage.BackupStore, error) {
	bs, ok := o.Store.(storage.BackupStore)
	if !ok {
		return nil, unsupportedBackups()
	}
	return bs, nil
}

// backupArn is the ARN dynamoat reports for a backup.
func (o *Ops) backupArn(t *storage.Table, id string) string {
	return o.tableArnForName(t.Name) + "/backup/" + id
}

// backupIDFromArn extracts the backup ID from a backup ARN. The backup ID is
// the last path segment; a table name cannot contain "/", so the split is
// unambiguous.
func backupIDFromArn(arn string) (string, error) {
	if i := strings.LastIndex(arn, "/"); i >= 0 && i < len(arn)-1 {
		return arn[i+1:], nil
	}
	return "", ddberr.Validation("1 validation error detected: Value '%s' at 'backupArn' failed to satisfy constraint: Member must satisfy regular expression pattern: arn:aws:dynamodb:[\\w-]+:[\\w-]+:[\\d]+:table/[\\w.-]+/backup/[\\w.-]+", arn)
}

// checkBackupName applies DynamoDB's BackupName constraints (the same
// characters as a table name).
func checkBackupName(name string) error {
	switch {
	case len(name) < 3:
		return ddberr.Validation("1 validation error detected: Value '%s' at 'backupName' failed to satisfy constraint: Member must have length greater than or equal to 3", name)
	case len(name) > 255:
		return ddberr.Validation("1 validation error detected: Value '%s' at 'backupName' failed to satisfy constraint: Member must have length less than or equal to 255", name)
	case !tableNameRE.MatchString(name):
		return ddberr.Validation("1 validation error detected: Value '%s' at 'backupName' failed to satisfy constraint: Member must satisfy regular expression pattern: [a-zA-Z0-9_.-]+", name)
	}
	return nil
}

// backupDetails renders BackupDetails: the fields shared by CreateBackup,
// DescribeBackup and DeleteBackup.
func (o *Ops) backupDetails(b *storage.Backup) map[string]any {
	return map[string]any{
		"BackupArn":              o.backupArn(&b.Table, b.ID),
		"BackupName":             b.Name,
		"BackupSizeBytes":        b.Bytes,
		"BackupStatus":           b.Status,
		"BackupType":             "USER",
		"BackupCreationDateTime": epochSeconds(b.CreatedAt),
	}
}

// backupSummary renders one BackupSummaries element of ListBackups.
func (o *Ops) backupSummary(b *storage.Backup) map[string]any {
	return map[string]any{
		"TableName":              b.Table.Name,
		"TableId":                tableID(&b.Table),
		"TableArn":               o.tableArn(&b.Table),
		"BackupArn":              o.backupArn(&b.Table, b.ID),
		"BackupName":             b.Name,
		"BackupCreationDateTime": epochSeconds(b.CreatedAt),
		"BackupStatus":           b.Status,
		"BackupType":             "USER",
		"BackupSizeBytes":        b.Bytes,
	}
}

// backupDescription renders DescribeBackup and DeleteBackup's
// BackupDescription.
func (o *Ops) backupDescription(b *storage.Backup) map[string]any {
	return map[string]any{
		"BackupDetails":             o.backupDetails(b),
		"SourceTableDetails":        o.sourceTableDetails(&b.Table),
		"SourceTableFeatureDetails": o.sourceTableFeatureDetails(&b.Table),
	}
}

// sourceTableDetails renders SourceTableDetails from the table definition the
// backup captured.
func (o *Ops) sourceTableDetails(t *storage.Table) map[string]any {
	return map[string]any{
		"TableName":             t.Name,
		"TableId":               tableID(t),
		"TableArn":              o.tableArn(t),
		"TableSizeBytes":        int64(0),
		"KeySchema":             keySchemaJSON(t.Key),
		"TableCreationDateTime": epochSeconds(t.CreatedAt),
		"ProvisionedThroughput": provisionedThroughputShortJSON(t.ReadCU, t.WriteCU),
		"ItemCount":             int64(0),
		"BillingMode":           billingMode(t),
	}
}

// sourceTableFeatureDetails renders SourceTableFeatureDetails, listing the
// source table's indexes and, when present, its stream and TTL settings.
func (o *Ops) sourceTableFeatureDetails(t *storage.Table) map[string]any {
	out := map[string]any{}
	gsis, lsis := splitIndexes(t.GSIs)
	if len(gsis) > 0 {
		out["GlobalSecondaryIndexes"] = sourceGSIInfos(gsis)
	}
	if len(lsis) > 0 {
		out["LocalSecondaryIndexes"] = sourceLSIInfos(lsis)
	}
	if t.StreamView != "" {
		out["StreamDescription"] = map[string]any{
			"StreamEnabled":  true,
			"StreamViewType": t.StreamView,
		}
	}
	if t.TTLAttr != "" || t.TTLEnabled {
		out["TimeToLiveDescription"] = map[string]any{
			"TimeToLiveStatus": "ENABLED",
			"AttributeName":    t.TTLAttr,
		}
	}
	return out
}

// sourceGSIInfos renders a GSI as DescribeBackup's GlobalSecondaryIndexInfo:
// the same as DescribeTable's GSI description without the status, ARN, item
// count and size fields.
func sourceGSIInfos(gsis []storage.Index) []map[string]any {
	out := make([]map[string]any, 0, len(gsis))
	for _, g := range gsis {
		out = append(out, map[string]any{
			"IndexName":             g.Name,
			"KeySchema":             keySchemaJSON(g.Key),
			"Projection":            projectionJSON(g),
			"ProvisionedThroughput": provisionedThroughputShortJSON(0, 0),
		})
	}
	return out
}

// sourceLSIInfos renders an LSI as DescribeBackup's LocalSecondaryIndexInfo.
func sourceLSIInfos(lsis []storage.Index) []map[string]any {
	out := make([]map[string]any, 0, len(lsis))
	for _, g := range lsis {
		out = append(out, map[string]any{
			"IndexName":  g.Name,
			"KeySchema":  keySchemaJSON(g.Key),
			"Projection": projectionJSON(g),
		})
	}
	return out
}

func billingMode(t *storage.Table) string {
	if t.Billing == "PAY_PER_REQUEST" {
		return "PAY_PER_REQUEST"
	}
	return "PROVISIONED"
}

// provisionedThroughputShortJSON is DynamoDB's ProvisionedThroughput type (as
// opposed to DescribeTable's ProvisionedThroughputDescription, which adds
// NumberOfDecreasesToday).
func provisionedThroughputShortJSON(read, write int64) map[string]any {
	return map[string]any{
		"ReadCapacityUnits":  read,
		"WriteCapacityUnits": write,
	}
}

// restoreSummaryJSON renders Table.Restore as DescribeTable's RestoreSummary.
// RestoreInProgress is true while the table is CREATING.
func (o *Ops) restoreSummaryJSON(t *storage.Table, status string) map[string]any {
	r := t.Restore
	sum := map[string]any{
		"SourceTableArn":    o.tableArnForName(r.SourceTable),
		"RestoreDateTime":   epochSeconds(r.At),
		"RestoreInProgress": orStatus(status, t.Status) == storage.StatusCreating,
	}
	if r.SourceBackupID != "" {
		sum["SourceBackupArn"] = o.backupArn(&storage.Table{Name: r.SourceTable}, r.SourceBackupID)
	}
	return sum
}

type createBackupInput struct {
	TableName  string `json:"TableName"`
	BackupName string `json:"BackupName"`
}

func (o *Ops) createBackup(ctx context.Context, body []byte) (any, error) {
	var req createBackupInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkTableName(req.TableName, "tableName"); err != nil {
		return nil, err
	}
	if err := checkBackupName(req.BackupName); err != nil {
		return nil, err
	}
	bs, err := o.backupStore()
	if err != nil {
		return nil, err
	}
	t, err := o.Store.GetTable(ctx, req.TableName)
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.ResourceNotFound("Requested resource not found: Table: " + req.TableName + " not found")
	}
	if err != nil {
		return nil, err
	}
	if t.Status != storage.StatusActive {
		return nil, ddberr.ResourceInUse("Backups are not available for the table while it is in the " + t.Status + " state")
	}
	b, err := bs.CreateBackup(ctx, t, req.BackupName)
	if err != nil {
		return nil, err
	}
	// DynamoDB reports a freshly created backup as CREATING with size 0 (the
	// size is computed later), even though the store's copy has already
	// finished. DescribeBackup and ListBackups keep reporting the real status.
	details := o.backupDetails(b)
	details["BackupStatus"] = "CREATING"
	details["BackupSizeBytes"] = int64(0)
	return map[string]any{"BackupDetails": details}, nil
}

type describeBackupInput struct {
	BackupArn string `json:"BackupArn"`
}

func (o *Ops) describeBackup(ctx context.Context, body []byte) (any, error) {
	var req describeBackupInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	id, err := backupIDFromArn(req.BackupArn)
	if err != nil {
		return nil, err
	}
	bs, err := o.backupStore()
	if err != nil {
		return nil, err
	}
	b, err := bs.GetBackup(ctx, id)
	if errors.Is(err, storage.ErrBackupNotFound) {
		return nil, ddberr.BackupNotFound("Backup not found: " + req.BackupArn)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"BackupDescription": o.backupDescription(b)}, nil
}

type listBackupsInput struct {
	TableName               string   `json:"TableName"`
	TimeRangeLowerBound     *float64 `json:"TimeRangeLowerBound"`
	TimeRangeUpperBound     *float64 `json:"TimeRangeUpperBound"`
	ExclusiveStartBackupArn string   `json:"ExclusiveStartBackupArn"`
	Limit                   *int     `json:"Limit"`
	BackupType              string   `json:"BackupType"`
}

func (o *Ops) listBackups(ctx context.Context, body []byte) (any, error) {
	var req listBackupsInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if req.TableName != "" {
		if err := checkTableName(req.TableName, "tableName"); err != nil {
			return nil, err
		}
	}
	switch req.BackupType {
	case "", "ALL", "USER", "SYSTEM", "AWS_BACKUP":
	default:
		return nil, ddberr.Validation("1 validation error detected: Value '%s' at 'backupType' failed to satisfy constraint: Member must satisfy enum value set: [USER, SYSTEM, AWS_BACKUP, ALL]", req.BackupType)
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
	// dynamoat only creates USER backups, so a filter for a kind it never makes
	// returns nothing.
	if req.BackupType == "SYSTEM" || req.BackupType == "AWS_BACKUP" {
		return map[string]any{"BackupSummaries": []map[string]any{}}, nil
	}
	bs, err := o.backupStore()
	if err != nil {
		return nil, err
	}
	from, to := timeFromEpoch(req.TimeRangeLowerBound), timeFromEpoch(req.TimeRangeUpperBound)
	startAfter := ""
	if req.ExclusiveStartBackupArn != "" {
		startAfter, err = backupIDFromArn(req.ExclusiveStartBackupArn)
		if err != nil {
			return nil, err
		}
	}
	// Read one extra backup to learn whether another page follows.
	backups, err := bs.ListBackups(ctx, req.TableName, from, to, startAfter, limit+1)
	if err != nil {
		return nil, err
	}
	if len(backups) > limit {
		last := &backups[limit-1]
		return map[string]any{
			"BackupSummaries":        o.backupSummaries(backups[:limit]),
			"LastEvaluatedBackupArn": o.backupArn(&last.Table, last.ID),
		}, nil
	}
	return map[string]any{"BackupSummaries": o.backupSummaries(backups)}, nil
}

func (o *Ops) backupSummaries(backups []storage.Backup) []map[string]any {
	if backups == nil {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(backups))
	for i := range backups {
		out = append(out, o.backupSummary(&backups[i]))
	}
	return out
}

// timeFromEpoch converts an epoch-seconds value (DynamoDB's timestamp form) to
// a time. A nil value means an open bound.
func timeFromEpoch(sec *float64) time.Time {
	if sec == nil {
		return time.Time{}
	}
	return time.UnixMilli(int64(*sec * 1000)).UTC()
}

type deleteBackupInput struct {
	BackupArn string `json:"BackupArn"`
}

func (o *Ops) deleteBackup(ctx context.Context, body []byte) (any, error) {
	var req deleteBackupInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	id, err := backupIDFromArn(req.BackupArn)
	if err != nil {
		return nil, err
	}
	bs, err := o.backupStore()
	if err != nil {
		return nil, err
	}
	b, err := bs.DeleteBackup(ctx, id)
	if errors.Is(err, storage.ErrBackupNotFound) {
		return nil, ddberr.BackupNotFound("Backup not found: " + req.BackupArn)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"BackupDescription": o.backupDescription(b)}, nil
}

// sseSpecificationOverride is accepted for SDK compatibility and ignored:
// dynamoat encrypts everything at rest through the managed Postgres server.
type sseSpecificationOverride struct {
	Enabled        bool   `json:"Enabled"`
	SSEType        string `json:"SSEType"`
	KMSMasterKeyId string `json:"KMSMasterKeyId"`
}

type restoreTableFromBackupInput struct {
	TargetTableName               string                    `json:"TargetTableName"`
	BackupArn                     string                    `json:"BackupArn"`
	BillingModeOverride           string                    `json:"BillingModeOverride"`
	GlobalSecondaryIndexOverride  []createGSI               `json:"GlobalSecondaryIndexOverride"`
	LocalSecondaryIndexOverride   []createGSI               `json:"LocalSecondaryIndexOverride"`
	ProvisionedThroughputOverride *provisionedThroughput    `json:"ProvisionedThroughputOverride"`
	SSESpecificationOverride      *sseSpecificationOverride `json:"SSESpecificationOverride"`
}

func (o *Ops) restoreTableFromBackup(ctx context.Context, body []byte) (any, error) {
	var req restoreTableFromBackupInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkTableName(req.TargetTableName, "targetTableName"); err != nil {
		return nil, err
	}
	id, err := backupIDFromArn(req.BackupArn)
	if err != nil {
		return nil, err
	}
	bs, err := o.backupStore()
	if err != nil {
		return nil, err
	}
	b, err := bs.GetBackup(ctx, id)
	if errors.Is(err, storage.ErrBackupNotFound) {
		return nil, ddberr.BackupNotFound("Backup not found: " + req.BackupArn)
	}
	if err != nil {
		return nil, err
	}

	target, err := o.restoreTarget(&b.Table, req)
	if err != nil {
		return nil, err
	}
	target.Restore = &storage.RestoreSummary{
		SourceBackupID: id,
		SourceTable:    b.Table.Name,
		At:             b.CreatedAt,
	}

	restored, err := bs.RestoreBackup(ctx, id, target)
	if errors.Is(err, storage.ErrTableExists) {
		return nil, ddberr.TableAlreadyExists("Table already exists: " + req.TargetTableName)
	}
	if errors.Is(err, storage.ErrBackupNotFound) {
		return nil, ddberr.BackupNotFound("Backup not found: " + req.BackupArn)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"TableDescription": o.tableDescription(restored, storage.StatusCreating, restoreTableDescFlags)}, nil
}

// restoreTarget derives the restored table from the backup's definition plus
// the request's overrides. Streams, TTL, tags and PITR are not carried over,
// matching DynamoDB's restore.
func (o *Ops) restoreTarget(src *storage.Table, req restoreTableFromBackupInput) (*storage.Table, error) {
	key := src.Key
	gsis, lsis := splitIndexes(src.GSIs)

	var err error
	gsis, err = applyGlobalSecondaryIndexOverride(gsis, req.GlobalSecondaryIndexOverride, src.AttrDefs)
	if err != nil {
		return nil, err
	}
	lsis, err = applyLocalSecondaryIndexOverride(key, lsis, req.LocalSecondaryIndexOverride, src.AttrDefs)
	if err != nil {
		return nil, err
	}

	target := &storage.Table{
		Name:      req.TargetTableName,
		Key:       key,
		AttrDefs:  cloneAttrDefs(src.AttrDefs),
		GSIs:      append(gsis, lsis...),
		Billing:   src.Billing,
		ReadCU:    src.ReadCU,
		WriteCU:   src.WriteCU,
		CreatedAt: o.now().UTC(),
	}

	if err := applyBillingOverride(target, req.BillingModeOverride, req.ProvisionedThroughputOverride); err != nil {
		return nil, err
	}
	return target, nil
}

func cloneAttrDefs(defs map[string]attr.Type) map[string]attr.Type {
	out := make(map[string]attr.Type, len(defs))
	for k, v := range defs {
		out[k] = v
	}
	return out
}

// applyBillingOverride applies BillingModeOverride and
// ProvisionedThroughputOverride, mirroring CreateTable's validation.
func applyBillingOverride(t *storage.Table, mode string, pt *provisionedThroughput) error {
	switch mode {
	case "PAY_PER_REQUEST":
		if pt != nil {
			return ddberr.Validation("One or more parameter values were invalid: Neither ReadCapacityUnits nor WriteCapacityUnits can be specified when BillingMode is PAY_PER_REQUEST")
		}
		t.Billing = "PAY_PER_REQUEST"
		t.ReadCU, t.WriteCU = 0, 0
	case "", "PROVISIONED":
		if mode == "PROVISIONED" {
			t.Billing = "PROVISIONED"
		}
		if pt != nil {
			if t.Billing == "PAY_PER_REQUEST" {
				return ddberr.Validation("One or more parameter values were invalid: ProvisionedThroughput cannot be specified when BillingMode is PAY_PER_REQUEST")
			}
			t.Billing = "PROVISIONED"
			t.ReadCU = pt.ReadCapacityUnits
			t.WriteCU = pt.WriteCapacityUnits
		}
	default:
		return ddberr.Validation("One or more parameter values were invalid: Invalid BillingModeOverride: %s", mode)
	}
	return nil
}

// applyGlobalSecondaryIndexOverride replaces or adds non-local indexes from the
// override list, validating each as CreateTable validates a GSI.
func applyGlobalSecondaryIndexOverride(gsis []storage.Index, overrides []createGSI, defs map[string]attr.Type) ([]storage.Index, error) {
	if len(overrides) == 0 {
		return gsis, nil
	}
	result := append([]storage.Index(nil), gsis...)
	seen := make(map[string]bool, len(overrides))
	for _, ov := range overrides {
		if err := checkIndexName(ov.IndexName); err != nil {
			return nil, err
		}
		if seen[ov.IndexName] {
			return nil, ddberr.Validation("One or more parameter values were invalid: Duplicate index name: %s", ov.IndexName)
		}
		seen[ov.IndexName] = true
		ks, ok := parseKeySchema(ov.KeySchema)
		if !ok {
			return nil, ddberr.Validation("Invalid KeySchema: Some index key schema element is not valid")
		}
		proj, err := validateProjection(ov.Projection)
		if err != nil {
			return nil, err
		}
		if err := checkOverrideKeyAttrs(ks, defs); err != nil {
			return nil, err
		}
		ks.HashType = defs[ks.Hash]
		if ks.Range != "" {
			ks.RangeType = defs[ks.Range]
		}
		idx := storage.Index{
			Name:        ov.IndexName,
			Key:         ks,
			Projection:  proj.ProjectionType,
			NonKeyAttrs: proj.NonKeyAttributes,
		}
		replaced := false
		for i := range result {
			if result[i].Name == ov.IndexName {
				result[i] = idx
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, idx)
		}
	}
	if len(result) > 20 {
		return nil, ddberr.Validation("One or more parameter values were invalid: Number of GlobalSecondaryIndexes exceeds the limit of 20")
	}
	return result, nil
}

// applyLocalSecondaryIndexOverride replaces or adds local indexes from the
// override list, validating each as CreateTable validates an LSI.
func applyLocalSecondaryIndexOverride(tableKey storage.KeySchema, lsis []storage.Index, overrides []createGSI, defs map[string]attr.Type) ([]storage.Index, error) {
	if len(overrides) == 0 {
		return lsis, nil
	}
	if tableKey.Range == "" {
		return nil, ddberr.Validation("One or more parameter values were invalid: Table KeySchema does not have a range key, which is required when specifying a LocalSecondaryIndex")
	}
	result := append([]storage.Index(nil), lsis...)
	seen := make(map[string]bool, len(overrides))
	for _, ov := range overrides {
		if err := checkIndexNameField(ov.IndexName, "localSecondaryIndexOverride"); err != nil {
			return nil, err
		}
		if seen[ov.IndexName] {
			return nil, ddberr.Validation("One or more parameter values were invalid: Duplicate index name: %s", ov.IndexName)
		}
		seen[ov.IndexName] = true
		if ov.ProvisionedThroughput != nil {
			return nil, ddberr.Validation("One or more parameter values were invalid: ProvisionedThroughput cannot be specified for LocalSecondaryIndex")
		}
		ks, ok := parseKeySchema(ov.KeySchema)
		if !ok {
			return nil, ddberr.Validation("Invalid KeySchema: Some index key schema element is not valid")
		}
		if ks.Hash != tableKey.Hash {
			return nil, ddberr.Validation("Index KeySchema does not have the same leading hash key as table KeySchema for index: %s", ov.IndexName)
		}
		if ks.Range == "" {
			return nil, ddberr.Validation("One or more parameter values were invalid: Local Secondary index must specify a range key")
		}
		if ks.Range == tableKey.Range {
			return nil, ddberr.Validation("One or more parameter values were invalid: Local Secondary index range key must be different from the table's range key")
		}
		proj, err := validateProjection(ov.Projection)
		if err != nil {
			return nil, err
		}
		if err := checkOverrideKeyAttrs(ks, defs); err != nil {
			return nil, err
		}
		ks.HashType = defs[ks.Hash]
		ks.RangeType = defs[ks.Range]
		idx := storage.Index{
			Name:        ov.IndexName,
			Key:         ks,
			Projection:  proj.ProjectionType,
			NonKeyAttrs: proj.NonKeyAttributes,
			Local:       true,
		}
		replaced := false
		for i := range result {
			if result[i].Name == ov.IndexName {
				result[i] = idx
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, idx)
		}
	}
	if len(result) > 5 {
		return nil, ddberr.Validation("One or more parameter values were invalid: Number of LocalSecondaryIndexes exceeds the limit of 5")
	}
	return result, nil
}

func checkOverrideKeyAttrs(ks storage.KeySchema, defs map[string]attr.Type) error {
	keys := []string{ks.Hash}
	if ks.Range != "" {
		keys = append(keys, ks.Range)
	}
	return checkKeyAttrsDefined(keys, defs)
}

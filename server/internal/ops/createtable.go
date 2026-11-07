package ops

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() { register("CreateTable", (*Ops).createTable) }

type keySchemaElement struct {
	AttributeName string `json:"AttributeName"`
	KeyType       string `json:"KeyType"`
}

type attributeDefinition struct {
	AttributeName string `json:"AttributeName"`
	AttributeType string `json:"AttributeType"`
}

type projection struct {
	ProjectionType   string   `json:"ProjectionType"`
	NonKeyAttributes []string `json:"NonKeyAttributes"`
}

type provisionedThroughput struct {
	ReadCapacityUnits  int64 `json:"ReadCapacityUnits"`
	WriteCapacityUnits int64 `json:"WriteCapacityUnits"`
}

type streamSpecification struct {
	StreamEnabled  bool   `json:"StreamEnabled"`
	StreamViewType string `json:"StreamViewType"`
}

type tag struct {
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

type createGSI struct {
	IndexName             string                 `json:"IndexName"`
	KeySchema             []keySchemaElement     `json:"KeySchema"`
	Projection            projection             `json:"Projection"`
	ProvisionedThroughput *provisionedThroughput `json:"ProvisionedThroughput"`
}

type createTableInput struct {
	TableName                 string                 `json:"TableName"`
	KeySchema                 []keySchemaElement     `json:"KeySchema"`
	AttributeDefinitions      []attributeDefinition  `json:"AttributeDefinitions"`
	BillingMode               string                 `json:"BillingMode"`
	ProvisionedThroughput     *provisionedThroughput `json:"ProvisionedThroughput"`
	GlobalSecondaryIndexes    []createGSI            `json:"GlobalSecondaryIndexes"`
	LocalSecondaryIndexes     []createGSI            `json:"LocalSecondaryIndexes"`
	StreamSpecification       *streamSpecification   `json:"StreamSpecification"`
	Tags                      []tag                  `json:"Tags"`
	DeletionProtectionEnabled *bool                  `json:"DeletionProtectionEnabled"`
}

func (o *Ops) createTable(ctx context.Context, body []byte) (any, error) {
	var req createTableInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}

	if err := checkTableName(req.TableName, "tableName"); err != nil {
		return nil, err
	}
	if err := validateTags(req.Tags); err != nil {
		return nil, err
	}

	key, err := tableKeySchema(req.KeySchema)
	if err != nil {
		return nil, err
	}

	// Parse every index key schema up front so the key-attribute checks below
	// see the full set of key attributes.
	gsiKeys := make([]storage.KeySchema, len(req.GlobalSecondaryIndexes))
	for i, g := range req.GlobalSecondaryIndexes {
		ks, ok := parseKeySchema(g.KeySchema)
		if !ok {
			return nil, ddberr.Validation("Invalid KeySchema: Some index key schema element is not valid")
		}
		gsiKeys[i] = ks
	}
	lsiKeys := make([]storage.KeySchema, len(req.LocalSecondaryIndexes))
	for i, l := range req.LocalSecondaryIndexes {
		ks, ok := parseKeySchema(l.KeySchema)
		if !ok {
			return nil, ddberr.Validation("Invalid KeySchema: Some index key schema element is not valid")
		}
		lsiKeys[i] = ks
	}

	defs, err := parseAttributeDefinitions(req.AttributeDefinitions)
	if err != nil {
		return nil, err
	}

	keys := keyAttributes(key, gsiKeys, lsiKeys)
	if err := checkKeyAttrsDefined(keys, defs); err != nil {
		return nil, err
	}
	if err := checkNoUnusedDefs(keys, defs); err != nil {
		return nil, err
	}

	key.HashType = defs[key.Hash]
	if key.Range != "" {
		key.RangeType = defs[key.Range]
	}

	billing, read, write, err := validateBilling(req.BillingMode, req.ProvisionedThroughput)
	if err != nil {
		return nil, err
	}

	gsis, gsiNames, err := buildGSIs(req.GlobalSecondaryIndexes, gsiKeys, defs)
	if err != nil {
		return nil, err
	}

	lsis, err := buildLSIs(key, req.LocalSecondaryIndexes, lsiKeys, defs, gsiNames)
	if err != nil {
		return nil, err
	}
	gsis = append(gsis, lsis...)

	streamView, err := validateStreamSpec(req.StreamSpecification)
	if err != nil {
		return nil, err
	}

	now := o.now().UTC()
	t := &storage.Table{
		Name:               req.TableName,
		Key:                key,
		AttrDefs:           defs,
		GSIs:               gsis,
		Billing:            billing,
		ReadCU:             read,
		WriteCU:            write,
		CreatedAt:          now,
		StreamView:         streamView,
		Tags:               tagsMap(req.Tags),
		DeletionProtection: req.DeletionProtectionEnabled != nil && *req.DeletionProtectionEnabled,
	}
	if streamView != "" {
		t.StreamLabel = now.Format("2006-01-02T15:04:05.000")
	}

	created, err := o.Store.CreateTable(ctx, t)
	if errors.Is(err, storage.ErrTableExists) {
		return nil, ddberr.ResourceInUse("Table already exists: " + req.TableName)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"TableDescription": o.tableDescription(created, storage.StatusCreating, tableDescFlags{noPayPerRequestDateTime: true})}, nil
}

// tableKeySchema validates the table KeySchema: exactly the HASH key and an
// optional RANGE key, in that order.
func tableKeySchema(elems []keySchemaElement) (storage.KeySchema, error) {
	if elems == nil {
		return storage.KeySchema{}, ddberr.Validation("1 validation error detected: Value null at 'keySchema' failed to satisfy constraint: Member must not be null")
	}
	ks, ok := parseKeySchema(elems)
	if !ok {
		return storage.KeySchema{}, ddberr.Validation("Invalid KeySchema: Some index key schema element is not valid")
	}
	return ks, nil
}

// parseKeySchema checks the shape of a key schema (1 or 2 elements, HASH then
// RANGE) and returns the attribute names without their types.
func parseKeySchema(elems []keySchemaElement) (storage.KeySchema, bool) {
	if len(elems) < 1 || len(elems) > 2 {
		return storage.KeySchema{}, false
	}
	if elems[0].KeyType != "HASH" {
		return storage.KeySchema{}, false
	}
	ks := storage.KeySchema{Hash: elems[0].AttributeName}
	if len(elems) == 2 {
		if elems[1].KeyType != "RANGE" {
			return storage.KeySchema{}, false
		}
		ks.Range = elems[1].AttributeName
	}
	return ks, true
}

// keyAttributes lists every key attribute, table first then each index, in
// request order.
func keyAttributes(key storage.KeySchema, gsiKeys, lsiKeys []storage.KeySchema) []string {
	keys := []string{key.Hash}
	if key.Range != "" {
		keys = append(keys, key.Range)
	}
	for _, ks := range gsiKeys {
		keys = append(keys, ks.Hash)
		if ks.Range != "" {
			keys = append(keys, ks.Range)
		}
	}
	for _, ks := range lsiKeys {
		keys = append(keys, ks.Hash)
		if ks.Range != "" {
			keys = append(keys, ks.Range)
		}
	}
	return keys
}

func parseAttributeDefinitions(defs []attributeDefinition) (map[string]attr.Type, error) {
	m := make(map[string]attr.Type, len(defs))
	for _, d := range defs {
		m[d.AttributeName] = attrTypeFromString(d.AttributeType)
	}
	return m, nil
}

func attrTypeFromString(s string) attr.Type {
	switch s {
	case "S":
		return attr.TypeS
	case "N":
		return attr.TypeN
	case "B":
		return attr.TypeB
	case "BOOL":
		return attr.TypeBOOL
	case "NULL":
		return attr.TypeNULL
	case "M":
		return attr.TypeM
	case "L":
		return attr.TypeL
	case "SS":
		return attr.TypeSS
	case "NS":
		return attr.TypeNS
	case "BS":
		return attr.TypeBS
	}
	return 0
}

func isScalarKeyType(t attr.Type) bool {
	return t == attr.TypeS || t == attr.TypeN || t == attr.TypeB
}

func checkKeyAttrsDefined(keys []string, defs map[string]attr.Type) error {
	var missing []string
	for _, k := range keys {
		if t, ok := defs[k]; !ok || !isScalarKeyType(t) {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return ddberr.Validation("One or more parameter values were invalid: Some index key attributes are not defined in AttributeDefinitions. Keys: [%s], AttributeDefinitions: [%s]",
		strings.Join(missing, ", "), strings.Join(sortedDefNames(defs), ", "))
}

func sortedDefNames(defs map[string]attr.Type) []string {
	names := make([]string, 0, len(defs))
	for n := range defs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func checkNoUnusedDefs(keys []string, defs map[string]attr.Type) error {
	distinct := make(map[string]bool, len(keys))
	for _, k := range keys {
		distinct[k] = true
	}
	if len(distinct) != len(defs) {
		return ddberr.Validation("One or more parameter values were invalid: Number of attributes in KeySchema does not exactly match number of attributes defined in AttributeDefinitions")
	}
	for k := range distinct {
		if _, ok := defs[k]; !ok {
			return ddberr.Validation("One or more parameter values were invalid: Number of attributes in KeySchema does not exactly match number of attributes defined in AttributeDefinitions")
		}
	}
	return nil
}

func validateBilling(mode string, pt *provisionedThroughput) (string, int64, int64, error) {
	if mode == "PAY_PER_REQUEST" {
		if pt != nil {
			return "", 0, 0, ddberr.Validation("One or more parameter values were invalid: Neither ReadCapacityUnits nor WriteCapacityUnits can be specified when BillingMode is PAY_PER_REQUEST")
		}
		return "PAY_PER_REQUEST", 0, 0, nil
	}
	// An absent BillingMode means PROVISIONED, DynamoDB's default.
	if pt == nil {
		return "", 0, 0, ddberr.Validation("One or more parameter values were invalid: ReadCapacityUnits and WriteCapacityUnits must both be specified when BillingMode is PROVISIONED")
	}
	return "PROVISIONED", pt.ReadCapacityUnits, pt.WriteCapacityUnits, nil
}

func buildGSIs(gsis []createGSI, keys []storage.KeySchema, defs map[string]attr.Type) ([]storage.Index, map[string]bool, error) {
	if len(gsis) > 20 {
		return nil, nil, ddberr.Validation("One or more parameter values were invalid: Number of GlobalSecondaryIndexes exceeds the limit of 20")
	}
	seen := make(map[string]bool, len(gsis))
	out := make([]storage.Index, 0, len(gsis))
	for i, g := range gsis {
		if err := checkIndexName(g.IndexName); err != nil {
			return nil, nil, err
		}
		if seen[g.IndexName] {
			return nil, nil, ddberr.Validation("One or more parameter values were invalid: Duplicate index name: %s", g.IndexName)
		}
		seen[g.IndexName] = true
		proj, err := validateProjection(g.Projection)
		if err != nil {
			return nil, nil, err
		}
		ks := keys[i]
		ks.HashType = defs[ks.Hash]
		if ks.Range != "" {
			ks.RangeType = defs[ks.Range]
		}
		out = append(out, storage.Index{
			Name:        g.IndexName,
			Key:         ks,
			Projection:  proj.ProjectionType,
			NonKeyAttrs: proj.NonKeyAttributes,
		})
	}
	return out, seen, nil
}

// buildLSIs validates and builds the table's local secondary indexes. taken
// holds the index names already used by the GSIs, so names stay unique across
// both kinds.
func buildLSIs(tableKey storage.KeySchema, lsis []createGSI, keys []storage.KeySchema, defs map[string]attr.Type, taken map[string]bool) ([]storage.Index, error) {
	if len(lsis) == 0 {
		return nil, nil
	}
	if len(lsis) > 5 {
		return nil, ddberr.Validation("One or more parameter values were invalid: Number of LocalSecondaryIndexes exceeds the limit of 5")
	}
	if tableKey.Range == "" {
		return nil, ddberr.Validation("One or more parameter values were invalid: Table KeySchema does not have a range key, which is required when specifying a LocalSecondaryIndex")
	}
	out := make([]storage.Index, 0, len(lsis))
	for i, l := range lsis {
		if err := checkIndexNameField(l.IndexName, "localSecondaryIndexes"); err != nil {
			return nil, err
		}
		if taken[l.IndexName] {
			return nil, ddberr.Validation("One or more parameter values were invalid: Duplicate index name: %s", l.IndexName)
		}
		taken[l.IndexName] = true
		if l.ProvisionedThroughput != nil {
			return nil, ddberr.Validation("One or more parameter values were invalid: ProvisionedThroughput cannot be specified for LocalSecondaryIndex")
		}
		ks := keys[i]
		if ks.Hash != tableKey.Hash {
			return nil, ddberr.Validation("Index KeySchema does not have the same leading hash key as table KeySchema for index: %s", l.IndexName)
		}
		if ks.Range == "" {
			return nil, ddberr.Validation("One or more parameter values were invalid: Local Secondary index must specify a range key")
		}
		if ks.Range == tableKey.Range {
			return nil, ddberr.Validation("One or more parameter values were invalid: Local Secondary index range key must be different from the table's range key")
		}
		proj, err := validateProjection(l.Projection)
		if err != nil {
			return nil, err
		}
		ks.HashType = defs[ks.Hash]
		ks.RangeType = defs[ks.Range]
		out = append(out, storage.Index{
			Name:        l.IndexName,
			Key:         ks,
			Projection:  proj.ProjectionType,
			NonKeyAttrs: proj.NonKeyAttributes,
			Local:       true,
		})
	}
	return out, nil
}

func checkIndexName(name string) error {
	return checkIndexNameField(name, "globalSecondaryIndexes")
}

func checkIndexNameField(name, field string) error {
	switch {
	case len(name) < 3:
		return ddberr.Validation("1 validation error detected: Value '%s' at '%s' failed to satisfy constraint: Member must have length greater than or equal to 3", name, field)
	case len(name) > 255:
		return ddberr.Validation("1 validation error detected: Value '%s' at '%s' failed to satisfy constraint: Member must have length less than or equal to 255", name, field)
	case !tableNameRE.MatchString(name):
		return ddberr.Validation("1 validation error detected: Value '%s' at '%s' failed to satisfy constraint: Member must satisfy regular expression pattern: [a-zA-Z0-9_.-]+", name, field)
	}
	return nil
}

func validateProjection(p projection) (projection, error) {
	switch p.ProjectionType {
	case storage.ProjectAll, storage.ProjectKeysOnly:
		if len(p.NonKeyAttributes) > 0 {
			return projection{}, ddberr.Validation("One or more parameter values were invalid: NonKeyAttributes can only be specified when ProjectionType is INCLUDE")
		}
	case storage.ProjectInclude:
		if len(p.NonKeyAttributes) > 100 {
			return projection{}, ddberr.Validation("One or more parameter values were invalid: The number of non-key attributes in the projected index exceeds the maximum number allowed")
		}
	default:
		return projection{}, ddberr.Validation("One or more parameter values were invalid: Invalid projection type. Valid values are ALL | KEYS_ONLY | INCLUDE")
	}
	return p, nil
}

func validateStreamSpec(spec *streamSpecification) (string, error) {
	if spec == nil || !spec.StreamEnabled {
		return "", nil
	}
	if spec.StreamViewType == "" {
		return "", ddberr.Validation("One or more parameter values were invalid: StreamViewType is required when StreamEnabled is true")
	}
	switch spec.StreamViewType {
	case storage.StreamKeysOnly, storage.StreamNewImage, storage.StreamOldImage, storage.StreamNewAndOld:
		return spec.StreamViewType, nil
	}
	return "", ddberr.Validation("One or more parameter values were invalid: Invalid StreamViewType: %s", spec.StreamViewType)
}

func tagsMap(tags []tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[t.Key] = t.Value
	}
	return m
}

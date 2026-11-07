package ops

import (
	"net/http"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/api"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
)

// storeNoTTL hides the TTL capability of an underlying store.
type storeNoTTL struct{ storage.Store }

func newTTLHandler(t *testing.T) (*Ops, http.Handler, *memstore.Store) {
	t.Helper()
	o, h, store := newOps(t)
	makeTable(t, store, "ttl", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	return o, h, store
}

func TestUpdateTimeToLiveEnableDescribe(t *testing.T) {
	_, h, _ := newTTLHandler(t)

	rec := post(t, h, "UpdateTimeToLive", `{"TableName":"ttl","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"expiresAt"}}`)
	m := wantStatus(t, rec, 200)
	equalJSON(t, m["TimeToLiveSpecification"], map[string]any{"Enabled": true, "AttributeName": "expiresAt"}, "UpdateTimeToLive response")

	rec = post(t, h, "DescribeTimeToLive", `{"TableName":"ttl"}`)
	m = wantStatus(t, rec, 200)
	equalJSON(t, m["TimeToLiveDescription"], map[string]any{"TimeToLiveStatus": "ENABLED", "AttributeName": "expiresAt"}, "DescribeTimeToLive response")
}

func TestDescribeTimeToLiveDisabled(t *testing.T) {
	_, h, _ := newTTLHandler(t)

	rec := post(t, h, "DescribeTimeToLive", `{"TableName":"ttl"}`)
	m := wantStatus(t, rec, 200)
	equalJSON(t, m["TimeToLiveDescription"], map[string]any{"TimeToLiveStatus": "DISABLED"}, "DescribeTimeToLive response")
}

func TestUpdateTimeToLiveDisable(t *testing.T) {
	_, h, _ := newTTLHandler(t)

	post(t, h, "UpdateTimeToLive", `{"TableName":"ttl","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"expiresAt"}}`)

	rec := post(t, h, "UpdateTimeToLive", `{"TableName":"ttl","TimeToLiveSpecification":{"Enabled":false,"AttributeName":"expiresAt"}}`)
	m := wantStatus(t, rec, 200)
	equalJSON(t, m["TimeToLiveSpecification"], map[string]any{"Enabled": false, "AttributeName": "expiresAt"}, "UpdateTimeToLive response")

	rec = post(t, h, "DescribeTimeToLive", `{"TableName":"ttl"}`)
	m = wantStatus(t, rec, 200)
	equalJSON(t, m["TimeToLiveDescription"], map[string]any{"TimeToLiveStatus": "DISABLED"}, "DescribeTimeToLive after disable")
}

func TestUpdateTimeToLiveAlreadyEnabled(t *testing.T) {
	_, h, _ := newTTLHandler(t)
	post(t, h, "UpdateTimeToLive", `{"TableName":"ttl","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"expiresAt"}}`)
	rec := post(t, h, "UpdateTimeToLive", `{"TableName":"ttl","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"expiresAt"}}`)
	wantErr(t, rec, typValidation, "TimeToLive is already enabled")
}

func TestUpdateTimeToLiveAlreadyDisabled(t *testing.T) {
	_, h, _ := newTTLHandler(t)
	rec := post(t, h, "UpdateTimeToLive", `{"TableName":"ttl","TimeToLiveSpecification":{"Enabled":false,"AttributeName":"expiresAt"}}`)
	wantErr(t, rec, typValidation, "TimeToLive is already disabled")
}

func TestUpdateTimeToLiveDifferentAttributeName(t *testing.T) {
	_, h, _ := newTTLHandler(t)
	post(t, h, "UpdateTimeToLive", `{"TableName":"ttl","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"expiresAt"}}`)
	rec := post(t, h, "UpdateTimeToLive", `{"TableName":"ttl","TimeToLiveSpecification":{"Enabled":false,"AttributeName":"other"}}`)
	wantErr(t, rec, typValidation, "TimeToLive is active on a different AttributeName: current AttributeName is expiresAt")
}

func TestUpdateTimeToLiveEmptyAttributeName(t *testing.T) {
	_, h, _ := newTTLHandler(t)
	rec := post(t, h, "UpdateTimeToLive", `{"TableName":"ttl","TimeToLiveSpecification":{"Enabled":true,"AttributeName":""}}`)
	wantErr(t, rec, typValidation,
		"1 validation error detected: Value '' at 'timeToLiveSpecification.attributeName' failed to satisfy constraint: Member must have length greater than or equal to 1")
}

func TestUpdateTimeToLiveMissingTable(t *testing.T) {
	_, h, _ := newTTLHandler(t)
	rec := post(t, h, "UpdateTimeToLive", `{"TableName":"nope","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"expiresAt"}}`)
	wantErr(t, rec, typResourceMissing, "Requested resource not found: Table: nope not found")
}

func TestDescribeTimeToLiveMissingTable(t *testing.T) {
	_, h, _ := newTTLHandler(t)
	rec := post(t, h, "DescribeTimeToLive", `{"TableName":"nope"}`)
	wantErr(t, rec, typResourceMissing, "Requested resource not found: Table: nope not found")
}

func TestTimeToLiveUnsupportedStore(t *testing.T) {
	store := memstore.New()
	o := &Ops{Store: storeNoTTL{store}}
	h := api.New(api.Options{})
	o.Register(h)

	rec := post(t, h, "UpdateTimeToLive", `{"TableName":"ttl","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"expiresAt"}}`)
	wantErr(t, rec, typValidation, "dynamoat: this store does not support TTL")

	rec = post(t, h, "DescribeTimeToLive", `{"TableName":"ttl"}`)
	wantErr(t, rec, typValidation, "dynamoat: this store does not support TTL")
}

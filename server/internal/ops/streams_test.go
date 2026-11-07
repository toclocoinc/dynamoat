package ops

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/api"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
	"github.com/toclocoinc/dynamoat/server/internal/streams"
)

var streamsNow = time.Date(2026, 11, 6, 12, 30, 45, 123000000, time.UTC)

const streamsLabel = "2026-11-06T12:30:45.123"

func streamArn(name string) string {
	return "arn:aws:dynamodb:" + testRegion + ":000000000000:table/" + name + "/stream/" + streamsLabel
}

func newStreamsEnv(t *testing.T, nowFn func() time.Time) (*Ops, http.Handler, *memstore.Store) {
	t.Helper()
	store := memstore.New()
	o := &Ops{Store: store, Now: nowFn, Region: testRegion}
	h := api.New(api.Options{})
	o.Register(h)
	return o, h, store
}

func postStreams(t *testing.T, h http.Handler, op, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "DynamoDBStreams_20120810."+op)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// createStreamTable creates a table with a NEW_AND_OLD_IMAGES stream and
// returns its stream ARN.
func createStreamTable(t *testing.T, h http.Handler, name string) string {
	t.Helper()
	body := fmt.Sprintf(`{"TableName":%q,"KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],`+
		`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],`+
		`"BillingMode":"PAY_PER_REQUEST","StreamSpecification":{"StreamEnabled":true,"StreamViewType":"NEW_AND_OLD_IMAGES"}}`, name)
	rec := post(t, h, "CreateTable", body)
	if rec.Code != 200 {
		t.Fatalf("CreateTable(%s) status %d: %s", name, rec.Code, rec.Body.String())
	}
	return streamArn(name)
}

// bulkPut writes n INSERT stream records directly to the store.
func bulkPut(t *testing.T, store *memstore.Store, name string, n int) {
	t.Helper()
	td, err := store.GetTable(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id-%06d", i)
		_, _, err := store.WriteItem(context.Background(), storage.Write{
			Table: td,
			Key:   storage.Key{Hash: attr.Value{Type: attr.TypeS, S: id}},
			Fn: func(old attr.Item) (storage.WriteResult, error) {
				return storage.WriteResult{
					Op: storage.Put,
					Item: attr.Item{
						"id": attr.Value{Type: attr.TypeS, S: id},
						"n":  attr.Value{Type: attr.TypeN, N: strconv.Itoa(i)},
					},
				}, nil
			},
		})
		if err != nil {
			t.Fatalf("bulkPut(%d): %v", i, err)
		}
	}
}

func shardIteratorOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	m := bodyJSON(t, rec)
	it, ok := m["ShardIterator"].(string)
	if !ok || it == "" {
		t.Fatalf("no ShardIterator in %v", m)
	}
	return it
}

func recordsOf(t *testing.T, rec *httptest.ResponseRecorder) ([]map[string]any, string) {
	t.Helper()
	m := bodyJSON(t, rec)
	raw, _ := m["Records"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	next, _ := m["NextShardIterator"].(string)
	return out, next
}

func seqOf(rec map[string]any) string {
	ddb := rec["dynamodb"].(map[string]any)
	return ddb["SequenceNumber"].(string)
}

func TestListStreams(t *testing.T) {
	_, h, _ := newStreamsEnv(t, func() time.Time { return streamsNow })
	for _, n := range []string{"saa", "sbb", "scc"} {
		createStreamTable(t, h, n)
	}
	// A table without a stream must not appear.
	rec := post(t, h, "CreateTable", `{"TableName":"none","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST"}`)
	if rec.Code != 200 {
		t.Fatalf("CreateTable(none): %s", rec.Body.String())
	}

	rec = postStreams(t, h, "ListStreams", `{}`)
	m := bodyJSON(t, rec)
	refs := m["Streams"].([]any)
	if len(refs) != 3 {
		t.Fatalf("Streams = %v, want 3 entries", refs)
	}
	if refs[0].(map[string]any)["TableName"] != "saa" || refs[1].(map[string]any)["TableName"] != "sbb" || refs[2].(map[string]any)["TableName"] != "scc" {
		t.Errorf("Streams not in name order: %v", refs)
	}
	if refs[0].(map[string]any)["StreamArn"] != streamArn("saa") {
		t.Errorf("StreamArn = %v", refs[0].(map[string]any)["StreamArn"])
	}
	if refs[0].(map[string]any)["StreamLabel"] != streamsLabel {
		t.Errorf("StreamLabel = %v", refs[0].(map[string]any)["StreamLabel"])
	}

	// TableName filters to one table's stream.
	rec = postStreams(t, h, "ListStreams", `{"TableName":"sbb"}`)
	m = bodyJSON(t, rec)
	refs = m["Streams"].([]any)
	if len(refs) != 1 || refs[0].(map[string]any)["TableName"] != "sbb" {
		t.Errorf("TableName filter Streams = %v", refs)
	}

	// Pagination by ExclusiveStartStreamArn.
	rec = postStreams(t, h, "ListStreams", `{"Limit":2}`)
	m = bodyJSON(t, rec)
	refs = m["Streams"].([]any)
	if len(refs) != 2 {
		t.Fatalf("page 1 = %v", refs)
	}
	lastArn := m["LastEvaluatedStreamArn"]
	if lastArn != streamArn("sbb") {
		t.Errorf("LastEvaluatedStreamArn = %v, want %s", lastArn, streamArn("sbb"))
	}

	rec = postStreams(t, h, "ListStreams", fmt.Sprintf(`{"Limit":2,"ExclusiveStartStreamArn":%q}`, streamArn("sbb")))
	m = bodyJSON(t, rec)
	refs = m["Streams"].([]any)
	if len(refs) != 1 || refs[0].(map[string]any)["TableName"] != "scc" {
		t.Errorf("page 2 = %v", refs)
	}
	if _, ok := m["LastEvaluatedStreamArn"]; ok {
		t.Errorf("page 2 has LastEvaluatedStreamArn: %v", m)
	}
}

func TestDescribeStream(t *testing.T) {
	_, h, store := newStreamsEnv(t, func() time.Time { return streamsNow })
	arn := createStreamTable(t, h, "desc")

	rec := postStreams(t, h, "DescribeStream", fmt.Sprintf(`{"StreamArn":%q}`, arn))
	if rec.Code != 200 {
		t.Fatalf("DescribeStream status %d: %s", rec.Code, rec.Body.String())
	}
	desc := bodyJSON(t, rec)["StreamDescription"].(map[string]any)
	if desc["StreamArn"] != arn {
		t.Errorf("StreamArn = %v", desc["StreamArn"])
	}
	if desc["StreamLabel"] != streamsLabel {
		t.Errorf("StreamLabel = %v", desc["StreamLabel"])
	}
	if desc["StreamStatus"] != "ENABLED" {
		t.Errorf("StreamStatus = %v", desc["StreamStatus"])
	}
	if desc["StreamViewType"] != "NEW_AND_OLD_IMAGES" {
		t.Errorf("StreamViewType = %v", desc["StreamViewType"])
	}
	if desc["CreationRequestDateTime"] != float64(streamsNow.UnixMilli())/1000 {
		t.Errorf("CreationRequestDateTime = %v", desc["CreationRequestDateTime"])
	}
	if desc["TableName"] != "desc" {
		t.Errorf("TableName = %v", desc["TableName"])
	}
	ks := desc["KeySchema"].([]any)
	if len(ks) != 1 || ks[0].(map[string]any)["AttributeName"] != "id" || ks[0].(map[string]any)["KeyType"] != "HASH" {
		t.Errorf("KeySchema = %v", ks)
	}

	td, err := store.GetTable(context.Background(), "desc")
	if err != nil {
		t.Fatal(err)
	}
	shards := desc["Shards"].([]any)
	if len(shards) != 1 {
		t.Fatalf("Shards = %v", shards)
	}
	shard := shards[0].(map[string]any)
	if shard["ShardId"] != streams.ShardID(td.ID) {
		t.Errorf("ShardId = %v, want %s", shard["ShardId"], streams.ShardID(td.ID))
	}
	rng := shard["SequenceNumberRange"].(map[string]any)
	if rng["StartingSequenceNumber"] != "000000000000000000001" {
		t.Errorf("StartingSequenceNumber = %v", rng["StartingSequenceNumber"])
	}
}

func TestGetShardIteratorTypes(t *testing.T) {
	_, h, store := newStreamsEnv(t, func() time.Time { return streamsNow })
	arn := createStreamTable(t, h, "iters")
	bulkPut(t, store, "iters", 3)

	shardID := streams.ShardID(1)

	// TRIM_HORIZON reads every record from the start.
	rec := postStreams(t, h, "GetShardIterator", fmt.Sprintf(`{"StreamArn":%q,"ShardId":%q,"ShardIteratorType":"TRIM_HORIZON"}`, arn, shardID))
	it := shardIteratorOf(t, rec)
	rec = postStreams(t, h, "GetRecords", fmt.Sprintf(`{"ShardIterator":%q}`, it))
	recs, _ := recordsOf(t, rec)
	if len(recs) != 3 || seqOf(recs[0]) != "000000000000000000001" || seqOf(recs[2]) != "000000000000000000003" {
		t.Errorf("TRIM_HORIZON records = %v", recs)
	}

	// LATEST starts after the newest record, so it is empty now.
	rec = postStreams(t, h, "GetShardIterator", fmt.Sprintf(`{"StreamArn":%q,"ShardId":%q,"ShardIteratorType":"LATEST"}`, arn, shardID))
	it = shardIteratorOf(t, rec)
	rec = postStreams(t, h, "GetRecords", fmt.Sprintf(`{"ShardIterator":%q}`, it))
	if recs, _ := recordsOf(t, rec); len(recs) != 0 {
		t.Errorf("LATEST records = %v, want empty", recs)
	}

	// AT_SEQUENCE_NUMBER includes the named record.
	rec = postStreams(t, h, "GetShardIterator", fmt.Sprintf(`{"StreamArn":%q,"ShardId":%q,"ShardIteratorType":"AT_SEQUENCE_NUMBER","SequenceNumber":"000000000000000000002"}`, arn, shardID))
	it = shardIteratorOf(t, rec)
	rec = postStreams(t, h, "GetRecords", fmt.Sprintf(`{"ShardIterator":%q}`, it))
	recs, _ = recordsOf(t, rec)
	if len(recs) != 2 || seqOf(recs[0]) != "000000000000000000002" || seqOf(recs[1]) != "000000000000000000003" {
		t.Errorf("AT_SEQUENCE_NUMBER records = %v", recs)
	}

	// AFTER_SEQUENCE_NUMBER excludes the named record.
	rec = postStreams(t, h, "GetShardIterator", fmt.Sprintf(`{"StreamArn":%q,"ShardId":%q,"ShardIteratorType":"AFTER_SEQUENCE_NUMBER","SequenceNumber":"000000000000000000002"}`, arn, shardID))
	it = shardIteratorOf(t, rec)
	rec = postStreams(t, h, "GetRecords", fmt.Sprintf(`{"ShardIterator":%q}`, it))
	recs, _ = recordsOf(t, rec)
	if len(recs) != 1 || seqOf(recs[0]) != "000000000000000000003" {
		t.Errorf("AFTER_SEQUENCE_NUMBER records = %v", recs)
	}
}

func TestGetRecordsPaging2500(t *testing.T) {
	_, h, store := newStreamsEnv(t, func() time.Time { return streamsNow })
	arn := createStreamTable(t, h, "pages")
	bulkPut(t, store, "pages", 2500)

	shardID := streams.ShardID(1)
	rec := postStreams(t, h, "GetShardIterator", fmt.Sprintf(`{"StreamArn":%q,"ShardId":%q,"ShardIteratorType":"TRIM_HORIZON"}`, arn, shardID))
	it := shardIteratorOf(t, rec)

	total := 0
	for page, want := range []int{1000, 1000, 500, 0} {
		rec = postStreams(t, h, "GetRecords", fmt.Sprintf(`{"ShardIterator":%q,"Limit":1000}`, it))
		recs, next := recordsOf(t, rec)
		if len(recs) != want {
			t.Fatalf("page %d has %d records, want %d", page, len(recs), want)
		}
		if next == "" {
			t.Fatalf("page %d has no NextShardIterator", page)
		}
		if len(recs) > 0 && seqOf(recs[0]) != fmt.Sprintf("%021d", total+1) {
			t.Errorf("page %d first seq = %s, want %s", page, seqOf(recs[0]), fmt.Sprintf("%021d", total+1))
		}
		total += len(recs)
		it = next
	}
	if total != 2500 {
		t.Errorf("total records = %d, want 2500", total)
	}
}

func TestStreamsErrors(t *testing.T) {
	t.Run("expired iterator", func(t *testing.T) {
		now := streamsNow
		_, h, _ := newStreamsEnv(t, func() time.Time { return now })
		arn := createStreamTable(t, h, "exp")
		rec := postStreams(t, h, "GetShardIterator", fmt.Sprintf(`{"StreamArn":%q,"ShardId":%q,"ShardIteratorType":"TRIM_HORIZON"}`, arn, streams.ShardID(1)))
		it := shardIteratorOf(t, rec)

		now = now.Add(16 * time.Minute)
		rec = postStreams(t, h, "GetRecords", fmt.Sprintf(`{"ShardIterator":%q}`, it))
		checkError(t, rec, 400, "com.amazonaws.dynamodb.v20120810#ExpiredIteratorException", "")
	})

	t.Run("trimmed data access", func(t *testing.T) {
		_, h, store := newStreamsEnv(t, func() time.Time { return streamsNow })
		arn := createStreamTable(t, h, "trim")
		bulkPut(t, store, "trim", 1)

		rec := postStreams(t, h, "GetShardIterator", fmt.Sprintf(`{"StreamArn":%q,"ShardId":%q,"ShardIteratorType":"AFTER_SEQUENCE_NUMBER","SequenceNumber":"000000000000000000001"}`, arn, streams.ShardID(1)))
		it := shardIteratorOf(t, rec)

		if _, err := store.TrimStreams(context.Background(), time.Now().Add(time.Hour), 0); err != nil {
			t.Fatal(err)
		}
		rec = postStreams(t, h, "GetRecords", fmt.Sprintf(`{"ShardIterator":%q}`, it))
		checkError(t, rec, 400, "com.amazonaws.dynamodb.v20120810#TrimmedDataAccessException", "")
	})

	t.Run("disabled stream", func(t *testing.T) {
		_, h, _ := newStreamsEnv(t, func() time.Time { return streamsNow })
		arn := createStreamTable(t, h, "off")
		rec := postStreams(t, h, "GetShardIterator", fmt.Sprintf(`{"StreamArn":%q,"ShardId":%q,"ShardIteratorType":"TRIM_HORIZON"}`, arn, streams.ShardID(1)))
		it := shardIteratorOf(t, rec)

		rec = post(t, h, "UpdateTable", `{"TableName":"off","StreamSpecification":{"StreamEnabled":false}}`)
		if rec.Code != 200 {
			t.Fatalf("disable stream: %s", rec.Body.String())
		}
		rec = postStreams(t, h, "GetRecords", fmt.Sprintf(`{"ShardIterator":%q}`, it))
		checkError(t, rec, 400, typResourceMissing, "Requested resource not found")
	})

	t.Run("missing stream", func(t *testing.T) {
		_, h, _ := newStreamsEnv(t, func() time.Time { return streamsNow })
		rec := postStreams(t, h, "DescribeStream", fmt.Sprintf(`{"StreamArn":%q}`, streamArn("nope")))
		checkError(t, rec, 400, typResourceMissing, "Requested resource not found")
	})
}

// TestStreamsUnsupportedStore checks every operation reports the same
// ValidationException when the store has no StreamStore.
func TestStreamsUnsupportedStore(t *testing.T) {
	// plainStore hides StreamStore: it embeds only the base storage.Store
	// interface, so the type assertion in the operations fails.
	store := &plainStore{Store: memstore.New()}
	o := &Ops{Store: store, Region: testRegion}
	h := api.New(api.Options{})
	o.Register(h)

	for op, body := range map[string]string{
		"ListStreams":      `{}`,
		"DescribeStream":   `{"StreamArn":"arn:aws:dynamodb:us-east-1:000000000000:table/t/stream/l"}`,
		"GetShardIterator": `{"StreamArn":"arn:aws:dynamodb:us-east-1:000000000000:table/t/stream/l","ShardId":"x","ShardIteratorType":"TRIM_HORIZON"}`,
		"GetRecords":       `{"ShardIterator":"x"}`,
	} {
		rec := postStreams(t, h, op, body)
		checkError(t, rec, 400, typValidation, "dynamoat: this store does not support streams")
	}
}

// plainStore wraps a storage.Store and exposes only its methods, hiding any
// optional StreamStore or TTLStore capability.
type plainStore struct{ storage.Store }

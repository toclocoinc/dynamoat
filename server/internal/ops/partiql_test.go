package ops

import (
	"context"
	"fmt"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/api"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
)

const typDuplicate = "com.amazonaws.dynamodb.v20120810#DuplicateItemException"

// countingStore wraps a memstore and counts Query and Scan calls, so tests can
// assert which read path a SELECT took.
type countingStore struct {
	*memstore.Store
	queries int
	scans   int
}

func (c *countingStore) Query(ctx context.Context, in storage.QueryInput) (storage.Page, error) {
	c.queries++
	return c.Store.Query(ctx, in)
}

func (c *countingStore) Scan(ctx context.Context, in storage.ScanInput) (storage.Page, error) {
	c.scans++
	return c.Store.Scan(ctx, in)
}

func newCountingOps(t *testing.T) (*Ops, *api.Handler, *countingStore) {
	t.Helper()
	base := memstore.New()
	store := &countingStore{Store: base}
	o := &Ops{Store: store}
	h := api.New(api.Options{})
	o.Register(h)
	return o, h, store
}

func selItems(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, _ := m["Items"].([]any)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i] = r.(map[string]any)
	}
	return out
}

func itemS(it map[string]any, k string) string {
	v, _ := it[k].(map[string]any)
	s, _ := v["S"].(string)
	return s
}

func TestExecuteStatementSelectQueryVsScan(t *testing.T) {
	_, h, store := newCountingOps(t)
	makeTable(t, store.Store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS, Range: "rk", RangeType: attr.TypeS})
	for _, it := range []string{
		`{"pk":{"S":"a"},"rk":{"S":"1"},"v":{"S":"x"}}`,
		`{"pk":{"S":"a"},"rk":{"S":"2"},"v":{"S":"x"}}`,
		`{"pk":{"S":"b"},"rk":{"S":"2"},"v":{"S":"x"}}`,
	} {
		wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":`+it+`}`), 200)
	}

	// Partition key fixed with = runs a Query.
	m := wantStatus(t, post(t, h, "ExecuteStatement", `{"Statement":"SELECT * FROM test WHERE pk = 'a'"}`), 200)
	if len(selItems(t, m)) != 2 {
		t.Errorf("SELECT pk='a' returned %d items, want 2", len(selItems(t, m)))
	}
	if store.queries != 1 || store.scans != 0 {
		t.Errorf("queries/scans = %d/%d, want 1/0", store.queries, store.scans)
	}

	// Only a sort key fixed with = is not a Query; it is a Scan with a filter.
	m = wantStatus(t, post(t, h, "ExecuteStatement", `{"Statement":"SELECT * FROM test WHERE rk = '2'"}`), 200)
	if len(selItems(t, m)) != 2 {
		t.Errorf("SELECT rk='2' returned %d items, want 2", len(selItems(t, m)))
	}
	if store.scans != 1 {
		t.Errorf("scans = %d, want 1", store.scans)
	}

	// Partition key plus sort key still one Query.
	m = wantStatus(t, post(t, h, "ExecuteStatement", `{"Statement":"SELECT * FROM test WHERE pk = 'a' AND rk = '1'"}`), 200)
	if len(selItems(t, m)) != 1 {
		t.Errorf("SELECT pk='a' AND rk='1' returned %d items, want 1", len(selItems(t, m)))
	}
	if store.queries != 2 || store.scans != 1 {
		t.Errorf("queries/scans = %d/%d, want 2/1", store.queries, store.scans)
	}

	// No WHERE is a Scan.
	m = wantStatus(t, post(t, h, "ExecuteStatement", `{"Statement":"SELECT * FROM test"}`), 200)
	if len(selItems(t, m)) != 3 {
		t.Errorf("SELECT * returned %d items, want 3", len(selItems(t, m)))
	}
	if store.scans != 2 {
		t.Errorf("scans = %d, want 2", store.scans)
	}
}

func TestExecuteStatementSelectProjectionAndFilter(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"pk":{"S":"a"},"v":{"S":"1"},"w":{"S":"drop"}}}`), 200)
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"pk":{"S":"b"},"v":{"S":"1"},"w":{"S":"drop"}}}`), 200)

	m := wantStatus(t, post(t, h, "ExecuteStatement", `{"Statement":"SELECT pk FROM test WHERE pk = 'a'"}`), 200)
	items := selItems(t, m)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if _, ok := items[0]["v"]; ok {
		t.Errorf("projection returned v: %v", items[0])
	}
	if itemS(items[0], "pk") != "a" {
		t.Errorf("pk = %q, want a", itemS(items[0], "pk"))
	}

	// Filter drops the only item in the partition, but the Query still reads it.
	m = wantStatus(t, post(t, h, "ExecuteStatement", `{"Statement":"SELECT * FROM test WHERE pk = 'a' AND v = '2'"}`), 200)
	if len(selItems(t, m)) != 0 {
		t.Errorf("filter returned %d items, want 0", len(selItems(t, m)))
	}
}

func TestExecuteStatementParameters(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"pk":{"S":"a"},"v":{"S":"1"}}}`), 200)

	m := wantStatus(t, post(t, h, "ExecuteStatement", `{"Statement":"SELECT * FROM test WHERE pk = ?","Parameters":[{"S":"a"}]}`), 200)
	if len(selItems(t, m)) != 1 {
		t.Errorf("got %d items, want 1", len(selItems(t, m)))
	}
}

func TestExecuteStatementInsertDuplicate(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS})

	wantStatus(t, post(t, h, "ExecuteStatement", `{"Statement":"INSERT INTO test VALUE {'pk':'a','v':'1'}"}`), 200)
	rec := post(t, h, "ExecuteStatement", `{"Statement":"INSERT INTO test VALUE {'pk':'a','v':'2'}"}`)
	wantErr(t, rec, typDuplicate, "Duplicate primary key exists in table")
}

func TestExecuteStatementWriteEmptyItems(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS})

	for _, stmt := range []string{
		`INSERT INTO test VALUE {'pk':'a','v':'1'}`,
		`UPDATE test SET v = '2' WHERE pk = 'a'`,
		`DELETE FROM test WHERE pk = 'a'`,
	} {
		m := wantStatus(t, post(t, h, "ExecuteStatement", `{"Statement":"`+stmt+`"}`), 200)
		items, ok := m["Items"].([]any)
		if !ok || len(items) != 0 {
			t.Errorf("%s response = %v, want {\"Items\": []}", stmt, m)
		}
	}
}

func TestExecuteStatementInsertMissingKey(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS})

	rec := post(t, h, "ExecuteStatement", `{"Statement":"INSERT INTO test VALUE {'v':'1'}"}`)
	wantErr(t, rec, typValidation, "One or more parameter values were invalid: Missing the key pk in the item")
}

func TestExecuteStatementUpdate(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"pk":{"S":"a"},"v":{"S":"1"}}}`), 200)

	wantStatus(t, post(t, h, "ExecuteStatement", `{"Statement":"UPDATE test SET v = '2' WHERE pk = 'a'"}`), 200)
	m := wantStatus(t, post(t, h, "GetItem", `{"TableName":"test","Key":{"pk":{"S":"a"}}}`), 200)
	if got := m["Item"].(map[string]any)["v"].(map[string]any)["S"].(string); got != "2" {
		t.Errorf("v = %q, want 2", got)
	}

	// Extra conditions become the ConditionExpression.
	rec := post(t, h, "ExecuteStatement", `{"Statement":"UPDATE test SET v = '3' WHERE pk = 'a' AND v = '9'"}`)
	wantErr(t, rec, typConditional, "The conditional request failed")

	// A missing item is not upserted.
	rec = post(t, h, "ExecuteStatement", `{"Statement":"UPDATE test SET v = '4' WHERE pk = 'zz'"}`)
	wantErr(t, rec, typConditional, "The conditional request failed")
}

func TestExecuteStatementUpdateMissingKeyEquality(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS, Range: "rk", RangeType: attr.TypeS})

	rec := post(t, h, "ExecuteStatement", `{"Statement":"UPDATE test SET v = '1' WHERE pk = 'a'"}`)
	wantErr(t, rec, typValidation, "Where clause does not contain a mandatory equality on all key attributes")

	rec = post(t, h, "ExecuteStatement", `{"Statement":"DELETE FROM test WHERE rk = '1'"}`)
	wantErr(t, rec, typValidation, "Where clause does not contain a mandatory equality on all key attributes")
}

func TestExecuteStatementDelete(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"pk":{"S":"a"}}}`), 200)

	wantStatus(t, post(t, h, "ExecuteStatement", `{"Statement":"DELETE FROM test WHERE pk = 'a'"}`), 200)
	m := wantStatus(t, post(t, h, "GetItem", `{"TableName":"test","Key":{"pk":{"S":"a"}}}`), 200)
	if _, ok := m["Item"]; ok {
		t.Errorf("item still present after delete: %v", m["Item"])
	}

	// Deleting a missing item is a no-op, not an error.
	wantStatus(t, post(t, h, "ExecuteStatement", `{"Statement":"DELETE FROM test WHERE pk = 'zz'"}`), 200)
}

func TestExecuteStatementNextTokenPaging(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS, Range: "rk", RangeType: attr.TypeS})
	for i := 0; i < 250; i++ {
		item := fmt.Sprintf(`{"pk":{"S":"g"},"rk":{"S":"r%03d"}}`, i)
		wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":`+item+`}`), 200)
	}

	seen := map[string]bool{}
	token := ""
	pages := 0
	for {
		body := `{"Statement":"SELECT * FROM test WHERE pk = 'g'","Limit":9`
		if token != "" {
			body += `,"NextToken":"` + token + `"`
		}
		body += `}`
		m := wantStatus(t, post(t, h, "ExecuteStatement", body), 200)
		for _, it := range selItems(t, m) {
			rk := itemS(it, "rk")
			if seen[rk] {
				t.Fatalf("item %q seen twice", rk)
			}
			seen[rk] = true
		}
		pages++
		tok, _ := m["NextToken"].(string)
		if tok == "" {
			break
		}
		token = tok
	}
	if len(seen) != 250 {
		t.Errorf("visited %d items, want 250", len(seen))
	}
	if pages != 28 {
		t.Errorf("pages = %d, want 28", pages)
	}
}

func TestExecuteStatementNextTokenStatementMismatch(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS, Range: "rk", RangeType: attr.TypeS})
	for i := 0; i < 3; i++ {
		item := fmt.Sprintf(`{"pk":{"S":"g"},"rk":{"S":"r%03d"}}`, i)
		wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":`+item+`}`), 200)
	}
	m := wantStatus(t, post(t, h, "ExecuteStatement", `{"Statement":"SELECT * FROM test WHERE pk = 'g'","Limit":1}`), 200)
	tok, _ := m["NextToken"].(string)
	if tok == "" {
		t.Fatal("no NextToken on a partial page")
	}
	rec := post(t, h, "ExecuteStatement", `{"Statement":"SELECT * FROM test WHERE pk = 'h'","NextToken":"`+tok+`"}`)
	wantErr(t, rec, typValidation, "Invalid NextToken")
}

func TestBatchExecuteStatementMixedOutcomes(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"pk":{"S":"existing"},"v":{"S":"1"}}}`), 200)

	body := `{"Statements":[
		{"Statement":"INSERT INTO test VALUE {'pk':'new1','v':'1'}"},
		{"Statement":"INSERT INTO test VALUE {'pk':'existing','v':'2'}"},
		{"Statement":"UPDATE test SET v = '3' WHERE pk = 'missing'"},
		{"Statement":"DELETE FROM test WHERE pk = 'existing'"},
		{"Statement":"SELECT * FROM test WHERE pk = 'new1'"}
	]}`
	// The batch mixes writes and a read, so it is rejected.
	rec := post(t, h, "BatchExecuteStatement", body)
	wantErr(t, rec, typValidation, "All statements in a BatchExecuteStatement request must be either read statements or write statements")

	// All writes: one succeeds, one duplicates, one misses, one deletes.
	writes := `{"Statements":[
		{"Statement":"INSERT INTO test VALUE {'pk':'new1','v':'1'}"},
		{"Statement":"INSERT INTO test VALUE {'pk':'existing','v':'2'}"},
		{"Statement":"UPDATE test SET v = '3' WHERE pk = 'missing'"},
		{"Statement":"DELETE FROM test WHERE pk = 'existing'"}
	]}`
	m := wantStatus(t, post(t, h, "BatchExecuteStatement", writes), 200)
	resps := m["Responses"].([]any)
	if len(resps) != 4 {
		t.Fatalf("got %d responses, want 4", len(resps))
	}
	// Statement 0: success.
	if r := resps[0].(map[string]any); r["TableName"] != "test" {
		t.Errorf("response 0 = %v, want TableName t", r)
	}
	// Statement 1: duplicate.
	checkBatchErr(t, resps[1], "DuplicateItem", "Duplicate primary key exists in table")
	// Statement 2: conditional check failed on a missing item.
	checkBatchErr(t, resps[2], "ConditionalCheckFailed", "The conditional request failed")
	// Statement 3: success.
	if r := resps[3].(map[string]any); r["TableName"] != "test" {
		t.Errorf("response 3 = %v, want TableName t", r)
	}

	// All reads: present and missing items.
	reads := `{"Statements":[
		{"Statement":"SELECT * FROM test WHERE pk = 'new1'"},
		{"Statement":"SELECT * FROM test WHERE pk = 'gone'"}
	]}`
	m = wantStatus(t, post(t, h, "BatchExecuteStatement", reads), 200)
	resps = m["Responses"].([]any)
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2", len(resps))
	}
	r0 := resps[0].(map[string]any)
	if r0["TableName"] != "test" || itemS(r0["Item"].(map[string]any), "pk") != "new1" {
		t.Errorf("response 0 = %v, want item new1", r0)
	}
	r1 := resps[1].(map[string]any)
	if r1["TableName"] != "test" {
		t.Errorf("response 1 = %v, want TableName t", r1)
	}
	if _, ok := r1["Item"]; ok {
		t.Errorf("missing item returned Item: %v", r1)
	}
}

func checkBatchErr(t *testing.T, resp any, code, msg string) {
	t.Helper()
	r, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("response %#v is not a map", resp)
	}
	e, ok := r["Error"].(map[string]any)
	if !ok {
		t.Fatalf("response %v has no Error", r)
	}
	if e["Code"] != code {
		t.Errorf("Error.Code = %v, want %s", e["Code"], code)
	}
	if e["Message"] != msg {
		t.Errorf("Error.Message = %v, want %s", e["Message"], msg)
	}
}

func TestBatchExecuteStatementCountValidation(t *testing.T) {
	_, h, _ := newOps(t)
	rec := post(t, h, "BatchExecuteStatement", `{"Statements":[]}`)
	wantErr(t, rec, typValidation, "1 validation error detected: Value '[]' at 'statements' failed to satisfy constraint: Member must have length greater than or equal to 1")
}

func TestExecuteTransactionWrites(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS})

	body := `{"TransactStatements":[
		{"Statement":"INSERT INTO test VALUE {'pk':'a','v':'1'}"},
		{"Statement":"INSERT INTO test VALUE {'pk':'b','v':'9'}"}
	]}`
	m := wantStatus(t, post(t, h, "ExecuteTransaction", body), 200)
	responses, ok := m["Responses"].([]any)
	if !ok || len(responses) != 0 {
		t.Errorf("write transaction response = %v, want {\"Responses\": []}", m)
	}
	got := wantStatus(t, post(t, h, "GetItem", `{"TableName":"test","Key":{"pk":{"S":"a"}}}`), 200)
	if v := got["Item"].(map[string]any)["v"].(map[string]any)["S"].(string); v != "1" {
		t.Errorf("a.v = %q, want 1", v)
	}
	got = wantStatus(t, post(t, h, "GetItem", `{"TableName":"test","Key":{"pk":{"S":"b"}}}`), 200)
	if v := got["Item"].(map[string]any)["v"].(map[string]any)["S"].(string); v != "9" {
		t.Errorf("b.v = %q, want 9", v)
	}
}

func TestExecuteTransactionReads(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"pk":{"S":"a"},"v":{"S":"1"}}}`), 200)

	body := `{"TransactStatements":[
		{"Statement":"SELECT * FROM test WHERE pk = 'a'"},
		{"Statement":"SELECT * FROM test WHERE pk = 'zz'"}
	]}`
	m := wantStatus(t, post(t, h, "ExecuteTransaction", body), 200)
	resps := m["Responses"].([]any)
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2", len(resps))
	}
	if itemS(resps[0].(map[string]any)["Item"].(map[string]any), "pk") != "a" {
		t.Errorf("response 0 = %v, want item a", resps[0])
	}
	if _, ok := resps[1].(map[string]any)["Item"]; ok {
		t.Errorf("missing item returned: %v", resps[1])
	}
}

func TestExecuteTransactionExistsCondition(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"pk":{"S":"a"},"v":{"S":"1"}}}`), 200)

	// EXISTS on a missing key cancels the whole transaction.
	body := `{"TransactStatements":[
		{"Statement":"EXISTS(SELECT * FROM test WHERE pk = 'zz')"},
		{"Statement":"INSERT INTO test VALUE {'pk':'b','v':'2'}"}
	]}`
	rec := post(t, h, "ExecuteTransaction", body)
	wantErr(t, rec, "com.amazonaws.dynamodb.v20120810#TransactionCanceledException", "Transaction cancelled, please refer cancellation reasons for specific reasons [ConditionalCheckFailed, None]")

	// Nothing was written.
	got := wantStatus(t, post(t, h, "GetItem", `{"TableName":"test","Key":{"pk":{"S":"b"}}}`), 200)
	if _, ok := got["Item"]; ok {
		t.Errorf("item b written despite cancelled transaction")
	}
}

func TestExecuteTransactionDuplicate(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "pk", HashType: attr.TypeS})
	wantStatus(t, post(t, h, "PutItem", `{"TableName":"test","Item":{"pk":{"S":"a"}}}`), 200)

	rec := post(t, h, "ExecuteTransaction", `{"TransactStatements":[{"Statement":"INSERT INTO test VALUE {'pk':'a'}"}]}`)
	wantErr(t, rec, "com.amazonaws.dynamodb.v20120810#TransactionCanceledException", "Transaction cancelled, please refer cancellation reasons for specific reasons [DuplicateItem]")
}

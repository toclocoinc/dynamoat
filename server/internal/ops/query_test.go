package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
)

// appTable creates a tenant-shaped table: hash key id, a GSI
// entity/comboKey with ALL projection and one with INCLUDE [name].
func appTable(t *testing.T, store *memstore.Store) {
	t.Helper()
	def := &storage.Table{
		Name: "app",
		Key:  storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs: map[string]attr.Type{
			"id": attr.TypeS, "entity": attr.TypeS, "comboKey": attr.TypeS, "name": attr.TypeS,
		},
		GSIs: []storage.Index{
			{Name: "entity-comboKey-index", Key: storage.KeySchema{Hash: "entity", HashType: attr.TypeS, Range: "comboKey", RangeType: attr.TypeS}, Projection: storage.ProjectAll},
			{Name: "entity-comboKey-name-index", Key: storage.KeySchema{Hash: "entity", HashType: attr.TypeS, Range: "comboKey", RangeType: attr.TypeS}, Projection: storage.ProjectInclude, NonKeyAttrs: []string{"name"}},
		},
	}
	if _, err := store.CreateTable(context.Background(), def); err != nil {
		t.Fatalf("CreateTable(df): %v", err)
	}
}

// seedApp writes 300 items: one entity, three boards of 100, with
// comboKey board<k>:<n> matching item<k*100+n>.
func seedApp(t *testing.T, h http.Handler) {
	t.Helper()
	for i := 0; i < 300; i++ {
		id := fmt.Sprintf("item%03d", i)
		combo := fmt.Sprintf("board%d:%03d", i/100, i%100)
		item := fmt.Sprintf(`{"id":{"S":%q},"entity":{"S":"e"},"comboKey":{"S":%q},"name":{"S":%q},"extra":{"S":"extra"}}`, id, combo, "n"+id)
		wantStatus(t, post(t, h, "PutItem", `{"TableName":"app","Item":`+item+`}`), 200)
	}
}

// combos extracts the comboKey of each returned item, in order.
func combos(t *testing.T, m map[string]any) []string {
	t.Helper()
	var out []string
	items, _ := m["Items"].([]any)
	for _, it := range items {
		im, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("item %#v is not a map", it)
		}
		ck, ok := im["comboKey"].(map[string]any)
		if !ok {
			t.Fatalf("comboKey %#v is not a map", im["comboKey"])
		}
		out = append(out, ck["S"].(string))
	}
	return out
}

func boardCombos(board string, n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = fmt.Sprintf("%s:%03d", board, i)
	}
	return out
}

func reversed(in []string) []string {
	out := make([]string, len(in))
	for i := range in {
		out[len(in)-1-i] = in[i]
	}
	return out
}

func countOf(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%s = %#v, want number", key, m[key])
	}
	return int(v)
}

func TestQueryBoardBeginsWith(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)
	seedApp(t, h)

	body := `{"TableName":"app","IndexName":"entity-comboKey-index","KeyConditionExpression":"entity = :e AND begins_with(comboKey, :b)","ExpressionAttributeValues":{":e":{"S":"e"},":b":{"S":"board1:"}}}`
	m := wantStatus(t, post(t, h, "Query", body), 200)

	want := boardCombos("board1", 100)
	got := combos(t, m)
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %q, want %q", i, got[i], want[i])
		}
	}
	if countOf(t, m, "Count") != 100 || countOf(t, m, "ScannedCount") != 100 {
		t.Errorf("Count/ScannedCount = %v/%v, want 100/100", m["Count"], m["ScannedCount"])
	}
	if _, ok := m["LastEvaluatedKey"]; ok {
		t.Errorf("LastEvaluatedKey present, want absent")
	}
}

func TestQueryReverseOrder(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)
	seedApp(t, h)

	body := `{"TableName":"app","IndexName":"entity-comboKey-index","KeyConditionExpression":"entity = :e AND begins_with(comboKey, :b)","ExpressionAttributeValues":{":e":{"S":"e"},":b":{"S":"board1:"}},"ScanIndexForward":false}`
	m := wantStatus(t, post(t, h, "Query", body), 200)

	got := combos(t, m)
	want := reversed(boardCombos("board1", 100))
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestQueryLimitPaging(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)
	seedApp(t, h)

	seen := map[string]bool{}
	start := ""
	pages := 0
	for {
		body := `{"TableName":"app","IndexName":"entity-comboKey-index","KeyConditionExpression":"entity = :e","ExpressionAttributeValues":{":e":{"S":"e"}},"Limit":7`
		if start != "" {
			body += `,"ExclusiveStartKey":` + start
		}
		body += `}`
		m := wantStatus(t, post(t, h, "Query", body), 200)
		for _, c := range combos(t, m) {
			if seen[c] {
				t.Fatalf("item %q seen twice", c)
			}
			seen[c] = true
		}
		pages++
		lek, ok := m["LastEvaluatedKey"]
		if !ok {
			break
		}
		b, err := json.Marshal(lek)
		if err != nil {
			t.Fatalf("marshal LastEvaluatedKey: %v", err)
		}
		start = string(b)
	}
	if len(seen) != 300 {
		t.Errorf("visited %d items, want 300", len(seen))
	}
	if pages != 43 {
		t.Errorf("pages = %d, want 43", pages)
	}
}

func TestQueryFilterScannedCount(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)
	seedApp(t, h)

	// item000 sorts first (board0:000); the filter drops exactly it.
	body := `{"TableName":"app","IndexName":"entity-comboKey-index","KeyConditionExpression":"entity = :e","FilterExpression":"id <> :x","ExpressionAttributeValues":{":e":{"S":"e"},":x":{"S":"item000"}},"Limit":7}`
	m := wantStatus(t, post(t, h, "Query", body), 200)

	if got := countOf(t, m, "ScannedCount"); got != 7 {
		t.Errorf("ScannedCount = %d, want 7", got)
	}
	if got := countOf(t, m, "Count"); got != 6 {
		t.Errorf("Count = %d, want 6", got)
	}
	if got := len(combos(t, m)); got != 6 {
		t.Errorf("got %d items, want 6", got)
	}
}

func TestQuerySelectCount(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)
	seedApp(t, h)

	body := `{"TableName":"app","IndexName":"entity-comboKey-index","KeyConditionExpression":"entity = :e","ExpressionAttributeValues":{":e":{"S":"e"}},"Select":"COUNT","Limit":5}`
	m := wantStatus(t, post(t, h, "Query", body), 200)

	if _, ok := m["Items"]; ok {
		t.Errorf("Items present with Select COUNT: %v", m["Items"])
	}
	if got := countOf(t, m, "Count"); got != 5 {
		t.Errorf("Count = %d, want 5", got)
	}
	if got := countOf(t, m, "ScannedCount"); got != 5 {
		t.Errorf("ScannedCount = %d, want 5", got)
	}
	if _, ok := m["LastEvaluatedKey"]; !ok {
		t.Errorf("LastEvaluatedKey absent, want present")
	}
}

func TestQueryIncludeIndexReturnsKeysAndName(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)
	seedApp(t, h)

	body := `{"TableName":"app","IndexName":"entity-comboKey-name-index","KeyConditionExpression":"entity = :e AND begins_with(comboKey, :b)","ExpressionAttributeValues":{":e":{"S":"e"},":b":{"S":"board1:"}},"Select":"ALL_PROJECTED_ATTRIBUTES"}`
	m := wantStatus(t, post(t, h, "Query", body), 200)

	items, _ := m["Items"].([]any)
	if len(items) != 100 {
		t.Fatalf("got %d items, want 100", len(items))
	}
	for _, it := range items {
		im := it.(map[string]any)
		if len(im) != 4 {
			t.Errorf("item has %d attributes, want 4 (entity, comboKey, id, name): %v", len(im), im)
		}
		for _, k := range []string{"entity", "comboKey", "id", "name"} {
			if _, ok := im[k]; !ok {
				t.Errorf("item missing %q: %v", k, im)
			}
		}
		if _, ok := im["extra"]; ok {
			t.Errorf("item has non-projected attribute extra: %v", im)
		}
	}
}

func TestQueryOneMegabytePage(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "big", storage.KeySchema{Hash: "hk", HashType: attr.TypeS, Range: "rk", RangeType: attr.TypeS})

	payload := strings.Repeat("a", 100*1024)
	for i := 0; i < 12; i++ {
		item := fmt.Sprintf(`{"hk":{"S":"g"},"rk":{"S":%q},"payload":{"S":%q}}`, fmt.Sprintf("k%03d", i), payload)
		wantStatus(t, post(t, h, "PutItem", `{"TableName":"big","Item":`+item+`}`), 200)
	}

	seen := map[string]bool{}
	start := ""
	total := 0
	pages := 0
	for {
		body := `{"TableName":"big","KeyConditionExpression":"hk = :h","ExpressionAttributeValues":{":h":{"S":"g"}}`
		if start != "" {
			body += `,"ExclusiveStartKey":` + start
		}
		body += `}`
		m := wantStatus(t, post(t, h, "Query", body), 200)
		items, _ := m["Items"].([]any)
		for _, it := range items {
			rk := it.(map[string]any)["rk"].(map[string]any)["S"].(string)
			if seen[rk] {
				t.Fatalf("item %q seen twice", rk)
			}
			seen[rk] = true
		}
		total += len(items)
		pages++
		lek, ok := m["LastEvaluatedKey"]
		if !ok {
			break
		}
		b, err := json.Marshal(lek)
		if err != nil {
			t.Fatalf("marshal LastEvaluatedKey: %v", err)
		}
		start = string(b)
	}
	if total != 12 {
		t.Errorf("visited %d items, want 12", total)
	}
	// About 100 KB each, so the 1 MB page returns 10 before truncating.
	if pages != 2 {
		t.Errorf("pages = %d, want 2", pages)
	}
}

func TestQueryBaseTable(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "base", storage.KeySchema{Hash: "pk", HashType: attr.TypeS, Range: "sk", RangeType: attr.TypeS})
	for _, sk := range []string{"b", "a", "c"} {
		wantStatus(t, post(t, h, "PutItem", fmt.Sprintf(`{"TableName":"base","Item":{"pk":{"S":"x"},"sk":{"S":%q}}}`, sk)), 200)
	}

	m := wantStatus(t, post(t, h, "Query", `{"TableName":"base","KeyConditionExpression":"pk = :p","ExpressionAttributeValues":{":p":{"S":"x"}}}`), 200)
	items, _ := m["Items"].([]any)
	var got []string
	for _, it := range items {
		got = append(got, it.(map[string]any)["sk"].(map[string]any)["S"].(string))
	}
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestQueryErrors(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)

	cases := []struct{ name, body, msg string }{
		{
			"missing key condition",
			`{"TableName":"app"}`,
			"Either the KeyConditions or KeyConditionExpression parameter must be specified in the request.",
		},
		{
			"consistent read on gsi",
			`{"TableName":"app","IndexName":"entity-comboKey-index","KeyConditionExpression":"entity = :e","ExpressionAttributeValues":{":e":{"S":"e"}},"ConsistentRead":true}`,
			"Consistent reads are not supported on global secondary indexes",
		},
		{
			"missing index",
			`{"TableName":"app","IndexName":"nope","KeyConditionExpression":"entity = :e","ExpressionAttributeValues":{":e":{"S":"e"}}}`,
			"The table does not have the specified index: nope",
		},
		{
			"key type mismatch",
			`{"TableName":"app","IndexName":"entity-comboKey-index","KeyConditionExpression":"entity = :e","ExpressionAttributeValues":{":e":{"N":"1"}}}`,
			"One or more parameter values were invalid: Condition parameter type does not match schema type",
		},
		{
			"invalid start key",
			`{"TableName":"app","IndexName":"entity-comboKey-index","KeyConditionExpression":"entity = :e","ExpressionAttributeValues":{":e":{"S":"e"}},"ExclusiveStartKey":{"id":{"S":"x"}}}`,
			"The provided starting key is invalid: The provided key element does not match the schema",
		},
		{
			"select all on non-all index",
			`{"TableName":"app","IndexName":"entity-comboKey-name-index","Select":"ALL_ATTRIBUTES","KeyConditionExpression":"entity = :e","ExpressionAttributeValues":{":e":{"S":"e"}}}`,
			"One or more parameter values were invalid: Select type ALL_ATTRIBUTES is not supported for global secondary index entity-comboKey-name-index because its projection type is not ALL",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(t, h, "Query", c.body)
			wantErr(t, rec, typValidation, c.msg)
		})
	}
}

func TestQueryValidationTypeOnly(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)

	// The exact wording of these two is not pinned; assert the type.
	cases := []struct{ name, body string }{
		{"all projected on base", `{"TableName":"app","KeyConditionExpression":"id = :i","ExpressionAttributeValues":{":i":{"S":"x"}},"Select":"ALL_PROJECTED_ATTRIBUTES"}`},
		{"specific without projection", `{"TableName":"app","KeyConditionExpression":"id = :i","ExpressionAttributeValues":{":i":{"S":"x"}},"Select":"SPECIFIC_ATTRIBUTES"}`},
		{"unprojected attribute", `{"TableName":"app","IndexName":"entity-comboKey-name-index","KeyConditionExpression":"entity = :e","ExpressionAttributeValues":{":e":{"S":"e"}},"Select":"ALL_PROJECTED_ATTRIBUTES","ProjectionExpression":"extra"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(t, h, "Query", c.body)
			if rec.Code != 400 {
				t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
			}
			m := bodyJSON(t, rec)
			if m["__type"] != typValidation {
				t.Errorf("__type = %v, want %s", m["__type"], typValidation)
			}
			if msg, _ := m["message"].(string); !strings.HasPrefix(msg, "One or more parameter values were invalid:") {
				t.Errorf("message = %q, want invalid-values prefix", msg)
			}
		})
	}
}

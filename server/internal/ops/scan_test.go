package ops

import (
	"encoding/json"
	"fmt"
	"testing"
)

// scanIDs extracts the id of every returned item, in order.
func scanIDs(t *testing.T, m map[string]any) []string {
	t.Helper()
	var out []string
	items, _ := m["Items"].([]any)
	for _, it := range items {
		im, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("item %#v is not a map", it)
		}
		id, ok := im["id"].(map[string]any)
		if !ok {
			t.Fatalf("id %#v is not a map", im["id"])
		}
		out = append(out, id["S"].(string))
	}
	return out
}

func TestScanLimitPaging(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)
	seedApp(t, h)

	seen := map[string]bool{}
	start := ""
	for {
		body := `{"TableName":"app","Limit":10`
		if start != "" {
			body += `,"ExclusiveStartKey":` + start
		}
		body += `}`
		m := wantStatus(t, post(t, h, "Scan", body), 200)
		for _, id := range scanIDs(t, m) {
			if seen[id] {
				t.Fatalf("item %q seen twice", id)
			}
			seen[id] = true
		}
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
}

func TestScanParallelSegments(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)
	seedApp(t, h)

	seen := map[string]bool{}
	for seg := 0; seg < 4; seg++ {
		start := ""
		for {
			body := fmt.Sprintf(`{"TableName":"app","Segment":%d,"TotalSegments":4,"Limit":7`, seg)
			if start != "" {
				body += `,"ExclusiveStartKey":` + start
			}
			body += `}`
			m := wantStatus(t, post(t, h, "Scan", body), 200)
			for _, id := range scanIDs(t, m) {
				if seen[id] {
					t.Fatalf("item %q seen twice", id)
				}
				seen[id] = true
			}
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
	}
	if len(seen) != 300 {
		t.Errorf("visited %d items, want 300", len(seen))
	}
}

func TestScanFilterScannedCount(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)
	seedApp(t, h)

	// begins_with(id, "item1") matches exactly item100..item199.
	body := `{"TableName":"app","FilterExpression":"begins_with(id, :p)","ExpressionAttributeValues":{":p":{"S":"item1"}}}`
	m := wantStatus(t, post(t, h, "Scan", body), 200)

	if got := countOf(t, m, "ScannedCount"); got != 300 {
		t.Errorf("ScannedCount = %d, want 300", got)
	}
	if got := countOf(t, m, "Count"); got != 100 {
		t.Errorf("Count = %d, want 100", got)
	}
	if countOf(t, m, "ScannedCount") <= countOf(t, m, "Count") {
		t.Errorf("ScannedCount must exceed Count: %v/%v", m["ScannedCount"], m["Count"])
	}
	if got := len(scanIDs(t, m)); got != 100 {
		t.Errorf("got %d items, want 100", got)
	}
}

func TestScanSelectCount(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)
	seedApp(t, h)

	body := `{"TableName":"app","Select":"COUNT","Limit":5}`
	m := wantStatus(t, post(t, h, "Scan", body), 200)

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

func TestScanIncludeIndexReturnsKeysAndName(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)
	seedApp(t, h)

	body := `{"TableName":"app","IndexName":"entity-comboKey-name-index","Select":"ALL_PROJECTED_ATTRIBUTES"}`
	m := wantStatus(t, post(t, h, "Scan", body), 200)

	items, _ := m["Items"].([]any)
	if len(items) != 300 {
		t.Fatalf("got %d items, want 300", len(items))
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

func TestScanSegmentErrors(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)

	cases := []struct{ name, body, msg string }{
		{
			"segment missing",
			`{"TableName":"app","TotalSegments":2}`,
			"The Segment parameter is required but was not present in the request when parameter TotalSegments is present",
		},
		{
			"total segments missing",
			`{"TableName":"app","Segment":0}`,
			"The TotalSegments parameter is required but was not present in the request when Segment parameter is present",
		},
		{
			"segment out of range",
			`{"TableName":"app","Segment":4,"TotalSegments":4}`,
			"The Segment parameter is zero-based and must be less than parameter TotalSegments: Segment: 4 is not less than TotalSegments: 4",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(t, h, "Scan", c.body)
			wantErr(t, rec, typValidation, c.msg)
		})
	}
}

func TestScanTotalSegmentsRange(t *testing.T) {
	_, h, store := newOps(t)
	appTable(t, store)

	cases := []struct{ name, body string }{
		{"zero", `{"TableName":"app","Segment":0,"TotalSegments":0}`},
		{"too large", `{"TableName":"app","Segment":0,"TotalSegments":1000001}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(t, h, "Scan", c.body)
			if rec.Code != 400 {
				t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
			}
			m := bodyJSON(t, rec)
			if m["__type"] != typValidation {
				t.Errorf("__type = %v, want %s", m["__type"], typValidation)
			}
		})
	}
}

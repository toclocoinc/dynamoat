package ops

import (
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/expr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// Select values accepted by Query and Scan.
const (
	selectAll          = "ALL_ATTRIBUTES"
	selectAllProjected = "ALL_PROJECTED_ATTRIBUTES"
	selectSpecific     = "SPECIFIC_ATTRIBUTES"
	selectCount        = "COUNT"
)

// pageBytes is the 1 MB result-set limit DynamoDB applies to Query and Scan.
const pageBytes = 1 << 20

// checkSelect validates Select and returns the canonical value ("" means the
// default ALL_ATTRIBUTES). hasProjection says whether the request carries a
// ProjectionExpression.
func checkSelect(v string, hasProjection, onIndex bool) (string, error) {
	switch v {
	case "":
		// DynamoDB's default: SPECIFIC_ATTRIBUTES with a ProjectionExpression,
		// otherwise ALL_PROJECTED_ATTRIBUTES on an index, ALL_ATTRIBUTES on a table.
		switch {
		case hasProjection:
			return selectSpecific, nil
		case onIndex:
			return selectAllProjected, nil
		}
		return selectAll, nil
	case selectAll:
		return selectAll, nil
	case selectAllProjected:
		return selectAllProjected, nil
	case selectSpecific:
		if !hasProjection {
			return "", ddberr.Validation("One or more parameter values were invalid: The Select type SPECIFIC_ATTRIBUTES must be used with a ProjectionExpression")
		}
		return selectSpecific, nil
	case selectCount:
		return selectCount, nil
	default:
		return "", ddberr.Validation("1 validation error detected: Value '%s' at 'select' failed to satisfy constraint: Member must satisfy enum value set: [ALL_ATTRIBUTES, ALL_PROJECTED_ATTRIBUTES, SPECIFIC_ATTRIBUTES, COUNT]", v)
	}
}

// checkExclusiveStartKey checks ExclusiveStartKey holds exactly the table key
// attributes, plus the index key attributes for a GSI, each of the right type.
func checkExclusiveStartKey(t *storage.Table, idx *storage.Index, esk attr.Item) error {
	if esk == nil {
		return nil
	}
	want := map[string]attr.Type{t.Key.Hash: t.Key.HashType}
	if t.Key.Range != "" {
		want[t.Key.Range] = t.Key.RangeType
	}
	if idx != nil {
		want[idx.Key.Hash] = idx.Key.HashType
		if idx.Key.Range != "" {
			want[idx.Key.Range] = idx.Key.RangeType
		}
	}
	if len(esk) != len(want) {
		return startKeyInvalid()
	}
	for name, typ := range want {
		v, ok := esk[name]
		if !ok || v.Type != typ {
			return startKeyInvalid()
		}
	}
	return nil
}

func startKeyInvalid() error {
	return ddberr.Validation("The provided starting key is invalid: The provided key element does not match the schema")
}

// checkProjected rejects a ProjectionExpression that names an attribute a
// non-ALL GSI does not project.
func checkProjected(t *storage.Table, idx *storage.Index, paths []expr.Path) error {
	if idx == nil || idx.Projection == storage.ProjectAll || paths == nil {
		return nil
	}
	projected := map[string]bool{
		t.Key.Hash:   true,
		idx.Key.Hash: true,
	}
	if t.Key.Range != "" {
		projected[t.Key.Range] = true
	}
	if idx.Key.Range != "" {
		projected[idx.Key.Range] = true
	}
	if idx.Projection == storage.ProjectInclude {
		for _, name := range idx.NonKeyAttrs {
			projected[name] = true
		}
	}
	for _, p := range paths {
		if len(p) > 0 && !projected[p[0].Name] {
			return ddberr.Validation("One or more parameter values were invalid: Projected attributes can not contain the attribute named \"%s\"", p[0].Name)
		}
	}
	return nil
}

// indexProjection cuts item to the attributes a non-ALL GSI projects: the
// index and table key attributes, plus NonKeyAttrs for INCLUDE.
func indexProjection(t *storage.Table, idx *storage.Index, item attr.Item) attr.Item {
	out := attr.Item{}
	add := func(name string) {
		if v, ok := item[name]; ok {
			out[name] = v
		}
	}
	add(idx.Key.Hash)
	if idx.Key.Range != "" {
		add(idx.Key.Range)
	}
	add(t.Key.Hash)
	if t.Key.Range != "" {
		add(t.Key.Range)
	}
	if idx.Projection == storage.ProjectInclude {
		for _, name := range idx.NonKeyAttrs {
			add(name)
		}
	}
	return out
}

// readResult filters, projects and shapes a Query or Scan page into the
// response fields the two operations share: Count, ScannedCount, Items (unless
// Select is COUNT) and LastEvaluatedKey.
func readResult(page storage.Page, t *storage.Table, idx *storage.Index, ex *expressions, sel string) (map[string]any, error) {
	out := map[string]any{}
	scanned := len(page.Items)
	count := 0
	items := []attr.Item{}
	for _, it := range page.Items {
		// A non-ALL GSI holds only its projected attributes, so the filter
		// sees no others. An LSI read with Select ALL_ATTRIBUTES fetches the
		// whole base item instead, so nothing is cut.
		p := it
		if idx != nil && idx.Projection != storage.ProjectAll && !(idx.Local && sel == selectAll) {
			p = indexProjection(t, idx, p)
		}
		if ex.filter != nil {
			ok, err := expr.Eval(ex.filter, p, ex.values, "FilterExpression")
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		count++
		if sel == selectCount {
			continue
		}
		if ex.proj != nil {
			p = expr.Project(p, ex.proj)
		}
		items = append(items, p)
	}
	out["Count"] = count
	out["ScannedCount"] = scanned
	if sel != selectCount {
		out["Items"] = items
	}
	if page.LastKey != nil {
		out["LastEvaluatedKey"] = page.LastKey
	}
	return out, nil
}

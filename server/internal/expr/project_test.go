package expr

import (
	"reflect"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
)

func project(t *testing.T, expr string) attr.Item {
	t.Helper()
	paths, err := ParseProjection(expr, &Env{})
	if err != nil {
		t.Fatalf("ParseProjection(%q): %v", expr, err)
	}
	return Project(testItem(), paths)
}

func TestProject(t *testing.T) {
	meta := testItem()["meta"]
	cases := []struct {
		name string
		expr string
		want attr.Item
	}{
		{
			"top-level",
			`id, phase`,
			attr.Item{"id": sval("123"), "phase": sval("ACTIVE")},
		},
		{
			"nested-map",
			`meta.k`,
			attr.Item{"meta": {Type: attr.TypeM, M: map[string]attr.Value{"k": sval("v")}}},
		},
		{
			"list-indexes-mixed-order",
			`children[2], children[0]`,
			attr.Item{"children": {Type: attr.TypeL, L: []attr.Value{
				sval("x"),
				{Type: attr.TypeM, M: map[string]attr.Value{"id": sval("c0")}},
			}}},
		},
		{
			"absent-paths-skipped",
			`id, absent, meta.k, children[9]`,
			attr.Item{
				"id":   sval("123"),
				"meta": {Type: attr.TypeM, M: map[string]attr.Value{"k": sval("v")}},
			},
		},
		{
			"overlapping-map",
			`meta, meta.k`,
			attr.Item{"meta": meta},
		},
		{
			"overlapping-list",
			`children, children[0]`,
			attr.Item{"children": testItem()["children"]},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := project(t, c.expr); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Project(%q) = %+v, want %+v", c.expr, got, c.want)
			}
		})
	}
}

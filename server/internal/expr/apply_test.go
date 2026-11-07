package expr

import (
	"reflect"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
)

func sv(s string) attr.Value { return attr.Value{Type: attr.TypeS, S: s} }
func nv(s string) attr.Value { return attr.Value{Type: attr.TypeN, N: s} }
func lv(vs ...attr.Value) attr.Value {
	return attr.Value{Type: attr.TypeL, L: vs}
}
func ssv(ss ...string) attr.Value { return attr.Value{Type: attr.TypeSS, SS: ss} }
func nsv(ss ...string) attr.Value { return attr.Value{Type: attr.TypeNS, NS: ss} }

// doApply parses expr and applies it, failing the test only on parse errors
// (Apply's own errors are returned for the caller to inspect).
func doApply(t *testing.T, expr string, names map[string]string, item attr.Item, values map[string]attr.Value) (attr.Item, []Path, error) {
	t.Helper()
	e := &Env{Names: names, Values: map[string]bool{}}
	for k := range values {
		e.Values[k] = true
	}
	u, err := ParseUpdate(expr, e)
	if err != nil {
		t.Fatalf("ParseUpdate(%q): %v", expr, err)
	}
	return Apply(u, item, values)
}

func pathStrs(paths []Path) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = p.String()
	}
	return out
}

func TestApplySet(t *testing.T) {
	tests := []struct {
		name   string
		expr   string
		item   attr.Item
		values map[string]attr.Value
		want   attr.Item
	}{
		{
			"create top level",
			"SET a = :v",
			attr.Item{},
			map[string]attr.Value{":v": sv("x")},
			attr.Item{"a": sv("x")},
		},
		{
			"replace top level",
			"SET a = :v",
			attr.Item{"a": sv("old")},
			map[string]attr.Value{":v": sv("new")},
			attr.Item{"a": sv("new")},
		},
		{
			"create nested map key",
			"SET a.b = :v",
			attr.Item{"a": attr.Value{Type: attr.TypeM, M: map[string]attr.Value{"c": sv("x")}}},
			map[string]attr.Value{":v": sv("y")},
			attr.Item{"a": attr.Value{Type: attr.TypeM, M: map[string]attr.Value{"c": sv("x"), "b": sv("y")}}},
		},
		{
			"replace list element",
			"SET l[0] = :v",
			attr.Item{"l": lv(sv("a"), sv("b"))},
			map[string]attr.Value{":v": sv("z")},
			attr.Item{"l": lv(sv("z"), sv("b"))},
		},
		{
			"append past end of list",
			"SET l[2] = :v",
			attr.Item{"l": lv(sv("a"), sv("b"))},
			map[string]attr.Value{":v": sv("z")},
			attr.Item{"l": lv(sv("a"), sv("b"), sv("z"))},
		},
		{
			"set from path",
			"SET a = b",
			attr.Item{"b": sv("x")},
			nil,
			attr.Item{"b": sv("x"), "a": sv("x")},
		},
		{
			"nil item",
			"SET a = :v",
			nil,
			map[string]attr.Value{":v": sv("x")},
			attr.Item{"a": sv("x")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := doApply(t, tc.expr, nil, tc.item, tc.values)
			if err != nil {
				t.Fatalf("Apply error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestApplySetErrors(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		item    attr.Item
		values  map[string]attr.Value
		wantMsg string
	}{
		{
			"missing intermediate map",
			"SET a.b = :v",
			attr.Item{},
			map[string]attr.Value{":v": sv("x")},
			"The document path provided in the update expression is invalid for update",
		},
		{
			"set through a list",
			"SET a.b = :v",
			attr.Item{"a": lv(sv("x"))},
			map[string]attr.Value{":v": sv("y")},
			"The document path provided in the update expression is invalid for update",
		},
		{
			"index into a map",
			"SET a[0] = :v",
			attr.Item{"a": attr.Value{Type: attr.TypeM, M: map[string]attr.Value{}}},
			map[string]attr.Value{":v": sv("y")},
			"The document path provided in the update expression is invalid for update",
		},
		{
			"intermediate list index out of range",
			"SET l[5].b = :v",
			attr.Item{"l": lv()},
			map[string]attr.Value{":v": sv("y")},
			"The document path provided in the update expression is invalid for update",
		},
		{
			"rhs path missing",
			"SET x = absent + :one",
			attr.Item{},
			map[string]attr.Value{":one": nv("1")},
			"The provided expression refers to an attribute that does not exist in the item",
		},
		{
			"rhs path through wrong type",
			"SET x = a.b + :one",
			attr.Item{"a": sv("s")},
			map[string]attr.Value{":one": nv("1")},
			"The document path provided in the update expression is invalid for update",
		},
		{
			"add string to number",
			"SET x = s + :one",
			attr.Item{"s": sv("x")},
			map[string]attr.Value{":one": nv("1")},
			"",
		},
		{
			"subtract non-number",
			"SET x = n - :s",
			attr.Item{"n": nv("5")},
			map[string]attr.Value{":s": sv("x")},
			"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := doApply(t, tc.expr, nil, tc.item, tc.values)
			wantErr(t, err, "ValidationException", tc.wantMsg)
		})
	}
}

func TestApplyArith(t *testing.T) {
	const ones = "11111111111111111111111111111111111111"  // 38 ones
	const nines = "99999999999999999999999999999999999999" // 38 nines
	const twos = "22222222222222222222222222222222222222"  // 38 twos

	tests := []struct {
		name    string
		expr    string
		item    attr.Item
		values  map[string]attr.Value
		want    attr.Item
		wantMsg string
	}{
		{
			"add exact",
			"SET n = n + :v",
			attr.Item{"n": nv("5")},
			map[string]attr.Value{":v": nv("3")},
			attr.Item{"n": nv("8")},
			"",
		},
		{
			"subtract",
			"SET n = n - :v",
			attr.Item{"n": nv("5")},
			map[string]attr.Value{":v": nv("8")},
			attr.Item{"n": nv("-3")},
			"",
		},
		{
			"add 38 digit exact",
			"SET n = n + :v",
			attr.Item{"n": nv(ones)},
			map[string]attr.Value{":v": nv(ones)},
			attr.Item{"n": nv(twos)},
			"",
		},
		{
			"add 38 digit overflow",
			"SET n = n + :v",
			attr.Item{"n": nv(nines)},
			map[string]attr.Value{":v": nv(nines)},
			nil,
			"Attempting to store more than 38 significant digits in a Number",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := doApply(t, tc.expr, nil, tc.item, tc.values)
			if tc.wantMsg != "" {
				wantErr(t, err, "ValidationException", tc.wantMsg)
				return
			}
			if err != nil {
				t.Fatalf("Apply error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestApplyFunctions(t *testing.T) {
	tests := []struct {
		name   string
		expr   string
		item   attr.Item
		values map[string]attr.Value
		want   attr.Item
	}{
		{
			"if_not_exists present",
			"SET n = if_not_exists(n, :zero)",
			attr.Item{"n": nv("5")},
			map[string]attr.Value{":zero": nv("0")},
			attr.Item{"n": nv("5")},
		},
		{
			"if_not_exists absent",
			"SET n = if_not_exists(n, :zero)",
			attr.Item{},
			map[string]attr.Value{":zero": nv("0")},
			attr.Item{"n": nv("0")},
		},
		{
			"if_not_exists arithmetic",
			"SET n = if_not_exists(n, :zero) + :inc",
			attr.Item{},
			map[string]attr.Value{":zero": nv("0"), ":inc": nv("7")},
			attr.Item{"n": nv("7")},
		},
		{
			"list_append",
			"SET l = list_append(l, :more)",
			attr.Item{"l": lv(sv("a"))},
			map[string]attr.Value{":more": lv(sv("b"), sv("c"))},
			attr.Item{"l": lv(sv("a"), sv("b"), sv("c"))},
		},
		{
			"list_append missing list uses default",
			"SET l = list_append(if_not_exists(l, :empty), :new)",
			attr.Item{},
			map[string]attr.Value{":empty": lv(), ":new": lv(sv("x"))},
			attr.Item{"l": lv(sv("x"))},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := doApply(t, tc.expr, nil, tc.item, tc.values)
			if err != nil {
				t.Fatalf("Apply error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestApplyFunctionsErrors(t *testing.T) {
	tests := []struct {
		name   string
		expr   string
		item   attr.Item
		values map[string]attr.Value
	}{
		{
			"list_append left not list",
			"SET l = list_append(s, :more)",
			attr.Item{"s": sv("x")},
			map[string]attr.Value{":more": lv()},
		},
		{
			"list_append right not list",
			"SET l = list_append(l, :s)",
			attr.Item{"l": lv()},
			map[string]attr.Value{":s": sv("x")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := doApply(t, tc.expr, nil, tc.item, tc.values)
			wantErr(t, err, "ValidationException", "")
		})
	}
}

func TestApplyRemove(t *testing.T) {
	t.Run("delete attribute", func(t *testing.T) {
		got, _, err := doApply(t, "REMOVE a", nil, attr.Item{"a": sv("x"), "b": sv("y")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := attr.Item{"b": sv("y")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v want %#v", got, want)
		}
	})

	t.Run("delete map key", func(t *testing.T) {
		got, _, err := doApply(t, "REMOVE a.b", nil, attr.Item{"a": attr.Value{Type: attr.TypeM, M: map[string]attr.Value{"b": sv("x"), "c": sv("y")}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := attr.Item{"a": attr.Value{Type: attr.TypeM, M: map[string]attr.Value{"c": sv("y")}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v want %#v", got, want)
		}
	})

	t.Run("remove list element shifts", func(t *testing.T) {
		got, _, err := doApply(t, "REMOVE l[0]", nil, attr.Item{"l": lv(sv("a"), sv("b"), sv("c"))}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := attr.Item{"l": lv(sv("b"), sv("c"))}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v want %#v", got, want)
		}
	})

	t.Run("missing path not an error", func(t *testing.T) {
		got, touched, err := doApply(t, "REMOVE absent, a.b", nil, attr.Item{"a": sv("x")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := attr.Item{"a": sv("x")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v want %#v", got, want)
		}
		if got, want := pathStrs(touched), []string{"absent", "a.b"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("paths %v want %v", got, want)
		}
	})

	t.Run("several indexes highest first", func(t *testing.T) {
		got, _, err := doApply(t, "REMOVE l[0], l[2]", nil, attr.Item{"l": lv(sv("a"), sv("b"), sv("c"), sv("d"))}, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Removing [2] (c) then [0] (a) leaves [b, d].
		want := attr.Item{"l": lv(sv("b"), sv("d"))}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v want %#v", got, want)
		}
	})
}

func TestApplyAdd(t *testing.T) {
	tests := []struct {
		name   string
		expr   string
		item   attr.Item
		values map[string]attr.Value
		want   attr.Item
	}{
		{
			"number missing",
			"ADD version :one",
			attr.Item{},
			map[string]attr.Value{":one": nv("1")},
			attr.Item{"version": nv("1")},
		},
		{
			"number present",
			"ADD version :one",
			attr.Item{"version": nv("3")},
			map[string]attr.Value{":one": nv("1")},
			attr.Item{"version": nv("4")},
		},
		{
			"set union",
			"ADD tags :t",
			attr.Item{"tags": ssv("a", "b")},
			map[string]attr.Value{":t": ssv("b", "c")},
			attr.Item{"tags": ssv("a", "b", "c")},
		},
		{
			"set missing",
			"ADD tags :t",
			attr.Item{},
			map[string]attr.Value{":t": ssv("b", "c")},
			attr.Item{"tags": ssv("b", "c")},
		},
		{
			"number set union",
			"ADD ns :t",
			attr.Item{"ns": nsv("1", "2")},
			map[string]attr.Value{":t": nsv("2", "3")},
			attr.Item{"ns": nsv("1", "2", "3")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := doApply(t, tc.expr, nil, tc.item, tc.values)
			if err != nil {
				t.Fatalf("Apply error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestApplyAddErrors(t *testing.T) {
	tests := []struct {
		name   string
		expr   string
		item   attr.Item
		values map[string]attr.Value
	}{
		{"string operand", "ADD n :s", attr.Item{}, map[string]attr.Value{":s": sv("x")}},
		{"number onto string", "ADD n :one", attr.Item{"n": sv("x")}, map[string]attr.Value{":one": nv("1")}},
		{"set type mismatch", "ADD tags :t", attr.Item{"tags": ssv("a")}, map[string]attr.Value{":t": nsv("1")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := doApply(t, tc.expr, nil, tc.item, tc.values)
			wantErr(t, err, "ValidationException", "")
		})
	}
}

func TestApplyDelete(t *testing.T) {
	t.Run("remove members", func(t *testing.T) {
		got, _, err := doApply(t, "DELETE tags :t", nil, attr.Item{"tags": ssv("a", "b", "c")}, map[string]attr.Value{":t": ssv("b")})
		if err != nil {
			t.Fatal(err)
		}
		want := attr.Item{"tags": ssv("a", "c")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v want %#v", got, want)
		}
	})

	t.Run("emptied set removes attribute", func(t *testing.T) {
		got, _, err := doApply(t, "DELETE tags :t", nil, attr.Item{"tags": ssv("a", "b"), "x": sv("y")}, map[string]attr.Value{":t": ssv("a", "b")})
		if err != nil {
			t.Fatal(err)
		}
		want := attr.Item{"x": sv("y")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v want %#v", got, want)
		}
	})

	t.Run("missing attribute not an error", func(t *testing.T) {
		got, touched, err := doApply(t, "DELETE tags :t", nil, attr.Item{"x": sv("y")}, map[string]attr.Value{":t": ssv("a")})
		if err != nil {
			t.Fatal(err)
		}
		want := attr.Item{"x": sv("y")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v want %#v", got, want)
		}
		if got, want := pathStrs(touched), []string{"tags"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("paths %v want %v", got, want)
		}
	})

	t.Run("number set", func(t *testing.T) {
		got, _, err := doApply(t, "DELETE ns :t", nil, attr.Item{"ns": nsv("1", "2", "3")}, map[string]attr.Value{":t": nsv("1", "3")})
		if err != nil {
			t.Fatal(err)
		}
		want := attr.Item{"ns": nsv("2")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v want %#v", got, want)
		}
	})
}

func TestApplyDeleteErrors(t *testing.T) {
	tests := []struct {
		name   string
		expr   string
		item   attr.Item
		values map[string]attr.Value
	}{
		{"non-set operand", "DELETE tags :s", attr.Item{"tags": ssv("a")}, map[string]attr.Value{":s": sv("x")}},
		{"set type mismatch", "DELETE tags :t", attr.Item{"tags": ssv("a")}, map[string]attr.Value{":t": nsv("1")}},
		{"attribute not a set", "DELETE tags :t", attr.Item{"tags": sv("a")}, map[string]attr.Value{":t": ssv("a")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := doApply(t, tc.expr, nil, tc.item, tc.values)
			wantErr(t, err, "ValidationException", "")
		})
	}
}

func TestApplyPaths(t *testing.T) {
	item := attr.Item{
		"a":    sv("x"),
		"b":    sv("y"),
		"r":    sv("z"),
		"c":    nv("1"),
		"tags": ssv("t"),
	}
	values := map[string]attr.Value{":v": sv("v"), ":w": sv("w"), ":one": nv("1"), ":t": ssv("t")}
	_, touched, err := doApply(t, "SET a = :v, b = :w REMOVE r ADD c :one DELETE tags :t", nil, item, values)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "r", "c", "tags"}
	if got := pathStrs(touched); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths %v want %v", got, want)
	}
}

func TestApplySetAddVersion(t *testing.T) {
	names := map[string]string{"#name": "name"}
	values := map[string]attr.Value{
		":name": sv("flow"),
		":now":  nv("1700000000"),
		":one":  nv("1"),
	}
	const expr = "SET #name = :name, updated = :now ADD version :one"

	t.Run("without version", func(t *testing.T) {
		item := attr.Item{"id": sv("x")}
		got, touched, err := doApply(t, expr, names, item, values)
		if err != nil {
			t.Fatal(err)
		}
		want := attr.Item{
			"id":      sv("x"),
			"name":    sv("flow"),
			"updated": nv("1700000000"),
			"version": nv("1"),
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v\nwant %#v", got, want)
		}
		if got, want := pathStrs(touched), []string{"name", "updated", "version"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("paths %v want %v", got, want)
		}
	})

	t.Run("with version", func(t *testing.T) {
		item := attr.Item{"id": sv("x"), "version": nv("3")}
		got, _, err := doApply(t, expr, names, item, values)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got["version"], nv("4")) {
			t.Fatalf("version = %#v, want 4", got["version"])
		}
	})

	t.Run("if_not_exists increment", func(t *testing.T) {
		got, _, err := doApply(t, "SET n = if_not_exists(n, :zero) + :inc", nil, attr.Item{},
			map[string]attr.Value{":zero": nv("0"), ":inc": nv("1")})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got["n"], nv("1")) {
			t.Fatalf("n = %#v, want 1", got["n"])
		}
	})

	t.Run("remove absent assignee", func(t *testing.T) {
		item := attr.Item{"id": sv("x")}
		got, touched, err := doApply(t, "REMOVE assignee", nil, item, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, item) {
			t.Fatalf("got %#v want %#v", got, item)
		}
		if got, want := pathStrs(touched), []string{"assignee"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("paths %v want %v", got, want)
		}
	})
}

func TestApplyDoesNotModifyItem(t *testing.T) {
	item := attr.Item{
		"l": lv(sv("a"), sv("b")),
		"m": attr.Value{Type: attr.TypeM, M: map[string]attr.Value{"x": sv("y")}},
	}
	before := cloneItem(item)
	_, _, err := doApply(t, "SET l[0] = :v, m.z = :w", nil, item, map[string]attr.Value{":v": sv("z"), ":w": sv("q")})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(item, before) {
		t.Fatalf("input item modified: got %#v want %#v", item, before)
	}
}

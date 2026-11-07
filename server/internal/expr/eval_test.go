package expr

import (
	"errors"
	"strings"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

func sval(v string) attr.Value  { return attr.Value{Type: attr.TypeS, S: v} }
func nval(v string) attr.Value  { return attr.Value{Type: attr.TypeN, N: v} }
func bval(v ...byte) attr.Value { return attr.Value{Type: attr.TypeB, B: v} }
func boolval(v bool) attr.Value { return attr.Value{Type: attr.TypeBOOL, Bool: v} }
func nullval() attr.Value       { return attr.Value{Type: attr.TypeNULL} }

func testItem() attr.Item {
	return attr.Item{
		"id":       sval("123"),
		"phase":    sval("ACTIVE"),
		"comboKey": sval("org#user"),
		"num":      nval("5"),
		"big":      nval("10.5"),
		"bin":      bval(0x01, 0x02, 0x03),
		"flag":     boolval(true),
		"nothing":  nullval(),
		"tags":     {Type: attr.TypeSS, SS: []string{"a", "b", "c"}},
		"nums":     {Type: attr.TypeNS, NS: []string{"1", "2", "3"}},
		"blobs":    {Type: attr.TypeBS, BS: [][]byte{{0x01}, {0x02}}},
		"children": {Type: attr.TypeL, L: []attr.Value{
			sval("x"),
			sval("y"),
			{Type: attr.TypeM, M: map[string]attr.Value{"id": sval("c0")}},
		}},
		"meta": {Type: attr.TypeM, M: map[string]attr.Value{"k": sval("v")}},
	}
}

// evalExpr parses and evaluates a condition against testItem.
func evalExpr(t *testing.T, expr string, values map[string]attr.Value) bool {
	t.Helper()
	e := &Env{Values: map[string]bool{}}
	for k := range values {
		e.Values[k] = true
	}
	c, err := ParseCondition(expr, e)
	if err != nil {
		t.Fatalf("ParseCondition(%q): %v", expr, err)
	}
	got, err := Eval(c, testItem(), values, "ConditionExpression")
	if err != nil {
		t.Fatalf("Eval(%q): %v", expr, err)
	}
	return got
}

func TestResolve(t *testing.T) {
	it := testItem()
	cases := []struct {
		name string
		path Path
		want attr.Value
		ok   bool
	}{
		{"top-level", Path{{Name: "id"}}, sval("123"), true},
		{"absent-top", Path{{Name: "absent"}}, attr.Value{}, false},
		{"nested-map", Path{{Name: "meta"}, {Name: "k"}}, sval("v"), true},
		{"nested-map-absent-key", Path{{Name: "meta"}, {Name: "zz"}}, attr.Value{}, false},
		{"list-index", Path{{Name: "children"}, {Index: 1, IsIndex: true}}, sval("y"), true},
		{"list-index-nested", Path{{Name: "children"}, {Index: 2, IsIndex: true}, {Name: "id"}}, sval("c0"), true},
		{"list-out-of-range", Path{{Name: "children"}, {Index: 9, IsIndex: true}}, attr.Value{}, false},
		{"index-into-non-list", Path{{Name: "id"}, {Index: 0, IsIndex: true}}, attr.Value{}, false},
		{"name-into-non-map", Path{{Name: "id"}, {Name: "x"}}, attr.Value{}, false},
		{"empty-path", Path{}, attr.Value{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Resolve(it, c.path)
			if ok != c.ok {
				t.Errorf("Resolve(%s) = %+v, %v; want ok %v", c.path, got, ok, c.ok)
				return
			}
			if ok && !equal(got, c.want) {
				t.Errorf("Resolve(%s) = %+v, want %+v", c.path, got, c.want)
			}
		})
	}
	t.Run("nil-item", func(t *testing.T) {
		if _, ok := Resolve(nil, Path{{Name: "id"}}); ok {
			t.Error("Resolve(nil) = ok, want absent")
		}
	})
}

func TestEvalCompare(t *testing.T) {
	list := attr.Value{Type: attr.TypeL, L: []attr.Value{
		sval("x"), sval("y"), {Type: attr.TypeM, M: map[string]attr.Value{"id": sval("c0")}},
	}}
	meta := attr.Value{Type: attr.TypeM, M: map[string]attr.Value{"k": sval("v")}}
	cases := []struct {
		name   string
		expr   string
		values map[string]attr.Value
		want   bool
	}{
		// A absent path makes every comparison false, including <>.
		{"absent-eq", `absent = :v`, map[string]attr.Value{":v": sval("x")}, false},
		{"absent-ne", `absent <> :v`, map[string]attr.Value{":v": sval("x")}, false},
		{"absent-lt", `absent < :v`, map[string]attr.Value{":v": sval("x")}, false},
		{"absent-le", `absent <= :v`, map[string]attr.Value{":v": sval("x")}, false},
		{"absent-gt", `absent > :v`, map[string]attr.Value{":v": sval("x")}, false},
		{"absent-ge", `absent >= :v`, map[string]attr.Value{":v": sval("x")}, false},

		// = and <> compare any types.
		{"eq-string", `id = :v`, map[string]attr.Value{":v": sval("123")}, true},
		{"eq-string-no", `id = :v`, map[string]attr.Value{":v": sval("x")}, false},
		{"ne-string", `id <> :v`, map[string]attr.Value{":v": sval("x")}, true},
		{"eq-number", `num = :v`, map[string]attr.Value{":v": nval("5")}, true},
		{"eq-number-decimal", `big = :v`, map[string]attr.Value{":v": nval("10.5")}, true},
		{"eq-bool", `flag = :v`, map[string]attr.Value{":v": boolval(true)}, true},
		{"eq-null", `nothing = :v`, map[string]attr.Value{":v": nullval()}, true},
		{"eq-different-types", `num = :v`, map[string]attr.Value{":v": sval("5")}, false},
		{"ne-different-types", `num <> :v`, map[string]attr.Value{":v": sval("5")}, true},
		{"eq-map", `meta = :v`, map[string]attr.Value{":v": meta}, true},
		{"eq-map-differs", `meta = :v`, map[string]attr.Value{":v": attr.Value{Type: attr.TypeM, M: map[string]attr.Value{"k": sval("DIFFERENT")}}}, false},
		{"eq-list", `children = :v`, map[string]attr.Value{":v": list}, true},
		{"eq-list-reordered", `children = :v`, map[string]attr.Value{":v": attr.Value{Type: attr.TypeL, L: []attr.Value{sval("y"), sval("x"), {Type: attr.TypeM, M: map[string]attr.Value{"id": sval("c0")}}}}}, false},
		{"eq-ss-ignores-order", `tags = :v`, map[string]attr.Value{":v": attr.Value{Type: attr.TypeSS, SS: []string{"c", "a", "b"}}}, true},
		{"eq-ns-ignores-order", `nums = :v`, map[string]attr.Value{":v": attr.Value{Type: attr.TypeNS, NS: []string{"3", "1", "2"}}}, true},
		{"eq-bs-ignores-order", `blobs = :v`, map[string]attr.Value{":v": attr.Value{Type: attr.TypeBS, BS: [][]byte{{0x02}, {0x01}}}}, true},

		// < <= > >= order N numerically and S, B by bytes.
		{"num-lt", `num < :v`, map[string]attr.Value{":v": nval("6")}, true},
		{"num-le", `num <= :v`, map[string]attr.Value{":v": nval("5")}, true},
		{"num-gt", `num > :v`, map[string]attr.Value{":v": nval("4")}, true},
		{"num-ge-no", `num >= :v`, map[string]attr.Value{":v": nval("6")}, false},
		{"decimal-le", `big <= :v`, map[string]attr.Value{":v": nval("10.5")}, true},
		{"string-lt", `id < :v`, map[string]attr.Value{":v": sval("2")}, true},
		{"string-gt", `comboKey > :v`, map[string]attr.Value{":v": sval("aaa")}, true},
		{"binary-lt", `bin < :v`, map[string]attr.Value{":v": bval(0x02)}, true},
		{"binary-gt", `bin > :v`, map[string]attr.Value{":v": bval(0x00)}, true},
		// Different types, or a non-orderable type, are false.
		{"order-different-types", `num < :v`, map[string]attr.Value{":v": sval("6")}, false},
		{"order-nonorderable", `flag < :v`, map[string]attr.Value{":v": boolval(false)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalExpr(t, c.expr, c.values); got != c.want {
				t.Errorf("eval(%q) = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}

func TestEvalBetween(t *testing.T) {
	cases := []struct {
		name   string
		expr   string
		values map[string]attr.Value
		want   bool
	}{
		{"in-range", `num BETWEEN :lo AND :hi`, map[string]attr.Value{":lo": nval("1"), ":hi": nval("10")}, true},
		{"below", `num BETWEEN :lo AND :hi`, map[string]attr.Value{":lo": nval("6"), ":hi": nval("10")}, false},
		{"inclusive-bounds", `big BETWEEN :lo AND :hi`, map[string]attr.Value{":lo": nval("10.5"), ":hi": nval("10.5")}, true},
		{"string-range", `id BETWEEN :lo AND :hi`, map[string]attr.Value{":lo": sval("1"), ":hi": sval("9")}, true},
		{"absent-path", `absent BETWEEN :lo AND :hi`, map[string]attr.Value{":lo": nval("1"), ":hi": nval("10")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalExpr(t, c.expr, c.values); got != c.want {
				t.Errorf("eval(%q) = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}

func TestEvalIn(t *testing.T) {
	cases := []struct {
		name   string
		expr   string
		values map[string]attr.Value
		want   bool
	}{
		{"match", `phase IN (:s1, :s2)`, map[string]attr.Value{":s1": sval("ACTIVE"), ":s2": sval("DONE")}, true},
		{"second-match", `phase IN (:s1, :s2)`, map[string]attr.Value{":s1": sval("X"), ":s2": sval("ACTIVE")}, true},
		{"no-match", `phase IN (:s1, :s2)`, map[string]attr.Value{":s1": sval("X"), ":s2": sval("Y")}, false},
		{"numbers", `num IN (:a, :b)`, map[string]attr.Value{":a": nval("1"), ":b": nval("5")}, true},
		{"different-type-no-match", `num IN (:a)`, map[string]attr.Value{":a": sval("5")}, false},
		{"absent-path", `absent IN (:v)`, map[string]attr.Value{":v": sval("x")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalExpr(t, c.expr, c.values); got != c.want {
				t.Errorf("eval(%q) = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}

func TestEvalFunc(t *testing.T) {
	cases := []struct {
		name   string
		expr   string
		values map[string]attr.Value
		want   bool
	}{
		{"attribute_exists", `attribute_exists(id)`, nil, true},
		{"attribute_exists-absent", `attribute_exists(absent)`, nil, false},
		{"attribute_not_exists", `attribute_not_exists(absent)`, nil, true},
		{"attribute_not_exists-present", `attribute_not_exists(id)`, nil, false},

		{"attribute_type-match", `attribute_type(id, :t)`, map[string]attr.Value{":t": sval("S")}, true},
		{"attribute_type-no-match", `attribute_type(id, :t)`, map[string]attr.Value{":t": sval("N")}, false},
		{"attribute_type-ss", `attribute_type(tags, :t)`, map[string]attr.Value{":t": sval("SS")}, true},
		{"attribute_type-l", `attribute_type(children, :t)`, map[string]attr.Value{":t": sval("L")}, true},
		{"attribute_type-m", `attribute_type(meta, :t)`, map[string]attr.Value{":t": sval("M")}, true},
		{"attribute_type-absent", `attribute_type(absent, :t)`, map[string]attr.Value{":t": sval("S")}, false},

		{"begins_with-s", `begins_with(comboKey, :p)`, map[string]attr.Value{":p": sval("org#")}, true},
		{"begins_with-s-no", `begins_with(comboKey, :p)`, map[string]attr.Value{":p": sval("xyz")}, false},
		{"begins_with-b", `begins_with(bin, :p)`, map[string]attr.Value{":p": bval(0x01)}, true},
		{"begins_with-b-no", `begins_with(bin, :p)`, map[string]attr.Value{":p": bval(0x02)}, false},
		{"begins_with-type-mismatch", `begins_with(id, :p)`, map[string]attr.Value{":p": bval(0x01)}, false},
		{"begins_with-absent", `begins_with(absent, :p)`, map[string]attr.Value{":p": sval("x")}, false},

		{"contains-substring", `contains(id, :x)`, map[string]attr.Value{":x": sval("23")}, true},
		{"contains-substring-no", `contains(id, :x)`, map[string]attr.Value{":x": sval("z")}, false},
		{"contains-ss", `contains(tags, :x)`, map[string]attr.Value{":x": sval("b")}, true},
		{"contains-ss-no", `contains(tags, :x)`, map[string]attr.Value{":x": sval("z")}, false},
		{"contains-ns", `contains(nums, :x)`, map[string]attr.Value{":x": nval("2")}, true},
		{"contains-ns-no", `contains(nums, :x)`, map[string]attr.Value{":x": nval("9")}, false},
		{"contains-bs", `contains(blobs, :x)`, map[string]attr.Value{":x": bval(0x02)}, true},
		{"contains-list", `contains(children, :x)`, map[string]attr.Value{":x": sval("y")}, true},
		{"contains-list-no", `contains(children, :x)`, map[string]attr.Value{":x": sval("z")}, false},
		{"contains-map-unsupported", `contains(meta, :x)`, map[string]attr.Value{":x": sval("v")}, false},
		{"contains-absent", `contains(absent, :x)`, map[string]attr.Value{":x": sval("v")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalExpr(t, c.expr, c.values); got != c.want {
				t.Errorf("eval(%q) = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}

func TestEvalSize(t *testing.T) {
	cases := []struct {
		name   string
		expr   string
		values map[string]attr.Value
		want   bool
	}{
		{"string-bytes", `size(id) = :v`, map[string]attr.Value{":v": nval("3")}, true},
		{"string-bytes-combo", `size(comboKey) = :v`, map[string]attr.Value{":v": nval("8")}, true},
		{"binary-bytes", `size(bin) = :v`, map[string]attr.Value{":v": nval("3")}, true},
		{"string-set", `size(tags) = :v`, map[string]attr.Value{":v": nval("3")}, true},
		{"number-set", `size(nums) = :v`, map[string]attr.Value{":v": nval("3")}, true},
		{"binary-set", `size(blobs) = :v`, map[string]attr.Value{":v": nval("2")}, true},
		{"list", `size(children) = :v`, map[string]attr.Value{":v": nval("3")}, true},
		{"map", `size(meta) = :v`, map[string]attr.Value{":v": nval("1")}, true},
		{"greater-than", `size(children) > :zero`, map[string]attr.Value{":zero": nval("0")}, true},
		{"both-sides", `size(id) = size(tags)`, nil, true},
		{"number-not-sizeable", `size(num) = :v`, map[string]attr.Value{":v": nval("1")}, false},
		{"bool-not-sizeable", `size(flag) = :v`, map[string]attr.Value{":v": nval("1")}, false},
		{"absent-path", `size(absent) = :v`, map[string]attr.Value{":v": nval("1")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalExpr(t, c.expr, c.values); got != c.want {
				t.Errorf("eval(%q) = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}

func TestEvalLogic(t *testing.T) {
	vs := map[string]attr.Value{
		":v": sval("123"), ":w": sval("ACTIVE"), ":zz": sval("no"), ":yy": sval("no"),
	}
	cases := []struct {
		name string
		expr string
		want bool
	}{
		{"and-true", `id = :v AND phase = :w`, true},
		{"and-false", `id = :v AND phase = :zz`, false},
		{"or-true", `id = :v OR phase = :zz`, true},
		{"or-false", `id = :zz OR phase = :yy`, false},
		{"not", `NOT id = :zz`, true},
		{"not-and", `NOT (id = :v AND phase = :w)`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evalExpr(t, c.expr, vs); got != c.want {
				t.Errorf("eval(%q) = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}

// evalFilter parses and evaluates a FilterExpression against testItem.
func evalFilter(t *testing.T, expr string, names map[string]string, values map[string]attr.Value) bool {
	t.Helper()
	e := &Env{Names: names, Values: map[string]bool{}}
	for k := range values {
		e.Values[k] = true
	}
	c, err := ParseFilter(expr, e)
	if err != nil {
		t.Fatalf("ParseFilter(%q): %v", expr, err)
	}
	got, err := Eval(c, testItem(), values, "FilterExpression")
	if err != nil {
		t.Fatalf("Eval(%q): %v", expr, err)
	}
	return got
}

func TestEvalFilters(t *testing.T) {
	t.Run("status-in-and-not-contains", func(t *testing.T) {
		names := map[string]string{"#status": "phase"}
		expr := `#status IN (:s1, :s2) AND NOT contains(tags, :t)`
		match := map[string]attr.Value{":s1": sval("ACTIVE"), ":s2": sval("DONE"), ":t": sval("z")}
		if !evalFilter(t, expr, names, match) {
			t.Errorf("eval(%q) = false, want true", expr)
		}
		noMatch := map[string]attr.Value{":s1": sval("ACTIVE"), ":s2": sval("DONE"), ":t": sval("b")}
		if evalFilter(t, expr, names, noMatch) {
			t.Errorf("eval(%q) = true, want false", expr)
		}
	})
	t.Run("attribute-not-exists", func(t *testing.T) {
		if !evalFilter(t, `attribute_not_exists(deleted)`, nil, nil) {
			t.Error("attribute_not_exists(deleted) = false, want true")
		}
	})
	t.Run("begins-with-and-size", func(t *testing.T) {
		expr := `begins_with(comboKey, :p) AND size(children) > :zero`
		values := map[string]attr.Value{":p": sval("org#"), ":zero": nval("0")}
		if !evalFilter(t, expr, nil, values) {
			t.Errorf("eval(%q) = false, want true", expr)
		}
	})
}

func TestEvalAttributeTypeError(t *testing.T) {
	cases := []struct {
		name string
		t    attr.Value
		want string
	}{
		{"invalid-name", sval("FOO"), `Invalid ConditionExpression: Invalid attribute type name found; type: FOO, valid types: { B,NULL,SS,BOOL,L,BS,N,NS,S,M }`},
		{"not-string", nval("5"), `Invalid ConditionExpression: Invalid attribute type name found; type: 5, valid types: { B,NULL,SS,BOOL,L,BS,N,NS,S,M }`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cond, err := ParseCondition(`attribute_type(id, :t)`, &Env{Values: map[string]bool{":t": true}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = Eval(cond, testItem(), map[string]attr.Value{":t": c.t}, "ConditionExpression")
			wantErr(t, err, "ValidationException", c.want)
		})
	}
}

func TestEvalBetweenLoGreaterThanHi(t *testing.T) {
	cond, err := ParseCondition(`num BETWEEN :lo AND :hi`, &Env{Values: map[string]bool{":lo": true, ":hi": true}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Eval(cond, testItem(), map[string]attr.Value{":lo": nval("10"), ":hi": nval("1")}, "ConditionExpression")
	if err == nil {
		t.Fatal("got nil error, want BETWEEN bound error")
	}
	var de *ddberr.Error
	if !errors.As(err, &de) || de.Code() != "ValidationException" || !strings.Contains(de.Message, "BETWEEN operator requires upper bound") {
		t.Errorf("error = %v, want ValidationException mentioning BETWEEN", err)
	}
}

func TestEvalMissingValuePlaceholder(t *testing.T) {
	c := Compare{Op: "=", L: PathOperand{Path: Path{{Name: "id"}}}, R: ValueOperand{Name: ":nope"}}
	_, err := Eval(c, testItem(), map[string]attr.Value{}, "ConditionExpression")
	if err == nil {
		t.Fatal("got nil error, want plain error for absent ExpressionAttributeValues")
	}
	var de *ddberr.Error
	if errors.As(err, &de) {
		t.Fatalf("error is *ddberr.Error %q, want plain error", de.Error())
	}
}

func FuzzEval(f *testing.F) {
	seeds := []string{
		`id = :v`,
		`#status IN (:s1, :s2) AND NOT contains(tags, :t)`,
		`attribute_not_exists(deleted)`,
		`begins_with(comboKey, :p) AND size(children) > :zero`,
		`num BETWEEN :lo AND :hi`,
		`attribute_type(id, :t)`,
		`size(id) = size(tags)`,
		`contains(children, :x)`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		e := &Env{
			Names: map[string]string{
				"#a": "a", "#b": "b", "#status": "phase", "#t": "tags",
				"#ck": "comboKey", "#ch": "children", "#n": "num", "#m": "meta",
			},
			Values: map[string]bool{
				":v": true, ":w": true, ":s1": true, ":s2": true, ":t": true,
				":p": true, ":zero": true, ":lo": true, ":hi": true, ":x": true,
			},
		}
		c, err := ParseCondition(expr, e)
		if err != nil {
			return
		}
		vals := map[string]attr.Value{
			":v": sval("x"), ":w": sval("y"), ":s1": sval("ACTIVE"), ":s2": sval("DONE"),
			":t": sval("z"), ":p": sval("org#"), ":zero": nval("0"),
			":lo": nval("1"), ":hi": nval("10"), ":x": sval("y"),
		}
		_, _ = Eval(c, testItem(), vals, "ConditionExpression")
	})
}

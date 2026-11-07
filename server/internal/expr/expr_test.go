package expr

import (
	"errors"
	"reflect"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

func env(names map[string]string, values ...string) *Env {
	e := &Env{Names: names, Values: map[string]bool{}}
	for _, v := range values {
		e.Values[v] = true
	}
	return e
}

func wantErr(t *testing.T, err error, code, msg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want %s", code)
	}
	var de *ddberr.Error
	if !errors.As(err, &de) {
		t.Fatalf("error is %T, want *ddberr.Error", err)
	}
	if de.Code() != code {
		t.Errorf("Code() = %q, want %q", de.Code(), code)
	}
	if msg != "" && de.Message != msg {
		t.Errorf("Message = %q, want %q", de.Message, msg)
	}
}

func TestParseCondition(t *testing.T) {
	cases := []struct {
		name   string
		expr   string
		names  map[string]string
		values []string
		want   string
	}{
		{"eq", `a = :v`, nil, []string{":v"}, `(a = :v)`},
		{"ne", `a <> :v`, nil, []string{":v"}, `(a <> :v)`},
		{"lt", `a < :v`, nil, []string{":v"}, `(a < :v)`},
		{"le", `a <= :v`, nil, []string{":v"}, `(a <= :v)`},
		{"gt", `a > :v`, nil, []string{":v"}, `(a > :v)`},
		{"ge", `a >= :v`, nil, []string{":v"}, `(a >= :v)`},
		{"between", `a BETWEEN :lo AND :hi`, nil, []string{":lo", ":hi"}, `(a BETWEEN :lo AND :hi)`},
		{"in-one", `a IN (:v)`, nil, []string{":v"}, `(a IN (:v))`},
		{"in-three", `a IN (:v1, :v2, :v3)`, nil, []string{":v1", ":v2", ":v3"}, `(a IN (:v1, :v2, :v3))`},
		{"attribute_exists", `attribute_exists(a)`, nil, nil, `(attribute_exists(a))`},
		{"attribute_not_exists", `attribute_not_exists(id)`, nil, nil, `(attribute_not_exists(id))`},
		{"attribute_type", `attribute_type(a, :t)`, nil, []string{":t"}, `(attribute_type(a, :t))`},
		{"begins_with", `begins_with(a, :p)`, nil, []string{":p"}, `(begins_with(a, :p))`},
		{"contains", `contains(a, :x)`, nil, []string{":x"}, `(contains(a, :x))`},
		{"not", `NOT a = :v`, nil, []string{":v"}, `(NOT (a = :v))`},
		{"and-precedence", `a = :v AND b = :w OR c = :x`, nil, []string{":v", ":w", ":x"}, `(((a = :v) AND (b = :w)) OR (c = :x))`},
		{"parens-override", `a = :v AND (b = :w OR c = :x)`, nil, []string{":v", ":w", ":x"}, `((a = :v) AND ((b = :w) OR (c = :x)))`},
		{"size-both-sides", `size(a) = size(b)`, nil, nil, `(size(a) = size(b))`},
		{"nested-path", `a.b[0].#c = :v`, map[string]string{"#c": "c"}, []string{":v"}, `(a.b[0].c = :v)`},
		{"lowercase-keywords", `not a = :v and b between :lo and :hi`, nil, []string{":v", ":lo", ":hi"}, `((NOT (a = :v)) AND (b BETWEEN :lo AND :hi))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := env(c.names, c.values...)
			got, err := ParseCondition(c.expr, e)
			if err != nil {
				t.Fatalf("ParseCondition(%q) error: %v", c.expr, err)
			}
			if s := String(got); s != c.want {
				t.Errorf("String() = %q, want %q", s, c.want)
			}
		})
	}
}

func TestParseFilter(t *testing.T) {
	cases := []struct {
		name   string
		expr   string
		names  map[string]string
		values []string
		want   string
	}{
		{
			"status in and not contains",
			`#status IN (:s1, :s2) AND NOT contains(tags, :t)`,
			map[string]string{"#status": "status"},
			[]string{":s1", ":s2", ":t"},
			`((status IN (:s1, :s2)) AND (NOT (contains(tags, :t))))`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := env(c.names, c.values...)
			got, err := ParseFilter(c.expr, e)
			if err != nil {
				t.Fatalf("ParseFilter(%q) error: %v", c.expr, err)
			}
			if s := String(got); s != c.want {
				t.Errorf("String() = %q, want %q", s, c.want)
			}
		})
	}
}

func TestParseKeyCondition(t *testing.T) {
	cases := []struct {
		name   string
		expr   string
		names  map[string]string
		values []string
		want   KeyCond
	}{
		{
			"pk-only",
			`entity = :e`,
			nil,
			[]string{":e"},
			KeyCond{PK: "entity", PKValue: ":e"},
		},
		{
			"begins-with",
			`entity = :e AND begins_with(comboKey, :ck)`,
			nil,
			[]string{":e", ":ck"},
			KeyCond{PK: "entity", PKValue: ":e", SK: "comboKey", SKOp: "begins_with", SKValues: []string{":ck"}},
		},
		{
			"begins-with-reversed",
			`begins_with(comboKey, :ck) AND entity = :e`,
			nil,
			[]string{":ck", ":e"},
			KeyCond{PK: "entity", PKValue: ":e", SK: "comboKey", SKOp: "begins_with", SKValues: []string{":ck"}},
		},
		{
			"between-with-names",
			`#e = :e AND #n BETWEEN :a AND :b`,
			map[string]string{"#e": "entity", "#n": "num"},
			[]string{":e", ":a", ":b"},
			KeyCond{PK: "entity", PKValue: ":e", SK: "num", SKOp: "BETWEEN", SKValues: []string{":a", ":b"}},
		},
		{
			"sk-lt",
			`pk = :p AND sk < :v`,
			nil,
			[]string{":p", ":v"},
			KeyCond{PK: "pk", PKValue: ":p", SK: "sk", SKOp: "<", SKValues: []string{":v"}},
		},
		{
			"sk-le",
			`pk = :p AND sk <= :v`,
			nil,
			[]string{":p", ":v"},
			KeyCond{PK: "pk", PKValue: ":p", SK: "sk", SKOp: "<=", SKValues: []string{":v"}},
		},
		{
			"sk-gt",
			`pk = :p AND sk > :v`,
			nil,
			[]string{":p", ":v"},
			KeyCond{PK: "pk", PKValue: ":p", SK: "sk", SKOp: ">", SKValues: []string{":v"}},
		},
		{
			"sk-ge",
			`pk = :p AND sk >= :v`,
			nil,
			[]string{":p", ":v"},
			KeyCond{PK: "pk", PKValue: ":p", SK: "sk", SKOp: ">=", SKValues: []string{":v"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := env(c.names, c.values...)
			got, err := ParseKeyCondition(c.expr, e)
			if err != nil {
				t.Fatalf("ParseKeyCondition(%q) error: %v", c.expr, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParseProjection(t *testing.T) {
	e := env(map[string]string{"#name": "name"})
	got, err := ParseProjection(`id, #name, comboKey, children[0].id`, e)
	if err != nil {
		t.Fatalf("ParseProjection error: %v", err)
	}
	var gotStr []string
	for _, p := range got {
		gotStr = append(gotStr, p.String())
	}
	want := []string{"id", "name", "comboKey", "children[0].id"}
	if !reflect.DeepEqual(gotStr, want) {
		t.Errorf("got %v, want %v", gotStr, want)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name    string
		parse   func(*Env) error
		wantMsg string
	}{
		{
			"syntax-non-eof",
			func(e *Env) error { _, err := ParseCondition(`a == :v`, e); return err },
			`Invalid ConditionExpression: Syntax error; token: "=", near: "=="`,
		},
		{
			"syntax-first-token",
			func(e *Env) error { _, err := ParseCondition(`= :v`, e); return err },
			`Invalid ConditionExpression: Syntax error; token: "=", near: "="`,
		},
		{
			"syntax-eof",
			func(e *Env) error { _, err := ParseCondition(`a = `, e); return err },
			`Invalid ConditionExpression: Syntax error; token: "<EOF>", near: "="`,
		},
		{
			"reserved",
			func(e *Env) error { _, err := ParseCondition(`name = :v`, e); return err },
			`Invalid ConditionExpression: Attribute name is a reserved keyword; reserved keyword: name`,
		},
		{
			"undefined-name",
			func(e *Env) error { _, err := ParseCondition(`#x = :v`, e); return err },
			`Invalid ConditionExpression: An expression attribute name used in the document path is not defined; attribute name: #x`,
		},
		{
			"undefined-value",
			func(e *Env) error { _, err := ParseCondition(`a = :x`, e); return err },
			`Invalid ConditionExpression: An expression attribute value used in expression is not defined; attribute value: :x`,
		},
		{
			"empty-condition",
			func(e *Env) error { _, err := ParseCondition(``, e); return err },
			`Invalid ConditionExpression: The expression can not be empty;`,
		},
		{
			"empty-filter",
			func(e *Env) error { _, err := ParseFilter(`   `, e); return err },
			`Invalid FilterExpression: The expression can not be empty;`,
		},
		{
			"empty-keycondition",
			func(e *Env) error { _, err := ParseKeyCondition(``, e); return err },
			`Invalid KeyConditionExpression: The expression can not be empty;`,
		},
		{
			"empty-projection",
			func(e *Env) error { _, err := ParseProjection(``, e); return err },
			`Invalid ProjectionExpression: The expression can not be empty;`,
		},
		{
			"keycondition-or",
			func(e *Env) error { _, err := ParseKeyCondition(`a = :v OR b = :w`, e); return err },
			`Query key condition not supported`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := env(nil, ":v", ":w")
			wantErr(t, c.parse(e), "ValidationException", c.wantMsg)
		})
	}
}

func TestCheckUnused(t *testing.T) {
	t.Run("names-before-values", func(t *testing.T) {
		e := env(map[string]string{"#a": "a", "#b": "b"}, ":x", ":y")
		if _, err := ParseCondition(`pk = :x`, e); err != nil {
			t.Fatal(err)
		}
		wantErr(t, e.CheckUnused(), "ValidationException",
			`1 validation error detected: Value provided in ExpressionAttributeNames unused in expressions: keys: {#a, #b}`)
	})
	t.Run("values", func(t *testing.T) {
		e := env(map[string]string{"#a": "a"}, ":x", ":y")
		if _, err := ParseCondition(`#a = :x`, e); err != nil {
			t.Fatal(err)
		}
		wantErr(t, e.CheckUnused(), "ValidationException",
			`1 validation error detected: Value provided in ExpressionAttributeValues unused in expressions: keys: {:y}`)
	})
	t.Run("none", func(t *testing.T) {
		e := env(map[string]string{"#a": "a"}, ":x")
		if _, err := ParseCondition(`#a = :x`, e); err != nil {
			t.Fatal(err)
		}
		if err := e.CheckUnused(); err != nil {
			t.Errorf("CheckUnused() = %v, want nil", err)
		}
	})
}

func TestCheckUnusedSharedAcrossExpressions(t *testing.T) {
	e := env(map[string]string{"#a": "a", "#b": "b"}, ":x")
	if _, err := ParseCondition(`#a = :x`, e); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseProjection(`#b`, e); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckUnused(); err != nil {
		t.Errorf("CheckUnused() = %v, want nil (all placeholders used)", err)
	}
}

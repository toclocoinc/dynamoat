package partiql

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/expr"
)

func mustParse(t *testing.T, stmt string, params ...attr.Value) Statement {
	t.Helper()
	s, err := Parse(stmt, params)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", stmt, err)
	}
	return s
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

// operValue returns the value a ValueOperand refers to.
func operValue(t *testing.T, values map[string]attr.Value, o expr.Operand) attr.Value {
	t.Helper()
	vop, ok := o.(expr.ValueOperand)
	if !ok {
		t.Fatalf("operand is %T, want ValueOperand", o)
	}
	v, ok := values[vop.Name]
	if !ok {
		t.Fatalf("values has no %q", vop.Name)
	}
	return v
}

func TestSelect(t *testing.T) {
	cases := []struct {
		name    string
		stmt    string
		want    string
		table   string
		index   string
		projNil bool
		proj    []expr.Path
	}{
		{"all", `SELECT * FROM Music`, `SELECT * FROM Music`, "Music", "", true, nil},
		{"index", `SELECT * FROM "t"."i"`, `SELECT * FROM t.i`, "t", "i", true, nil},
		{"quoted-table", `SELECT * FROM "select"`, `SELECT * FROM "select"`, "select", "", true, nil},
		{"projection", `SELECT a, b.c[0] FROM t`, `SELECT a, b.c[0] FROM t`, "t", "", false, []expr.Path{
			{{Name: "a"}},
			{{Name: "b"}, {Name: "c"}, {Index: 0, IsIndex: true}},
		}},
		{"where", `SELECT * FROM t WHERE a = 1`, `SELECT * FROM t WHERE (a = 1)`, "t", "", true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustParse(t, c.stmt)
			sel, ok := s.(SelectPlan)
			if !ok {
				t.Fatalf("got %T, want SelectPlan", s)
			}
			if sel.Table != c.table || sel.Index != c.index {
				t.Errorf("table/index = %q/%q, want %q/%q", sel.Table, sel.Index, c.table, c.index)
			}
			if c.projNil {
				if sel.Projection != nil {
					t.Errorf("Projection = %v, want nil", sel.Projection)
				}
			} else if !reflect.DeepEqual(sel.Projection, c.proj) {
				t.Errorf("Projection = %v, want %v", sel.Projection, c.proj)
			}
			if got := s.String(); got != c.want {
				t.Errorf("String() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestInsert(t *testing.T) {
	cases := []struct {
		name string
		stmt string
		want string
		item attr.Item
	}{
		{"basic", `INSERT INTO "Music" VALUE {'Artist': 'x', 'Count': 5}`, `INSERT INTO Music VALUE {Artist: 'x', Count: 5}`, attr.Item{
			"Artist": {Type: attr.TypeS, S: "x"},
			"Count":  {Type: attr.TypeN, N: "5"},
		}},
		{"empty", `INSERT INTO t VALUE {}`, `INSERT INTO t VALUE {}`, attr.Item{}},
		{"quoted-key", `INSERT INTO t VALUE {"select": 1}`, `INSERT INTO t VALUE {"select": 1}`, attr.Item{
			"select": {Type: attr.TypeN, N: "1"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustParse(t, c.stmt)
			ins, ok := s.(InsertPlan)
			if !ok {
				t.Fatalf("got %T, want InsertPlan", s)
			}
			if !reflect.DeepEqual(ins.Item, c.item) {
				t.Errorf("Item = %v, want %v", ins.Item, c.item)
			}
			if got := s.String(); got != c.want {
				t.Errorf("String() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestUpdate(t *testing.T) {
	cases := []struct {
		name string
		stmt string
		want string
	}{
		{"plain", `UPDATE t SET a = 5 WHERE id = 1`, `UPDATE t SET a = 5 WHERE (id = 1)`},
		{"add", `UPDATE t SET a = a + 1 WHERE id = 1`, `UPDATE t SET a = a + 1 WHERE (id = 1)`},
		{"list_append", `UPDATE t SET a = list_append(a, [1, 2]) WHERE id = 1`, `UPDATE t SET a = list_append(a, [1, 2]) WHERE (id = 1)`},
		{"set_add", `UPDATE t SET a = set_add(a, <<1, 2>>) WHERE id = 1`, `UPDATE t SET a = set_add(a, <<1, 2>>) WHERE (id = 1)`},
		{"set_delete", `UPDATE t SET a = set_delete(a, <<'x'>>) WHERE id = 1`, `UPDATE t SET a = set_delete(a, <<'x'>>) WHERE (id = 1)`},
		{"remove", `UPDATE t SET a = 1 REMOVE b.c WHERE id = 1`, `UPDATE t SET a = 1 REMOVE b.c WHERE (id = 1)`},
		{"cond", `UPDATE t SET a = 1 WHERE id = 1 AND x = 'y'`, `UPDATE t SET a = 1 WHERE ((id = 1) AND (x = 'y'))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustParse(t, c.stmt)
			if _, ok := s.(UpdatePlan); !ok {
				t.Fatalf("got %T, want UpdatePlan", s)
			}
			if got := s.String(); got != c.want {
				t.Errorf("String() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestUpdateStructure(t *testing.T) {
	s := mustParse(t, `UPDATE t SET a = b + 1, c = set_add(c, <<1>>), d = set_delete(d, <<2>>) REMOVE e WHERE id = 1`)
	u := s.(UpdatePlan)
	if u.Table != "t" {
		t.Errorf("Table = %q", u.Table)
	}
	if len(u.Update.Set) != 1 {
		t.Fatalf("len(Set) = %d, want 1", len(u.Update.Set))
	}
	if len(u.Update.Add) != 1 || len(u.Update.Delete) != 1 || len(u.Update.Remove) != 1 {
		t.Fatalf("Add/Delete/Remove = %d/%d/%d, want 1/1/1", len(u.Update.Add), len(u.Update.Delete), len(u.Update.Remove))
	}
	setAction := u.Update.Set[0]
	if arith, ok := setAction.Value.(expr.SetArith); !ok {
		t.Fatalf("Set[0].Value = %T, want SetArith", setAction.Value)
	} else if arith.Op != "+" {
		t.Errorf("arith.Op = %q", arith.Op)
	}
	if _, ok := u.Where.(expr.Compare); !ok {
		t.Errorf("Where = %T, want Compare", u.Where)
	}
}

func TestDelete(t *testing.T) {
	cases := []struct {
		name string
		stmt string
		want string
	}{
		{"basic", `DELETE FROM t WHERE id = 1`, `DELETE FROM t WHERE (id = 1)`},
		{"cond", `DELETE FROM t WHERE id = 1 AND a = 'x'`, `DELETE FROM t WHERE ((id = 1) AND (a = 'x'))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustParse(t, c.stmt)
			if _, ok := s.(DeletePlan); !ok {
				t.Fatalf("got %T, want DeletePlan", s)
			}
			if got := s.String(); got != c.want {
				t.Errorf("String() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestExists(t *testing.T) {
	s := mustParse(t, `EXISTS(SELECT * FROM t WHERE a = 1)`)
	ex, ok := s.(ExistsPlan)
	if !ok {
		t.Fatalf("got %T, want ExistsPlan", s)
	}
	if ex.Select.Table != "t" {
		t.Errorf("Table = %q", ex.Select.Table)
	}
	if got := s.String(); got != `EXISTS(SELECT * FROM t WHERE (a = 1))` {
		t.Errorf("String() = %q", got)
	}
}

func TestValues(t *testing.T) {
	cases := []struct {
		name string
		stmt string
		item attr.Item
	}{
		{"string-escape", `INSERT INTO t VALUE {'s': 'a''b'}`, attr.Item{"s": {Type: attr.TypeS, S: "a'b"}}},
		{"number-int", `INSERT INTO t VALUE {'n': -5}`, attr.Item{"n": {Type: attr.TypeN, N: "-5"}}},
		{"number-decimal", `INSERT INTO t VALUE {'n': 1.50}`, attr.Item{"n": {Type: attr.TypeN, N: "1.5"}}},
		{"number-exponent", `INSERT INTO t VALUE {'n': 1e2}`, attr.Item{"n": {Type: attr.TypeN, N: "100"}}},
		{"bool", `INSERT INTO t VALUE {'b': true, 'c': false}`, attr.Item{
			"b": {Type: attr.TypeBOOL, Bool: true},
			"c": {Type: attr.TypeBOOL, Bool: false},
		}},
		{"null", `INSERT INTO t VALUE {'n': null}`, attr.Item{"n": {Type: attr.TypeNULL}}},
		{"list", `INSERT INTO t VALUE {'l': [1, 'a', true]}`, attr.Item{"l": {Type: attr.TypeL, L: []attr.Value{
			{Type: attr.TypeN, N: "1"},
			{Type: attr.TypeS, S: "a"},
			{Type: attr.TypeBOOL, Bool: true},
		}}}},
		{"map", `INSERT INTO t VALUE {'m': {'x': 1}}`, attr.Item{"m": {Type: attr.TypeM, M: map[string]attr.Value{
			"x": {Type: attr.TypeN, N: "1"},
		}}}},
		{"string-set", `INSERT INTO t VALUE {'ss': <<'a', 'b'>>}`, attr.Item{"ss": {Type: attr.TypeSS, SS: []string{"a", "b"}}}},
		{"number-set", `INSERT INTO t VALUE {'ns': <<1, 2>>}`, attr.Item{"ns": {Type: attr.TypeNS, NS: []string{"1", "2"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustParse(t, c.stmt)
			ins := s.(InsertPlan)
			if !reflect.DeepEqual(ins.Item, c.item) {
				t.Errorf("Item = %v, want %v", ins.Item, c.item)
			}
		})
	}
}

func TestParams(t *testing.T) {
	params := []attr.Value{
		{Type: attr.TypeS, S: "x"},
		{Type: attr.TypeN, N: "5"},
	}
	s := mustParse(t, `SELECT * FROM t WHERE a = ? AND b = ?`, params...)
	sel := s.(SelectPlan)
	and := sel.Where.(expr.And)
	l := and.L.(expr.Compare)
	if v := operValue(t, sel.Values, l.R); !reflect.DeepEqual(v, params[0]) {
		t.Errorf("first param = %v, want %v", v, params[0])
	}
	if len(sel.Values) != 2 {
		t.Errorf("len(Values) = %d, want 2", len(sel.Values))
	}
	if got := s.String(); got != `SELECT * FROM t WHERE ((a = 'x') AND (b = 5))` {
		t.Errorf("String() = %q", got)
	}
}

func TestParamCountErrors(t *testing.T) {
	_, err := Parse(`SELECT * FROM t WHERE a = ?`, nil)
	wantErr(t, err, "ValidationException", "Statement wasn't well formed, can't be processed: Not enough parameters: statement requires 1 but only 0 were provided")

	_, err = Parse(`SELECT * FROM t`, []attr.Value{{Type: attr.TypeS, S: "x"}})
	wantErr(t, err, "ValidationException", "Statement wasn't well formed, can't be processed: Too many parameters: statement requires 0 but 1 were provided")
}

func TestConditions(t *testing.T) {
	cases := []struct {
		name string
		stmt string
		want string
	}{
		{"ne", `SELECT * FROM t WHERE a <> 1`, `SELECT * FROM t WHERE (a <> 1)`},
		{"lt", `SELECT * FROM t WHERE a < 1`, `SELECT * FROM t WHERE (a < 1)`},
		{"ge", `SELECT * FROM t WHERE a >= 1`, `SELECT * FROM t WHERE (a >= 1)`},
		{"between", `SELECT * FROM t WHERE a BETWEEN 1 AND 9`, `SELECT * FROM t WHERE (a BETWEEN 1 AND 9)`},
		{"in-paren", `SELECT * FROM t WHERE a IN (1, 2)`, `SELECT * FROM t WHERE (a IN (1, 2))`},
		{"in-list", `SELECT * FROM t WHERE a IN [1, 2]`, `SELECT * FROM t WHERE (a IN (1, 2))`},
		{"and-or", `SELECT * FROM t WHERE a = 1 AND b = 2 OR c = 3`, `SELECT * FROM t WHERE (((a = 1) AND (b = 2)) OR (c = 3))`},
		{"not", `SELECT * FROM t WHERE NOT a = 1`, `SELECT * FROM t WHERE (NOT (a = 1))`},
		{"paren", `SELECT * FROM t WHERE a = 1 AND (b = 2 OR c = 3)`, `SELECT * FROM t WHERE ((a = 1) AND ((b = 2) OR (c = 3)))`},
		{"begins_with", `SELECT * FROM t WHERE begins_with(a, 'x')`, `SELECT * FROM t WHERE (begins_with(a, 'x'))`},
		{"contains", `SELECT * FROM t WHERE contains(a, 'x')`, `SELECT * FROM t WHERE (contains(a, 'x'))`},
		{"attribute_type", `SELECT * FROM t WHERE attribute_type(a, 'S')`, `SELECT * FROM t WHERE (attribute_type(a, 'S'))`},
		{"size", `SELECT * FROM t WHERE size(a) = 0`, `SELECT * FROM t WHERE (size(a) = 0)`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustParse(t, c.stmt)
			if got := s.String(); got != c.want {
				t.Errorf("String() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestIsForms(t *testing.T) {
	cases := []struct {
		name string
		stmt string
		want string
	}{
		{"missing", `SELECT * FROM t WHERE a IS MISSING`, `SELECT * FROM t WHERE (attribute_not_exists(a))`},
		{"not-missing", `SELECT * FROM t WHERE a IS NOT MISSING`, `SELECT * FROM t WHERE (attribute_exists(a))`},
		{"null", `SELECT * FROM t WHERE a IS NULL`, `SELECT * FROM t WHERE (attribute_type(a, 'NULL'))`},
		{"not-null", `SELECT * FROM t WHERE a IS NOT NULL`, `SELECT * FROM t WHERE ((attribute_exists(a)) AND (NOT (attribute_type(a, 'NULL'))))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustParse(t, c.stmt)
			if got := s.String(); got != c.want {
				t.Errorf("String() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestIdentifiers(t *testing.T) {
	// Double quotes allow reserved words and special characters.
	s := mustParse(t, `SELECT * FROM "select" WHERE "value" = 1`)
	sel := s.(SelectPlan)
	if sel.Table != "select" {
		t.Errorf("Table = %q", sel.Table)
	}
	cmp := sel.Where.(expr.Compare)
	if got := cmp.L.(expr.PathOperand).Path[0].Name; got != "value" {
		t.Errorf("attribute = %q", got)
	}
	// A double-quoted identifier containing a double quote.
	s = mustParse(t, `SELECT * FROM t WHERE "a""b" = 1`)
	sel = s.(SelectPlan)
	cmp = sel.Where.(expr.Compare)
	if got := cmp.L.(expr.PathOperand).Path[0].Name; got != `a"b` {
		t.Errorf("attribute = %q", got)
	}
}

func TestErrors(t *testing.T) {
	long := "SELECT * FROM t WHERE a = '" + strings.Repeat("x", 8192) + "'"
	cases := []struct {
		name string
		stmt string
		msg  string
	}{
		{"empty", ``, "Statement wasn't well formed, can't be processed: Unexpected from source: <EOF>"},
		{"unexpected-token", `SELECT FROM t`, "Statement wasn't well formed, can't be processed: Unexpected from source: FROM"},
		{"unsupported", `CREATE TABLE t`, "Statement wasn't well formed, can't be processed: Unsupported operation: CREATE"},
		{"too-long", long, "1 validation error detected: Value at 'statement' failed to satisfy constraint: Member must have length less than or equal to 8192"},
		{"empty-set", `INSERT INTO t VALUE {'s': <<>>}`, "Statement wasn't well formed, can't be processed: A set cannot be empty"},
		{"mixed-set", `INSERT INTO t VALUE {'s': <<1, 'a'>>}`, "Statement wasn't well formed, can't be processed: Set elements must be of the same type"},
		{"dup-set", `INSERT INTO t VALUE {'s': <<1, 1>>}`, "Statement wasn't well formed, can't be processed: A set cannot contain duplicate elements"},
		{"negative-index", `SELECT * FROM t WHERE a[-1] = 1`, "Statement wasn't well formed, can't be processed: Unexpected from source: -1"},
		{"unterminated", `SELECT * FROM t WHERE a = 'x`, "Statement wasn't well formed, can't be processed: Unterminated string literal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.stmt, nil)
			wantErr(t, err, "ValidationException", c.msg)
		})
	}
}

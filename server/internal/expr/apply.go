package expr

import (
	"sort"

	"github.com/cockroachdb/apd/v3"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

// Apply returns a new item with u applied to item (nil means no item yet), and
// the paths that SET, ADD, DELETE or REMOVE touched, in expression order.
// item is not modified.
func Apply(u Update, item attr.Item, values map[string]attr.Value) (attr.Item, []Path, error) {
	out := cloneItem(item)

	// SET right-hand sides read the item as it was before the update, so
	// evaluate them all first, against the original item.
	setVals := make([]attr.Value, len(u.Set))
	for i, a := range u.Set {
		v, err := evalSetValue(a.Value, item, values)
		if err != nil {
			return nil, nil, err
		}
		setVals[i] = v
	}

	var touched []Path
	for i, a := range u.Set {
		if err := setPath(out, a.Path, setVals[i]); err != nil {
			return nil, nil, err
		}
		touched = append(touched, a.Path)
	}

	// Removing a list element shifts the rest, so apply the list indexes in
	// one REMOVE highest first; the paths are reported in expression order.
	removeOrder := append([]Path(nil), u.Remove...)
	sort.SliceStable(removeOrder, func(i, j int) bool {
		return lastListIndex(removeOrder[i]) > lastListIndex(removeOrder[j])
	})
	for _, p := range removeOrder {
		removePath(out, p)
	}
	touched = append(touched, u.Remove...)

	for _, a := range u.Add {
		v, err := valueAt(values, a.Value)
		if err != nil {
			return nil, nil, err
		}
		if err := addPath(out, a.Path, v); err != nil {
			return nil, nil, err
		}
		touched = append(touched, a.Path)
	}

	for _, a := range u.Delete {
		v, err := valueAt(values, a.Value)
		if err != nil {
			return nil, nil, err
		}
		if err := deletePath(out, a.Path, v); err != nil {
			return nil, nil, err
		}
		touched = append(touched, a.Path)
	}

	return out, touched, nil
}

func wrongType(op string, t attr.Type) *ddberr.Error {
	return ddberr.Validation("Invalid UpdateExpression: Incorrect operand type for operator or function; operator: %s, operand type: %s", op, t.String())
}

func invalidPath() *ddberr.Error {
	return ddberr.Validation("The document path provided in the update expression is invalid for update")
}

func pathMissing() *ddberr.Error {
	return ddberr.Validation("The provided expression refers to an attribute that does not exist in the item")
}

// valueAt looks up an ExpressionAttributeValue placeholder.
func valueAt(values map[string]attr.Value, name string) (attr.Value, error) {
	v, ok := values[name]
	if !ok {
		return attr.Value{}, ddberr.Validation("Invalid UpdateExpression: An expression attribute value used in expression is not defined; attribute value: %s", name)
	}
	return v, nil
}

// evalSetValue evaluates one SET right-hand side against item.
func evalSetValue(v SetValue, item attr.Item, values map[string]attr.Value) (attr.Value, error) {
	switch v := v.(type) {
	case SetOperand:
		return updateOperand(v.O, item, values)
	case SetArith:
		l, err := evalSetValue(v.L, item, values)
		if err != nil {
			return attr.Value{}, err
		}
		r, err := evalSetValue(v.R, item, values)
		if err != nil {
			return attr.Value{}, err
		}
		return arith(v.Op, l, r)
	case IfNotExists:
		cur, found, err := getPath(item, v.Path)
		if err != nil {
			return attr.Value{}, err
		}
		if found {
			return cur, nil
		}
		return evalSetValue(v.Default, item, values)
	case ListAppend:
		l, err := evalSetValue(v.A, item, values)
		if err != nil {
			return attr.Value{}, err
		}
		r, err := evalSetValue(v.B, item, values)
		if err != nil {
			return attr.Value{}, err
		}
		return listAppend(l, r)
	}
	return attr.Value{}, nil
}

// updateOperand reads a SET operand; unlike a condition, a missing path is an error.
func updateOperand(o Operand, item attr.Item, values map[string]attr.Value) (attr.Value, error) {
	switch o := o.(type) {
	case ValueOperand:
		return valueAt(values, o.Name)
	case PathOperand:
		v, found, err := getPath(item, o.Path)
		if err != nil {
			return attr.Value{}, err
		}
		if !found {
			return attr.Value{}, pathMissing()
		}
		return v, nil
	}
	return attr.Value{}, nil
}

// arith adds or subtracts two numbers. The inputs are already canonical, so
// apd computes exactly; NormalizeNumber then enforces DynamoDB's 38-digit and
// exponent limits.
func arith(op string, a, b attr.Value) (attr.Value, error) {
	if a.Type != attr.TypeN {
		return attr.Value{}, wrongType(op, a.Type)
	}
	if b.Type != attr.TypeN {
		return attr.Value{}, wrongType(op, b.Type)
	}
	var da, db, res apd.Decimal
	_, _, _ = da.SetString(a.N)
	_, _, _ = db.SetString(b.N)
	var err error
	if op == "+" {
		_, err = apd.BaseContext.Add(&res, &da, &db)
	} else {
		_, err = apd.BaseContext.Sub(&res, &da, &db)
	}
	if err != nil {
		// Unreachable for canonical DynamoDB numbers; apd's exponent range is
		// far wider. Report the same error attr would.
		return attr.Value{}, ddberr.Validation("Number overflow. Attempting to store a number with magnitude larger than supported range")
	}
	n, err := attr.NormalizeNumber(res.Text('f'))
	if err != nil {
		return attr.Value{}, err
	}
	return attr.Value{Type: attr.TypeN, N: n}, nil
}

func listAppend(a, b attr.Value) (attr.Value, error) {
	if a.Type != attr.TypeL {
		return attr.Value{}, wrongType("list_append", a.Type)
	}
	if b.Type != attr.TypeL {
		return attr.Value{}, wrongType("list_append", b.Type)
	}
	out := make([]attr.Value, 0, len(a.L)+len(b.L))
	out = append(out, a.L...)
	out = append(out, b.L...)
	return attr.Value{Type: attr.TypeL, L: out}, nil
}

// getPath resolves p in item. found is false when any element is missing;
// a wrongly typed container along the way is an error rather than "missing".
func getPath(item attr.Item, p Path) (attr.Value, bool, error) {
	v, ok := item[p[0].Name]
	if !ok {
		return attr.Value{}, false, nil
	}
	for _, e := range p[1:] {
		if e.IsIndex {
			if v.Type != attr.TypeL {
				return attr.Value{}, false, invalidPath()
			}
			if e.Index >= len(v.L) {
				return attr.Value{}, false, nil
			}
			v = v.L[e.Index]
		} else {
			if v.Type != attr.TypeM {
				return attr.Value{}, false, invalidPath()
			}
			mv, ok := v.M[e.Name]
			if !ok {
				return attr.Value{}, false, nil
			}
			v = mv
		}
	}
	return v, true, nil
}

// setPath writes val at p, creating or replacing the final element. Missing
// intermediate maps are an error; l[i] past the end appends to the list.
func setPath(item attr.Item, p Path, val attr.Value) error {
	if len(p) == 1 {
		item[p[0].Name] = val
		return nil
	}
	child, ok := item[p[0].Name]
	if !ok {
		return invalidPath()
	}
	if err := setAt(&child, p[1:], val); err != nil {
		return err
	}
	item[p[0].Name] = child
	return nil
}

func setAt(v *attr.Value, p Path, val attr.Value) error {
	e := p[0]
	if e.IsIndex {
		if v.Type != attr.TypeL {
			return invalidPath()
		}
		if len(p) == 1 {
			if e.Index < len(v.L) {
				v.L[e.Index] = val
			} else {
				v.L = append(v.L, val)
			}
			return nil
		}
		if e.Index >= len(v.L) {
			return invalidPath()
		}
		return setAt(&v.L[e.Index], p[1:], val)
	}
	if v.Type != attr.TypeM {
		return invalidPath()
	}
	if len(p) == 1 {
		v.M[e.Name] = val
		return nil
	}
	child, ok := v.M[e.Name]
	if !ok {
		return invalidPath()
	}
	if err := setAt(&child, p[1:], val); err != nil {
		return err
	}
	v.M[e.Name] = child
	return nil
}

// removePath deletes p. A missing path is not an error; removing a list
// element shifts the rest down.
func removePath(item attr.Item, p Path) {
	if len(p) == 1 {
		delete(item, p[0].Name)
		return
	}
	child, ok := item[p[0].Name]
	if !ok {
		return
	}
	removeAt(&child, p[1:])
	item[p[0].Name] = child
}

func removeAt(v *attr.Value, p Path) {
	e := p[0]
	if e.IsIndex {
		if v.Type != attr.TypeL {
			return
		}
		if len(p) == 1 {
			if e.Index < len(v.L) {
				v.L = append(v.L[:e.Index], v.L[e.Index+1:]...)
			}
			return
		}
		if e.Index >= len(v.L) {
			return
		}
		removeAt(&v.L[e.Index], p[1:])
		return
	}
	if v.Type != attr.TypeM {
		return
	}
	if len(p) == 1 {
		delete(v.M, e.Name)
		return
	}
	child, ok := v.M[e.Name]
	if !ok {
		return
	}
	removeAt(&child, p[1:])
	v.M[e.Name] = child
}

// lastListIndex is the index a removal targets, or -1 when it targets a map
// key (or attribute) rather than a list element.
func lastListIndex(p Path) int {
	if n := len(p); n > 0 && p[n-1].IsIndex {
		return p[n-1].Index
	}
	return -1
}

// addPath applies an ADD action: N adds (missing means 0), a set unions its
// members (missing means empty), anything else is a wrong-type error.
func addPath(item attr.Item, p Path, v attr.Value) error {
	switch v.Type {
	case attr.TypeN:
		cur, found, err := getPath(item, p)
		if err != nil {
			return err
		}
		if !found {
			return setPath(item, p, v)
		}
		if cur.Type != attr.TypeN {
			return wrongType("ADD", cur.Type)
		}
		sum, err := arith("+", cur, v)
		if err != nil {
			return err
		}
		return setPath(item, p, sum)
	case attr.TypeSS, attr.TypeNS, attr.TypeBS:
		cur, found, err := getPath(item, p)
		if err != nil {
			return err
		}
		if !found {
			return setPath(item, p, v)
		}
		if cur.Type != v.Type {
			return wrongType("ADD", cur.Type)
		}
		return setPath(item, p, setUnion(v.Type, cur, v))
	default:
		return wrongType("ADD", v.Type)
	}
}

// deletePath applies a DELETE action: removes members from a set of the same
// type; an emptied set removes the attribute; a missing attribute is a no-op.
func deletePath(item attr.Item, p Path, v attr.Value) error {
	if v.Type != attr.TypeSS && v.Type != attr.TypeNS && v.Type != attr.TypeBS {
		return wrongType("DELETE", v.Type)
	}
	cur, found, err := getPath(item, p)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if cur.Type != v.Type {
		return wrongType("DELETE", cur.Type)
	}
	diff := setDifference(v.Type, cur, v)
	if setEmpty(diff) {
		removePath(item, p)
		return nil
	}
	return setPath(item, p, diff)
}

func setUnion(t attr.Type, a, b attr.Value) attr.Value {
	switch t {
	case attr.TypeSS:
		out := append([]string(nil), a.SS...)
		seen := make(map[string]bool, len(out))
		for _, s := range a.SS {
			seen[s] = true
		}
		for _, s := range b.SS {
			if !seen[s] {
				out = append(out, s)
				seen[s] = true
			}
		}
		return attr.Value{Type: t, SS: out}
	case attr.TypeNS:
		out := append([]string(nil), a.NS...)
		seen := make(map[string]bool, len(out))
		for _, s := range a.NS {
			seen[s] = true
		}
		for _, s := range b.NS {
			if !seen[s] {
				out = append(out, s)
				seen[s] = true
			}
		}
		return attr.Value{Type: t, NS: out}
	case attr.TypeBS:
		out := append([][]byte(nil), a.BS...)
		seen := make(map[string]bool, len(out))
		for _, s := range a.BS {
			seen[string(s)] = true
		}
		for _, s := range b.BS {
			if !seen[string(s)] {
				out = append(out, s)
				seen[string(s)] = true
			}
		}
		return attr.Value{Type: t, BS: out}
	}
	return attr.Value{}
}

func setDifference(t attr.Type, a, b attr.Value) attr.Value {
	switch t {
	case attr.TypeSS:
		exclude := make(map[string]bool, len(b.SS))
		for _, s := range b.SS {
			exclude[s] = true
		}
		out := make([]string, 0, len(a.SS))
		for _, s := range a.SS {
			if !exclude[s] {
				out = append(out, s)
			}
		}
		return attr.Value{Type: t, SS: out}
	case attr.TypeNS:
		exclude := make(map[string]bool, len(b.NS))
		for _, s := range b.NS {
			exclude[s] = true
		}
		out := make([]string, 0, len(a.NS))
		for _, s := range a.NS {
			if !exclude[s] {
				out = append(out, s)
			}
		}
		return attr.Value{Type: t, NS: out}
	case attr.TypeBS:
		exclude := make(map[string]bool, len(b.BS))
		for _, s := range b.BS {
			exclude[string(s)] = true
		}
		out := make([][]byte, 0, len(a.BS))
		for _, s := range a.BS {
			if !exclude[string(s)] {
				out = append(out, s)
			}
		}
		return attr.Value{Type: t, BS: out}
	}
	return attr.Value{}
}

func setEmpty(v attr.Value) bool {
	switch v.Type {
	case attr.TypeSS:
		return len(v.SS) == 0
	case attr.TypeNS:
		return len(v.NS) == 0
	case attr.TypeBS:
		return len(v.BS) == 0
	}
	return true
}

// cloneItem deep-copies an item so Apply never mutates its input.
func cloneItem(item attr.Item) attr.Item {
	out := make(attr.Item, len(item))
	for k, v := range item {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v attr.Value) attr.Value {
	switch v.Type {
	case attr.TypeM:
		m := make(map[string]attr.Value, len(v.M))
		for k, mv := range v.M {
			m[k] = cloneValue(mv)
		}
		v.M = m
	case attr.TypeL:
		l := make([]attr.Value, len(v.L))
		for i, lv := range v.L {
			l[i] = cloneValue(lv)
		}
		v.L = l
	case attr.TypeSS:
		v.SS = append([]string(nil), v.SS...)
	case attr.TypeNS:
		v.NS = append([]string(nil), v.NS...)
	case attr.TypeBS:
		bs := make([][]byte, len(v.BS))
		for i, b := range v.BS {
			bs[i] = append([]byte(nil), b...)
		}
		v.BS = bs
	case attr.TypeB:
		v.B = append([]byte(nil), v.B...)
	}
	return v
}

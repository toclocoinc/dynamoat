package expr

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

// Resolve returns the value at path in item (nested maps and list indexes).
func Resolve(item attr.Item, p Path) (attr.Value, bool) {
	if len(p) == 0 {
		return attr.Value{}, false
	}
	v, ok := item[p[0].Name]
	if !ok {
		return attr.Value{}, false
	}
	for _, e := range p[1:] {
		if e.IsIndex {
			if v.Type != attr.TypeL || e.Index < 0 || e.Index >= len(v.L) {
				return attr.Value{}, false
			}
			v = v.L[e.Index]
			continue
		}
		if v.Type != attr.TypeM {
			return attr.Value{}, false
		}
		v, ok = v.M[e.Name]
		if !ok {
			return attr.Value{}, false
		}
	}
	return v, true
}

// Eval reports whether item satisfies c. values is ExpressionAttributeValues.
// param is the request parameter for messages ("ConditionExpression",
// "FilterExpression"). item may be nil (no current item: every path is missing).
func Eval(c Cond, item attr.Item, values map[string]attr.Value, param string) (bool, error) {
	return evalCond(c, item, values, param)
}

func evalCond(c Cond, item attr.Item, values map[string]attr.Value, param string) (bool, error) {
	switch c := c.(type) {
	case And:
		l, err := evalCond(c.L, item, values, param)
		if err != nil || !l {
			return l, err
		}
		return evalCond(c.R, item, values, param)
	case Or:
		l, err := evalCond(c.L, item, values, param)
		if err != nil || l {
			return l, err
		}
		return evalCond(c.R, item, values, param)
	case Not:
		b, err := evalCond(c.C, item, values, param)
		return !b, err
	case Compare:
		return evalCompare(c, item, values, param)
	case Between:
		return evalBetween(c, item, values, param)
	case In:
		return evalIn(c, item, values, param)
	case Func:
		return evalFunc(c, item, values, param)
	}
	return false, nil
}

// evalOperand resolves an operand to its value and whether it is present. A
// missing path and size of a non-sizeable type both read as "not present",
// which makes the surrounding comparison false.
func evalOperand(o Operand, item attr.Item, values map[string]attr.Value, param string) (attr.Value, bool, error) {
	switch o := o.(type) {
	case PathOperand:
		v, ok := Resolve(item, o.Path)
		return v, ok, nil
	case ValueOperand:
		v, ok := values[o.Name]
		if !ok {
			return attr.Value{}, false, fmt.Errorf("expr: %s: ExpressionAttributeValues missing %s", param, o.Name)
		}
		return v, true, nil
	case SizeOperand:
		v, ok := Resolve(item, o.Path)
		if !ok {
			return attr.Value{}, false, nil
		}
		n, ok := sizeOf(v)
		if !ok {
			return attr.Value{}, false, nil
		}
		return attr.Value{Type: attr.TypeN, N: strconv.Itoa(n)}, true, nil
	}
	return attr.Value{}, false, nil
}

func evalCompare(c Compare, item attr.Item, values map[string]attr.Value, param string) (bool, error) {
	l, lok, err := evalOperand(c.L, item, values, param)
	if err != nil {
		return false, err
	}
	r, rok, err := evalOperand(c.R, item, values, param)
	if err != nil {
		return false, err
	}
	if !lok || !rok {
		return false, nil
	}
	switch c.Op {
	case "=":
		return equal(l, r), nil
	case "<>":
		return !equal(l, r), nil
	}
	cmp, ok := compare(l, r)
	if !ok {
		return false, nil
	}
	switch c.Op {
	case "<":
		return cmp < 0, nil
	case "<=":
		return cmp <= 0, nil
	case ">":
		return cmp > 0, nil
	case ">=":
		return cmp >= 0, nil
	}
	return false, nil
}

func evalBetween(c Between, item attr.Item, values map[string]attr.Value, param string) (bool, error) {
	v, vok, err := evalOperand(c.V, item, values, param)
	if err != nil {
		return false, err
	}
	lo, look, err := evalOperand(c.Lo, item, values, param)
	if err != nil {
		return false, err
	}
	hi, hiok, err := evalOperand(c.Hi, item, values, param)
	if err != nil {
		return false, err
	}
	if look && hiok {
		if cmp, ok := compare(lo, hi); ok && cmp > 0 {
			return false, ddberr.Validation(
				"Invalid %s: The BETWEEN operator requires upper bound to be greater than or equal to lower bound; lower bound operand: AttributeValue: {%s:%s}, upper bound operand: AttributeValue: {%s:%s}",
				param, lo.Type.String(), valueText(lo), hi.Type.String(), valueText(hi))
		}
	}
	if !vok || !look || !hiok {
		return false, nil
	}
	loCmp, lok := compare(v, lo)
	hiCmp, hok := compare(v, hi)
	if !lok || !hok {
		return false, nil
	}
	return loCmp >= 0 && hiCmp <= 0, nil
}

func evalIn(c In, item attr.Item, values map[string]attr.Value, param string) (bool, error) {
	v, vok, err := evalOperand(c.V, item, values, param)
	if err != nil {
		return false, err
	}
	if !vok {
		return false, nil
	}
	for _, o := range c.List {
		e, eok, err := evalOperand(o, item, values, param)
		if err != nil {
			return false, err
		}
		if eok && equal(v, e) {
			return true, nil
		}
	}
	return false, nil
}

func evalFunc(f Func, item attr.Item, values map[string]attr.Value, param string) (bool, error) {
	switch f.Name {
	case "attribute_exists":
		_, ok := Resolve(item, f.Args[0].(PathOperand).Path)
		return ok, nil
	case "attribute_not_exists":
		_, ok := Resolve(item, f.Args[0].(PathOperand).Path)
		return !ok, nil
	case "attribute_type":
		t, _, err := evalOperand(f.Args[1], item, values, param)
		if err != nil {
			return false, err
		}
		name, err := attrTypeName(param, t)
		if err != nil {
			return false, err
		}
		v, ok := Resolve(item, f.Args[0].(PathOperand).Path)
		if !ok {
			return false, nil
		}
		return v.Type.String() == name, nil
	case "begins_with":
		return evalBeginsWith(f.Args[0], f.Args[1], item, values, param)
	case "contains":
		return evalContains(f.Args[0], f.Args[1], item, values, param)
	}
	return false, nil
}

func evalBeginsWith(p, x Operand, item attr.Item, values map[string]attr.Value, param string) (bool, error) {
	pv, pok, err := evalOperand(p, item, values, param)
	if err != nil {
		return false, err
	}
	if !pok {
		return false, nil
	}
	xv, xok, err := evalOperand(x, item, values, param)
	if err != nil {
		return false, err
	}
	if !xok {
		return false, nil
	}
	switch {
	case pv.Type == attr.TypeS && xv.Type == attr.TypeS:
		return strings.HasPrefix(pv.S, xv.S), nil
	case pv.Type == attr.TypeB && xv.Type == attr.TypeB:
		return bytes.HasPrefix(pv.B, xv.B), nil
	}
	return false, nil
}

func evalContains(p, x Operand, item attr.Item, values map[string]attr.Value, param string) (bool, error) {
	pv, pok, err := evalOperand(p, item, values, param)
	if err != nil {
		return false, err
	}
	if !pok {
		return false, nil
	}
	xv, xok, err := evalOperand(x, item, values, param)
	if err != nil {
		return false, err
	}
	if !xok {
		return false, nil
	}
	switch pv.Type {
	case attr.TypeS:
		return xv.Type == attr.TypeS && strings.Contains(pv.S, xv.S), nil
	case attr.TypeSS:
		return xv.Type == attr.TypeS && containsString(pv.SS, xv.S), nil
	case attr.TypeNS:
		return xv.Type == attr.TypeN && containsNumber(pv.NS, xv.N), nil
	case attr.TypeBS:
		return xv.Type == attr.TypeB && containsBytes(pv.BS, xv.B), nil
	case attr.TypeL:
		for _, e := range pv.L {
			if equal(e, xv) {
				return true, nil
			}
		}
	}
	return false, nil
}

// sizeOf is the size(path) operand: the length of S and B, and the element
// count of sets, L and M. Other types have no size, so the comparison is false.
func sizeOf(v attr.Value) (int, bool) {
	switch v.Type {
	case attr.TypeS:
		return len(v.S), true
	case attr.TypeB:
		return len(v.B), true
	case attr.TypeSS:
		return len(v.SS), true
	case attr.TypeNS:
		return len(v.NS), true
	case attr.TypeBS:
		return len(v.BS), true
	case attr.TypeL:
		return len(v.L), true
	case attr.TypeM:
		return len(v.M), true
	}
	return 0, false
}

// equal reports value equality. Different types are unequal; maps, lists and
// sets compare by value, and sets ignore order.
func equal(a, b attr.Value) bool {
	if a.Type != b.Type {
		return false
	}
	switch a.Type {
	case attr.TypeS:
		return a.S == b.S
	case attr.TypeN:
		return attr.CompareNumbers(a.N, b.N) == 0
	case attr.TypeB:
		return bytes.Equal(a.B, b.B)
	case attr.TypeBOOL:
		return a.Bool == b.Bool
	case attr.TypeNULL:
		return true
	case attr.TypeM:
		if len(a.M) != len(b.M) {
			return false
		}
		for k, av := range a.M {
			bv, ok := b.M[k]
			if !ok || !equal(av, bv) {
				return false
			}
		}
		return true
	case attr.TypeL:
		if len(a.L) != len(b.L) {
			return false
		}
		for i := range a.L {
			if !equal(a.L[i], b.L[i]) {
				return false
			}
		}
		return true
	case attr.TypeSS:
		return equalStrings(a.SS, b.SS)
	case attr.TypeNS:
		return equalNumbers(a.NS, b.NS)
	case attr.TypeBS:
		return equalBytes(a.BS, b.BS)
	}
	return false
}

// compare orders two values of the same N, S or B type. It is not ok for any
// other type, or for two values of different types, and those comparisons are
// false.
func compare(a, b attr.Value) (int, bool) {
	if a.Type != b.Type {
		return 0, false
	}
	switch a.Type {
	case attr.TypeN:
		return attr.CompareNumbers(a.N, b.N), true
	case attr.TypeS:
		return strings.Compare(a.S, b.S), true
	case attr.TypeB:
		return bytes.Compare(a.B, b.B), true
	}
	return 0, false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
	}
	return true
}

func equalNumbers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
	}
	return true
}

func equalBytes(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[string(s)] = true
	}
	for _, s := range b {
		if !seen[string(s)] {
			return false
		}
	}
	return true
}

func containsString(ss []string, s string) bool {
	for _, e := range ss {
		if e == s {
			return true
		}
	}
	return false
}

func containsNumber(ns []string, n string) bool {
	for _, e := range ns {
		if attr.CompareNumbers(e, n) == 0 {
			return true
		}
	}
	return false
}

func containsBytes(bs [][]byte, b []byte) bool {
	for _, e := range bs {
		if bytes.Equal(e, b) {
			return true
		}
	}
	return false
}

// attrTypeNames are the type names attribute_type accepts.
var attrTypeNames = map[string]bool{
	"S": true, "SS": true, "N": true, "NS": true, "B": true,
	"BS": true, "BOOL": true, "NULL": true, "L": true, "M": true,
}

const validAttrTypes = "{ B,NULL,SS,BOOL,L,BS,N,NS,S,M }"

// attrTypeName checks the :t operand of attribute_type: it must be an S naming
// one of the valid types.
func attrTypeName(param string, v attr.Value) (string, error) {
	if v.Type == attr.TypeS && attrTypeNames[v.S] {
		return v.S, nil
	}
	t := valueText(v)
	return "", ddberr.Validation("Invalid %s: Invalid attribute type name found; type: %s, valid types: %s", param, t, validAttrTypes)
}

// valueText renders a value for an error message: the bare value for S and N,
// base64 for B, and the single-letter type name for everything else.
func valueText(v attr.Value) string {
	switch v.Type {
	case attr.TypeS:
		return v.S
	case attr.TypeN:
		return v.N
	case attr.TypeB:
		return base64.StdEncoding.EncodeToString(v.B)
	case attr.TypeBOOL:
		return strconv.FormatBool(v.Bool)
	case attr.TypeNULL:
		return "true"
	case attr.TypeSS, attr.TypeNS, attr.TypeBS, attr.TypeL, attr.TypeM:
		return v.Type.String()
	}
	return ""
}

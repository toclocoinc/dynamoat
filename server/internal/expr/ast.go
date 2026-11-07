// Package expr parses DynamoDB's expression language (ConditionExpression,
// FilterExpression, KeyConditionExpression and ProjectionExpression) into an
// abstract syntax tree. Parsing only: evaluating a condition against an item
// is a separate card, and update expressions build on this lexer and path
// parser, so the AST below is the contract those cards consume.
package expr

import (
	"strconv"
	"strings"
)

// PathElem is one segment of a document path. For a named segment IsIndex is
// false and Name holds the (already resolved) attribute name; for an index
// segment IsIndex is true and Index holds the element position.
type PathElem struct {
	Name    string
	Index   int
	IsIndex bool
}

// Path is a document path such as a.b[2].c. Names are already resolved from
// #placeholders.
type Path []PathElem

// String renders the path as a.b[2].c, for messages and tests.
func (p Path) String() string {
	var b strings.Builder
	for i, e := range p {
		if i > 0 && !e.IsIndex {
			b.WriteByte('.')
		}
		if e.IsIndex {
			b.WriteByte('[')
			b.WriteString(strconv.Itoa(e.Index))
			b.WriteByte(']')
		} else {
			b.WriteString(e.Name)
		}
	}
	return b.String()
}

// Operand is a value the condition compares against: a path, a :value
// placeholder, or size(path).
type Operand interface{ operand() }

// PathOperand is a document path used as an operand.
type PathOperand struct{ Path Path }

// ValueOperand is an ExpressionAttributeValue placeholder such as ":v".
type ValueOperand struct{ Name string }

// SizeOperand is the size(path) function.
type SizeOperand struct{ Path Path }

func (PathOperand) operand()  {}
func (ValueOperand) operand() {}
func (SizeOperand) operand()  {}

// Cond is a boolean condition.
type Cond interface{ cond() }

// Compare is `operand comparator operand`. Op is one of = <> < <= > >=.
type Compare struct {
	Op   string
	L, R Operand
}

// Between is `operand BETWEEN operand AND operand`.
type Between struct {
	V, Lo, Hi Operand
}

// In is `operand IN (operand, ...)`. List has 1 to 100 entries.
type In struct {
	V    Operand
	List []Operand
}

// And is `condition AND condition`.
type And struct {
	L, R Cond
}

// Or is `condition OR condition`.
type Or struct {
	L, R Cond
}

// Not is `NOT condition`.
type Not struct{ C Cond }

// Func is a condition function: attribute_exists, attribute_not_exists,
// attribute_type, begins_with or contains.
type Func struct {
	Name string
	Args []Operand
}

func (Compare) cond() {}
func (Between) cond() {}
func (In) cond()      {}
func (And) cond()     {}
func (Or) cond()      {}
func (Not) cond()     {}
func (Func) cond()    {}

// KeyCond is a parsed KeyConditionExpression. PK is the partition key
// condition (always `pk = :v`); SK is the optional sort key condition.
type KeyCond struct {
	PK       string   // partition key attribute name
	PKValue  string   // ":v"
	SK       string   // "" when there is no sort key condition
	SKOp     string   // < <= > >= BETWEEN begins_with
	SKValues []string // one placeholder, or two for BETWEEN
}

// String renders a condition fully parenthesised, for tests.
func String(c Cond) string {
	switch c := c.(type) {
	case Compare:
		return "(" + operandString(c.L) + " " + c.Op + " " + operandString(c.R) + ")"
	case Between:
		return "(" + operandString(c.V) + " BETWEEN " + operandString(c.Lo) + " AND " + operandString(c.Hi) + ")"
	case In:
		parts := make([]string, len(c.List))
		for i, o := range c.List {
			parts[i] = operandString(o)
		}
		return "(" + operandString(c.V) + " IN (" + strings.Join(parts, ", ") + "))"
	case And:
		return "(" + String(c.L) + " AND " + String(c.R) + ")"
	case Or:
		return "(" + String(c.L) + " OR " + String(c.R) + ")"
	case Not:
		return "(NOT " + String(c.C) + ")"
	case Func:
		parts := make([]string, len(c.Args))
		for i, o := range c.Args {
			parts[i] = operandString(o)
		}
		return "(" + c.Name + "(" + strings.Join(parts, ", ") + "))"
	}
	return ""
}

func operandString(o Operand) string {
	switch o := o.(type) {
	case PathOperand:
		return o.Path.String()
	case ValueOperand:
		return o.Name
	case SizeOperand:
		return "size(" + o.Path.String() + ")"
	}
	return ""
}

// Package partiql parses the PartiQL subset DynamoDB accepts in
// ExecuteStatement and ExecuteTransaction into plans. Parsing only: the
// operations card turns these plans into store calls. Conditions and updates
// are built as expr AST nodes so the existing evaluator runs them; literal
// values become expr.ValueOperand placeholders whose values are returned in
// each plan's Values map.
package partiql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/expr"
)

// maxStatementLen is DynamoDB's limit on an ExecuteStatement statement.
const maxStatementLen = 8192

// Statement is a parsed PartiQL statement.
type Statement interface {
	String() string
	statement()
}

// SelectPlan is a parsed SELECT.
type SelectPlan struct {
	Table      string
	Index      string // "" for the base table
	Projection []expr.Path
	Where      expr.Cond
	Values     map[string]attr.Value
}

// InsertPlan is a parsed INSERT.
type InsertPlan struct {
	Table string
	Item  attr.Item
}

// UpdatePlan is a parsed UPDATE.
type UpdatePlan struct {
	Table  string
	Where  expr.Cond
	Update expr.Update
	Values map[string]attr.Value
}

// DeletePlan is a parsed DELETE.
type DeletePlan struct {
	Table  string
	Where  expr.Cond
	Values map[string]attr.Value
}

// ExistsPlan marks EXISTS(SELECT ...), valid only as an ExecuteTransaction
// condition.
type ExistsPlan struct {
	Select SelectPlan
}

func (SelectPlan) statement() {}
func (InsertPlan) statement() {}
func (UpdatePlan) statement() {}
func (DeletePlan) statement() {}
func (ExistsPlan) statement() {}

// Parse parses a single PartiQL statement. params supply the ? placeholders in
// order; the count must match exactly.
func Parse(stmt string, params []attr.Value) (Statement, error) {
	if len(stmt) > maxStatementLen {
		return nil, statementTooLong()
	}
	toks, lexErr := lex(stmt)
	if lexErr != nil {
		return nil, lexErr
	}
	p := &parser{toks: toks, params: params, values: map[string]attr.Value{}}
	for _, t := range toks {
		if t.typ == tParam {
			p.paramCount++
		}
	}

	var (
		s   Statement
		err error
	)
	switch p.cur().typ {
	case tSelect:
		s, err = p.parseSelectStmt()
	case tInsert:
		s, err = p.parseInsertStmt()
	case tUpdate:
		s, err = p.parseUpdateStmt()
	case tDelete:
		s, err = p.parseDeleteStmt()
	case tExists:
		s, err = p.parseExistsStmt()
	default:
		if p.cur().typ == tIdent {
			switch strings.ToLower(p.cur().text) {
			case "create", "drop", "alter", "explain":
				return nil, notWellFormed("Unsupported operation: " + p.cur().text)
			}
		}
		return nil, p.unexpected()
	}
	if err != nil {
		return nil, err
	}
	if p.paramIdx != len(p.params) {
		return nil, notWellFormed(fmt.Sprintf("Too many parameters: statement requires %d but %d were provided", p.paramCount, len(p.params)))
	}
	return s, nil
}

// notWellFormed wraps a detail in DynamoDB's PartiQL error envelope.
func notWellFormed(detail string) *ddberr.Error {
	return ddberr.Validation("Statement wasn't well formed, can't be processed: %s", detail)
}

func statementTooLong() *ddberr.Error {
	return ddberr.Validation("1 validation error detected: Value at 'statement' failed to satisfy constraint: Member must have length less than or equal to 8192")
}

// String renders a SELECT back to PartiQL, canonicalising values and quoting.
func (p SelectPlan) String() string {
	var b strings.Builder
	b.WriteString("SELECT ")
	if p.Projection == nil {
		b.WriteString("*")
	} else {
		parts := make([]string, len(p.Projection))
		for i, path := range p.Projection {
			parts[i] = renderPath(path)
		}
		b.WriteString(strings.Join(parts, ", "))
	}
	b.WriteString(" FROM ")
	b.WriteString(renderIdent(p.Table))
	if p.Index != "" {
		b.WriteString(".")
		b.WriteString(renderIdent(p.Index))
	}
	if p.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(renderCond(p.Where, p.Values))
	}
	return b.String()
}

func (p InsertPlan) String() string {
	return "INSERT INTO " + renderIdent(p.Table) + " VALUE " +
		renderValue(attr.Value{Type: attr.TypeM, M: map[string]attr.Value(p.Item)})
}

func (p UpdatePlan) String() string {
	var b strings.Builder
	b.WriteString("UPDATE ")
	b.WriteString(renderIdent(p.Table))
	b.WriteString(" SET ")
	parts := make([]string, 0, len(p.Update.Set)+len(p.Update.Add)+len(p.Update.Delete))
	for _, a := range p.Update.Set {
		parts = append(parts, renderPath(a.Path)+" = "+renderSetValue(a.Value, p.Values))
	}
	for _, a := range p.Update.Add {
		parts = append(parts, renderPath(a.Path)+" = set_add("+renderPath(a.Path)+", "+renderValue(p.Values[a.Value])+")")
	}
	for _, a := range p.Update.Delete {
		parts = append(parts, renderPath(a.Path)+" = set_delete("+renderPath(a.Path)+", "+renderValue(p.Values[a.Value])+")")
	}
	b.WriteString(strings.Join(parts, ", "))
	if len(p.Update.Remove) > 0 {
		rp := make([]string, len(p.Update.Remove))
		for i, path := range p.Update.Remove {
			rp[i] = renderPath(path)
		}
		b.WriteString(" REMOVE ")
		b.WriteString(strings.Join(rp, ", "))
	}
	b.WriteString(" WHERE ")
	b.WriteString(renderCond(p.Where, p.Values))
	return b.String()
}

func (p DeletePlan) String() string {
	return "DELETE FROM " + renderIdent(p.Table) + " WHERE " + renderCond(p.Where, p.Values)
}

func (p ExistsPlan) String() string {
	return "EXISTS(" + p.Select.String() + ")"
}

// renderIdent renders a name bare when it needs no quoting, else double-quoted
// with "" as the escape.
func renderIdent(name string) string {
	if isBareIdent(name) && !keywords[strings.ToLower(name)] {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func isBareIdent(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if isNameStart(c) || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

func renderString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// renderValue renders a value as a PartiQL literal. Binary has no literal, so
// B and BS render as a placeholder marker that does not round-trip.
func renderValue(v attr.Value) string {
	switch v.Type {
	case attr.TypeS:
		return renderString(v.S)
	case attr.TypeN:
		return v.N
	case attr.TypeBOOL:
		if v.Bool {
			return "true"
		}
		return "false"
	case attr.TypeNULL:
		return "null"
	case attr.TypeM:
		keys := make([]string, 0, len(v.M))
		for k := range v.M {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = renderIdent(k) + ": " + renderValue(v.M[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case attr.TypeL:
		parts := make([]string, len(v.L))
		for i, e := range v.L {
			parts[i] = renderValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case attr.TypeSS:
		parts := make([]string, len(v.SS))
		for i, s := range v.SS {
			parts[i] = renderString(s)
		}
		return "<<" + strings.Join(parts, ", ") + ">>"
	case attr.TypeNS:
		parts := make([]string, len(v.NS))
		for i, n := range v.NS {
			parts[i] = n
		}
		return "<<" + strings.Join(parts, ", ") + ">>"
	case attr.TypeB:
		return "<binary>"
	case attr.TypeBS:
		return "<binary set>"
	}
	return ""
}

func renderPath(p expr.Path) string {
	var b strings.Builder
	for i, e := range p {
		if i > 0 && !e.IsIndex {
			b.WriteByte('.')
		}
		if e.IsIndex {
			fmt.Fprintf(&b, "[%d]", e.Index)
		} else {
			b.WriteString(renderIdent(e.Name))
		}
	}
	return b.String()
}

func renderCond(c expr.Cond, values map[string]attr.Value) string {
	switch c := c.(type) {
	case expr.Compare:
		return "(" + renderOperand(c.L, values) + " " + c.Op + " " + renderOperand(c.R, values) + ")"
	case expr.Between:
		return "(" + renderOperand(c.V, values) + " BETWEEN " + renderOperand(c.Lo, values) + " AND " + renderOperand(c.Hi, values) + ")"
	case expr.In:
		parts := make([]string, len(c.List))
		for i, o := range c.List {
			parts[i] = renderOperand(o, values)
		}
		return "(" + renderOperand(c.V, values) + " IN (" + strings.Join(parts, ", ") + "))"
	case expr.And:
		return "(" + renderCond(c.L, values) + " AND " + renderCond(c.R, values) + ")"
	case expr.Or:
		return "(" + renderCond(c.L, values) + " OR " + renderCond(c.R, values) + ")"
	case expr.Not:
		return "(NOT " + renderCond(c.C, values) + ")"
	case expr.Func:
		parts := make([]string, len(c.Args))
		for i, o := range c.Args {
			parts[i] = renderOperand(o, values)
		}
		return "(" + c.Name + "(" + strings.Join(parts, ", ") + "))"
	}
	return ""
}

func renderOperand(o expr.Operand, values map[string]attr.Value) string {
	switch o := o.(type) {
	case expr.PathOperand:
		return renderPath(o.Path)
	case expr.ValueOperand:
		return renderValue(values[o.Name])
	case expr.SizeOperand:
		return "size(" + renderPath(o.Path) + ")"
	}
	return ""
}

func renderSetValue(v expr.SetValue, values map[string]attr.Value) string {
	switch v := v.(type) {
	case expr.SetOperand:
		return renderOperand(v.O, values)
	case expr.SetArith:
		return renderSetValue(v.L, values) + " " + v.Op + " " + renderSetValue(v.R, values)
	case expr.IfNotExists:
		return "if_not_exists(" + renderPath(v.Path) + ", " + renderSetValue(v.Default, values) + ")"
	case expr.ListAppend:
		return "list_append(" + renderSetValue(v.A, values) + ", " + renderSetValue(v.B, values) + ")"
	}
	return ""
}

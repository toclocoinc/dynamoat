package partiql

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/expr"
)

// parser is a recursive-descent parser over the lexed tokens of one statement.
type parser struct {
	toks       []token
	pos        int
	params     []attr.Value
	paramIdx   int
	paramCount int // total ? in the statement
	values     map[string]attr.Value
	next       int // placeholder counter
}

func (p *parser) cur() token { return p.toks[p.pos] }

func (p *parser) advance() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *parser) peek() token {
	if p.pos+1 >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.pos+1]
}

// unexpected builds DynamoDB's PartiQL syntax error for the current token.
func (p *parser) unexpected() error {
	tok := p.cur().text
	if p.cur().typ == tEOF {
		tok = "<EOF>"
	}
	return notWellFormed("Unexpected from source: " + tok)
}

func (p *parser) expectEOF() error {
	if p.cur().typ != tEOF {
		return p.unexpected()
	}
	return nil
}

func (p *parser) nextPlaceholder() string {
	name := fmt.Sprintf(":p%d", p.next)
	p.next++
	return name
}

// valueOperand parses a value (literal or ? parameter) and returns it as an
// expr.ValueOperand, recording the value under a fresh placeholder name.
func (p *parser) valueOperand() (expr.Operand, error) {
	v, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	name := p.nextPlaceholder()
	p.values[name] = v
	return expr.ValueOperand{Name: name}, nil
}

// nullOperand returns the string "NULL" as a value operand, for IS NULL.
func (p *parser) nullOperand() expr.Operand {
	name := p.nextPlaceholder()
	p.values[name] = attr.Value{Type: attr.TypeS, S: "NULL"}
	return expr.ValueOperand{Name: name}
}

// parseValue parses one value: string, number, bool, null, map, list, set or a
// ? parameter.
func (p *parser) parseValue() (attr.Value, error) {
	switch p.cur().typ {
	case tString:
		v := p.cur().text
		p.advance()
		return attr.Value{Type: attr.TypeS, S: v}, nil
	case tNumber:
		raw := p.cur().text
		p.advance()
		n, err := attr.NormalizeNumber(raw)
		if err != nil {
			return attr.Value{}, err
		}
		return attr.Value{Type: attr.TypeN, N: n}, nil
	case tTrue:
		p.advance()
		return attr.Value{Type: attr.TypeBOOL, Bool: true}, nil
	case tFalse:
		p.advance()
		return attr.Value{Type: attr.TypeBOOL, Bool: false}, nil
	case tNull:
		p.advance()
		return attr.Value{Type: attr.TypeNULL}, nil
	case tParam:
		p.advance()
		if p.paramIdx >= len(p.params) {
			return attr.Value{}, notWellFormed(fmt.Sprintf("Not enough parameters: statement requires %d but only %d were provided", p.paramCount, len(p.params)))
		}
		v := p.params[p.paramIdx]
		p.paramIdx++
		return v, nil
	case tLBrace:
		return p.parseMap()
	case tLBracket:
		return p.parseList()
	case tLLAngle:
		return p.parseSet()
	}
	return attr.Value{}, p.unexpected()
}

func (p *parser) parseMap() (attr.Value, error) {
	p.advance() // {
	m := map[string]attr.Value{}
	if p.cur().typ == tRBrace {
		p.advance()
		return attr.Value{Type: attr.TypeM, M: m}, nil
	}
	for {
		key, err := p.parseMapKey()
		if err != nil {
			return attr.Value{}, err
		}
		if p.cur().typ != tColon {
			return attr.Value{}, p.unexpected()
		}
		p.advance()
		v, err := p.parseValue()
		if err != nil {
			return attr.Value{}, err
		}
		m[key] = v
		if p.cur().typ != tComma {
			break
		}
		p.advance()
	}
	if p.cur().typ != tRBrace {
		return attr.Value{}, p.unexpected()
	}
	p.advance()
	return attr.Value{Type: attr.TypeM, M: m}, nil
}

func (p *parser) parseMapKey() (string, error) {
	switch p.cur().typ {
	case tIdent, tQuotedIdent, tString:
		k := p.cur().text
		p.advance()
		return k, nil
	}
	return "", p.unexpected()
}

func (p *parser) parseList() (attr.Value, error) {
	p.advance() // [
	var l []attr.Value
	if p.cur().typ == tRBracket {
		p.advance()
		return attr.Value{Type: attr.TypeL, L: l}, nil
	}
	for {
		v, err := p.parseValue()
		if err != nil {
			return attr.Value{}, err
		}
		l = append(l, v)
		if p.cur().typ != tComma {
			break
		}
		p.advance()
	}
	if p.cur().typ != tRBracket {
		return attr.Value{}, p.unexpected()
	}
	p.advance()
	return attr.Value{Type: attr.TypeL, L: l}, nil
}

func (p *parser) parseSet() (attr.Value, error) {
	p.advance() // <<
	if p.cur().typ == tRRAngle {
		p.advance()
		return attr.Value{}, notWellFormed("A set cannot be empty")
	}
	var elems []attr.Value
	for {
		v, err := p.parseValue()
		if err != nil {
			return attr.Value{}, err
		}
		elems = append(elems, v)
		if p.cur().typ != tComma {
			break
		}
		p.advance()
	}
	if p.cur().typ != tRRAngle {
		return attr.Value{}, p.unexpected()
	}
	p.advance()
	return makeSet(elems)
}

// makeSet validates a set literal: non-empty, all elements the same scalar
// type among S, N and B, and no duplicates.
func makeSet(elems []attr.Value) (attr.Value, error) {
	if len(elems) == 0 {
		return attr.Value{}, notWellFormed("A set cannot be empty")
	}
	t := elems[0].Type
	if t != attr.TypeS && t != attr.TypeN && t != attr.TypeB {
		return attr.Value{}, notWellFormed("Set elements must be strings, numbers or binary")
	}
	switch t {
	case attr.TypeS:
		seen := make(map[string]bool, len(elems))
		ss := make([]string, len(elems))
		for i, e := range elems {
			if e.Type != attr.TypeS {
				return attr.Value{}, notWellFormed("Set elements must be of the same type")
			}
			if seen[e.S] {
				return attr.Value{}, notWellFormed("A set cannot contain duplicate elements")
			}
			seen[e.S] = true
			ss[i] = e.S
		}
		return attr.Value{Type: attr.TypeSS, SS: ss}, nil
	case attr.TypeN:
		seen := make(map[string]bool, len(elems))
		ns := make([]string, len(elems))
		for i, e := range elems {
			if e.Type != attr.TypeN {
				return attr.Value{}, notWellFormed("Set elements must be of the same type")
			}
			if seen[e.N] {
				return attr.Value{}, notWellFormed("A set cannot contain duplicate elements")
			}
			seen[e.N] = true
			ns[i] = e.N
		}
		return attr.Value{Type: attr.TypeNS, NS: ns}, nil
	case attr.TypeB:
		seen := make(map[string]bool, len(elems))
		bs := make([][]byte, len(elems))
		for i, e := range elems {
			if e.Type != attr.TypeB {
				return attr.Value{}, notWellFormed("Set elements must be of the same type")
			}
			if seen[string(e.B)] {
				return attr.Value{}, notWellFormed("A set cannot contain duplicate elements")
			}
			seen[string(e.B)] = true
			bs[i] = e.B
		}
		return attr.Value{Type: attr.TypeBS, BS: bs}, nil
	}
	return attr.Value{}, notWellFormed("Set elements must be strings, numbers or binary")
}

// parsePath parses a document path: a name followed by any number of .name or
// [n] segments.
func (p *parser) parsePath() (expr.Path, error) {
	elem, err := p.parsePathElem()
	if err != nil {
		return nil, err
	}
	path := expr.Path{elem}
	for {
		switch p.cur().typ {
		case tDot:
			p.advance()
			elem, err := p.parsePathElem()
			if err != nil {
				return nil, err
			}
			path = append(path, elem)
		case tLBracket:
			p.advance()
			if p.cur().typ != tNumber {
				return nil, p.unexpected()
			}
			idx, err := strconv.Atoi(p.cur().text)
			if err != nil || idx < 0 {
				return nil, p.unexpected()
			}
			p.advance()
			if p.cur().typ != tRBracket {
				return nil, p.unexpected()
			}
			p.advance()
			path = append(path, expr.PathElem{Index: idx, IsIndex: true})
		default:
			return path, nil
		}
	}
}

func (p *parser) parsePathElem() (expr.PathElem, error) {
	switch p.cur().typ {
	case tIdent, tQuotedIdent:
		name := p.cur().text
		p.advance()
		return expr.PathElem{Name: name}, nil
	}
	return expr.PathElem{}, p.unexpected()
}

// parseName parses a bare or double-quoted table, index or map key name.
func (p *parser) parseName() (string, error) {
	switch p.cur().typ {
	case tIdent, tQuotedIdent:
		name := p.cur().text
		p.advance()
		return name, nil
	}
	return "", p.unexpected()
}

// parseOperand parses size(path), a value, or a path.
func (p *parser) parseOperand() (expr.Operand, error) {
	if p.cur().typ == tIdent && strings.ToLower(p.cur().text) == "size" && p.peek().typ == tLParen {
		p.advance() // size
		p.advance() // (
		path, err := p.parsePath()
		if err != nil {
			return nil, err
		}
		if p.cur().typ != tRParen {
			return nil, p.unexpected()
		}
		p.advance()
		return expr.SizeOperand{Path: path}, nil
	}
	if isValueStart(p.cur().typ) {
		return p.valueOperand()
	}
	path, err := p.parsePath()
	if err != nil {
		return nil, err
	}
	return expr.PathOperand{Path: path}, nil
}

func isValueStart(t tokenType) bool {
	switch t {
	case tString, tNumber, tTrue, tFalse, tNull, tParam, tLBrace, tLBracket, tLLAngle:
		return true
	}
	return false
}

// parseCond parses a condition: OR, then AND, then NOT, then primaries.
func (p *parser) parseCond() (expr.Cond, error) {
	return p.parseOr()
}

func (p *parser) parseOr() (expr.Cond, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.cur().typ == tOr {
		p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = expr.Or{L: left, R: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (expr.Cond, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.cur().typ == tAnd {
		p.advance()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = expr.And{L: left, R: right}
	}
	return left, nil
}

func (p *parser) parseNot() (expr.Cond, error) {
	if p.cur().typ == tNot {
		p.advance()
		c, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return expr.Not{C: c}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (expr.Cond, error) {
	if p.cur().typ == tLParen {
		p.advance()
		c, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.cur().typ != tRParen {
			return nil, p.unexpected()
		}
		p.advance()
		return c, nil
	}
	if p.cur().typ == tIdent && p.peek().typ == tLParen && isCondFunc(strings.ToLower(p.cur().text)) {
		name := strings.ToLower(p.cur().text)
		p.advance() // name
		return p.parseCondFunc(name)
	}

	op, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	switch p.cur().typ {
	case tEq, tNe, tLt, tLe, tGt, tGe:
		opTok := p.cur()
		p.advance()
		rhs, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		return expr.Compare{Op: opTok.text, L: op, R: rhs}, nil
	case tBetween:
		p.advance()
		lo, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		if p.cur().typ != tAnd {
			return nil, p.unexpected()
		}
		p.advance()
		hi, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		return expr.Between{V: op, Lo: lo, Hi: hi}, nil
	case tIn:
		p.advance()
		return p.parseIn(op)
	case tIs:
		p.advance()
		path, ok := op.(expr.PathOperand)
		if !ok {
			return nil, p.unexpected()
		}
		return p.parseIs(path.Path)
	}
	return nil, p.unexpected()
}

func isCondFunc(name string) bool {
	switch name {
	case "attribute_exists", "attribute_not_exists", "attribute_type", "begins_with", "contains":
		return true
	}
	return false
}

func (p *parser) parseCondFunc(name string) (expr.Cond, error) {
	p.advance() // (
	var args []expr.Operand
	switch name {
	case "attribute_exists", "attribute_not_exists":
		path, err := p.parsePath()
		if err != nil {
			return nil, err
		}
		args = append(args, expr.PathOperand{Path: path})
		if p.cur().typ != tRParen {
			return nil, p.unexpected()
		}
	case "attribute_type", "begins_with", "contains":
		path, err := p.parsePath()
		if err != nil {
			return nil, err
		}
		args = append(args, expr.PathOperand{Path: path})
		if p.cur().typ != tComma {
			return nil, p.unexpected()
		}
		p.advance()
		v, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		args = append(args, v)
		if p.cur().typ != tRParen {
			return nil, p.unexpected()
		}
	}
	p.advance() // )
	return expr.Func{Name: name, Args: args}, nil
}

// parseIn parses `operand IN (..)` or `operand IN [..]`.
func (p *parser) parseIn(op expr.Operand) (expr.Cond, error) {
	var close tokenType
	switch p.cur().typ {
	case tLParen:
		close = tRParen
		p.advance()
	case tLBracket:
		close = tRBracket
		p.advance()
	default:
		return nil, p.unexpected()
	}
	var list []expr.Operand
	o, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	list = append(list, o)
	for p.cur().typ == tComma {
		p.advance()
		o, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		list = append(list, o)
	}
	if p.cur().typ != close {
		return nil, p.unexpected()
	}
	p.advance()
	return expr.In{V: op, List: list}, nil
}

// parseIs maps IS [NOT] MISSING and IS [NOT] NULL onto expr nodes the
// evaluator already understands: MISSING is attribute_not_exists, NOT MISSING
// is attribute_exists, NULL is attribute_type(path,'NULL'), and NOT NULL is
// attribute_exists AND NOT attribute_type(path,'NULL').
func (p *parser) parseIs(path expr.Path) (expr.Cond, error) {
	neg := false
	if p.cur().typ == tNot {
		neg = true
		p.advance()
	}
	var missing bool
	switch p.cur().typ {
	case tMissing:
		missing = true
	case tNull:
		missing = false
	default:
		return nil, p.unexpected()
	}
	p.advance()

	pathOp := expr.PathOperand{Path: path}
	if missing {
		if neg {
			return expr.Func{Name: "attribute_exists", Args: []expr.Operand{pathOp}}, nil
		}
		return expr.Func{Name: "attribute_not_exists", Args: []expr.Operand{pathOp}}, nil
	}
	attrType := expr.Func{Name: "attribute_type", Args: []expr.Operand{pathOp, p.nullOperand()}}
	if neg {
		exists := expr.Func{Name: "attribute_exists", Args: []expr.Operand{pathOp}}
		return expr.And{L: exists, R: expr.Not{C: attrType}}, nil
	}
	return attrType, nil
}

// Statement parsers.

func (p *parser) parseSelectStmt() (Statement, error) {
	sel, err := p.parseSelectCore()
	if err != nil {
		return nil, err
	}
	if err := p.expectEOF(); err != nil {
		return nil, err
	}
	return sel, nil
}

func (p *parser) parseSelectCore() (SelectPlan, error) {
	p.advance() // SELECT
	proj, err := p.parseProjection()
	if err != nil {
		return SelectPlan{}, err
	}
	if p.cur().typ != tFrom {
		return SelectPlan{}, p.unexpected()
	}
	p.advance()
	table, index, err := p.parseTableAndIndex()
	if err != nil {
		return SelectPlan{}, err
	}
	var where expr.Cond
	if p.cur().typ == tWhere {
		p.advance()
		where, err = p.parseCond()
		if err != nil {
			return SelectPlan{}, err
		}
	}
	return SelectPlan{Table: table, Index: index, Projection: proj, Where: where, Values: p.values}, nil
}

func (p *parser) parseProjection() ([]expr.Path, error) {
	if p.cur().typ == tStar {
		p.advance()
		return nil, nil
	}
	var paths []expr.Path
	path, err := p.parsePath()
	if err != nil {
		return nil, err
	}
	paths = append(paths, path)
	for p.cur().typ == tComma {
		p.advance()
		path, err := p.parsePath()
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func (p *parser) parseTableAndIndex() (table, index string, err error) {
	table, err = p.parseName()
	if err != nil {
		return "", "", err
	}
	if p.cur().typ == tDot {
		p.advance()
		index, err = p.parseName()
		if err != nil {
			return "", "", err
		}
	}
	return table, index, nil
}

func (p *parser) parseInsertStmt() (Statement, error) {
	p.advance() // INSERT
	if p.cur().typ != tInto {
		return nil, p.unexpected()
	}
	p.advance()
	table, err := p.parseName()
	if err != nil {
		return nil, err
	}
	if p.cur().typ != tValue {
		return nil, p.unexpected()
	}
	p.advance()
	if p.cur().typ != tLBrace {
		return nil, p.unexpected()
	}
	itemVal, err := p.parseMap()
	if err != nil {
		return nil, err
	}
	if err := p.expectEOF(); err != nil {
		return nil, err
	}
	return InsertPlan{Table: table, Item: attr.Item(itemVal.M)}, nil
}

func (p *parser) parseUpdateStmt() (Statement, error) {
	p.advance() // UPDATE
	table, err := p.parseName()
	if err != nil {
		return nil, err
	}
	if p.cur().typ != tSet {
		return nil, p.unexpected()
	}
	p.advance()
	var u expr.Update
	if err := p.parseSetActions(&u); err != nil {
		return nil, err
	}
	if p.cur().typ == tRemove {
		p.advance()
		if err := p.parseRemovePaths(&u); err != nil {
			return nil, err
		}
	}
	if p.cur().typ != tWhere {
		return nil, p.unexpected()
	}
	p.advance()
	where, err := p.parseCond()
	if err != nil {
		return nil, err
	}
	if err := p.expectEOF(); err != nil {
		return nil, err
	}
	return UpdatePlan{Table: table, Where: where, Update: u, Values: p.values}, nil
}

func (p *parser) parseDeleteStmt() (Statement, error) {
	p.advance() // DELETE
	if p.cur().typ != tFrom {
		return nil, p.unexpected()
	}
	p.advance()
	table, err := p.parseName()
	if err != nil {
		return nil, err
	}
	if p.cur().typ != tWhere {
		return nil, p.unexpected()
	}
	p.advance()
	where, err := p.parseCond()
	if err != nil {
		return nil, err
	}
	if err := p.expectEOF(); err != nil {
		return nil, err
	}
	return DeletePlan{Table: table, Where: where, Values: p.values}, nil
}

func (p *parser) parseExistsStmt() (Statement, error) {
	p.advance() // EXISTS
	if p.cur().typ != tLParen {
		return nil, p.unexpected()
	}
	p.advance()
	if p.cur().typ != tSelect {
		return nil, p.unexpected()
	}
	sel, err := p.parseSelectCore()
	if err != nil {
		return nil, err
	}
	if p.cur().typ != tRParen {
		return nil, p.unexpected()
	}
	p.advance()
	if err := p.expectEOF(); err != nil {
		return nil, err
	}
	return ExistsPlan{Select: sel}, nil
}

func (p *parser) parseSetActions(u *expr.Update) error {
	for {
		path, err := p.parsePath()
		if err != nil {
			return err
		}
		if p.cur().typ != tEq {
			return p.unexpected()
		}
		p.advance()
		if p.cur().typ == tIdent && p.peek().typ == tLParen && isSetFunc(strings.ToLower(p.cur().text)) {
			if err := p.parseSetAddDelete(u, path, strings.ToLower(p.cur().text)); err != nil {
				return err
			}
		} else {
			v, err := p.parseSetValue()
			if err != nil {
				return err
			}
			u.Set = append(u.Set, expr.SetAction{Path: path, Value: v})
		}
		if p.cur().typ != tComma {
			return nil
		}
		p.advance()
	}
}

func isSetFunc(name string) bool {
	return name == "set_add" || name == "set_delete"
}

// parseSetAddDelete parses `SET path = set_add(path, set)` or
// `SET path = set_delete(path, set)` into Update.Add / Update.Delete. The
// first argument must be the same path being set.
func (p *parser) parseSetAddDelete(u *expr.Update, target expr.Path, name string) error {
	p.advance() // name
	p.advance() // (
	arg, err := p.parsePath()
	if err != nil {
		return err
	}
	if !pathsEqual(target, arg) {
		return notWellFormed(name + " requires its first argument to be the path being set")
	}
	if p.cur().typ != tComma {
		return p.unexpected()
	}
	p.advance()
	op, err := p.valueOperand()
	if err != nil {
		return err
	}
	if p.cur().typ != tRParen {
		return p.unexpected()
	}
	p.advance()
	vop := op.(expr.ValueOperand)
	entry := struct {
		Path  expr.Path
		Value string
	}{target, vop.Name}
	if name == "set_add" {
		u.Add = append(u.Add, entry)
	} else {
		u.Delete = append(u.Delete, entry)
	}
	return nil
}

// parseSetValue parses a SET right-hand side: operand [(+|-) operand].
func (p *parser) parseSetValue() (expr.SetValue, error) {
	l, err := p.parseSetOperand()
	if err != nil {
		return nil, err
	}
	if p.cur().typ == tPlus || p.cur().typ == tMinus {
		op := p.cur().text
		p.advance()
		r, err := p.parseSetOperand()
		if err != nil {
			return nil, err
		}
		return expr.SetArith{Op: op, L: l, R: r}, nil
	}
	return l, nil
}

// parseSetOperand parses a path, a value, or list_append(a, b).
func (p *parser) parseSetOperand() (expr.SetValue, error) {
	if p.cur().typ == tIdent && p.peek().typ == tLParen {
		switch name := strings.ToLower(p.cur().text); name {
		case "list_append":
			p.advance()
			p.advance() // (
			a, err := p.parseSetOperand()
			if err != nil {
				return nil, err
			}
			if p.cur().typ != tComma {
				return nil, p.unexpected()
			}
			p.advance()
			b, err := p.parseSetOperand()
			if err != nil {
				return nil, err
			}
			if p.cur().typ != tRParen {
				return nil, p.unexpected()
			}
			p.advance()
			return expr.ListAppend{A: a, B: b}, nil
		case "set_add", "set_delete":
			return nil, p.unexpected()
		}
	}
	if isValueStart(p.cur().typ) {
		op, err := p.valueOperand()
		if err != nil {
			return nil, err
		}
		return expr.SetOperand{O: op}, nil
	}
	path, err := p.parsePath()
	if err != nil {
		return nil, err
	}
	return expr.SetOperand{O: expr.PathOperand{Path: path}}, nil
}

func (p *parser) parseRemovePaths(u *expr.Update) error {
	for {
		path, err := p.parsePath()
		if err != nil {
			return err
		}
		u.Remove = append(u.Remove, path)
		if p.cur().typ != tComma {
			return nil
		}
		p.advance()
	}
}

func pathsEqual(a, b expr.Path) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

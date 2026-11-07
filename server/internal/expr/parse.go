package expr

import (
	"strconv"
	"strings"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

// parser is a recursive-descent parser over the lexed tokens of one
// expression. cur() is the lookahead token; advance() consumes it, so the
// token immediately before the offending token is always toks[pos-1].
type parser struct {
	param string
	input string
	toks  []token
	pos   int
	env   *Env
}

func newParser(param, input string, env *Env) *parser {
	return &parser{param: param, input: input, toks: lex(input), env: env}
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

// syntaxError builds DynamoDB's syntax error message: the offending token and
// the "near" span running from the previous token to it.
func (p *parser) syntaxError() *ddberr.Error {
	cur := p.toks[p.pos]
	token, near := cur.text, ""
	switch {
	case cur.typ == tEOF:
		token = "<EOF>"
		if p.pos > 0 {
			near = p.toks[p.pos-1].text
		}
	case p.pos == 0:
		near = cur.text
	default:
		near = p.input[p.toks[p.pos-1].start:cur.end]
	}
	return ddberr.Validation("Invalid %s: Syntax error; token: \"%s\", near: \"%s\"", p.param, token, near)
}

// ParseCondition parses a ConditionExpression.
func ParseCondition(s string, env *Env) (Cond, error) {
	return parseCond("ConditionExpression", s, env)
}

// ParseFilter parses a FilterExpression.
func ParseFilter(s string, env *Env) (Cond, error) {
	return parseCond("FilterExpression", s, env)
}

func parseCond(param, s string, env *Env) (Cond, error) {
	if strings.TrimSpace(s) == "" {
		return nil, ddberr.Validation("Invalid %s: The expression can not be empty;", param)
	}
	p := newParser(param, s, env)
	c, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.cur().typ != tEOF {
		return nil, p.syntaxError()
	}
	return c, nil
}

// condition, lowest to highest precedence: OR, AND, NOT, then the primaries.
func (p *parser) parseOr() (Cond, error) {
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
		left = Or{L: left, R: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Cond, error) {
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
		left = And{L: left, R: right}
	}
	return left, nil
}

func (p *parser) parseNot() (Cond, error) {
	if p.cur().typ == tNot {
		p.advance()
		c, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return Not{C: c}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Cond, error) {
	if p.cur().typ == tLParen {
		p.advance()
		c, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.cur().typ != tRParen {
			return nil, p.syntaxError()
		}
		p.advance()
		return c, nil
	}
	if p.cur().typ == tName && isCondFunc(p.cur().text) && p.peek().typ == tLParen {
		return p.parseFunc()
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
		return Compare{Op: opTok.text, L: op, R: rhs}, nil
	case tBetween:
		p.advance()
		lo, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		if p.cur().typ != tAnd {
			return nil, p.syntaxError()
		}
		p.advance()
		hi, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		return Between{V: op, Lo: lo, Hi: hi}, nil
	case tIn:
		p.advance()
		if p.cur().typ != tLParen {
			return nil, p.syntaxError()
		}
		p.advance()
		list := []Operand{}
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
		if p.cur().typ != tRParen {
			return nil, p.syntaxError()
		}
		p.advance()
		return In{V: op, List: list}, nil
	}
	return nil, p.syntaxError()
}

// isCondFunc reports whether name is a condition function (size is an operand,
// handled in parseOperand).
func isCondFunc(name string) bool {
	switch name {
	case "attribute_exists", "attribute_not_exists", "attribute_type", "begins_with", "contains":
		return true
	}
	return false
}

func (p *parser) parseFunc() (Cond, error) {
	name := p.cur().text
	p.advance() // function name
	p.advance() // (
	var args []Operand
	switch name {
	case "attribute_exists", "attribute_not_exists":
		path, err := p.parsePath()
		if err != nil {
			return nil, err
		}
		args = append(args, PathOperand{Path: path})
		if p.cur().typ != tRParen {
			return nil, p.syntaxError()
		}
	case "attribute_type":
		path, err := p.parsePath()
		if err != nil {
			return nil, err
		}
		args = append(args, PathOperand{Path: path})
		if p.cur().typ != tComma {
			return nil, p.syntaxError()
		}
		p.advance()
		if p.cur().typ != tValue {
			return nil, p.syntaxError()
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args = append(args, v)
		if p.cur().typ != tRParen {
			return nil, p.syntaxError()
		}
	case "begins_with", "contains":
		path, err := p.parsePath()
		if err != nil {
			return nil, err
		}
		args = append(args, PathOperand{Path: path})
		if p.cur().typ != tComma {
			return nil, p.syntaxError()
		}
		p.advance()
		o, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		args = append(args, o)
		if p.cur().typ != tRParen {
			return nil, p.syntaxError()
		}
	}
	p.advance() // )
	return Func{Name: name, Args: args}, nil
}

// parseOperand parses :value, size(path) or a path.
func (p *parser) parseOperand() (Operand, error) {
	switch p.cur().typ {
	case tValue:
		return p.parseValue()
	case tName:
		if p.cur().text == "size" && p.peek().typ == tLParen {
			p.advance() // size
			p.advance() // (
			path, err := p.parsePath()
			if err != nil {
				return nil, err
			}
			if p.cur().typ != tRParen {
				return nil, p.syntaxError()
			}
			p.advance() // )
			return SizeOperand{Path: path}, nil
		}
	}
	path, err := p.parsePath()
	if err != nil {
		return nil, err
	}
	return PathOperand{Path: path}, nil
}

func (p *parser) parseValue() (Operand, error) {
	key := p.cur().text
	p.advance()
	if err := p.env.useValue(p.param, key); err != nil {
		return nil, err
	}
	return ValueOperand{Name: key}, nil
}

// parsePath parses a document path: a name or #placeholder followed by any
// number of .name-or-#placeholder or [digits].
func (p *parser) parsePath() (Path, error) {
	elem, err := p.parsePathElem()
	if err != nil {
		return nil, err
	}
	path := Path{elem}
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
				return nil, p.syntaxError()
			}
			idx, err := strconv.Atoi(p.cur().text)
			if err != nil {
				return nil, p.syntaxError()
			}
			p.advance()
			if p.cur().typ != tRBracket {
				return nil, p.syntaxError()
			}
			p.advance()
			path = append(path, PathElem{Index: idx, IsIndex: true})
		default:
			return path, nil
		}
	}
}

func (p *parser) parsePathElem() (PathElem, error) {
	switch p.cur().typ {
	case tName:
		word := p.cur().text
		if reserved[strings.ToLower(word)] {
			return PathElem{}, ddberr.Validation("Invalid %s: Attribute name is a reserved keyword; reserved keyword: %s", p.param, word)
		}
		p.advance()
		return PathElem{Name: word}, nil
	case tHashName:
		key := p.cur().text
		p.advance()
		name, err := p.env.resolveName(p.param, key)
		if err != nil {
			return PathElem{}, err
		}
		return PathElem{Name: name}, nil
	}
	return PathElem{}, p.syntaxError()
}

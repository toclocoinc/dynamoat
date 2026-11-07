package expr

import (
	"strconv"
	"strings"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

const updateParam = "UpdateExpression"

// SetValue is the right-hand side of a SET action.
type SetValue interface{ setValue() }

// SetOperand is a path or :value.
type SetOperand struct{ O Operand }

// SetArith is `operand + operand` or `operand - operand`. Op is "+" or "-";
// each side is a SetOperand or a function.
type SetArith struct {
	Op   string
	L, R SetValue
}

// IfNotExists is if_not_exists(path, value).
type IfNotExists struct {
	Path    Path
	Default SetValue
}

// ListAppend is list_append(value, value).
type ListAppend struct{ A, B SetValue }

func (SetOperand) setValue()  {}
func (SetArith) setValue()    {}
func (IfNotExists) setValue() {}
func (ListAppend) setValue()  {}

// SetAction is one `path = value` in a SET clause.
type SetAction struct {
	Path  Path
	Value SetValue
}

// pathValue is a path with a ":placeholder" value, used by ADD and DELETE.
type pathValue = struct {
	Path  Path
	Value string
}

// Update is a parsed UpdateExpression.
type Update struct {
	Set    []SetAction
	Remove []Path
	Add    []struct {
		Path  Path
		Value string
	}
	Delete []struct {
		Path  Path
		Value string
	}
}

// ParseUpdate parses an UpdateExpression.
func ParseUpdate(s string, env *Env) (Update, error) {
	var u Update
	if strings.TrimSpace(s) == "" {
		return u, ddberr.Validation("Invalid %s: The expression can not be empty;", updateParam)
	}
	p := newParser(updateParam, s, env)
	seen := map[string]bool{}
	var paths []Path // in source order, for the overlap check

	for p.cur().typ != tEOF {
		kw := clauseKeyword(p.cur())
		if kw == "" {
			return Update{}, p.syntaxError()
		}
		if seen[kw] {
			return Update{}, ddberr.Validation("Invalid %s: The \"%s\" section can only be used once in an update expression;", updateParam, kw)
		}
		seen[kw] = true
		p.advance()
		for {
			path, err := p.parsePath()
			if err != nil {
				return Update{}, err
			}
			paths = append(paths, path)
			switch kw {
			case "SET":
				if p.cur().typ != tEq {
					return Update{}, p.syntaxError()
				}
				p.advance()
				v, err := p.parseSetExpr()
				if err != nil {
					return Update{}, err
				}
				u.Set = append(u.Set, SetAction{Path: path, Value: v})
			case "REMOVE":
				u.Remove = append(u.Remove, path)
			case "ADD", "DELETE":
				if p.cur().typ != tValue {
					return Update{}, p.syntaxError()
				}
				v, err := p.parseValue()
				if err != nil {
					return Update{}, err
				}
				pv := pathValue{Path: path, Value: v.(ValueOperand).Name}
				if kw == "ADD" {
					u.Add = append(u.Add, pv)
				} else {
					u.Delete = append(u.Delete, pv)
				}
			}
			if p.cur().typ != tComma {
				break
			}
			p.advance()
		}
		if t := p.cur(); t.typ != tEOF && clauseKeyword(t) == "" {
			return Update{}, p.syntaxError()
		}
	}

	for i := range paths {
		for j := i + 1; j < len(paths); j++ {
			if pathsOverlap(paths[i], paths[j]) {
				return Update{}, ddberr.Validation("Invalid %s: Two document paths overlap with each other; must remove or rewrite one of these paths; path one: %s, path two: %s",
					updateParam, bracketPath(paths[i]), bracketPath(paths[j]))
			}
		}
	}
	return u, nil
}

func clauseKeyword(t token) string {
	if t.typ != tName {
		return ""
	}
	switch kw := strings.ToUpper(t.text); kw {
	case "SET", "REMOVE", "ADD", "DELETE":
		return kw
	}
	return ""
}

// parseSetExpr parses the right-hand side: operand [(+|-) operand].
func (p *parser) parseSetExpr() (SetValue, error) {
	l, err := p.parseSetOperand()
	if err != nil {
		return nil, err
	}
	// The lexer has no +/- token; they arrive as tInvalid.
	if t := p.cur(); t.typ == tInvalid && (t.text == "+" || t.text == "-") {
		p.advance()
		r, err := p.parseSetOperand()
		if err != nil {
			return nil, err
		}
		return SetArith{Op: t.text, L: l, R: r}, nil
	}
	return l, nil
}

// parseSetOperand parses a path, :value, if_not_exists(...) or list_append(...).
func (p *parser) parseSetOperand() (SetValue, error) {
	t := p.cur()
	if t.typ == tValue {
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return SetOperand{O: v}, nil
	}
	if t.typ == tName && p.peek().typ == tLParen {
		switch t.text {
		case "if_not_exists":
			p.advance()
			p.advance()
			path, err := p.parsePath()
			if err != nil {
				return nil, err
			}
			if p.cur().typ != tComma {
				return nil, p.syntaxError()
			}
			p.advance()
			d, err := p.parseSetOperand()
			if err != nil {
				return nil, err
			}
			if p.cur().typ != tRParen {
				return nil, p.syntaxError()
			}
			p.advance()
			return IfNotExists{Path: path, Default: d}, nil
		case "list_append":
			p.advance()
			p.advance()
			a, err := p.parseSetOperand()
			if err != nil {
				return nil, err
			}
			if p.cur().typ != tComma {
				return nil, p.syntaxError()
			}
			p.advance()
			b, err := p.parseSetOperand()
			if err != nil {
				return nil, err
			}
			if p.cur().typ != tRParen {
				return nil, p.syntaxError()
			}
			p.advance()
			return ListAppend{A: a, B: b}, nil
		}
		return nil, ddberr.Validation("Invalid %s: Invalid function name; function: %s", updateParam, t.text)
	}
	path, err := p.parsePath()
	if err != nil {
		return nil, err
	}
	return SetOperand{O: PathOperand{Path: path}}, nil
}

// pathsOverlap reports whether the paths are equal or one is a prefix of the
// other.
func pathsOverlap(a, b Path) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// bracketPath writes a path as DynamoDB does in the overlap error: elements in
// brackets, indexes as [n] elements, e.g. [a, [0]].
func bracketPath(p Path) string {
	parts := make([]string, len(p))
	for i, e := range p {
		if e.IsIndex {
			parts[i] = "[" + strconv.Itoa(e.Index) + "]"
		} else {
			parts[i] = e.Name
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// StringUpdate renders an Update canonically (clauses in SET, REMOVE, ADD,
// DELETE order), for tests.
func StringUpdate(u Update) string {
	var clauses []string
	if len(u.Set) > 0 {
		parts := make([]string, len(u.Set))
		for i, a := range u.Set {
			parts[i] = a.Path.String() + " = " + setValueString(a.Value)
		}
		clauses = append(clauses, "SET "+strings.Join(parts, ", "))
	}
	if len(u.Remove) > 0 {
		parts := make([]string, len(u.Remove))
		for i, p := range u.Remove {
			parts[i] = p.String()
		}
		clauses = append(clauses, "REMOVE "+strings.Join(parts, ", "))
	}
	for _, c := range []struct {
		name string
		list []pathValue
	}{{"ADD", u.Add}, {"DELETE", u.Delete}} {
		if len(c.list) == 0 {
			continue
		}
		parts := make([]string, len(c.list))
		for i, e := range c.list {
			parts[i] = e.Path.String() + " " + e.Value
		}
		clauses = append(clauses, c.name+" "+strings.Join(parts, ", "))
	}
	return strings.Join(clauses, " ")
}

func setValueString(v SetValue) string {
	switch v := v.(type) {
	case SetOperand:
		return operandString(v.O)
	case SetArith:
		return setValueString(v.L) + " " + v.Op + " " + setValueString(v.R)
	case IfNotExists:
		return "if_not_exists(" + v.Path.String() + ", " + setValueString(v.Default) + ")"
	case ListAppend:
		return "list_append(" + setValueString(v.A) + ", " + setValueString(v.B) + ")"
	}
	return ""
}

package expr

import "strings"

// tokenType identifies a lexed token.
type tokenType int

const (
	tEOF      tokenType = iota
	tName               // bare name or function name
	tHashName           // #name placeholder
	tValue              // :name placeholder
	tNumber             // digit sequence, only valid inside [ ]
	tLParen
	tRParen
	tLBracket
	tRBracket
	tComma
	tDot
	tEq
	tNe
	tLt
	tLe
	tGt
	tGe
	tAnd
	tOr
	tNot
	tBetween
	tIn
	tInvalid // any character that can never appear in an expression
)

// token is one lexed token. text is the source text as written; start and end
// are byte offsets into the input, used to reconstruct the "near" span in
// syntax error messages.
type token struct {
	typ   tokenType
	text  string
	start int
	end   int
}

// lex splits an expression into tokens. The grammar is simple enough that no
// parser generator is needed: keywords are recognised case-insensitively,
// comparators are two-character maximal-munch, and whitespace separates.
func lex(input string) []token {
	var toks []token
	i, n := 0, len(input)
	for i < n {
		c := input[i]
		if isSpace(c) {
			i++
			continue
		}
		start := i
		switch {
		case isNameStart(c):
			i++
			for i < n && isNameChar(input[i]) {
				i++
			}
			word := input[start:i]
			toks = append(toks, token{keyword(word), word, start, i})
		case c == '#':
			i++
			for i < n && isNameChar(input[i]) {
				i++
			}
			toks = append(toks, token{tHashName, input[start:i], start, i})
		case c == ':':
			i++
			for i < n && isNameChar(input[i]) {
				i++
			}
			toks = append(toks, token{tValue, input[start:i], start, i})
		case c >= '0' && c <= '9':
			i++
			for i < n && input[i] >= '0' && input[i] <= '9' {
				i++
			}
			toks = append(toks, token{tNumber, input[start:i], start, i})
		case c == '(':
			i++
			toks = append(toks, token{tLParen, "(", start, i})
		case c == ')':
			i++
			toks = append(toks, token{tRParen, ")", start, i})
		case c == '[':
			i++
			toks = append(toks, token{tLBracket, "[", start, i})
		case c == ']':
			i++
			toks = append(toks, token{tRBracket, "]", start, i})
		case c == ',':
			i++
			toks = append(toks, token{tComma, ",", start, i})
		case c == '.':
			i++
			toks = append(toks, token{tDot, ".", start, i})
		case c == '=':
			i++
			toks = append(toks, token{tEq, "=", start, i})
		case c == '<':
			switch {
			case i+1 < n && input[i+1] == '>':
				i += 2
				toks = append(toks, token{tNe, "<>", start, i})
			case i+1 < n && input[i+1] == '=':
				i += 2
				toks = append(toks, token{tLe, "<=", start, i})
			default:
				i++
				toks = append(toks, token{tLt, "<", start, i})
			}
		case c == '>':
			if i+1 < n && input[i+1] == '=' {
				i += 2
				toks = append(toks, token{tGe, ">=", start, i})
			} else {
				i++
				toks = append(toks, token{tGt, ">", start, i})
			}
		default:
			i++
			toks = append(toks, token{tInvalid, string(c), start, i})
		}
	}
	toks = append(toks, token{tEOF, "", n, n})
	return toks
}

func keyword(word string) tokenType {
	switch strings.ToUpper(word) {
	case "AND":
		return tAnd
	case "OR":
		return tOr
	case "NOT":
		return tNot
	case "BETWEEN":
		return tBetween
	case "IN":
		return tIn
	}
	return tName
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func isNameStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

func isNameChar(c byte) bool {
	return isNameStart(c) || c >= '0' && c <= '9'
}

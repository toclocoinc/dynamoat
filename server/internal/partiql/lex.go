package partiql

import (
	"strings"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

// tokenType identifies a lexed PartiQL token.
type tokenType int

const (
	tEOF         tokenType = iota
	tIdent                 // bare identifier
	tQuotedIdent           // "double-quoted" identifier, text holds the unescaped name
	tString                // 'single-quoted' string, text holds the unescaped value
	tNumber                // number literal, text holds the source text
	tParam                 // ? placeholder
	tStar
	tLParen
	tRParen
	tLBracket
	tRBracket
	tLBrace
	tRBrace
	tLLAngle // << set open
	tRRAngle // >> set close
	tComma
	tDot
	tColon
	tEq
	tNe
	tLt
	tLe
	tGt
	tGe
	tPlus
	tMinus
	tInvalid
	// Keywords, recognised case-insensitively. token.text keeps the source case.
	tSelect
	tFrom
	tWhere
	tInsert
	tInto
	tValue
	tUpdate
	tSet
	tRemove
	tDelete
	tAnd
	tOr
	tNot
	tBetween
	tIn
	tIs
	tMissing
	tNull
	tExists
	tTrue
	tFalse
)

// token is one lexed token. text is the source text as written, except for
// tString and tQuotedIdent where it holds the unescaped value; start and end
// are byte offsets into the input.
type token struct {
	typ   tokenType
	text  string
	start int
	end   int
}

// lex splits a PartiQL statement into tokens. String literals use ” as the
// escape for a single quote and double-quoted identifiers use "" for a double
// quote, matching SQL-style quoting.
func lex(input string) ([]token, *ddberr.Error) {
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
			toks = append(toks, token{keywordType(word), word, start, i})
		case c == '"':
			i++
			var b strings.Builder
			for {
				if i >= n {
					return nil, notWellFormed("Unterminated quoted identifier")
				}
				if input[i] == '"' {
					if i+1 < n && input[i+1] == '"' {
						b.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(input[i])
				i++
			}
			toks = append(toks, token{tQuotedIdent, b.String(), start, i})
		case c == '\'':
			i++
			var b strings.Builder
			for {
				if i >= n {
					return nil, notWellFormed("Unterminated string literal")
				}
				if input[i] == '\'' {
					if i+1 < n && input[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(input[i])
				i++
			}
			toks = append(toks, token{tString, b.String(), start, i})
		case c == '-' || (c >= '0' && c <= '9'):
			// A lone '-' is the subtraction operator; otherwise scan a number.
			if c == '-' && (i+1 >= n || input[i+1] < '0' || input[i+1] > '9') {
				toks = append(toks, token{tMinus, "-", start, i + 1})
				i++
				continue
			}
			if c == '-' {
				i++
			}
			for i < n && input[i] >= '0' && input[i] <= '9' {
				i++
			}
			if i < n && input[i] == '.' && i+1 < n && input[i+1] >= '0' && input[i+1] <= '9' {
				i++
				for i < n && input[i] >= '0' && input[i] <= '9' {
					i++
				}
			}
			if i < n && (input[i] == 'e' || input[i] == 'E') {
				j := i + 1
				if j < n && (input[j] == '+' || input[j] == '-') {
					j++
				}
				if j < n && input[j] >= '0' && input[j] <= '9' {
					i = j
					for i < n && input[i] >= '0' && input[i] <= '9' {
						i++
					}
				}
			}
			toks = append(toks, token{tNumber, input[start:i], start, i})
		case c == '?':
			i++
			toks = append(toks, token{tParam, "?", start, i})
		case c == '*':
			i++
			toks = append(toks, token{tStar, "*", start, i})
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
		case c == '{':
			i++
			toks = append(toks, token{tLBrace, "{", start, i})
		case c == '}':
			i++
			toks = append(toks, token{tRBrace, "}", start, i})
		case c == ',':
			i++
			toks = append(toks, token{tComma, ",", start, i})
		case c == '.':
			i++
			toks = append(toks, token{tDot, ".", start, i})
		case c == ':':
			i++
			toks = append(toks, token{tColon, ":", start, i})
		case c == '+':
			i++
			toks = append(toks, token{tPlus, "+", start, i})
		case c == '=':
			i++
			toks = append(toks, token{tEq, "=", start, i})
		case c == '<':
			switch {
			case i+1 < n && input[i+1] == '<':
				toks = append(toks, token{tLLAngle, "<<", start, i + 2})
				i += 2
			case i+1 < n && input[i+1] == '=':
				toks = append(toks, token{tLe, "<=", start, i + 2})
				i += 2
			case i+1 < n && input[i+1] == '>':
				toks = append(toks, token{tNe, "<>", start, i + 2})
				i += 2
			default:
				toks = append(toks, token{tLt, "<", start, i + 1})
				i++
			}
		case c == '>':
			switch {
			case i+1 < n && input[i+1] == '>':
				toks = append(toks, token{tRRAngle, ">>", start, i + 2})
				i += 2
			case i+1 < n && input[i+1] == '=':
				toks = append(toks, token{tGe, ">=", start, i + 2})
				i += 2
			default:
				toks = append(toks, token{tGt, ">", start, i + 1})
				i++
			}
		default:
			i++
			toks = append(toks, token{tInvalid, string(c), start, i})
		}
	}
	toks = append(toks, token{tEOF, "", n, n})
	return toks, nil
}

func keywordType(word string) tokenType {
	switch strings.ToUpper(word) {
	case "SELECT":
		return tSelect
	case "FROM":
		return tFrom
	case "WHERE":
		return tWhere
	case "INSERT":
		return tInsert
	case "INTO":
		return tInto
	case "VALUE":
		return tValue
	case "UPDATE":
		return tUpdate
	case "SET":
		return tSet
	case "REMOVE":
		return tRemove
	case "DELETE":
		return tDelete
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
	case "IS":
		return tIs
	case "MISSING":
		return tMissing
	case "NULL":
		return tNull
	case "EXISTS":
		return tExists
	case "TRUE":
		return tTrue
	case "FALSE":
		return tFalse
	}
	return tIdent
}

// keywords are the bare identifiers that must be double-quoted when used as a
// name, lower-cased for the case-insensitive check.
var keywords = map[string]bool{
	"select": true, "from": true, "where": true, "insert": true, "into": true,
	"value": true, "update": true, "set": true, "remove": true, "delete": true,
	"and": true, "or": true, "not": true, "between": true, "in": true,
	"is": true, "missing": true, "null": true, "exists": true,
	"true": true, "false": true,
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

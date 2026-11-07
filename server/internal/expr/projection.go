package expr

import (
	"strings"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

// ParseProjection parses a ProjectionExpression: one or more paths separated
// by commas.
func ParseProjection(s string, env *Env) ([]Path, error) {
	if strings.TrimSpace(s) == "" {
		return nil, ddberr.Validation("Invalid ProjectionExpression: The expression can not be empty;")
	}
	p := newParser("ProjectionExpression", s, env)
	var paths []Path
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
	if p.cur().typ != tEOF {
		return nil, p.syntaxError()
	}
	return paths, nil
}

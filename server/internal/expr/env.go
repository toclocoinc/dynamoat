package expr

import (
	"sort"
	"strings"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

// Env carries the expression attribute placeholders for one request: Names
// maps #name to the real attribute name and Values holds the keys of
// ExpressionAttributeValues (":v"). Values stay as placeholders in the AST.
//
// One Env is shared by every expression of a request; CheckUnused runs after
// all of them are parsed, because DynamoDB reports unused placeholders across
// the whole request.
type Env struct {
	Names  map[string]string
	Values map[string]bool
	used   map[string]bool
}

func (e *Env) mark(key string) {
	if e.used == nil {
		e.used = map[string]bool{}
	}
	e.used[key] = true
}

// resolveName looks up a #name placeholder and records it as used.
func (e *Env) resolveName(param, key string) (string, error) {
	name, ok := e.Names[key]
	if !ok {
		return "", ddberr.Validation("Invalid %s: An expression attribute name used in the document path is not defined; attribute name: %s", param, key)
	}
	e.mark(key)
	return name, nil
}

// useValue records a :value placeholder as used, erroring when it is not
// defined.
func (e *Env) useValue(param, key string) error {
	if !e.Values[key] {
		return ddberr.Validation("Invalid %s: An expression attribute value used in expression is not defined; attribute value: %s", param, key)
	}
	e.mark(key)
	return nil
}

// CheckUnused reports placeholders provided but never consumed. Names are
// checked before values, matching DynamoDB.
func (e *Env) CheckUnused() error {
	if names := unusedKeys(e.Names, e.used); len(names) > 0 {
		return ddberr.Validation("1 validation error detected: Value provided in ExpressionAttributeNames unused in expressions: keys: {%s}", strings.Join(names, ", "))
	}
	if values := unusedKeys(e.Values, e.used); len(values) > 0 {
		return ddberr.Validation("1 validation error detected: Value provided in ExpressionAttributeValues unused in expressions: keys: {%s}", strings.Join(values, ", "))
	}
	return nil
}

func unusedKeys[T any](m map[string]T, used map[string]bool) []string {
	var out []string
	for k := range m {
		if !used[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

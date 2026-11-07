package expr

import (
	"strings"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

// keySide is one side of a key condition AND: a partition or sort key
// comparison. op is "=" for the partition key, or one of < <= > >= BETWEEN
// begins_with for the sort key.
type keySide struct {
	name string
	op   string
	vals []string
}

func keyCondUnsupported() *ddberr.Error {
	return ddberr.Validation("Query key condition not supported")
}

// ParseKeyCondition parses a KeyConditionExpression.
//
// The grammar is `pk = :v` optionally followed by AND and one sort key
// condition (`sk op :v` for op in < <= > >=, `sk BETWEEN :a AND :b`, or
// `begins_with(sk, :v)`); either side of the AND may come first and names may
// be #placeholders.
func ParseKeyCondition(s string, env *Env) (KeyCond, error) {
	if strings.TrimSpace(s) == "" {
		return KeyCond{}, ddberr.Validation("Invalid KeyConditionExpression: The expression can not be empty;")
	}
	p := newParser("KeyConditionExpression", s, env)

	var pk, sk *keySide
	first, err := p.parseKeySide()
	if err != nil {
		return KeyCond{}, err
	}
	if err := assignKeySide(&pk, &sk, first); err != nil {
		return KeyCond{}, err
	}
	if p.cur().typ == tAnd {
		p.advance()
		second, err := p.parseKeySide()
		if err != nil {
			return KeyCond{}, err
		}
		if err := assignKeySide(&pk, &sk, second); err != nil {
			return KeyCond{}, err
		}
	}
	if p.cur().typ != tEOF {
		return KeyCond{}, keyCondUnsupported()
	}
	if pk == nil {
		return KeyCond{}, keyCondUnsupported()
	}
	kc := KeyCond{PK: pk.name, PKValue: pk.vals[0]}
	if sk != nil {
		if sk.name == pk.name {
			return KeyCond{}, keyCondUnsupported()
		}
		kc.SK = sk.name
		kc.SKOp = sk.op
		kc.SKValues = sk.vals
	}
	return kc, nil
}

// assignKeySide records a side as the partition key (op "=") or the sort key
// (anything else). DynamoDB also accepts `sk = :v`, so a second "=" side is
// the sort key; which of two "=" sides is really the partition key is only
// known from the key schema, and ForSchema settles it.
func assignKeySide(pk, sk **keySide, s keySide) error {
	if s.op == "=" && *pk == nil {
		*pk = &s
		return nil
	}
	if *sk != nil {
		return keyCondUnsupported()
	}
	*sk = &s
	return nil
}

// ForSchema checks the key condition against the queried table's or index's
// key schema (rangeKey is "" for a hash-only key) and returns it with PK and
// SK the right way round: `sk = :a AND pk = :b` parses with the names swapped.
func (k KeyCond) ForSchema(hashKey, rangeKey string) (KeyCond, error) {
	if k.SKOp == "=" && k.PK != hashKey && k.SK == hashKey {
		k.PK, k.SK = k.SK, k.PK
		k.PKValue, k.SKValues = k.SKValues[0], []string{k.PKValue}
	}
	if k.PK != hashKey {
		return KeyCond{}, ddberr.Validation("Query condition missed key schema element: %s", hashKey)
	}
	if k.SK != "" && rangeKey == "" {
		return KeyCond{}, keyCondUnsupported()
	}
	if k.SK != "" && k.SK != rangeKey {
		return KeyCond{}, ddberr.Validation("Query condition missed key schema element: %s", rangeKey)
	}
	return k, nil
}

func (p *parser) parseKeySide() (keySide, error) {
	// begins_with(name, :v)
	if p.cur().typ == tName && p.cur().text == "begins_with" && p.peek().typ == tLParen {
		p.advance() // begins_with
		p.advance() // (
		name, err := p.parseKeyName()
		if err != nil {
			return keySide{}, err
		}
		if p.cur().typ != tComma {
			return keySide{}, keyCondUnsupported()
		}
		p.advance()
		if p.cur().typ != tValue {
			return keySide{}, keyCondUnsupported()
		}
		v, err := p.parseKeyValue()
		if err != nil {
			return keySide{}, err
		}
		if p.cur().typ != tRParen {
			return keySide{}, keyCondUnsupported()
		}
		p.advance()
		return keySide{name: name, op: "begins_with", vals: []string{v}}, nil
	}

	// name op :v  or  name BETWEEN :a AND :b
	name, err := p.parseKeyName()
	if err != nil {
		return keySide{}, err
	}
	switch p.cur().typ {
	case tEq, tLt, tLe, tGt, tGe:
		op := p.cur().text
		p.advance()
		v, err := p.parseKeyValue()
		if err != nil {
			return keySide{}, err
		}
		return keySide{name: name, op: op, vals: []string{v}}, nil
	case tBetween:
		p.advance()
		a, err := p.parseKeyValue()
		if err != nil {
			return keySide{}, err
		}
		if p.cur().typ != tAnd {
			return keySide{}, keyCondUnsupported()
		}
		p.advance()
		b, err := p.parseKeyValue()
		if err != nil {
			return keySide{}, err
		}
		return keySide{name: name, op: "BETWEEN", vals: []string{a, b}}, nil
	}
	return keySide{}, keyCondUnsupported()
}

// parseKeyName parses a single (non-nested) key attribute name or
// #placeholder. Anything else — a value, a nested path, a parenthesis — is a
// shape error.
func (p *parser) parseKeyName() (string, error) {
	switch p.cur().typ {
	case tName:
		word := p.cur().text
		if reserved[strings.ToLower(word)] {
			return "", ddberr.Validation("Invalid %s: Attribute name is a reserved keyword; reserved keyword: %s", p.param, word)
		}
		p.advance()
		return word, nil
	case tHashName:
		key := p.cur().text
		p.advance()
		return p.env.resolveName(p.param, key)
	}
	return "", keyCondUnsupported()
}

// parseKeyValue parses a :value placeholder on the right of a key comparison.
func (p *parser) parseKeyValue() (string, error) {
	if p.cur().typ != tValue {
		return "", keyCondUnsupported()
	}
	key := p.cur().text
	p.advance()
	if err := p.env.useValue(p.param, key); err != nil {
		return "", err
	}
	return key, nil
}

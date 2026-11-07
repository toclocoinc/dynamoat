package attr

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

const (
	emptyMessage = "Supplied AttributeValue is empty, must contain exactly one of the supported datatypes"
	multiMessage = "Supplied AttributeValue has more than one datatypes set, must contain exactly one of the supported datatypes"
)

// MarshalJSON writes the value in DynamoDB JSON form: a single-key object such
// as {"S":"x"} or {"N":"5"}. N and NS are emitted in canonical form.
func (v Value) MarshalJSON() ([]byte, error) {
	var obj map[string]any
	switch v.Type {
	case TypeS:
		obj = map[string]any{"S": v.S}
	case TypeN:
		obj = map[string]any{"N": v.N}
	case TypeB:
		obj = map[string]any{"B": base64.StdEncoding.EncodeToString(v.B)}
	case TypeBOOL:
		obj = map[string]any{"BOOL": v.Bool}
	case TypeNULL:
		obj = map[string]any{"NULL": true}
	case TypeM:
		obj = map[string]any{"M": v.M}
	case TypeL:
		obj = map[string]any{"L": v.L}
	case TypeSS:
		obj = map[string]any{"SS": v.SS}
	case TypeNS:
		obj = map[string]any{"NS": v.NS}
	case TypeBS:
		bs := make([]string, len(v.BS))
		for i, b := range v.BS {
			bs[i] = base64.StdEncoding.EncodeToString(b)
		}
		obj = map[string]any{"BS": bs}
	default:
		return nil, fmt.Errorf("attr: cannot marshal unknown type %d", v.Type)
	}
	return json.Marshal(obj)
}

// UnmarshalJSON parses a DynamoDB JSON AttributeValue and validates it. It
// returns a *ddberr.Error so errors.As finds it after json.Unmarshal.
func (v *Value) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return typeMismatch()
	}

	var key string
	var val json.RawMessage
	count := 0
	for k, rv := range raw {
		if !isTypeKey(k) {
			continue
		}
		count++
		key = k
		val = rv
	}
	if count == 0 {
		return ddberr.Validation(emptyMessage)
	}
	if count > 1 {
		return ddberr.Validation(multiMessage)
	}

	*v = Value{}
	switch key {
	case "S":
		var s string
		if err := json.Unmarshal(val, &s); err != nil {
			return typeMismatch()
		}
		v.Type = TypeS
		v.S = s
	case "N":
		var n string
		if err := json.Unmarshal(val, &n); err != nil {
			return typeMismatch()
		}
		canon, err := NormalizeNumber(n)
		if err != nil {
			return err
		}
		v.Type = TypeN
		v.N = canon
	case "B":
		var s string
		if err := json.Unmarshal(val, &s); err != nil {
			return typeMismatch()
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return invalidBase64()
		}
		v.Type = TypeB
		v.B = b
	case "BOOL":
		var b bool
		if err := json.Unmarshal(val, &b); err != nil {
			return typeMismatch()
		}
		v.Type = TypeBOOL
		v.Bool = b
	case "NULL":
		var b bool
		if err := json.Unmarshal(val, &b); err != nil {
			return typeMismatch()
		}
		if !b {
			return ddberr.Validation("One or more parameter values were invalid: Null attribute value types must have the true value")
		}
		v.Type = TypeNULL
	case "M":
		var m map[string]Value
		if err := json.Unmarshal(val, &m); err != nil {
			return err
		}
		v.Type = TypeM
		v.M = m
	case "L":
		var l []Value
		if err := json.Unmarshal(val, &l); err != nil {
			return err
		}
		v.Type = TypeL
		v.L = l
	case "SS":
		var ss []string
		if err := json.Unmarshal(val, &ss); err != nil {
			return typeMismatch()
		}
		if len(ss) == 0 {
			return ddberr.Validation("One or more parameter values were invalid: An string set  may not be empty")
		}
		if hasDupStrings(ss) {
			return dupMessage()
		}
		v.Type = TypeSS
		v.SS = ss
	case "NS":
		var ns []string
		if err := json.Unmarshal(val, &ns); err != nil {
			return typeMismatch()
		}
		if len(ns) == 0 {
			return ddberr.Validation("One or more parameter values were invalid: An number set  may not be empty")
		}
		canon := make([]string, len(ns))
		seen := make(map[string]bool, len(ns))
		for i, s := range ns {
			c, err := NormalizeNumber(s)
			if err != nil {
				return err
			}
			if seen[c] {
				return dupMessage()
			}
			seen[c] = true
			canon[i] = c
		}
		v.Type = TypeNS
		v.NS = canon
	case "BS":
		var bs []string
		if err := json.Unmarshal(val, &bs); err != nil {
			return typeMismatch()
		}
		if len(bs) == 0 {
			return ddberr.Validation("One or more parameter values were invalid: An binary set  may not be empty")
		}
		out := make([][]byte, len(bs))
		seen := make(map[string]bool, len(bs))
		for i, s := range bs {
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return invalidBase64()
			}
			if seen[string(b)] {
				return dupMessage()
			}
			seen[string(b)] = true
			out[i] = b
		}
		v.Type = TypeBS
		v.BS = out
	}
	return nil
}

// MarshalJSON writes an item as an object of attribute name to value.
func (it Item) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]Value(it))
}

// UnmarshalJSON parses an item; nested value errors propagate as *ddberr.Error.
func (it *Item) UnmarshalJSON(data []byte) error {
	var m map[string]Value
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*it = m
	return nil
}

func isTypeKey(k string) bool {
	switch k {
	case "S", "N", "B", "BOOL", "NULL", "M", "L", "SS", "NS", "BS":
		return true
	}
	return false
}

func typeMismatch() error {
	return ddberr.Validation("Start of structure or map found where not expected.")
}

func invalidBase64() error {
	return ddberr.Validation("One or more parameter values were invalid: Invalid base64 encoded binary value")
}

func dupMessage() error {
	return ddberr.Validation("One or more parameter values were invalid: Input collection contains duplicates")
}

func hasDupStrings(ss []string) bool {
	seen := make(map[string]bool, len(ss))
	for _, s := range ss {
		if seen[s] {
			return true
		}
		seen[s] = true
	}
	return false
}

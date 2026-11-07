// Package attr models DynamoDB attribute values in memory: the typed value,
// its DynamoDB JSON form, number validation and normalisation, the
// order-preserving byte encoding used for keys, and the item size rules.
package attr

// Type is the DynamoDB attribute value data type.
type Type uint8

const (
	TypeS Type = iota + 1
	TypeN
	TypeB
	TypeBOOL
	TypeNULL
	TypeM
	TypeL
	TypeSS
	TypeNS
	TypeBS
)

// String returns the single-letter DynamoDB type name as it appears in the
// JSON form of an AttributeValue.
func (t Type) String() string {
	switch t {
	case TypeS:
		return "S"
	case TypeN:
		return "N"
	case TypeB:
		return "B"
	case TypeBOOL:
		return "BOOL"
	case TypeNULL:
		return "NULL"
	case TypeM:
		return "M"
	case TypeL:
		return "L"
	case TypeSS:
		return "SS"
	case TypeNS:
		return "NS"
	case TypeBS:
		return "BS"
	}
	return ""
}

// Value is one DynamoDB attribute value. Only the field matching Type is
// meaningful. N and the members of NS are always in canonical form (see
// NormalizeNumber); SS and NS keep request order.
type Value struct {
	Type Type
	S    string
	N    string
	B    []byte
	Bool bool
	M    map[string]Value
	L    []Value
	SS   []string
	NS   []string
	BS   [][]byte
}

// Item is one DynamoDB item: an attribute name to attribute value map.
type Item map[string]Value

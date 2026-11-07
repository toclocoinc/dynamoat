package attr

import "github.com/toclocoinc/dynamoat/server/internal/ddberr"

// MaxItemSize is the largest item DynamoDB accepts, in bytes.
const MaxItemSize = 400 * 1024

// ValueSize returns the DynamoDB item-size contribution of a single attribute
// value, following DynamoDB's documented rules.
func ValueSize(v Value) int {
	switch v.Type {
	case TypeS:
		return len(v.S)
	case TypeN:
		digits, _ := numberDigits(v.N)
		return (len(digits)+1)/2 + 1
	case TypeB:
		return len(v.B)
	case TypeBOOL, TypeNULL:
		return 1
	case TypeSS:
		n := 0
		for _, s := range v.SS {
			n += len(s)
		}
		return n
	case TypeNS:
		n := 0
		for _, s := range v.NS {
			digits, _ := numberDigits(s)
			n += (len(digits)+1)/2 + 1
		}
		return n
	case TypeBS:
		n := 0
		for _, b := range v.BS {
			n += len(b)
		}
		return n
	case TypeM:
		n := 3
		for k, mv := range v.M {
			n += len(k) + ValueSize(mv) + 1
		}
		return n
	case TypeL:
		n := 3
		for _, lv := range v.L {
			n += ValueSize(lv) + 1
		}
		return n
	}
	return 0
}

// ItemSize returns the size of an item: the sum over its attributes of the
// UTF-8 length of the name plus the value size.
func ItemSize(it Item) int {
	n := 0
	for name, v := range it {
		n += len(name) + ValueSize(v)
	}
	return n
}

// CheckItemSize reports an error when the item exceeds MaxItemSize.
func CheckItemSize(it Item) error {
	if ItemSize(it) > MaxItemSize {
		return ddberr.Validation("Item size has exceeded the maximum allowed size")
	}
	return nil
}

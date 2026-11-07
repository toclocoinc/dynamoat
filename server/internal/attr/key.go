package attr

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

// Key encoding markers. The leading byte separates negatives (0x40), zero
// (0x80) and positives (0xC0) so encoded keys sort in numeric order.
const (
	encNeg  = 0x40
	encZero = 0x80
	encPos  = 0xC0
)

const scalarKeyError = "One or more parameter values were invalid: Key attributes must be scalars; list, map, and set types are not allowed"

// EncodeKey encodes a scalar key value so that comparing the returned bytes
// with bytes.Compare matches how DynamoDB orders the values. Only S, N and B
// are allowed. N must already be in canonical form.
func EncodeKey(v Value) ([]byte, error) {
	switch v.Type {
	case TypeS:
		return []byte(v.S), nil
	case TypeB:
		return append([]byte(nil), v.B...), nil
	case TypeN:
		return encodeNumberKey(v.N), nil
	}
	return nil, ddberr.Validation(scalarKeyError)
}

func encodeNumberKey(n string) []byte {
	digits, exp := numberDigits(n)
	if digits == "" {
		return []byte{encZero}
	}
	if n[0] == '-' {
		out := make([]byte, 0, len(digits)+3)
		out = append(out, encNeg, byte(126-exp))
		for i := 0; i < len(digits); i++ {
			out = append(out, byte(10-(digits[i]-'0')))
		}
		return append(out, 0xFF)
	}
	out := make([]byte, 0, len(digits)+3)
	out = append(out, encPos, byte(exp+129))
	for i := 0; i < len(digits); i++ {
		out = append(out, byte(digits[i]-'0'+1))
	}
	return append(out, 0x00)
}

// DecodeKey is the inverse of EncodeKey for a known key type. N comes back in
// canonical form.
func DecodeKey(t Type, b []byte) (Value, error) {
	switch t {
	case TypeS:
		return Value{Type: TypeS, S: string(b)}, nil
	case TypeB:
		return Value{Type: TypeB, B: append([]byte(nil), b...)}, nil
	case TypeN:
		v, err := decodeNumberKey(b)
		if err != nil {
			return Value{}, err
		}
		return v, nil
	}
	return Value{}, ddberr.Validation(scalarKeyError)
}

func decodeNumberKey(b []byte) (Value, error) {
	if len(b) == 1 && b[0] == encZero {
		return Value{Type: TypeN, N: "0"}, nil
	}
	if len(b) < 3 {
		return Value{}, ddberr.Validation("Invalid number key encoding")
	}
	switch b[0] {
	case encPos:
		if b[len(b)-1] != 0x00 {
			return Value{}, ddberr.Validation("Invalid number key encoding")
		}
		exp := int(b[1]) - 129
		return Value{Type: TypeN, N: numberFromDigits(positiveDigits(b[2:len(b)-1]), exp, false)}, nil
	case encNeg:
		if b[len(b)-1] != 0xFF {
			return Value{}, ddberr.Validation("Invalid number key encoding")
		}
		exp := 126 - int(b[1])
		return Value{Type: TypeN, N: numberFromDigits(negativeDigits(b[2:len(b)-1]), exp, true)}, nil
	}
	return Value{}, ddberr.Validation("Invalid number key encoding")
}

func positiveDigits(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(byte(c-1) + '0')
	}
	return sb.String()
}

func negativeDigits(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(byte(10-c) + '0')
	}
	return sb.String()
}

// numberFromDigits rebuilds the canonical plain decimal for ±0.d1d2...dn x
// 10^exp, where digits holds d1..dn (no leading or trailing zeros) and exp is
// the encoding exponent E. plainDecimal takes the coefficient exponent, which
// is E - len(digits).
func numberFromDigits(digits string, exp int, neg bool) string {
	s := plainDecimal(digits, exp-len(digits))
	if neg {
		return "-" + s
	}
	return s
}

// KeyHash returns the first four bytes of SHA-256 over the encoded partition
// key, big-endian, as a signed int32.
func KeyHash(encodedPK []byte) int32 {
	sum := sha256.Sum256(encodedPK)
	return int32(binary.BigEndian.Uint32(sum[:4]))
}

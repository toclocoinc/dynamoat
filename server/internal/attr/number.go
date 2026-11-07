package attr

import (
	"strings"

	"github.com/cockroachdb/apd/v3"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

// NormalizeNumber validates s as a DynamoDB number and returns its canonical
// text: plain decimal notation, never an exponent; no leading zeros (a single
// "0" before the point is kept, as in "0.5"); no trailing zeros after the
// point and no trailing point; "-" for negatives; and "0" for every form of
// zero. Leading "+" is accepted. The canonical form is what DynamoDB echoes
// back and what EncodeKey, DecodeKey and ValueSize expect.
func NormalizeNumber(s string) (string, error) {
	var d apd.Decimal
	if _, _, err := d.SetString(s); err != nil {
		return "", notANumber(s)
	}
	if d.Form != apd.Finite {
		return "", notANumber(s)
	}
	if d.Coeff.Sign() == 0 {
		return "0", nil
	}

	// value = coeff x 10^exp, with coeff already free of leading zeros.
	coeff := d.Coeff.String()
	exp := int64(d.Exponent)
	for strings.HasSuffix(coeff, "0") {
		coeff = coeff[:len(coeff)-1]
		exp++
	}

	n := int64(len(coeff))
	if n > 38 {
		return "", ddberr.Validation("Attempting to store more than 38 significant digits in a Number")
	}
	// The largest representable magnitude is 9.99...9 (38 nines) x 10^125; the
	// smallest non-zero one is 1 x 10^-130. A number with n significant digits
	// and exponent exp lies in [10^(n+exp-1), 10^(n+exp)), which makes both
	// limits collapse to comparisons on n+exp.
	if n+exp > 126 {
		return "", ddberr.Validation("Number overflow. Attempting to store a number with magnitude larger than supported range")
	}
	if n+exp < -129 {
		return "", ddberr.Validation("Number underflow. Attempting to store a number with magnitude smaller than supported range")
	}

	out := plainDecimal(coeff, int(exp))
	if d.Negative {
		return "-" + out, nil
	}
	return out, nil
}

func notANumber(s string) error {
	return ddberr.Validation("The parameter cannot be converted to a numeric value: %s", s)
}

// CompareNumbers compares two canonical number strings and returns -1, 0 or 1.
func CompareNumbers(a, b string) int {
	var da, db apd.Decimal
	_, _, _ = da.SetString(a)
	_, _, _ = db.SetString(b)
	return da.Cmp(&db)
}

// plainDecimal writes digits x 10^exp in plain decimal notation. digits must
// have no leading or trailing zeros, and exp must be small enough that the
// result fits the caller's range.
func plainDecimal(digits string, exp int) string {
	if exp >= 0 {
		return digits + strings.Repeat("0", exp)
	}
	k := -exp
	if k >= len(digits) {
		return "0." + strings.Repeat("0", k-len(digits)) + digits
	}
	return digits[:len(digits)-k] + "." + digits[len(digits)-k:]
}

// numberDigits splits canonical number n into its significant digits and the
// exponent E such that n = ±0.d1d2...dn x 10^E (d1 != 0). It returns ("", 0)
// for zero. Callers must pass a plain-decimal (canonical) string.
func numberDigits(n string) (string, int) {
	if n[0] == '-' {
		n = n[1:]
	}
	if n == "0" {
		return "", 0
	}
	intPart, fracPart := n, ""
	if i := strings.IndexByte(n, '.'); i >= 0 {
		intPart, fracPart = n[:i], n[i+1:]
	}
	all := intPart + fracPart
	first := 0
	for first < len(all) && all[first] == '0' {
		first++
	}
	if first == len(all) {
		return "", 0
	}
	last := len(all) - 1
	for all[last] == '0' {
		last--
	}
	return all[first : last+1], len(intPart) - first
}

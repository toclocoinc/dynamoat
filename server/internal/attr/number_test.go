package attr

import (
	"errors"
	"strings"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

func TestNormalizeNumberExamples(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1.50", "1.5"},
		{"1e2", "100"},
		{"-0.0100", "-0.01"},
		{"007", "7"},
		{"1E-3", "0.001"},
		{"12345678901234567890123456789012345678", "12345678901234567890123456789012345678"},
		{"+5", "5"},
		{"0", "0"},
		{"-0", "0"},
		{"0.000", "0"},
		{"0e5", "0"},
		{"0.5", "0.5"},
		{"100", "100"},
		{"0.001", "0.001"},
		{"-0.01", "-0.01"},
		{"1.2300", "1.23"},
	}
	for _, c := range cases {
		got, err := NormalizeNumber(c.in)
		if err != nil {
			t.Errorf("NormalizeNumber(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeNumber(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeNumberNotANumber(t *testing.T) {
	for _, in := range []string{"", " 1", "1a", "NaN", "Infinity", "0x10"} {
		_, err := NormalizeNumber(in)
		want := "The parameter cannot be converted to a numeric value: " + in
		checkDDBErr(t, err, want)
	}
}

func TestNormalizeNumberBoundaries(t *testing.T) {
	// Exactly 38 significant digits is allowed.
	normalizeOK(t, strings.Repeat("1", 38), strings.Repeat("1", 38))
	// 39 significant digits is not, even with trailing zeros to strip.
	normalizeErr(t, strings.Repeat("1", 39), "Attempting to store more than 38 significant digits in a Number")
	normalizeErr(t, strings.Repeat("1", 39)+"000", "Attempting to store more than 38 significant digits in a Number")
	// Trailing zeros do not count toward the 38.
	normalizeOK(t, strings.Repeat("1", 38)+"000", strings.Repeat("1", 38)+"000")

	// Magnitude: just inside and just outside the upper limit.
	normalizeOK(t, "1e125", "1"+strings.Repeat("0", 125))
	normalizeErr(t, "1e126", "Number overflow. Attempting to store a number with magnitude larger than supported range")
	// The exact maximum magnitude: 9.999...9 (38 nines) x 10^125.
	normalizeOK(t, "9."+strings.Repeat("9", 37)+"E+125", strings.Repeat("9", 38)+strings.Repeat("0", 88))

	// Magnitude: just inside and just outside the lower limit.
	normalizeOK(t, "1e-130", "0."+strings.Repeat("0", 129)+"1")
	normalizeErr(t, "1e-131", "Number underflow. Attempting to store a number with magnitude smaller than supported range")
}

func TestCompareNumbers(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1", "2", -1},
		{"2", "1", 1},
		{"1", "1", 0},
		{"-1", "1", -1},
		{"0", "0", 0},
		{"0", "1", -1},
		{"0.5", "0.05", 1},
		{"100", "99", 1},
		{"-0.01", "-0.02", 1},
		{"0.001", "0.0010", 0},
	}
	for _, c := range cases {
		if got := CompareNumbers(c.a, c.b); got != c.want {
			t.Errorf("CompareNumbers(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func normalizeOK(t *testing.T, in, want string) {
	t.Helper()
	got, err := NormalizeNumber(in)
	if err != nil {
		t.Errorf("NormalizeNumber(%q) unexpected error: %v", in, err)
		return
	}
	if got != want {
		t.Errorf("NormalizeNumber(%q) = %q, want %q", in, got, want)
	}
}

func normalizeErr(t *testing.T, in, wantMsg string) {
	t.Helper()
	_, err := NormalizeNumber(in)
	checkDDBErr(t, err, wantMsg)
}

func checkDDBErr(t *testing.T, err error, wantMsg string) {
	t.Helper()
	var de *ddberr.Error
	if !errors.As(err, &de) {
		t.Errorf("error %v is not a *ddberr.Error", err)
		return
	}
	if de.Message != wantMsg {
		t.Errorf("message = %q, want %q", de.Message, wantMsg)
	}
}

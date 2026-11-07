package attr

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

func TestKeyHash(t *testing.T) {
	// SHA-256("abc") starts with 0xba7816bf; read big-endian as a signed int32.
	if got := KeyHash([]byte("abc")); got != -1166534977 {
		t.Errorf("KeyHash(abc) = %d, want -1166534977", got)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	ss := []string{"", "a", "abc", "日本語", "😀", "a\x00b", "\xff\xfe"}
	for _, s := range ss {
		b, err := EncodeKey(Value{Type: TypeS, S: s})
		if err != nil {
			t.Fatalf("EncodeKey(S %q): %v", s, err)
		}
		got, err := DecodeKey(TypeS, b)
		if err != nil {
			t.Fatalf("DecodeKey(S %q): %v", s, err)
		}
		if got.Type != TypeS || got.S != s {
			t.Errorf("round trip S %q -> %v", s, got)
		}
	}

	bins := [][]byte{nil, {}, {0}, {1, 2, 3}, {0xff, 0x00, 0x80}}
	for _, bs := range bins {
		b, err := EncodeKey(Value{Type: TypeB, B: bs})
		if err != nil {
			t.Fatalf("EncodeKey(B %v): %v", bs, err)
		}
		got, err := DecodeKey(TypeB, b)
		if err != nil {
			t.Fatalf("DecodeKey(B %v): %v", bs, err)
		}
		if got.Type != TypeB || !bytes.Equal(got.B, bs) {
			t.Errorf("round trip B %v -> %v", bs, got)
		}
	}

	nums := []string{
		"0", "1", "-1", "1.5", "-1.5", "100", "0.001", "-0.01",
		"12345678901234567890123456789012345678",
		"-12345678901234567890123456789012345678",
		"0.5", "10", "-10", "3.14159", "-3.14159",
	}
	for _, n := range nums {
		b, err := EncodeKey(Value{Type: TypeN, N: n})
		if err != nil {
			t.Fatalf("EncodeKey(N %q): %v", n, err)
		}
		got, err := DecodeKey(TypeN, b)
		if err != nil {
			t.Fatalf("DecodeKey(N %q): %v", n, err)
		}
		if got.Type != TypeN || got.N != n {
			t.Errorf("round trip N %q -> %v", n, got)
		}
	}
}

func TestEncodeKeyNonScalar(t *testing.T) {
	for _, v := range []Value{
		{Type: TypeM, M: map[string]Value{}},
		{Type: TypeL, L: []Value{}},
		{Type: TypeSS, SS: []string{"a"}},
		{Type: TypeNS, NS: []string{"1"}},
		{Type: TypeBS, BS: [][]byte{{1}}},
		{Type: TypeBOOL, Bool: true},
		{Type: TypeNULL},
	} {
		_, err := EncodeKey(v)
		checkDDBErr(t, err, "One or more parameter values were invalid: Key attributes must be scalars; list, map, and set types are not allowed")
	}
}

func TestNumberKeyOrdering(t *testing.T) {
	r := rand.New(rand.NewSource(1))

	// Zero and the magnitude limits, so they are compared against random
	// neighbours too.
	nums := []string{
		"0",
		plainDecimal("1", 125), // 10^125
		plainDecimal(strings.Repeat("9", 38), 88), // maximum magnitude
		plainDecimal("1", -130),                   // 10^-130, minimum magnitude
		"-" + plainDecimal("1", 125),
		"-" + plainDecimal(strings.Repeat("9", 38), 88),
		"-" + plainDecimal("1", -130),
	}
	for i := 0; i < 20000; i++ {
		nums = append(nums, randomNumber(r))
	}

	// Adjacent pairs are >= 20,000 random pairs; with mixed signs and the full
	// exponent range every ordering boundary is exercised.
	for i := 0; i < len(nums); i++ {
		checkNumberPair(t, nums[i], nums[(i+1)%len(nums)])
	}
}

func TestStringKeyOrdering(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	strs := []string{"", "a", "b", "aa", "ab", "\x00", "\xff", "日", "😀", "é"}
	for i := 0; i < 20000; i++ {
		strs = append(strs, randomUTF8(r))
	}
	for i := 0; i < len(strs); i++ {
		checkStringPair(t, strs[i], strs[(i+1)%len(strs)])
	}
}

func randomNumber(r *rand.Rand) string {
	n := r.Intn(38) + 1             // 1..38 significant digits
	e := r.Intn(126-(-129)+1) - 129 // encoding exponent in -129..126
	digits := make([]byte, n)
	digits[0] = byte(r.Intn(9)) + '1'
	for i := 1; i < n-1; i++ {
		digits[i] = byte(r.Intn(10)) + '0'
	}
	if n > 1 {
		digits[n-1] = byte(r.Intn(9)) + '1'
	}
	s := plainDecimal(string(digits), e-n)
	if r.Intn(2) == 0 {
		s = "-" + s
	}
	return s
}

func randomUTF8(r *rand.Rand) string {
	runes := []rune{'a', 'b', 'c', '0', '1', '日', 'é', '😀', 'ÿ', '€', 'z', ' '}
	var sb strings.Builder
	for i := 0; i < r.Intn(20); i++ {
		sb.WriteRune(runes[r.Intn(len(runes))])
	}
	return sb.String()
}

func checkNumberPair(t *testing.T, a, b string) {
	t.Helper()
	ea, err := EncodeKey(Value{Type: TypeN, N: a})
	if err != nil {
		t.Fatalf("EncodeKey(%q): %v", a, err)
	}
	eb, err := EncodeKey(Value{Type: TypeN, N: b})
	if err != nil {
		t.Fatalf("EncodeKey(%q): %v", b, err)
	}
	want := sign(CompareNumbers(a, b))
	if got := sign(bytes.Compare(ea, eb)); got != want {
		t.Fatalf("number ordering mismatch: %q vs %q: encoded cmp %d, numeric cmp %d", a, b, got, want)
	}
}

func checkStringPair(t *testing.T, a, b string) {
	t.Helper()
	ea, err := EncodeKey(Value{Type: TypeS, S: a})
	if err != nil {
		t.Fatalf("EncodeKey(%q): %v", a, err)
	}
	eb, err := EncodeKey(Value{Type: TypeS, S: b})
	if err != nil {
		t.Fatalf("EncodeKey(%q): %v", b, err)
	}
	want := sign(strings.Compare(a, b))
	if got := sign(bytes.Compare(ea, eb)); got != want {
		t.Fatalf("string ordering mismatch: %q vs %q: encoded cmp %d, string cmp %d", a, b, got, want)
	}
}

func sign(i int) int {
	switch {
	case i < 0:
		return -1
	case i > 0:
		return 1
	}
	return 0
}

package attr

import (
	"strings"
	"testing"
)

func TestValueSize(t *testing.T) {
	cases := []struct {
		name string
		v    Value
		want int
	}{
		// S is the UTF-8 byte length.
		{"S ascii", Value{Type: TypeS, S: "abc"}, 3},
		{"S multibyte", Value{Type: TypeS, S: "é"}, 2}, // é is two UTF-8 bytes
		{"S empty", Value{Type: TypeS, S: ""}, 0},

		// N is (significant digits + 1)/2 + 1, integer division.
		{"N zero", Value{Type: TypeN, N: "0"}, 1},      // 0 digits -> (0+1)/2+1 = 1
		{"N one digit", Value{Type: TypeN, N: "5"}, 2}, // (1+1)/2+1 = 2
		{"N two digits", Value{Type: TypeN, N: "12"}, 2},
		{"N three digits", Value{Type: TypeN, N: "123"}, 3},
		{"N four digits", Value{Type: TypeN, N: "1234"}, 3},
		{"N trailing zero not significant", Value{Type: TypeN, N: "100"}, 2}, // 1 significant digit

		// B is the byte length.
		{"B", Value{Type: TypeB, B: []byte{1, 2, 3}}, 3},
		{"B empty", Value{Type: TypeB, B: nil}, 0},

		// BOOL and NULL are always one byte.
		{"BOOL", Value{Type: TypeBOOL, Bool: true}, 1},
		{"NULL", Value{Type: TypeNULL}, 1},

		// Sets are the sum of their members' sizes.
		{"SS", Value{Type: TypeSS, SS: []string{"a", "bc"}}, 3},   // 1 + 2
		{"NS", Value{Type: TypeNS, NS: []string{"5", "12"}}, 4},   // 2 + 2
		{"BS", Value{Type: TypeBS, BS: [][]byte{{1}, {2, 3}}}, 3}, // 1 + 2

		// M is 3 + sum over entries of len(name) + size + 1.
		{"M", Value{Type: TypeM, M: map[string]Value{"a": {Type: TypeS, S: "x"}}}, 6},
		//  3 + (len("a")=1 + size("x")=1 + 1) = 6

		// L is 3 + sum over elements of size + 1.
		{"L", Value{Type: TypeL, L: []Value{{Type: TypeN, N: "5"}}}, 6},
		//  3 + (size("5")=2 + 1) = 6
	}
	for _, c := range cases {
		if got := ValueSize(c.v); got != c.want {
			t.Errorf("ValueSize(%s) = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestItemSize(t *testing.T) {
	it := Item{
		"a":   {Type: TypeS, S: "x"}, // len("a")=1 + size("x")=1 = 2
		"num": {Type: TypeN, N: "5"}, // len("num")=3 + size("5")=2 = 5
	}
	if got := ItemSize(it); got != 7 {
		t.Errorf("ItemSize = %d, want 7", got)
	}
}

func TestCheckItemSize(t *testing.T) {
	atLimit := Item{"": {Type: TypeS, S: strings.Repeat("x", MaxItemSize)}}
	if err := CheckItemSize(atLimit); err != nil {
		t.Errorf("CheckItemSize(at limit) = %v, want nil", err)
	}
	over := Item{"": {Type: TypeS, S: strings.Repeat("x", MaxItemSize+1)}}
	checkDDBErr(t, CheckItemSize(over), "Item size has exceeded the maximum allowed size")
}

package expr

import "testing"

// list_append(if_not_exists(l, :empty), :new) is a common DynamoDB idiom.
func TestUpdateNestedFunctions(t *testing.T) {
	for _, in := range []string{
		`SET l = list_append(if_not_exists(l, :empty), :new)`,
		`SET n = if_not_exists(n, :zero) + :inc`,
	} {
		env := &Env{Values: map[string]bool{":empty": true, ":new": true, ":zero": true, ":inc": true}}
		u, err := ParseUpdate(in, env)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := StringUpdate(u); got != in {
			t.Errorf("%s: printed back as %s", in, got)
		}
	}
}

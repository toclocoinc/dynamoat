package expr

import (
	"errors"
	"reflect"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

// DynamoDB accepts an equality on the sort key; with two "=" sides only the
// key schema says which is the partition key.
func TestKeyConditionSortKeyEquality(t *testing.T) {
	want := KeyCond{PK: "pk", PKValue: ":p", SK: "sk", SKOp: "=", SKValues: []string{":s"}}
	for _, s := range []string{`pk = :p AND sk = :s`, `sk = :s AND pk = :p`} {
		env := &Env{Values: map[string]bool{":p": true, ":s": true}}
		kc, err := ParseKeyCondition(s, env)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		got, err := kc.ForSchema("pk", "sk")
		if err != nil {
			t.Fatalf("%s: ForSchema: %v", s, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v, want %+v", s, got, want)
		}
	}
}

func TestKeyConditionForSchema(t *testing.T) {
	cases := []struct {
		expr, hash, rng, msg string
	}{
		{`pk = :p`, "pk", "sk", ""},
		{`pk = :p AND begins_with(sk, :s)`, "pk", "sk", ""},
		{`notkey = :p`, "pk", "sk", "Query condition missed key schema element: pk"},
		{`pk = :p AND notkey > :s`, "pk", "sk", "Query condition missed key schema element: sk"},
		{`pk = :p AND sk > :s`, "pk", "", "Query key condition not supported"},
		{`sk = :s AND notkey = :p`, "pk", "sk", "Query condition missed key schema element: pk"},
	}
	for _, c := range cases {
		env := &Env{Values: map[string]bool{":p": true, ":s": true}}
		kc, err := ParseKeyCondition(c.expr, env)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		_, err = kc.ForSchema(c.hash, c.rng)
		if c.msg == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", c.expr, err)
			}
			continue
		}
		var de *ddberr.Error
		if !errors.As(err, &de) || de.Message != c.msg {
			t.Errorf("%s: error %v, want %q", c.expr, err, c.msg)
		}
	}
}

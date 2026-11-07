package attr

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

func TestJSONRoundTrip(t *testing.T) {
	in := `{"id":{"S":"c1"},"points":{"N":"12345678901234567890123456789012345678"},"p2":{"S":"5"},"tags":{"SS":["a","b"]},"bin":{"B":"AAEC"},"m":{"M":{"x":{"L":[{"N":"1"},{"NULL":true},{"BOOL":false}]}}}}`
	var it Item
	if err := json.Unmarshal([]byte(in), &it); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(it)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var a, b any
	if err := json.Unmarshal([]byte(in), &a); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("round trip not equal as JSON:\n in: %s\nout: %s", in, out)
	}
}

func TestMarshalValues(t *testing.T) {
	cases := []struct {
		v    Value
		want string
	}{
		{Value{Type: TypeS, S: "x"}, `{"S":"x"}`},
		{Value{Type: TypeN, N: "5"}, `{"N":"5"}`},
		{Value{Type: TypeB, B: []byte{0, 1, 2}}, `{"B":"AAEC"}`},
		{Value{Type: TypeBOOL, Bool: true}, `{"BOOL":true}`},
		{Value{Type: TypeBOOL, Bool: false}, `{"BOOL":false}`},
		{Value{Type: TypeNULL}, `{"NULL":true}`},
		{Value{Type: TypeM, M: map[string]Value{"a": {Type: TypeS, S: "b"}}}, `{"M":{"a":{"S":"b"}}}`},
		{Value{Type: TypeL, L: []Value{{Type: TypeN, N: "1"}}}, `{"L":[{"N":"1"}]}`},
		{Value{Type: TypeSS, SS: []string{"a", "b"}}, `{"SS":["a","b"]}`},
		{Value{Type: TypeNS, NS: []string{"1", "2"}}, `{"NS":["1","2"]}`},
		{Value{Type: TypeBS, BS: [][]byte{{0, 1, 2}}}, `{"BS":["AAEC"]}`},
	}
	for _, c := range cases {
		out, err := json.Marshal(c.v)
		if err != nil {
			t.Fatalf("marshal %+v: %v", c.v, err)
		}
		var a, b any
		if err := json.Unmarshal([]byte(c.want), &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(out, &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("marshal %+v = %s, want %s", c.v, out, c.want)
		}
	}
}

func TestUnmarshalValues(t *testing.T) {
	cases := []struct {
		in   string
		want Value
	}{
		{`{"S":"x"}`, Value{Type: TypeS, S: "x"}},
		{`{"N":"1.50"}`, Value{Type: TypeN, N: "1.5"}},
		{`{"B":"AAEC"}`, Value{Type: TypeB, B: []byte{0, 1, 2}}},
		{`{"BOOL":true}`, Value{Type: TypeBOOL, Bool: true}},
		{`{"BOOL":false}`, Value{Type: TypeBOOL, Bool: false}},
		{`{"NULL":true}`, Value{Type: TypeNULL}},
		{`{"M":{"a":{"S":"b"}}}`, Value{Type: TypeM, M: map[string]Value{"a": {Type: TypeS, S: "b"}}}},
		{`{"L":[{"N":"1"}]}`, Value{Type: TypeL, L: []Value{{Type: TypeN, N: "1"}}}},
		{`{"SS":["a","b"]}`, Value{Type: TypeSS, SS: []string{"a", "b"}}},
		{`{"NS":["1","2.0"]}`, Value{Type: TypeNS, NS: []string{"1", "2"}}},
		{`{"BS":["AAEC"]}`, Value{Type: TypeBS, BS: [][]byte{{0, 1, 2}}}},
	}
	for _, c := range cases {
		var v Value
		if err := json.Unmarshal([]byte(c.in), &v); err != nil {
			t.Errorf("unmarshal %s: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(v, c.want) {
			t.Errorf("unmarshal %s = %+v, want %+v", c.in, v, c.want)
		}
	}
}

func TestUnmarshalErrors(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{}`, "Supplied AttributeValue is empty, must contain exactly one of the supported datatypes"},
		{`{"S":"x","N":"1"}`, "Supplied AttributeValue has more than one datatypes set, must contain exactly one of the supported datatypes"},
		{`{"X":"1"}`, "Supplied AttributeValue is empty, must contain exactly one of the supported datatypes"},
		{`{"SS":[]}`, "One or more parameter values were invalid: An string set  may not be empty"},
		{`{"NS":[]}`, "One or more parameter values were invalid: An number set  may not be empty"},
		{`{"BS":[]}`, "One or more parameter values were invalid: An binary set  may not be empty"},
		{`{"N":"1a"}`, "The parameter cannot be converted to a numeric value: 1a"},
		{`{"N":"NaN"}`, "The parameter cannot be converted to a numeric value: NaN"},
		{`{"N":"123456789012345678901234567890123456789"}`, "Attempting to store more than 38 significant digits in a Number"},
		{`{"N":"1e126"}`, "Number overflow. Attempting to store a number with magnitude larger than supported range"},
		{`{"N":"1e-131"}`, "Number underflow. Attempting to store a number with magnitude smaller than supported range"},
	}
	for _, c := range cases {
		var v Value
		err := json.Unmarshal([]byte(c.in), &v)
		var de *ddberr.Error
		if !errors.As(err, &de) {
			t.Errorf("%s: error is not a *ddberr.Error: %v", c.in, err)
			continue
		}
		if de.Message != c.want {
			t.Errorf("%s: message = %q, want %q", c.in, de.Message, c.want)
		}
	}
}

func TestUnmarshalErrorIsDDBError(t *testing.T) {
	var v Value
	err := json.Unmarshal([]byte(`{"N":"1a"}`), &v)
	var de *ddberr.Error
	if !errors.As(err, &de) {
		t.Fatalf("errors.As did not find *ddberr.Error in %v (%T)", err, err)
	}
	if de.Code() != "ValidationException" {
		t.Errorf("code = %q, want ValidationException", de.Code())
	}
}

func TestUnmarshalUnconfirmedErrors(t *testing.T) {
	// Only the error code is asserted; the exact text is unconfirmed.
	cases := []string{
		`{"SS":["a","a"]}`,
		`{"NS":["1","1.0"]}`,
		`{"NS":["1.5","1.50"]}`,
		`{"BS":["AAEC","AAEC"]}`,
		`{"NULL":false}`,
		`{"B":"!!not-base64!!"}`,
	}
	for _, in := range cases {
		var v Value
		err := json.Unmarshal([]byte(in), &v)
		var de *ddberr.Error
		if !errors.As(err, &de) {
			t.Errorf("%s: error is not a *ddberr.Error: %v", in, err)
			continue
		}
		if de.Code() != "ValidationException" {
			t.Errorf("%s: code = %q, want ValidationException", in, de.Code())
		}
	}
}

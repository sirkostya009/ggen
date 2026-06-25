package integrationtests

//go:generate ../ggen $GOFILE

// Coverage for the `any` field kind — stdlib-default float64 numbers and
// the usenumber opt-in (json.Number).

import (
	"encoding/json"
	"testing"

	"github.com/sirkostya009/ggen/encode"
)

// AnyStruct: bare `any` field, stdlib-default float64 numbers.
//
//ggen:generate
type AnyStruct struct {
	Name string `json:"name"`
	Body any    `json:"body"`
}

// AnyNumberStruct: same shape, usenumber → json.Number (exact digits).
//
//ggen:generate usenumber
type AnyNumberStruct struct {
	Name string `json:"name"`
	Body any    `json:"body"`
}

func TestAny_DecodeObject(t *testing.T) {
	in := []byte(`{"name":"x","body":{"k":1,"l":[1,2,3]}}`)
	got, _, err := AnyStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m, ok := got.Body.(map[string]any)
	if !ok {
		t.Fatalf("Body type = %T, want map[string]any", got.Body)
	}
	if m["k"].(float64) != 1 {
		t.Errorf("Body.k = %v", m["k"])
	}
}

func TestAny_DecodeScalars(t *testing.T) {
	cases := []struct {
		json string
		want any
	}{
		{`{"name":"x","body":42}`, float64(42)},
		{`{"name":"x","body":"hello"}`, "hello"},
		{`{"name":"x","body":true}`, true},
		{`{"name":"x","body":null}`, nil},
	}
	for _, tc := range cases {
		got, _, err := AnyStruct{}.DecodeFrom([]byte(tc.json))
		if err != nil {
			t.Fatalf("unmarshal %s: %v", tc.json, err)
		}
		if got.Body != tc.want {
			t.Errorf("Body = %v, want %v", got.Body, tc.want)
		}
	}
}

func TestAny_MarshalRoundtrip(t *testing.T) {
	in := AnyStruct{Name: "x", Body: map[string]any{"k": float64(1), "v": "y"}}
	out, _ := encode.Marshal(in)
	got, _, err := AnyStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	gotMap := got.Body.(map[string]any)
	if gotMap["k"].(float64) != 1 || gotMap["v"].(string) != "y" {
		t.Errorf("Body = %+v", gotMap)
	}
}

func TestAnyNumber_Preservesint64Precision(t *testing.T) {
	// 9007199254740993 = 2^53 + 1 — loses precision when round-tripped via float64.
	in := []byte(`{"name":"x","body":9007199254740993}`)
	got, _, err := AnyNumberStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	num, ok := got.Body.(json.Number)
	if !ok {
		t.Fatalf("Body type = %T, want json.Number", got.Body)
	}
	if string(num) != "9007199254740993" {
		t.Errorf("Body = %q, want exact digits preserved", num)
	}
	n, err := num.Int64()
	if err != nil || n != 9007199254740993 {
		t.Errorf("Int64() = (%d, %v), want (9007199254740993, nil)", n, err)
	}
}

func TestAnyNumber_NestedShape(t *testing.T) {
	in := []byte(`{"name":"x","body":{"k":[1,2.5,3],"big":12345678901234567}}`)
	got, _, err := AnyNumberStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m := got.Body.(map[string]any)
	if _, ok := m["big"].(json.Number); !ok {
		t.Errorf("nested number = %T, want json.Number", m["big"])
	}
	arr := m["k"].([]any)
	if _, ok := arr[1].(json.Number); !ok {
		t.Errorf("array elem = %T, want json.Number", arr[1])
	}
}

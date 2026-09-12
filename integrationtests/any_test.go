package integrationtests

//go:generate ../ggen $GOFILE

// Coverage for the `any` field kind — default float64 numbers and the
// usenumber opt-in (json.Number).

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/sirkostya009/ggen"
)

// AnyStruct: bare `any` field, default float64 numbers.
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	in := AnyStruct{Name: "x", Body: map[string]any{"k": float64(1), "v": "y"}}
	out, _ := ggen.Marshal(in)
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
	t.Parallel()
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
	t.Parallel()
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

// R10DurationAny: a Duration field and an `any` holding the same value.
//
//ggen:generate
type R10DurationAny struct {
	D time.Duration `json:"d"`
	V any           `json:"v"`
}

// A pointer-receiver marshaler (big.Rat / big.Float have only those) is
// reached for a VALUE held in an any, directly or nested — jsonv2 parity;
// v1 emitted {} for it.
func TestAny_PointerReceiverValueMarshals(t *testing.T) {
	t.Parallel()
	for name, v := range map[string]any{
		"rat":          *big.NewRat(1, 2),
		"float":        *big.NewFloat(1.5),
		"rat_in_map":   map[string]any{"k": *big.NewRat(1, 2)},
		"rat_in_slice": []any{*big.NewRat(1, 2)},
	} {
		in := AnyStruct{Name: name, Body: v}
		got, err := ggen.Marshal(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want, err := jsonv2.Marshal(in)
		if err != nil {
			t.Fatalf("%s: jsonv2: %v", name, err)
		}
		// Generated member order differs from jsonv2's; compare parsed.
		var gv, wv any
		if err := jsonv2.Unmarshal(got, &gv); err != nil {
			t.Fatalf("%s: reparse %s: %v", name, got, err)
		}
		if err := jsonv2.Unmarshal(want, &wv); err != nil {
			t.Fatalf("%s: reparse %s: %v", name, want, err)
		}
		if !reflect.DeepEqual(gv, wv) {
			t.Errorf("%s:\n ggen   %s\n jsonv2 %s", name, got, want)
		}
	}
}

// A time.Duration carries the field default (`format:units`) inside an any
// too, so one document holds one wire shape for the type and the any-path
// form decodes back into a Duration field.
func TestAny_DurationMatchesFieldWire(t *testing.T) {
	t.Parallel()
	out, err := ggen.Marshal(R10DurationAny{D: 90 * time.Second, V: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"d":"1m30s","v":"1m30s"}` {
		t.Fatalf("field and any paths disagree on time.Duration: %s", out)
	}
	back, _, err := R10DurationAny{}.DecodeFrom([]byte(`{"d":` + string(out[len(`{"d":"1m30s","v":`):len(out)-1]) + `}`))
	if err != nil || back.D != 90*time.Second {
		t.Errorf("any-path form does not decode into the field: %v, %v", back.D, err)
	}
}

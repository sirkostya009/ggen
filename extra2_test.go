package main

// sql.Null* family + bare `any` field type + dual-UUID-lib coverage.

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	gofrs "github.com/gofrs/uuid/v5"
	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
)

// SQLNullStruct exercises every database/sql.NullX flavor in one shot.
//
//ggen:generate
type SQLNullStruct struct {
	S   sql.NullString  `json:"s"`
	I   sql.NullInt64   `json:"i"`
	I32 sql.NullInt32   `json:"i32"`
	I16 sql.NullInt16   `json:"i16"`
	B   sql.NullByte    `json:"b"`
	BL  sql.NullBool    `json:"bl"`
	F   sql.NullFloat64 `json:"f"`
	T   sql.NullTime    `json:"t"`
}

// AnyStruct: a single bare `any` field exercises the hand-rolled scan.Any
// path with stdlib-default float64 numbers.
//
//ggen:generate
type AnyStruct struct {
	Name string `json:"name"`
	Body any    `json:"body"`
}

// AnyNumberStruct: same shape but opts into json.Number for numeric values
// via the `usenumber` annotation — preserves exact digits, no float64 cast.
//
//ggen:generate usenumber
type AnyNumberStruct struct {
	Name string `json:"name"`
	Body any    `json:"body"`
}

// GofrsUUIDStruct uses gofrs's UUID type. Both its UUID and google's are
// `type UUID [16]byte` so the same generated [16]byte ↔ canonical-form
// path serves both — no per-lib code in generated output.
//
//ggen:generate
type GofrsUUIDStruct struct {
	ID gofrs.UUID `json:"id"`
}

// --- sql.Null tests ---

func TestSQLNull_NullValues(t *testing.T) {
	in := []byte(`{"s":null,"i":null,"i32":null,"i16":null,"b":null,"bl":null,"f":null,"t":null}`)
	got, err := decode.Unmarshal[SQLNullStruct](in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.S.Valid || got.I.Valid || got.I32.Valid || got.I16.Valid ||
		got.B.Valid || got.BL.Valid || got.F.Valid || got.T.Valid {
		t.Errorf("expected all Valid=false, got %+v", got)
	}
}

func TestSQLNull_PresentValues(t *testing.T) {
	in := []byte(`{"s":"hello","i":42,"i32":33,"i16":7,"b":255,"bl":true,"f":3.14,"t":"2023-11-14T22:13:20Z"}`)
	got, err := decode.Unmarshal[SQLNullStruct](in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.S.Valid || got.S.String != "hello" {
		t.Errorf("S = %+v", got.S)
	}
	if !got.I.Valid || got.I.Int64 != 42 {
		t.Errorf("I = %+v", got.I)
	}
	if !got.I32.Valid || got.I32.Int32 != 33 {
		t.Errorf("I32 = %+v", got.I32)
	}
	if !got.I16.Valid || got.I16.Int16 != 7 {
		t.Errorf("I16 = %+v", got.I16)
	}
	if !got.B.Valid || got.B.Byte != 255 {
		t.Errorf("B = %+v", got.B)
	}
	if !got.BL.Valid || got.BL.Bool != true {
		t.Errorf("BL = %+v", got.BL)
	}
	if !got.F.Valid || got.F.Float64 != 3.14 {
		t.Errorf("F = %+v", got.F)
	}
	if !got.T.Valid {
		t.Errorf("T not valid")
	}
}

func TestSQLNull_MarshalNullEmitsNull(t *testing.T) {
	in := SQLNullStruct{} // all zero → all !Valid → all "null"
	out, _ := encode.MarshalString(in)
	// Each field key must be present with `null` value.
	for _, k := range []string{"s", "i", "i32", "i16", "b", "bl", "f", "t"} {
		needle := `"` + k + `":null`
		if !strings.Contains(out, needle) {
			t.Errorf("missing %q in %s", needle, out)
		}
	}
}

func TestSQLNull_MarshalPresentEmitsValue(t *testing.T) {
	when := time.Unix(1700000000, 0).UTC()
	in := SQLNullStruct{
		S:   sql.NullString{String: "x", Valid: true},
		I:   sql.NullInt64{Int64: 9, Valid: true},
		I32: sql.NullInt32{Int32: 5, Valid: true},
		BL:  sql.NullBool{Bool: true, Valid: true},
		F:   sql.NullFloat64{Float64: 1.5, Valid: true},
		T:   sql.NullTime{Time: when, Valid: true},
	}
	out, _ := encode.MarshalString(in)
	if !strings.Contains(out, `"s":"x"`) {
		t.Errorf("s missing: %s", out)
	}
	if !strings.Contains(out, `"i":9`) {
		t.Errorf("i missing: %s", out)
	}
	if !strings.Contains(out, `"bl":true`) {
		t.Errorf("bl missing: %s", out)
	}
}

func TestSQLNull_Roundtrip(t *testing.T) {
	when := time.Unix(1700000000, 0).UTC()
	in := SQLNullStruct{
		S: sql.NullString{String: "abc", Valid: true},
		I: sql.NullInt64{Int64: -42, Valid: true},
		T: sql.NullTime{Time: when, Valid: true},
	}
	bs, _ := encode.Marshal(in)
	got, err := decode.Unmarshal[SQLNullStruct](bs)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, bs)
	}
	if got.S != in.S || got.I != in.I {
		t.Errorf("roundtrip mismatch:\n in:  %+v\n out: %+v", in, got)
	}
	if !got.T.Time.Equal(in.T.Time) || got.T.Valid != in.T.Valid {
		t.Errorf("T roundtrip: got %+v want %+v", got.T, in.T)
	}
}

// --- any tests ---

func TestAny_DecodeObject(t *testing.T) {
	in := []byte(`{"name":"x","body":{"k":1,"l":[1,2,3]}}`)
	got, err := decode.Unmarshal[AnyStruct](in)
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
		got, err := decode.Unmarshal[AnyStruct]([]byte(tc.json))
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
	got, err := decode.Unmarshal[AnyStruct](out)
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
	got, err := decode.Unmarshal[AnyNumberStruct](in)
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
	got, err := decode.Unmarshal[AnyNumberStruct](in)
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

// --- gofrs UUID test (proves dual-lib support without per-lib code) ---

func TestGofrsUUID_Roundtrip(t *testing.T) {
	id, _ := gofrs.FromString("550e8400-e29b-41d4-a716-446655440000")
	in := GofrsUUIDStruct{ID: id}
	out, _ := encode.Marshal(in)
	got, err := decode.Unmarshal[GofrsUUIDStruct](out)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.ID != in.ID {
		t.Errorf("ID = %s, want %s", got.ID, in.ID)
	}
}

// TestGofrsUUID_AltForms confirms the generated decoder delegates to the
// lib's UnmarshalText, which accepts more forms than canonical 8-4-4-4-12:
// hyphen-less and urn-prefixed pass too. Bytes-malformed input still errors.
func TestGofrsUUID_AltForms(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"id":"550e8400-e29b-41d4-a716-446655440000"}`), // canonical
		[]byte(`{"id":"550e8400e29b41d4a716446655440000"}`),     // hyphen-less
		[]byte(`{"id":"urn:uuid:550e8400-e29b-41d4-a716-446655440000"}`),
	}
	for _, c := range cases {
		if _, err := decode.Unmarshal[GofrsUUIDStruct](c); err != nil {
			t.Errorf("unmarshal %s: %v", c, err)
		}
	}
	// Bytes-shaped garbage still fails.
	bad := []byte(`{"id":"not-a-uuid-at-all"}`)
	if _, err := decode.Unmarshal[GofrsUUIDStruct](bad); err == nil {
		t.Error("expected error on garbage")
	}
}

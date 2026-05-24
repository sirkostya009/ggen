package integrationtests

// Coverage for the database/sql.Null* family. ggen emits inner-value-or-null
// on the wire (the convention every driver expects); stdlib v1/v2 serialize
// these as `{"Field":val,"Valid":true}` plain structs, so cross-compat lives
// only in this file via the round-trip path.

import (
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

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

func TestSQLNull_NullValues(t *testing.T) {
	in := []byte(`{"s":null,"i":null,"i32":null,"i16":null,"b":null,"bl":null,"f":null,"t":null}`)
	got, _, err := SQLNullStruct{}.DecodeFrom(in)
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
	got, _, err := SQLNullStruct{}.DecodeFrom(in)
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
	got, _, err := SQLNullStruct{}.DecodeFrom(bs)
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

// populatedSQLNull builds an SQLNullStruct with every flavor set to a
// non-trivial Valid=true value — used by the JSONSize cap-guard test
// for the present branch.
func populatedSQLNull() SQLNullStruct {
	return populatedSQLNullAt(time.Unix(1700000000, 0).UTC())
}

func populatedSQLNullAt(when time.Time) SQLNullStruct {
	out := SQLNullStruct{}
	out.S.String, out.S.Valid = strings.Repeat("a", 100), true
	out.I.Int64, out.I.Valid = math.MinInt64, true
	out.I32.Int32, out.I32.Valid = math.MinInt32, true
	out.I16.Int16, out.I16.Valid = math.MinInt16, true
	out.B.Byte, out.B.Valid = math.MaxUint8, true
	out.BL.Bool, out.BL.Valid = true, true
	out.F.Float64, out.F.Valid = math.MaxFloat64, true
	out.T.Time, out.T.Valid = when, true
	return out
}

// TestJSONSize_SQLNullStruct: cap-guard for every database/sql.NullX flavor.
// Both Valid=true and Valid=false branches because the size code chooses
// max(innerSize, len("null")) — both arms must absorb without realloc.
func TestJSONSize_SQLNullStruct_NoRealloc(t *testing.T) {
	cases := []struct {
		name string
		v    SQLNullStruct
	}{
		{"all_null", SQLNullStruct{}},
		{"all_present", populatedSQLNull()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			size := c.v.JSONSize()
			got, err := c.v.AppendJSON(make([]byte, 0, size))
			if err != nil {
				t.Fatalf("AppendJSON: %v", err)
			}
			if cap(got) != size {
				t.Errorf("realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
			}
			if len(got) > size {
				t.Errorf("undersized: len=%d > size=%d", len(got), size)
			}
		})
	}
}

package integrationtests

//go:generate ../ggen $GOFILE

// Coverage for the database/sql.Null* family. ggen emits inner-value-or-null
// on the wire; stdlib emits {"Field":val,"Valid":true}, so cross-compat here
// is round-trip only.

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
)

// Each sql.NullX flavor as its own single-field struct so per-type JSONSize
// cap-guards and decode paths are exercised in isolation.

//ggen:generate
type SQLNullStringStruct struct {
	S sql.NullString `json:"s"`
}

//ggen:generate
type SQLNullInt64Struct struct {
	I sql.NullInt64 `json:"i"`
}

//ggen:generate
type SQLNullInt32Struct struct {
	I32 sql.NullInt32 `json:"i32"`
}

//ggen:generate
type SQLNullInt16Struct struct {
	I16 sql.NullInt16 `json:"i16"`
}

//ggen:generate
type SQLNullByteStruct struct {
	B sql.NullByte `json:"b"`
}

//ggen:generate
type SQLNullBoolStruct struct {
	BL sql.NullBool `json:"bl"`
}

//ggen:generate
type SQLNullFloat64Struct struct {
	F sql.NullFloat64 `json:"f"`
}

//ggen:generate
type SQLNullTimeStruct struct {
	T sql.NullTime `json:"t"`
}

// Generic sql.Null[T] (Go 1.22) carriers — inner kinds with no named
// counterpart (int / uint64 / float32) plus a string/bool/time spread to prove
// the V-field path matches the named NullX wire shape.

//ggen:generate
type SQLNullGenStringStruct struct {
	S sql.Null[string] `json:"s"`
}

//ggen:generate
type SQLNullGenIntStruct struct {
	I sql.Null[int] `json:"i"`
}

//ggen:generate
type SQLNullGenUint64Struct struct {
	U sql.Null[uint64] `json:"u"`
}

//ggen:generate
type SQLNullGenFloat32Struct struct {
	F sql.Null[float32] `json:"f"`
}

//ggen:generate
type SQLNullGenBoolStruct struct {
	BL sql.Null[bool] `json:"bl"`
}

//ggen:generate
type SQLNullGenTimeStruct struct {
	T sql.Null[time.Time] `json:"t"`
}

// Custom inner types: a named int and named string ride their primitive wire
// via the per-inner fallback; a TextMarshaler type (uuid.UUID) routes through
// its text methods. All carry the inner-or-null wire shape.

type SQLAccountID int64
type SQLLabel string

//ggen:generate
type SQLNullGenAccountStruct struct {
	A sql.Null[SQLAccountID] `json:"a"`
}

//ggen:generate
type SQLNullGenLabelStruct struct {
	L sql.Null[SQLLabel] `json:"l"`
}

//ggen:generate
type SQLNullGenUUIDStruct struct {
	ID sql.Null[uuid.UUID] `json:"id"`
}

// SQLNullStruct is the composite of every named sql.NullX flavor.
//
//ggen:generate
type SQLNullStruct struct {
	SQLNullStringStruct
	SQLNullInt64Struct
	SQLNullInt32Struct
	SQLNullInt16Struct
	SQLNullByteStruct
	SQLNullBoolStruct
	SQLNullFloat64Struct
	SQLNullTimeStruct
}

// sqlWhen is the fixed timestamp (2023-11-14T22:13:20Z) used across these tests.
var sqlWhen = time.Unix(1700000000, 0).UTC()

// runSQLNullPerType drives one single-field sql.Null* struct through the wire:
//   - marshal-zero == {"key":null}
//   - marshal-present == {"key":presentVal}
//   - decode each then re-marshal reproduces the same bytes (roundtrip fixed point)
//
// presentVal is the raw JSON value text for the set field; T is inferred from present.
func runSQLNullPerType[T interface {
	encode.Marshaler
	decode.Decoder[T]
}](t *testing.T, name, key, presentVal string, present T) {
	t.Helper()
	nullWire := `{"` + key + `":null}`
	presentWire := `{"` + key + `":` + presentVal + `}`
	var zero T
	roundtrip := func(t *testing.T, in string) {
		t.Helper()
		got, _, err := zero.DecodeFrom([]byte(in))
		if err != nil {
			t.Fatalf("decode %s: %v", in, err)
		}
		out, err := encode.MarshalString(got)
		if err != nil {
			t.Fatalf("remarshal: %v", err)
		}
		if out != in {
			t.Errorf("roundtrip = %s, want %s", out, in)
		}
	}
	t.Run(name+"/marshal_null", func(t *testing.T) {
		t.Parallel()
		out, err := encode.MarshalString(zero)
		if err != nil {
			t.Fatalf("marshal zero: %v", err)
		}
		if out != nullWire {
			t.Errorf("marshal zero = %s, want %s", out, nullWire)
		}
	})
	t.Run(name+"/marshal_present", func(t *testing.T) {
		t.Parallel()
		out, err := encode.MarshalString(present)
		if err != nil {
			t.Fatalf("marshal present: %v", err)
		}
		if out != presentWire {
			t.Errorf("marshal present = %s, want %s", out, presentWire)
		}
	})
	t.Run(name+"/decode_null", func(t *testing.T) {
		t.Parallel()
		roundtrip(t, nullWire)
	})
	t.Run(name+"/decode_present", func(t *testing.T) {
		t.Parallel()
		roundtrip(t, presentWire)
	})
}

// TestSQLNull_PerType runs each single-field sql.Null* struct through the full
// marshal/decode matrix so every inner kind is asserted on its own.
func TestSQLNull_PerType(t *testing.T) {
	t.Parallel()
	runSQLNullPerType(t, "NullString", "s", `"hello"`,
		SQLNullStringStruct{S: sql.NullString{String: "hello", Valid: true}})
	runSQLNullPerType(t, "NullInt64", "i", `42`,
		SQLNullInt64Struct{I: sql.NullInt64{Int64: 42, Valid: true}})
	runSQLNullPerType(t, "NullInt32", "i32", `33`,
		SQLNullInt32Struct{I32: sql.NullInt32{Int32: 33, Valid: true}})
	runSQLNullPerType(t, "NullInt16", "i16", `7`,
		SQLNullInt16Struct{I16: sql.NullInt16{Int16: 7, Valid: true}})
	runSQLNullPerType(t, "NullByte", "b", `255`,
		SQLNullByteStruct{B: sql.NullByte{Byte: 255, Valid: true}})
	runSQLNullPerType(t, "NullBool", "bl", `true`,
		SQLNullBoolStruct{BL: sql.NullBool{Bool: true, Valid: true}})
	runSQLNullPerType(t, "NullFloat64", "f", `3.14`,
		SQLNullFloat64Struct{F: sql.NullFloat64{Float64: 3.14, Valid: true}})
	runSQLNullPerType(t, "NullTime", "t", `"2023-11-14T22:13:20Z"`,
		SQLNullTimeStruct{T: sql.NullTime{Time: sqlWhen, Valid: true}})

	// Generic sql.Null[T].
	runSQLNullPerType(t, "GenString", "s", `"hello"`,
		SQLNullGenStringStruct{S: sql.Null[string]{V: "hello", Valid: true}})
	runSQLNullPerType(t, "GenInt", "i", `42`,
		SQLNullGenIntStruct{I: sql.Null[int]{V: 42, Valid: true}})
	runSQLNullPerType(t, "GenUint64", "u", `18446744073709551615`,
		SQLNullGenUint64Struct{U: sql.Null[uint64]{V: 18446744073709551615, Valid: true}})
	runSQLNullPerType(t, "GenFloat32", "f", `1.5`,
		SQLNullGenFloat32Struct{F: sql.Null[float32]{V: 1.5, Valid: true}})
	runSQLNullPerType(t, "GenBool", "bl", `true`,
		SQLNullGenBoolStruct{BL: sql.Null[bool]{V: true, Valid: true}})
	runSQLNullPerType(t, "GenTime", "t", `"2023-11-14T22:13:20Z"`,
		SQLNullGenTimeStruct{T: sql.Null[time.Time]{V: sqlWhen, Valid: true}})

	// Custom inner types ride the inner type's own wire, not the {V,Valid} dump.
	runSQLNullPerType(t, "GenAccountID", "a", `9001`,
		SQLNullGenAccountStruct{A: sql.Null[SQLAccountID]{V: 9001, Valid: true}})
	runSQLNullPerType(t, "GenLabel", "l", `"vip"`,
		SQLNullGenLabelStruct{L: sql.Null[SQLLabel]{V: "vip", Valid: true}})
	// uuid.UUID routes through TextMarshaler/TextUnmarshaler.
	runSQLNullPerType(t, "GenUUID", "id", `"550e8400-e29b-41d4-a716-446655440000"`,
		SQLNullGenUUIDStruct{ID: sql.Null[uuid.UUID]{V: uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"), Valid: true}})
}

// TestSQLNull_Composite exercises the embedded composite: every field set and
// every field null, both directions.
func TestSQLNull_Composite(t *testing.T) {
	t.Parallel()
	full := SQLNullStruct{
		SQLNullStringStruct:  SQLNullStringStruct{S: sql.NullString{String: "hello", Valid: true}},
		SQLNullInt64Struct:   SQLNullInt64Struct{I: sql.NullInt64{Int64: 42, Valid: true}},
		SQLNullInt32Struct:   SQLNullInt32Struct{I32: sql.NullInt32{Int32: 33, Valid: true}},
		SQLNullInt16Struct:   SQLNullInt16Struct{I16: sql.NullInt16{Int16: 7, Valid: true}},
		SQLNullByteStruct:    SQLNullByteStruct{B: sql.NullByte{Byte: 255, Valid: true}},
		SQLNullBoolStruct:    SQLNullBoolStruct{BL: sql.NullBool{Bool: true, Valid: true}},
		SQLNullFloat64Struct: SQLNullFloat64Struct{F: sql.NullFloat64{Float64: 3.14, Valid: true}},
		SQLNullTimeStruct:    SQLNullTimeStruct{T: sql.NullTime{Time: sqlWhen, Valid: true}},
	}

	// assertFullSQLNull checks every field against full. NullTime compares via
	// Time.Equal; the rest are comparable.
	assertFullSQLNull := func(t *testing.T, got SQLNullStruct) {
		t.Helper()
		want := full
		if got.S != want.S {
			t.Errorf("S = %+v, want %+v", got.S, want.S)
		}
		if got.I != want.I {
			t.Errorf("I = %+v, want %+v", got.I, want.I)
		}
		if got.I32 != want.I32 {
			t.Errorf("I32 = %+v, want %+v", got.I32, want.I32)
		}
		if got.I16 != want.I16 {
			t.Errorf("I16 = %+v, want %+v", got.I16, want.I16)
		}
		if got.B != want.B {
			t.Errorf("B = %+v, want %+v", got.B, want.B)
		}
		if got.BL != want.BL {
			t.Errorf("BL = %+v, want %+v", got.BL, want.BL)
		}
		if got.F != want.F {
			t.Errorf("F = %+v, want %+v", got.F, want.F)
		}
		if got.T.Valid != want.T.Valid || !got.T.Time.Equal(want.T.Time) {
			t.Errorf("T = %+v, want %+v", got.T, want.T)
		}
	}

	t.Run("decode_all_null", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"s":null,"i":null,"i32":null,"i16":null,"b":null,"bl":null,"f":null,"t":null}`)
		got, _, err := SQLNullStruct{}.DecodeFrom(in)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.S.Valid || got.I.Valid || got.I32.Valid || got.I16.Valid ||
			got.B.Valid || got.BL.Valid || got.F.Valid || got.T.Valid {
			t.Errorf("expected all Valid=false, got %+v", got)
		}
	})

	t.Run("decode_all_present", func(t *testing.T) {
		t.Parallel()
		in := []byte(`{"s":"hello","i":42,"i32":33,"i16":7,"b":255,"bl":true,"f":3.14,"t":"2023-11-14T22:13:20Z"}`)
		got, _, err := SQLNullStruct{}.DecodeFrom(in)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		assertFullSQLNull(t, got)
	})

	t.Run("marshal_all_null", func(t *testing.T) {
		t.Parallel()
		out, err := encode.MarshalString(SQLNullStruct{})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		for _, k := range []string{"s", "i", "i32", "i16", "b", "bl", "f", "t"} {
			needle := `"` + k + `":null`
			if !strings.Contains(out, needle) {
				t.Errorf("missing %q in %s", needle, out)
			}
		}
	})

	t.Run("marshal_all_present", func(t *testing.T) {
		t.Parallel()
		out, err := encode.MarshalString(full)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		for _, want := range []string{
			`"s":"hello"`, `"i":42`, `"i32":33`, `"i16":7`,
			`"b":255`, `"bl":true`, `"f":3.14`, `"t":"2023-11-14T22:13:20Z"`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in %s", want, out)
			}
		}
	})

	t.Run("roundtrip_all_present", func(t *testing.T) {
		t.Parallel()
		bs, _ := encode.Marshal(full)
		got, _, err := SQLNullStruct{}.DecodeFrom(bs)
		if err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, bs)
		}
		assertFullSQLNull(t, got)
	})
}

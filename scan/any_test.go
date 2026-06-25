package scan

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// anyCases covers shapes scan.Any walks. Each input is fed to both
// stdlib json.Unmarshal (with and without UseNumber) and scan.Any /
// scan.AnyNumber; outputs must match via reflect.DeepEqual.
var anyCases = []struct {
	name string
	in   string
}{
	{"null", `null`},
	{"true", `true`},
	{"false", `false`},
	{"empty_string", `""`},
	{"plain_string", `"hello"`},
	{"escaped_string", `"a\\b\tc\n\"é"`},
	{"int", `42`},
	{"neg_int", `-7`},
	{"float", `3.14`},
	{"sci", `1.5e3`},
	{"large_int", `9007199254740993`},
	{"empty_array", `[]`},
	{"empty_object", `{}`},
	{"flat_array", `[1, 2, 3]`},
	{"mixed_array", `[1, "two", true, null, 4.5]`},
	{"nested_array", `[[1,2],[3,4]]`},
	{"flat_object", `{"a":1,"b":"x","c":true}`},
	{"nested_object", `{"x":{"y":{"z":[1,2,3]}}}`},
	{"whitespace", "  {  \"a\" : [ 1 , 2 ] , \"b\" : null }  "},
	{"complex", `{
		"name": "alice",
		"age": 30,
		"active": true,
		"score": 87.5,
		"big": 9007199254740993,
		"tags": ["go", "rust", "json"],
		"address": {"city": "Lviv", "zip": "79000"},
		"nested": [[1,2,3],[4,5,6]],
		"missing": null
	}`},
}

func TestAny_StdlibParity(t *testing.T) {
	t.Parallel()
	for _, tc := range anyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var want any
			if err := json.Unmarshal([]byte(tc.in), &want); err != nil {
				t.Fatalf("stdlib: %v", err)
			}
			got, _, err := Any([]byte(tc.in), 0)
			if err != nil {
				t.Fatalf("scan.Any: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("mismatch\n got: %#v (%T)\nwant: %#v (%T)", got, got, want, want)
			}
		})
	}
}

func TestAnyNumber_StdlibParity(t *testing.T) {
	t.Parallel()
	for _, tc := range anyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dec := json.NewDecoder(strings.NewReader(tc.in))
			dec.UseNumber()
			var want any
			if err := dec.Decode(&want); err != nil {
				t.Fatalf("stdlib UseNumber: %v", err)
			}
			got, _, err := AnyNumber([]byte(tc.in), 0)
			if err != nil {
				t.Fatalf("scan.AnyNumber: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("mismatch\n got: %#v (%T)\nwant: %#v (%T)", got, got, want, want)
			}
		})
	}
}

// payload covers the shape mix scan.Any walks: nested object, array of
// scalars + nested array, mixed numeric (int + float + large int), strings
// (no escapes — hits zero-copy alias path), booleans, null.
var anyPayload = []byte(`{
  "name": "alice",
  "age": 30,
  "active": true,
  "score": 87.5,
  "big": 9007199254740993,
  "tags": ["go", "rust", "json"],
  "address": {"city": "Lviv", "zip": "79000"},
  "nested": [[1,2,3],[4,5,6]],
  "missing": null
}`)

func BenchmarkAny_stdlib(b *testing.B) {
	b.SetBytes(int64(len(anyPayload)))
	b.ReportAllocs()
	for b.Loop() {
		var v any
		if err := json.Unmarshal(anyPayload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAny_scan(b *testing.B) {
	b.SetBytes(int64(len(anyPayload)))
	b.ReportAllocs()
	for b.Loop() {
		_, _, err := Any(anyPayload, 0)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAny_scanNumber(b *testing.B) {
	b.SetBytes(int64(len(anyPayload)))
	b.ReportAllocs()
	for b.Loop() {
		_, _, err := AnyNumber(anyPayload, 0)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAny_Shapes — per-shape decode benches over anyShapeInputs (the
// same shape mix the AppendAny bench covers), three rows each: stdjson,
// Any, AnyNumber.
func BenchmarkAny_Shapes(b *testing.B) {
	codecs := []struct {
		name string
		fn   func([]byte) error
	}{
		{"stdjson", func(p []byte) error { var v any; return json.Unmarshal(p, &v) }},
		{"ggen", func(p []byte) error { _, _, err := Any(p, 0); return err }},
		{"ggen_number", func(p []byte) error { _, _, err := AnyNumber(p, 0); return err }},
	}
	for _, sh := range anyShapeInputs {
		b.Run(sh.name, func(b *testing.B) {
			for _, c := range codecs {
				b.Run(c.name, func(b *testing.B) {
					b.SetBytes(int64(len(sh.payload)))
					b.ReportAllocs()
					for b.Loop() {
						if err := c.fn(sh.payload); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

// anyShapeInputs is the per-shape input table for BenchmarkAny_Shapes.
// Strings are inlined so the scan benches are independent of the
// encode package's bench fixtures — same shape mix, locally generated.
var anyShapeInputs = func() []struct {
	name    string
	payload []byte
} {
	out := []struct {
		name    string
		payload []byte
	}{
		{"null", []byte(`null`)},
		{"bool", []byte(`true`)},
		{"string", []byte(`"hello, ggen benchmark world!"`)},
		{"int", []byte(`42`)},
		{"int64", []byte(`1125899906842624`)},
		{"float64", []byte(`3.14159`)},
		// 30-digit number — beyond float64 precision (15-17 digits). Any
		// parses to float64 with silent precision loss; AnyNumber
		// preserves the exact digits via zero-copy alias, so the
		// stdjson-vs-ggen-vs-ggen_number row spread is meaningful here.
		{"large_number", []byte(`123456789012345.678901234567890123`)},
	}
	// 32-element typed-slice payloads. Plain digits / floats / bools
	// keep the parser on its happy path (no escapes, no whitespace).
	var sb strings.Builder
	sb.WriteByte('[')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("1234567890") // 10-digit positive int
	}
	sb.WriteByte(']')
	out = append(out, struct {
		name    string
		payload []byte
	}{"[]int_32", []byte(sb.String())})

	sb.Reset()
	sb.WriteByte('[')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("3.141592653589793")
	}
	sb.WriteByte(']')
	out = append(out, struct {
		name    string
		payload []byte
	}{"[]float_32", []byte(sb.String())})

	sb.Reset()
	sb.WriteByte('[')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		if i%2 == 0 {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	}
	sb.WriteByte(']')
	out = append(out, struct {
		name    string
		payload []byte
	}{"[]bool_32", []byte(sb.String())})

	sb.Reset()
	sb.WriteByte('[')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`"sampleString0123"`)
	}
	sb.WriteByte(']')
	out = append(out, struct {
		name    string
		payload []byte
	}{"[]string_32", []byte(sb.String())})

	// Object shape with 32 string-keyed entries — exercise the
	// per-key path under the same map-construction cost as the
	// AppendAny side.
	sb.Reset()
	sb.WriteByte('{')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"key%d":%d`, i, 1234567890+i)
	}
	sb.WriteByte('}')
	out = append(out, struct {
		name    string
		payload []byte
	}{"map[string]int_32", []byte(sb.String())})

	sb.Reset()
	sb.WriteByte('{')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"key%d":%f`, i, 3.14159+float64(i))
	}
	sb.WriteByte('}')
	out = append(out, struct {
		name    string
		payload []byte
	}{"map[string]float_32", []byte(sb.String())})

	sb.Reset()
	sb.WriteByte('{')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		if i%2 == 0 {
			fmt.Fprintf(&sb, `"key%d":true`, i)
		} else {
			fmt.Fprintf(&sb, `"key%d":false`, i)
		}
	}
	sb.WriteByte('}')
	out = append(out, struct {
		name    string
		payload []byte
	}{"map[string]bool_32", []byte(sb.String())})

	sb.Reset()
	sb.WriteByte('{')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"key%d":"sampleString0123"`, i)
	}
	sb.WriteByte('}')
	out = append(out, struct {
		name    string
		payload []byte
	}{"map[string]string_32", []byte(sb.String())})

	return out
}()

package scan

import (
	"encoding/json"
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
	for _, tc := range anyCases {
		t.Run(tc.name, func(t *testing.T) {
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
	for _, tc := range anyCases {
		t.Run(tc.name, func(t *testing.T) {
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

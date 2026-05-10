package scan

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

var floatCases = []string{
	"0", "-0", "1", "-1", "42", "-42",
	"3.14", "-3.14", "0.5", "-0.5",
	"1e3", "1E3", "1e+3", "1e-3", "-1e3",
	"1.5e10", "1.5e-10",
	"9007199254740992",       // 2^53
	"9007199254740993",       // 2^53+1 (loses precision in float64)
	"1.7976931348623157e308", // ~math.MaxFloat64
}

func TestFloat64_StdlibParity(t *testing.T) {
	for _, in := range floatCases {
		t.Run(in, func(t *testing.T) {
			var want float64
			if err := json.Unmarshal([]byte(in), &want); err != nil {
				t.Fatalf("stdlib: %v", err)
			}
			got, j, err := Float64([]byte(in), 0)
			if err != nil {
				t.Fatalf("scan.Float64: %v", err)
			}
			if got != want && !(math.IsNaN(got) && math.IsNaN(want)) {
				t.Errorf("mismatch got=%g want=%g", got, want)
			}
			if j != len(in) {
				t.Errorf("position = %d, want %d", j, len(in))
			}
		})
	}
}

func TestFloat64_ErrorParity(t *testing.T) {
	// Note: scan primitives expect the caller to have skipped leading
	// whitespace already (contract per package doc). Stdlib auto-skips,
	// so " 1" would falsely look like a parity violation — it's not in
	// scope here.
	cases := []string{
		"",    // empty
		"-",   // bare sign
		"abc", // non-digit
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, _, err := Float64([]byte(in), 0)
			if err == nil {
				t.Error("scan accepted invalid number")
			}
			var f float64
			if json.Unmarshal([]byte(in), &f) == nil {
				t.Errorf("stdlib accepted %q (decoded to %g); scan rejected — parity violation", in, f)
			}
		})
	}
}

func TestInt64_StdlibParity(t *testing.T) {
	cases := []string{"0", "-0", "1", "-1", "42", "-42",
		"9223372036854775807", "-9223372036854775808"}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			var want int64
			if err := json.Unmarshal([]byte(in), &want); err != nil {
				t.Fatalf("stdlib: %v", err)
			}
			got, j, err := Int64([]byte(in), 0)
			if err != nil {
				t.Fatalf("scan.Int64: %v", err)
			}
			if got != want {
				t.Errorf("mismatch got=%d want=%d", got, want)
			}
			if j != len(in) {
				t.Errorf("position = %d, want %d", j, len(in))
			}
		})
	}
}

// Stdlib rejects float-shaped values when unmarshaling into int64, so
// scan's same rejection is a parity property — assert both.
func TestInt64_RejectsFloatsParity(t *testing.T) {
	for _, in := range []string{"1.5", "1e3", "1.0", "1E5"} {
		t.Run(in, func(t *testing.T) {
			_, _, err := Int64([]byte(in), 0)
			if !errors.Is(err, ErrBadNumber) {
				t.Errorf("scan: got %v, want ErrBadNumber", err)
			}
			var n int64
			if json.Unmarshal([]byte(in), &n) == nil {
				t.Errorf("stdlib accepted %q into int64 (got %d); scan rejected — parity violation", in, n)
			}
		})
	}
}

func TestUint64_StdlibParity(t *testing.T) {
	cases := []string{"0", "1", "42", "18446744073709551615"} // max uint64
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			var want uint64
			if err := json.Unmarshal([]byte(in), &want); err != nil {
				t.Fatalf("stdlib: %v", err)
			}
			got, j, err := Uint64([]byte(in), 0)
			if err != nil {
				t.Fatalf("scan.Uint64: %v", err)
			}
			if got != want {
				t.Errorf("mismatch got=%d want=%d", got, want)
			}
			if j != len(in) {
				t.Errorf("position = %d, want %d", j, len(in))
			}
		})
	}
}

func TestNumber_StdlibParity(t *testing.T) {
	for _, in := range floatCases {
		t.Run(in, func(t *testing.T) {
			dec := json.NewDecoder(strings.NewReader(in))
			dec.UseNumber()
			var want any
			if err := dec.Decode(&want); err != nil {
				t.Fatalf("stdlib: %v", err)
			}
			got, j, err := Number([]byte(in), 0)
			if err != nil {
				t.Fatalf("scan.Number: %v", err)
			}
			if string(got) != string(want.(json.Number)) {
				t.Errorf("mismatch got=%q want=%q", got, want)
			}
			if j != len(in) {
				t.Errorf("position = %d, want %d", j, len(in))
			}
		})
	}
}

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
	t.Parallel()
	for _, in := range floatCases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
	for _, in := range []string{"1.5", "1e3", "1.0", "1E5"} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
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

// TestInt64_OverflowBoundaryLattice pins the 18-digit unchecked-prefix
// optimization: the checked tail must still catch every overflow and the
// values around the 18/19/20-digit boundary stay bit-exact. Leading-zero
// runs that push the significant digits past the 18-byte window must also
// resume correctly in the checked loop.
func TestInt64_OverflowBoundaryLattice(t *testing.T) {
	t.Parallel()
	ok := map[string]int64{
		"99999999999999999":                      99999999999999999,    // 17 digits
		"999999999999999999":                     999999999999999999,   // 18 digits
		"1000000000000000000":                    1000000000000000000,  // 19 digits, valid
		"9223372036854775807":                    math.MaxInt64,        // MaxInt64 (19 digits)
		"-9223372036854775808":                   math.MinInt64,        // MinInt64
		"-9223372036854775807":                   -9223372036854775807, // MinInt64+1
		"0000000000000000007":                    7,                    // 18 leading zeros + 7
		"00000000000000000009223372036854775807": math.MaxInt64,        // zeros push MaxInt64 past window
	}
	for in, want := range ok {
		t.Run("ok/"+in, func(t *testing.T) {
			t.Parallel()
			got, j, err := Int64([]byte(in), 0)
			if err != nil {
				t.Fatalf("Int64(%q): unexpected err %v", in, err)
			}
			if got != want {
				t.Errorf("Int64(%q) = %d, want %d", in, got, want)
			}
			if j != len(in) {
				t.Errorf("Int64(%q) pos = %d, want %d", in, j, len(in))
			}
		})
	}
	overflow := []string{
		"9223372036854775808",   // MaxInt64+1 (positive overflow)
		"9999999999999999999",   // 19 nines
		"99999999999999999999",  // 20 nines
		"-9223372036854775809",  // MinInt64-1
		"-99999999999999999999", // negative 20 nines
	}
	for _, in := range overflow {
		t.Run("overflow/"+in, func(t *testing.T) {
			t.Parallel()
			if _, _, err := Int64([]byte(in), 0); !errors.Is(err, ErrNumberOverflow) {
				t.Errorf("Int64(%q): got %v, want ErrNumberOverflow", in, err)
			}
		})
	}
}

// TestUint64_OverflowBoundaryLattice mirrors the Int64 lattice for the
// 19-digit unchecked prefix.
func TestUint64_OverflowBoundaryLattice(t *testing.T) {
	t.Parallel()
	ok := map[string]uint64{
		"9999999999999999999":                       9999999999999999999,  // 19 nines, valid
		"10000000000000000000":                      10000000000000000000, // 20 digits, valid
		"18446744073709551615":                      math.MaxUint64,       // MaxUint64 (20 digits)
		"00000000000000000000018446744073709551615": math.MaxUint64,       // leading zeros past window
	}
	for in, want := range ok {
		t.Run("ok/"+in, func(t *testing.T) {
			t.Parallel()
			got, j, err := Uint64([]byte(in), 0)
			if err != nil {
				t.Fatalf("Uint64(%q): unexpected err %v", in, err)
			}
			if got != want {
				t.Errorf("Uint64(%q) = %d, want %d", in, got, want)
			}
			if j != len(in) {
				t.Errorf("Uint64(%q) pos = %d, want %d", in, j, len(in))
			}
		})
	}
	for _, in := range []string{
		"18446744073709551616",  // MaxUint64+1
		"99999999999999999999",  // 20 nines
		"184467440737095516150", // 21 digits
	} {
		t.Run("overflow/"+in, func(t *testing.T) {
			t.Parallel()
			if _, _, err := Uint64([]byte(in), 0); !errors.Is(err, ErrNumberOverflow) {
				t.Errorf("Uint64(%q): got %v, want ErrNumberOverflow", in, err)
			}
		})
	}
}

func TestUint64_StdlibParity(t *testing.T) {
	t.Parallel()
	cases := []string{"0", "1", "42", "18446744073709551615"} // max uint64
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
	for _, in := range floatCases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
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

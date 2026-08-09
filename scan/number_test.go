package scan

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"strconv"
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

// Uint64 twin of the float-rejection parity property above — it used to
// accept "1.5" as 1 with a nil error where Int64 (and stdlib) reject.
func TestUint64_RejectsFloatsParity(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"1.5", "1e3", "1.0", "1E5"} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, _, err := Uint64([]byte(in), 0)
			if !errors.Is(err, ErrBadNumber) {
				t.Errorf("scan: got %v, want ErrBadNumber", err)
			}
			var n uint64
			if json.Unmarshal([]byte(in), &n) == nil {
				t.Errorf("stdlib accepted %q into uint64 (got %d); scan rejected — parity violation", in, n)
			}
			var s Stream
			s.Reset(strings.NewReader(in), nil)
			if _, err := s.Uint64(); !errors.Is(err, ErrBadNumber) {
				t.Errorf("stream: got %v, want ErrBadNumber", err)
			}
		})
	}
}

// TestInt64_OverflowBoundaryLattice checks values around the 18/19/20-digit
// boundary stay bit-exact and every overflow is caught, including leading-zero
// runs that push significant digits past the 18-byte window.
func TestInt64_OverflowBoundaryLattice(t *testing.T) {
	t.Parallel()
	ok := map[string]int64{
		"99999999999999999":                      99999999999999999,    // 17 digits
		"999999999999999999":                     999999999999999999,   // 18 digits
		"1000000000000000000":                    1000000000000000000,  // 19 digits, valid
		"9223372036854775807":                    math.MaxInt64,        // MaxInt64 (19 digits)
		"-9223372036854775808":                   math.MinInt64,        // MinInt64
		"-9223372036854775807":                   -9223372036854775807, // MinInt64+1
	}
	// Leading zeros are invalid JSON (RFC 8259) and now rejected — they used to
	// be accepted and doubled as unchecked-window coverage. The window is still
	// covered by the 17/18/19-digit rows above.
	for _, in := range []string{"01", "-01", "0000000000000000007", "00000000000000000009223372036854775807"} {
		t.Run("leadingzero/"+in, func(t *testing.T) {
			t.Parallel()
			if _, _, err := Int64([]byte(in), 0); !errors.Is(err, ErrBadNumber) {
				t.Errorf("Int64(%q): got %v, want ErrBadNumber", in, err)
			}
		})
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

// TestUint64_OverflowBoundaryLattice mirrors the Int64 lattice around the
// 19/20-digit boundary.
func TestUint64_OverflowBoundaryLattice(t *testing.T) {
	t.Parallel()
	ok := map[string]uint64{
		"9999999999999999999":                       9999999999999999999,  // 19 nines, valid
		"10000000000000000000":                      10000000000000000000, // 20 digits, valid
		"18446744073709551615":                      math.MaxUint64,       // MaxUint64 (20 digits)
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

// TestExactShort_ParseFloatDifferential exhaustively cross-checks the
// exactShort fast path in Float64 against strconv.ParseFloat over randomized
// short spans — bit-identity (incl. -0) and identical accept/reject.
func TestExactShort_ParseFloatDifferential(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(1))
	chars := []byte("0123456789.-eE+")
	check := func(s string) {
		t.Helper()
		got, gotOK := exactShort([]byte(s))
		want, err := strconv.ParseFloat(s, 64)
		if !gotOK {
			return // bail is always legal — ParseFloat path takes over
		}
		if err != nil {
			t.Fatalf("exactShort(%q) accepted, ParseFloat rejects: %v", s, err)
		}
		if math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("exactShort(%q) = %v (%#x), ParseFloat = %v (%#x)",
				s, got, math.Float64bits(got), want, math.Float64bits(want))
		}
	}
	// The differential below only checks ACCEPTED results, so pin that the
	// arms actually accept: a fast path that silently always bails would
	// pass every check() while quietly costing the win it exists for.
	for _, s := range []string{
		"1e5", "1e-5", "1.5e3", "-2.25e-4", "1e22", "1e-22", "1E7",
		"9007199254740991", "4503599627370496", "12345.6789e-18",
	} {
		if _, ok := exactShort([]byte(s)); !ok {
			t.Fatalf("exactShort(%q) declined — fast path should cover it", s)
		}
	}
	// ...and that it still declines what it cannot do exactly.
	for _, s := range []string{"1e23", "1e-23", "9007199254740993", "1e"} {
		if _, ok := exactShort([]byte(s)); ok {
			t.Fatalf("exactShort(%q) accepted — outside the exact range", s)
		}
	}
	// Directed cases at the exactness boundaries.
	for _, s := range []string{
		"0", "-0", "0.0", "-0.0", "1.", "-1.", ".5", "-.5", ".",
		"4503599627370495", "4503599627370496", // 2^52-1, 2^52
		"9007199254740991", "999999999999999.9", "0.000000000000001",
		"-4503599627370495", "1234567890.12345", "00000000000000.1",
		// Mantissa gate: exact through 2^53-1, rounds from 2^53 up.
		"9007199254740992", "9007199254740993", "-9007199254740991",
		// Exponent arm — the |power| ≤ 22 boundary from both directions,
		// with frac digits shifting power (power = exp - frac).
		"1e0", "1e22", "1e23", "1e-22", "1e-23", "-1e22", "1E22",
		"1e+22", "1e+23", "1.5e3", "1.5e-3", "0e0", "-0e0", "0.0e0",
		"1.234567890123e5", "12345.6789e-18", "1.2345678e22",
		"9007199254740991e-22", "1e-1", "5e-324", "1e308", "1e309",
		// Malformed exponent forms exactShort must decline, not mis-parse.
		"1e", "1e+", "1e-", "1ee5", "1e5.5", "1e5e5", "e5", "1.e5",
	} {
		check(s)
	}
	for range 500000 {
		n := 1 + rng.Intn(16)
		b := make([]byte, n)
		for i := range b {
			b[i] = chars[rng.Intn(len(chars))]
		}
		check(string(b))
	}
	// Well-formed exponent spans — the random alphabet above almost never
	// assembles a valid one, so the Clinger arm would go uncovered. Exponents
	// are drawn to straddle the |power| ≤ 22 acceptance boundary.
	for range 500000 {
		b := make([]byte, 0, 16)
		if rng.Intn(4) == 0 {
			b = append(b, '-')
		}
		for range 1 + rng.Intn(8) {
			b = append(b, byte('0'+rng.Intn(10)))
		}
		if rng.Intn(2) == 0 {
			b = append(b, '.')
			for range 1 + rng.Intn(4) {
				b = append(b, byte('0'+rng.Intn(10)))
			}
		}
		if rng.Intn(8) == 0 {
			b = append(b, 'E')
		} else {
			b = append(b, 'e')
		}
		switch rng.Intn(3) {
		case 0:
			b = append(b, '+')
		case 1:
			b = append(b, '-')
		}
		b = append(b, byte('0'+rng.Intn(10)))
		if rng.Intn(2) == 0 {
			b = append(b, byte('0'+rng.Intn(10)))
		}
		if len(b) <= 16 {
			check(string(b))
		}
	}
	// Digit-heavy spans (the alphabet above rarely forms long valid numbers).
	for range 500000 {
		n := 1 + rng.Intn(16)
		b := make([]byte, 0, n+1)
		if rng.Intn(4) == 0 {
			b = append(b, '-')
		}
		dot := -1
		if rng.Intn(2) == 0 {
			dot = rng.Intn(n)
		}
		for i := range n {
			if i == dot {
				b = append(b, '.')
			} else {
				b = append(b, byte('0'+rng.Intn(10)))
			}
		}
		check(string(b))
	}
}

// TestInt_ReferenceDifferential pins Int64/Uint64 against a plain per-digit
// reference implementation over randomized digit runs (1..25 digits, leading
// zeros, signs, non-digit tails) — value, end position, and error identity.
// (Kept from the rejected SWAR-chunk experiment; it caught a real classifier
// bug there and guards the unchecked-prefix window generally.)
func TestInt_ReferenceDifferential(t *testing.T) {
	t.Parallel()
	refInt := func(data []byte, i int) (int64, int, error) {
		neg := false
		if i < len(data) && data[i] == '-' {
			neg = true
			i++
		}
		if i >= len(data) || data[i] < '0' || data[i] > '9' {
			return 0, 0, ErrBadNumber
		}
		// RFC 8259: no leading zeros (mirrors Int64).
		if data[i] == '0' && i+1 < len(data) && data[i+1] >= '0' && data[i+1] <= '9' {
			return 0, 0, ErrBadNumber
		}
		limit := uint64(math.MaxInt64)
		if neg {
			limit = SignedNeg
		}
		var u uint64
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			d := uint64(data[i] - '0')
			if u > limit/10 || (u == limit/10 && d > limit%10) {
				return 0, 0, ErrNumberOverflow
			}
			u = u*10 + d
			i++
		}
		if i < len(data) {
			c := data[i]
			if c == '.' || c == 'e' || c == 'E' {
				return 0, 0, ErrBadNumber
			}
		}
		if neg {
			if u == SignedNeg {
				return math.MinInt64, i, nil
			}
			return -int64(u), i, nil
		}
		return int64(u), i, nil
	}
	refUint := func(data []byte, i int) (uint64, int, error) {
		if i >= len(data) || data[i] < '0' || data[i] > '9' {
			return 0, 0, ErrBadNumber
		}
		// RFC 8259: no leading zeros (mirrors Uint64).
		if data[i] == '0' && i+1 < len(data) && data[i+1] >= '0' && data[i+1] <= '9' {
			return 0, 0, ErrBadNumber
		}
		var n uint64
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			d := uint64(data[i] - '0')
			if n > Uint64Limit/10 || (n == Uint64Limit/10 && d > Uint64Limit%10) {
				return 0, 0, ErrNumberOverflow
			}
			n = n*10 + d
			i++
		}
		if i < len(data) {
			c := data[i]
			if c == '.' || c == 'e' || c == 'E' {
				return 0, 0, ErrBadNumber
			}
		}
		return n, i, nil
	}
	rng := rand.New(rand.NewSource(3))
	tails := []byte{',', '}', ']', ' ', '.', 'e', 'x', 'a', '/', ':'}
	for round := range 400000 {
		nd := 1 + rng.Intn(25)
		b := make([]byte, 0, nd+2)
		if round%3 == 0 {
			b = append(b, '-')
		}
		for range nd {
			b = append(b, byte('0'+rng.Intn(10)))
		}
		if rng.Intn(2) == 0 {
			b = append(b, tails[rng.Intn(len(tails))])
		}
		gi, gp, ge := Int64(b, 0)
		wi, wp, we := refInt(b, 0)
		if gi != wi || gp != wp || ge != we {
			t.Fatalf("Int64(%q) = (%d,%d,%v), ref (%d,%d,%v)", b, gi, gp, ge, wi, wp, we)
		}
		if b[0] != '-' {
			gu, gp, ge := Uint64(b, 0)
			wu, wp, we := refUint(b, 0)
			if gu != wu || gp != wp || ge != we {
				t.Fatalf("Uint64(%q) = (%d,%d,%v), ref (%d,%d,%v)", b, gu, gp, ge, wu, wp, we)
			}
		}
	}
}

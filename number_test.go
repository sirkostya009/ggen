package ggen

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
				t.Fatalf("Float64: %v", err)
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

// TestFloat32_StdlibParity checks that Float32 rounds the decimal once, like
// strconv.ParseFloat(s, 32). Parsing as float64 and then converting rounds
// twice. That is off by one step when the float64 result lands exactly
// halfway between two float32 values. So the random cases are decimals near
// such halfway points, at many digit counts. Both []byte input and Stream
// must match bit for bit.
func TestFloat32_StdlibParity(t *testing.T) {
	t.Parallel()
	cases := append([]string{
		"1.0000000596046448", "1.0000001788139343", "1.0000002980232239",
		"3.4028235677973366e38", "3.4028235e38", "3.4028236e38", "1e39", "-1e39",
		"1e-50", "1.4e-45", "1.17549435e-38",
	}, floatCases...)
	r := rand.New(rand.NewSource(1))
	for range 20000 {
		f := math.Float32frombits(r.Uint32() &^ (1 << 31))
		if math.IsInf(float64(f), 0) || math.IsNaN(float64(f)) {
			continue
		}
		mid := (float64(f) + float64(math.Nextafter32(f, math.MaxFloat32))) / 2
		for _, prec := range []int{6, 8, 10, 12, 14, 15, 16, 17, -1} {
			cases = append(cases, strconv.FormatFloat(mid, 'g', prec, 64))
		}
	}
	for _, in := range cases {
		want, werr := strconv.ParseFloat(in, 32)
		got, j, err := Float32([]byte(in), 0)
		var s Stream
		s.Reset(strings.NewReader(in), make([]byte, 0, 3))
		sgot, serr := s.Float32()
		if werr != nil {
			if !errors.Is(err, ErrNumberOverflow) || !errors.Is(serr, ErrNumberOverflow) {
				t.Errorf("%s: strconv %v; bytes %v, stream %v, want ErrNumberOverflow", in, werr, err, serr)
			}
			continue
		}
		if err != nil || j != len(in) || got != float32(want) {
			t.Errorf("%s: bytes %v (%v, end %d), want %v", in, got, err, j, float32(want))
		}
		if serr != nil || sgot != float32(want) {
			t.Errorf("%s: stream %v (%v), want %v", in, sgot, serr, float32(want))
		}
	}
}

func TestFloat64_ErrorParity(t *testing.T) {
	t.Parallel()
	// The scan functions expect the caller to skip leading whitespace
	// first. The standard library skips it itself, so " 1" would look
	// like a mismatch. That case is left out on purpose.
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
	cases := []string{
		"0", "-0", "1", "-1", "42", "-42",
		"9223372036854775807", "-9223372036854775808",
	}
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
				t.Fatalf("Int64: %v", err)
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

// Int64 must reject a number with a fraction or exponent. The standard
// library also rejects it for an int64, so the test checks both.
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

// Uint64 must reject a number with a fraction or exponent, like Int64 and
// the standard library. It must not return 1 for "1.5" with a nil error.
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

// TestInt64_OverflowBoundaryLattice checks values near 18, 19 and 20 digits.
// Int64 reads the first 18 digits without an overflow check, because 18
// digits always fit in int64. Values around that edge must parse exactly, and
// every overflow must be caught.
func TestInt64_OverflowBoundaryLattice(t *testing.T) {
	t.Parallel()
	ok := map[string]int64{
		"99999999999999999":    99999999999999999,    // 17 digits
		"999999999999999999":   999999999999999999,   // 18 digits
		"1000000000000000000":  1000000000000000000,  // 19 digits, valid
		"9223372036854775807":  math.MaxInt64,        // MaxInt64 (19 digits)
		"-9223372036854775808": math.MinInt64,        // MinInt64
		"-9223372036854775807": -9223372036854775807, // MinInt64+1
	}
	// Leading zeros are invalid JSON (RFC 8259) and must be rejected.
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

// TestUint64_OverflowBoundaryLattice does the same as the Int64 test, near
// 19 and 20 digits.
func TestUint64_OverflowBoundaryLattice(t *testing.T) {
	t.Parallel()
	ok := map[string]uint64{
		"9999999999999999999":  9999999999999999999,  // 19 nines, valid
		"10000000000000000000": 10000000000000000000, // 20 digits, valid
		"18446744073709551615": math.MaxUint64,       // MaxUint64 (20 digits)
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
				t.Fatalf("Uint64: %v", err)
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
				t.Fatalf("Number: %v", err)
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

// TestExactShort_ParseFloatDifferential checks exactShort, the fast path in
// Float64 for short numbers, against strconv.ParseFloat on many random short
// inputs. Each result it accepts must be the same bits, including -0.
func TestExactShort_ParseFloatDifferential(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(1))
	chars := []byte("0123456789.-eE+")
	check := func(s string) {
		t.Helper()
		got, gotOK := exactShort([]byte(s))
		want, err := strconv.ParseFloat(s, 64)
		if !gotOK {
			return // declining is always fine, ParseFloat handles it
		}
		if err != nil {
			t.Fatalf("exactShort(%q) accepted, ParseFloat rejects: %v", s, err)
		}
		if math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("exactShort(%q) = %v (%#x), ParseFloat = %v (%#x)",
				s, got, math.Float64bits(got), want, math.Float64bits(want))
		}
	}
	// check only looks at results exactShort accepts. So first make sure it
	// does accept these. A fast path that always declined would pass every
	// check() and give no speedup.
	for _, s := range []string{
		"1e5", "1e-5", "1.5e3", "-2.25e-4", "1e22", "1e-22", "1E7",
		"9007199254740991", "4503599627370496", "12345.6789e-18",
	} {
		if _, ok := exactShort([]byte(s)); !ok {
			t.Fatalf("exactShort(%q) declined — fast path should cover it", s)
		}
	}
	// It must still decline what it cannot compute exactly.
	for _, s := range []string{"1e23", "1e-23", "9007199254740993", "1e"} {
		if _, ok := exactShort([]byte(s)); ok {
			t.Fatalf("exactShort(%q) accepted — outside the exact range", s)
		}
	}
	// Hand-picked cases at the edges of the exact range.
	for _, s := range []string{
		"0", "-0", "0.0", "-0.0", "1.", "-1.", ".5", "-.5", ".",
		"4503599627370495", "4503599627370496", // 2^52-1, 2^52
		"9007199254740991", "999999999999999.9", "0.000000000000001",
		"-4503599627370495", "1234567890.12345", "00000000000000.1",
		// Digit value: exact up to 2^53-1, rounding starts at 2^53.
		"9007199254740992", "9007199254740993", "-9007199254740991",
		// Exponent forms near the |power| <= 22 limit from both sides.
		// Fraction digits change power: power = exp - fraction digits.
		"1e0", "1e22", "1e23", "1e-22", "1e-23", "-1e22", "1E22",
		"1e+22", "1e+23", "1.5e3", "1.5e-3", "0e0", "-0e0", "0.0e0",
		"1.234567890123e5", "12345.6789e-18", "1.2345678e22",
		"9007199254740991e-22", "1e-1", "5e-324", "1e308", "1e309",
		// Broken exponent forms. exactShort must decline them.
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
	// Valid numbers with an exponent. The random characters above almost
	// never form one, so the exponent path would go untested. That path is
	// exact when the digits fit in 53 bits and |power| <= 22 (Clinger's fast
	// path). Exponents are picked to fall on both sides of that limit.
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
	// Mostly digits. The random characters above rarely form long valid
	// numbers.
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

// TestInt_ReferenceDifferential checks Int64 and Uint64 against a simple
// digit-by-digit reference on random digit runs. Runs have 1 to 25 digits,
// leading zeros, signs and non-digit tails. The value, end position and
// error must all match. This also guards the first 18 digits that Int64
// reads without an overflow check.
func TestInt_ReferenceDifferential(t *testing.T) {
	t.Parallel()
	refInt := func(data []byte, i int) (int64, int, error) {
		neg := false
		if i < len(data) && data[i] == '-' {
			neg = true
			i++
		}
		if i >= len(data) || data[i] < '0' || data[i] > '9' {
			return 0, i, ErrBadNumber
		}
		// RFC 8259 forbids leading zeros, same as Int64.
		if data[i] == '0' && i+1 < len(data) && data[i+1] >= '0' && data[i+1] <= '9' {
			return 0, i, ErrBadNumber
		}
		limit := uint64(math.MaxInt64)
		if neg {
			limit = SignedNeg
		}
		var u uint64
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			d := uint64(data[i] - '0')
			if u > limit/10 || (u == limit/10 && d > limit%10) {
				return 0, i, ErrNumberOverflow
			}
			u = u*10 + d
			i++
		}
		if i < len(data) {
			c := data[i]
			if c == '.' || c == 'e' || c == 'E' {
				return 0, i, ErrBadNumber
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
			return 0, i, ErrBadNumber
		}
		// RFC 8259 forbids leading zeros, same as Uint64.
		if data[i] == '0' && i+1 < len(data) && data[i+1] >= '0' && data[i+1] <= '9' {
			return 0, i, ErrBadNumber
		}
		var n uint64
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			d := uint64(data[i] - '0')
			if n > Uint64Limit/10 || (n == Uint64Limit/10 && d > Uint64Limit%10) {
				return 0, i, ErrNumberOverflow
			}
			n = n*10 + d
			i++
		}
		if i < len(data) {
			c := data[i]
			if c == '.' || c == 'e' || c == 'E' {
				return 0, i, ErrBadNumber
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

// numberAccepted reports whether SkipValue consumes tok in full as a number.
func numberAccepted(tok string) bool {
	pos, err := SkipValue([]byte(tok), 0)
	return err == nil && pos == len(tok)
}

func TestSkipNumber_AcceptSetMatchesJSONGrammar(t *testing.T) {
	t.Parallel()
	cases := []string{
		// valid
		"0", "-0", "123", "-123", "1.5", "-1.5", "0.5", "1e5", "1E5",
		"1e+5", "1e-5", "1.5e10", "-1.5E-10", "1e400", "1e999", "1e1000000",
		"12345678901234567890", "0.0000001", "-0.0",
		// invalid: leading '+', leading zero, lone or trailing dot
		"+1", "01", "007", "00.5", "1.", "-.5", ".5", "-", "+",
		// invalid: broken exponent, two dots, junk after the number
		"1e", "1e+", "1e-", "1..2", "1.2.3", "--", "e5", "1.e5",
		"1ee5", "0x1", "1.2e", "1.2e+",
		"", "0123", "00",
	}
	for _, tok := range cases {
		got := numberAccepted(tok)
		// A token that starts with '-' or a digit is a valid number exactly
		// when json.Valid accepts it. Other tokens never reach skipNumber, so
		// they are not compared.
		want := json.Valid([]byte(tok))
		if tok == "" || tok[0] != '-' && (tok[0] < '0' || tok[0] > '9') {
			// Not a number token, so skipNumber is never called.
			if got {
				t.Errorf("%q: skipNumber accepted a non-number-leading token", tok)
			}
			continue
		}
		if got != want {
			t.Errorf("%q: skipNumber accepted=%v, json.Valid=%v", tok, got, want)
		}
	}
}

// Stream.SkipValue must accept exactly the numbers that SkipValue on []byte
// accepts. This must hold when a number is split across reads. The Stream
// keeps a window, a buffered slice of the reader, and a refill reads more
// into it. A reader that returns one byte per Read forces a refill at every
// offset.
func TestSkipNumber_StreamMatchesBytes(t *testing.T) {
	t.Parallel()
	cases := []string{
		"0", "-0", "123", "-123", "1.5", "1e5", "1e+5", "1.5e10", "1e400",
		"12345678901234567890", "+1", "01", "1.", ".5", "-", "1e", "1..2",
		"1.2.3", "00.5", "1.2e+",
	}
	for _, tok := range cases {
		bp, bErr := SkipValue([]byte(tok), 0)
		bytesOK := bErr == nil && bp == len(tok)

		var s Stream
		s.Reset(&chunkedReader{data: []byte(tok)}, make([]byte, 0, 4))
		sErr := s.SkipValue()
		// Use Offset, not Pos. The skip code drops consumed bytes from the
		// buffer, like SkipSpace and Int64 do, so Pos is relative to the
		// buffer, not the input.
		streamOK := sErr == nil && s.Offset() == len(tok)
		if streamOK != bytesOK {
			t.Errorf("%q: stream accepted=%v, bytes accepted=%v", tok, streamOK, bytesOK)
		}
	}
}

// BenchmarkFloat64Forms groups numbers by the path they take. Plain decimals
// and short exponent forms take exactShort. The reject rows fall back to
// strconv.ParseFloat. They show what the fast path costs when it declines.
func BenchmarkFloat64Forms(b *testing.B) {
	forms := []struct{ name, span string }{
		{"plain_int", "12345"},
		{"plain_frac", "1234.5678"},
		{"plain_wide", "1234567890.12345"},
		{"exp_small", "1.5e3"},
		{"exp_neg", "-2.25e-4"},
		{"exp_wide", "1.234567890123e5"},
		{"exp_reject", "1e23"},                  // |power| > 22, uses ParseFloat
		{"wide_reject", "1.234567890123456789"}, // over 16 bytes, uses ParseFloat
	}
	for _, f := range forms {
		data := []byte(f.span + ",")
		b.Run(f.name, func(b *testing.B) {
			for b.Loop() {
				if _, _, err := Float64(data, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	// Many numbers back to back, like a real payload. Half use an exponent.
	var sb strings.Builder
	for i := range 200 {
		if i > 0 {
			sb.WriteByte(',')
		}
		if i%2 == 0 {
			sb.WriteString("1.5e3")
		} else {
			sb.WriteString("1234.5678")
		}
	}
	mixed := []byte(sb.String())
	b.Run("mixed_200", func(b *testing.B) {
		b.SetBytes(int64(len(mixed)))
		for b.Loop() {
			i := 0
			for i < len(mixed) {
				_, n, err := Float64(mixed, i)
				if err != nil {
					b.Fatal(err)
				}
				i = n + 1
			}
		}
	})
}

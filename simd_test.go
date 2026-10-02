//go:build goexperiment.simd

package ggen

import (
	"bytes"
	"io"
	"math/rand"
	"strings"
	"testing"
)

var stringTiers = []struct {
	name string
	fn   func([]byte, int, bool) (string, int, error)
}{
	{"AVX", StringAVX},
	{"AVX2", StringAVX2},
	{"AVX512", StringAVX512},
}

// TestStringSIMD_Parity checks every SIMD version of String against the
// scalar one. SIMD means handling 16, 32 or 64 bytes with one CPU
// instruction. The value, position and exact error must match. Cases vary
// where escapes and control bytes sit, cut strings short, and place special
// bytes right at the edge of a vector-sized block.
func TestStringSIMD_Parity(t *testing.T) {
	t.Parallel()
	cases := [][]byte{
		[]byte(`""`), []byte(`"a"`), []byte(`"ab"`), []byte(`not a string`),
		{},
		[]byte(`"unterminated`), []byte(`"trailing\`), []byte(`"bad\u12`),
		[]byte(`"esc\nape"`), []byte(`"\u0041\ud83d\ude00"`), []byte(`"\q"`),
		// Broken escapes at the very end of the input, next to inputs that
		// are only cut short.
		[]byte(`"\u12"`), []byte(`"ab\uZ"`), []byte(`"\u00`), []byte(`"\ud83d\uDE`),
		[]byte(`"\ud83d"`), []byte("\"ab\x01"), []byte("\"ab\x01}"),
	}
	for _, n := range []int{1, 7, 15, 16, 17, 31, 32, 33, 63, 64, 65, 127, 500} {
		body := strings.Repeat("x", n)
		cases = append(cases,
			[]byte(`"`+body+`"`),
			[]byte(`"`+body),               // unterminated
			[]byte(`"`+body+`\n"`),         // escape at a block edge
			[]byte(`"`+body+"\x01"+`"`),    // control byte at a block edge
			[]byte(`"`+body+`\n`+body+`"`), // escape mid-string
			[]byte(`"`+"\x1f"+body+`"`),    // control byte first
			[]byte(`"a`+"\x01"+`b\nc"`),    // control byte before an escape
			[]byte(`"`+body+"\xff"+`"`),    // invalid UTF-8 at a block edge
			[]byte(`"`+"\xff"+body+`"`),    // invalid UTF-8 first
			[]byte(`"`+body+"é😀"+`"`),      // valid multi-byte at a block edge
			[]byte(`"`+body+"\xe2("+`"`),   // cut-off 3-byte character
		)
	}
	rng := rand.New(rand.NewSource(1))
	for range 2000 {
		n := rng.Intn(200)
		b := make([]byte, n+2)
		b[0] = '"'
		for i := 1; i <= n; i++ {
			b[i] = byte(rng.Intn(130)) // control bytes, quote, backslash and ASCII
		}
		b[n+1] = '"'
		cases = append(cases, b)
	}
	// Both modes must match scalar. With validate, invalid UTF-8 is
	// rejected like jsonv2 does. Without it (allowinvalidutf8), raw bytes
	// pass through.
	for _, validate := range []bool{true, false} {
		for _, tier := range stringTiers {
			for _, c := range cases {
				s1, p1, e1 := String(c, 0, validate)
				s2, p2, e2 := tier.fn(c, 0, validate)
				if e1 != e2 {
					t.Fatalf("%s(validate=%v) %q: err %v vs %v", tier.name, validate, c, e1, e2)
				}
				if s1 != s2 || p1 != p2 {
					t.Fatalf("%s(validate=%v) %q: (%q,%d) vs (%q,%d)", tier.name, validate, c, s1, p1, s2, p2)
				}
			}
		}
	}
}

// chunkReader returns at most n bytes per Read. This makes the Stream read
// more input in the middle of a string.
type chunkReader struct {
	r io.Reader
	n int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}

// TestStreamStringSIMD_Parity checks the SIMD versions of Stream.String
// against the scalar one. The Stream keeps a window, a buffered slice of the
// reader, and reads more when it runs out. Cases vary where that read
// happens: string lengths near vector edges, escapes and control bytes at
// many positions, reads of 1 to 64 bytes, and random strings. StringAVX*,
// StringViewAVX* and KeyViewAVX* all call the same inner function, so
// checking StringAVX* covers all three.
func TestStreamStringSIMD_Parity(t *testing.T) {
	t.Parallel()
	tiers := []struct {
		name string
		fn   func(*Stream, bool) (string, error)
	}{
		{"AVX", (*Stream).StringAVX},
		{"AVX2", (*Stream).StringAVX2},
		{"AVX512", (*Stream).StringAVX512},
	}
	var bodies []string
	bodies = append(bodies, "", "a", "hello", `esc\"aped`, `tail\\`, `uni\u00e9code`)
	for _, n := range []int{1, 15, 16, 17, 31, 32, 33, 63, 64, 65, 130, 600} {
		b := bytes.Repeat([]byte{'x'}, n)
		bodies = append(bodies, string(b))
		for _, c := range []byte{'\\', 0x01} {
			for pos := 0; pos < n; pos += max(1, n/5) {
				bb := bytes.Repeat([]byte{'y'}, n)
				bb[pos] = c
				if c == '\\' && pos == n-1 {
					continue // a final backslash would escape the closing quote
				}
				if c == '\\' {
					bb[pos+1] = 'n'
				}
				bodies = append(bodies, string(bb))
			}
		}
	}
	// Invalid UTF-8 near vector edges. Without validate, the bytes must
	// pass through as is. With validate, the error must match scalar.
	for _, n := range []int{1, 15, 16, 31, 32, 63, 64} {
		pad := strings.Repeat("x", n)
		bodies = append(bodies, pad+"\xff", "\xff"+pad, pad+"\xe2(")
	}
	rng := rand.New(rand.NewSource(11))
	alphabet := []byte("abcdefgh \x01\x1fé日\xff")
	for range 500 {
		n := rng.Intn(150)
		b := make([]byte, 0, n)
		for len(b) < n {
			b = append(b, alphabet[rng.Intn(len(alphabet))])
		}
		bodies = append(bodies, string(b))
	}
	decode := func(fn func(*Stream, bool) (string, error), payload []byte, chunk int, validate bool) (string, error) {
		var s Stream
		s.Reset(&chunkReader{bytes.NewReader(payload), chunk}, make([]byte, 0, 8))
		return fn(&s, validate)
	}
	for _, body := range bodies {
		payload := []byte(`"` + body + `"`)
		for _, chunk := range []int{1, 3, 7, 16, 64} {
			for _, validate := range []bool{true, false} {
				want, wantErr := decode((*Stream).String, payload, chunk, validate)
				for _, tier := range tiers {
					got, gotErr := decode(tier.fn, payload, chunk, validate)
					if (wantErr == nil) != (gotErr == nil) || wantErr != gotErr {
						t.Fatalf("%s(%q, chunk=%d, validate=%v): err %v, scalar err %v", tier.name, body, chunk, validate, gotErr, wantErr)
					}
					if got != want {
						t.Fatalf("%s(%q, chunk=%d, validate=%v) = %q, scalar %q", tier.name, body, chunk, validate, got, want)
					}
				}
			}
		}
	}
}

// errAfterReader returns data, then a non-EOF error on every later Read.
type errAfterReader struct {
	data []byte
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// TestStreamStringSIMD_RefillErrorIdentity checks the errors the SIMD
// versions return when reading more input fails. A reader that hits EOF must
// give ErrExpectString or ErrUnterminated, like the scalar code. Any other
// reader error must be returned as is, not replaced by a JSON error.
func TestStreamStringSIMD_RefillErrorIdentity(t *testing.T) {
	t.Parallel()
	tiers := []struct {
		name string
		fn   func(*Stream, bool) (string, error)
	}{
		{"AVX", (*Stream).StringAVX},
		{"AVX2", (*Stream).StringAVX2},
		{"AVX512", (*Stream).StringAVX512},
	}
	boom := io.ErrNoProgress
	for _, tier := range tiers {
		t.Run(tier.name, func(t *testing.T) {
			t.Parallel()
			// EOF before the string starts gives ErrExpectString.
			var s Stream
			s.Reset(strings.NewReader(""), nil)
			if _, err := tier.fn(&s, true); err != ErrExpectString {
				t.Errorf("drained head: got %v, want ErrExpectString", err)
			}
			// EOF inside the string gives ErrUnterminated.
			s.Reset(strings.NewReader(`"abc`), nil)
			if _, err := tier.fn(&s, true); err != ErrUnterminated {
				t.Errorf("drained mid-string: got %v, want ErrUnterminated", err)
			}
			// A reader error before the string is returned as is.
			s.Reset(&errAfterReader{err: boom}, nil)
			if _, err := tier.fn(&s, true); err != boom {
				t.Errorf("transient head: got %v, want raw reader error", err)
			}
			// A reader error inside the string is returned as is, not as
			// ErrUnterminated.
			s.Reset(&errAfterReader{data: []byte(`"abc`), err: boom}, nil)
			if _, err := tier.fn(&s, true); err != boom {
				t.Errorf("transient mid-string: got %v, want raw reader error", err)
			}
		})
	}
}

// TestStreamStringSIMD_ErrorPos is the SIMD version of
// TestStreamString_ErrorPos. Every error return in stringViewAVX* must leave
// Offset() where the scalar code does. The prefix is read first, so later
// reads move the unread bytes to the front of the buffer. An error return
// that does not adjust Pos for that move reports an offset that is too
// large. On the unterminated row it would point past the end of the input.
func TestStreamStringSIMD_ErrorPos(t *testing.T) {
	t.Parallel()
	const prefix = `"pre"`
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"ctrl after refills", `"` + strings.Repeat("a", 100) + "\x01\"", ErrBadString},
		{"invalid utf8 after refills", `"` + strings.Repeat("a", 100) + "\xff\"", ErrInvalidUTF8},
		{"unterminated", `"` + strings.Repeat("a", 100), ErrUnterminated},
		{"not a string at compacted head", `[]`, ErrExpectString},
		{"drained at compacted head", ``, ErrExpectString},
		{"ctrl in first window", "\"ab\x01cd\"", ErrBadString},
		{"truncated escape", `"ab\u12`, ErrBadString},
	}
	tiers := []struct {
		name string
		fn   func(*Stream, bool) (string, error)
	}{
		{"AVX", (*Stream).StringAVX},
		{"AVX2", (*Stream).StringAVX2},
		{"AVX512", (*Stream).StringAVX512},
	}
	for _, tc := range cases {
		doc := prefix + tc.in
		for _, chunk := range []int{5, 7, 64} {
			var ref Stream
			ref.Reset(strings.NewReader(doc), make([]byte, 0, chunk))
			if err := ref.skipString(); err != nil {
				t.Fatal(err)
			}
			if _, err := ref.String(true); err != tc.want {
				t.Fatalf("%s chunk=%d: scalar err %v, want %v", tc.name, chunk, err, tc.want)
			}
			for _, tier := range tiers {
				var s Stream
				s.Reset(strings.NewReader(doc), make([]byte, 0, chunk))
				if err := s.skipString(); err != nil {
					t.Fatal(err)
				}
				if _, err := tier.fn(&s, true); err != tc.want {
					t.Errorf("%s/%s chunk=%d: err %v, want %v", tc.name, tier.name, chunk, err, tc.want)
					continue
				}
				if s.Offset() != ref.Offset() || s.Pos > len(s.Bytes()) {
					t.Errorf("%s/%s chunk=%d: Offset %d (Pos %d, len(buf) %d, doc len %d), scalar %d",
						tc.name, tier.name, chunk, s.Offset(), s.Pos, len(s.Bytes()), len(doc), ref.Offset())
				}
			}
		}
	}
}

// Every SIMD version must report an unterminated string that holds an escape
// exactly like String does, with the escape at every position in a vector.
// It must also do so without allocating.
func TestStringSIMD_UnterminatedEscaped(t *testing.T) { //nolint:paralleltest // measures allocations
	tails := []string{`\n`, `\`, `\u`, `\u12`, `\u12zz`, `\q`, `\ud800`, `\ud800abc`, `\ud800\udc0`, "\\n\x01", `\ud83d\ude00x`}
	for n := range 70 {
		for _, tail := range tails {
			data := []byte(`"` + strings.Repeat("a", n) + tail)
			for _, validate := range []bool{true, false} {
				_, wp, we := String(data, 0, validate)
				for _, tier := range stringTiers {
					if _, p, err := tier.fn(data, 0, validate); p != wp || err != we {
						t.Errorf("%s(%q, validate=%v) = (%d, %v), String = (%d, %v)", tier.name, data, validate, p, err, wp, we)
					}
				}
			}
		}
	}
	data := append([]byte(`"a\n`), bytes.Repeat([]byte("x"), 1<<20)...)
	for _, tier := range stringTiers {
		if allocs := testing.AllocsPerRun(5, func() { _, _, _ = tier.fn(data, 0, true) }); allocs != 0 {
			t.Errorf("%s(unterminated escaped) allocates %v per call, want 0", tier.name, allocs)
		}
	}
}

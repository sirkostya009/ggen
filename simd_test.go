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

// TestStringSIMD_Parity pins every tier byte-identical to scalar String:
// same value, same position, same error identity — across escape placement,
// control bytes, truncation, and vector-width phase alignment.
func TestStringSIMD_Parity(t *testing.T) {
	cases := [][]byte{
		[]byte(`""`), []byte(`"a"`), []byte(`"ab"`), []byte(`not a string`), {},
		[]byte(`"unterminated`), []byte(`"trailing\`), []byte(`"bad\u12`),
		[]byte(`"esc\nape"`), []byte(`"A😀"`), []byte(`"\q"`),
		// Final malformations at the end of data vs truncated prefixes.
		[]byte(`"\u12"`), []byte(`"ab\uZ"`), []byte(`"\u00`), []byte(`"\ud83d\uDE`),
		[]byte(`"\ud83d"`), []byte("\"ab\x01"), []byte("\"ab\x01}"),
	}
	for _, n := range []int{1, 7, 15, 16, 17, 31, 32, 33, 63, 64, 65, 127, 500} {
		body := strings.Repeat("x", n)
		cases = append(cases,
			[]byte(`"`+body+`"`),
			[]byte(`"`+body),               // unterminated
			[]byte(`"`+body+`\n"`),         // escape at phase boundary
			[]byte(`"`+body+"\x01"+`"`),    // ctrl at phase boundary
			[]byte(`"`+body+`\n`+body+`"`), // escape mid-string
			[]byte(`"`+"\x1f"+body+`"`),    // ctrl first
			[]byte(`"a`+"\x01"+`b\nc"`),    // ctrl before escape
			[]byte(`"`+body+"\xff"+`"`),    // invalid UTF-8 at phase boundary
			[]byte(`"`+"\xff"+body+`"`),    // invalid UTF-8 first
			[]byte(`"`+body+"é😀"+`"`),      // valid multi-byte at phase boundary
			[]byte(`"`+body+"\xe2("+`"`),   // truncated 3-byte rune
		)
	}
	rng := rand.New(rand.NewSource(1))
	for range 2000 {
		n := rng.Intn(200)
		b := make([]byte, n+2)
		b[0] = '"'
		for i := 1; i <= n; i++ {
			b[i] = byte(rng.Intn(130)) // bias into ctrl/quote/backslash space
		}
		b[n+1] = '"'
		cases = append(cases, b)
	}
	// Both validate arms: strict (jsonv2 reject) AND permissive
	// (allowinvalidutf8 — raw bytes pass through) must match scalar.
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

// chunkReader yields at most n bytes per Read to force mid-string refills.
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

// TestStreamStringSIMD_Parity pins the stream tier scanners against scalar
// Stream.String across every refill shape: bodies sized around lane seams,
// escapes/ctrl at each phase, chunked reads of 1..64 bytes, and randomized
// mixed bodies. Value, view, and key contracts all route through the same
// per-tier core, so String* parity covers all three shells.
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
	bodies = append(bodies, "", "a", "hello", `esc\"aped`, `tail\\`, `uniécode`)
	for _, n := range []int{1, 15, 16, 17, 31, 32, 33, 63, 64, 65, 130, 600} {
		b := bytes.Repeat([]byte{'x'}, n)
		bodies = append(bodies, string(b))
		for _, c := range []byte{'\\', 0x01} {
			for pos := 0; pos < n; pos += max(1, n/5) {
				bb := bytes.Repeat([]byte{'y'}, n)
				bb[pos] = c
				if c == '\\' && pos == n-1 {
					continue // trailing backslash escapes the closing quote
				}
				if c == '\\' {
					bb[pos+1] = 'n'
				}
				bodies = append(bodies, string(bb))
			}
		}
	}
	// Invalid UTF-8 at window seams: permissive mode must pass bytes
	// through verbatim, strict must reject identically to scalar.
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

// errAfterReader yields data, then a transient (non-EOF) error forever.
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

// TestStreamStringSIMD_RefillErrorIdentity pins the tier refill arms against
// the scalar contracts (round-7 fix): a drained reader at a string position
// maps to ErrExpectString / ErrUnterminated like the scalar path, and a
// transient reader error propagates raw instead of being relabeled.
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
			// Drained at the head: scalar maps to ErrExpectString.
			var s Stream
			s.Reset(strings.NewReader(""), nil)
			if _, err := tier.fn(&s, true); err != ErrExpectString {
				t.Errorf("drained head: got %v, want ErrExpectString", err)
			}
			// Drained mid-string: ErrUnterminated.
			s.Reset(strings.NewReader(`"abc`), nil)
			if _, err := tier.fn(&s, true); err != ErrUnterminated {
				t.Errorf("drained mid-string: got %v, want ErrUnterminated", err)
			}
			// Transient error at the head propagates raw.
			s.Reset(&errAfterReader{err: boom}, nil)
			if _, err := tier.fn(&s, true); err != boom {
				t.Errorf("transient head: got %v, want raw reader error", err)
			}
			// Transient error mid-string propagates raw, not ErrUnterminated.
			s.Reset(&errAfterReader{data: []byte(`"abc`), err: boom}, nil)
			if _, err := tier.fn(&s, true); err != boom {
				t.Errorf("transient mid-string: got %v, want raw reader error", err)
			}
		})
	}
}

// TestStreamStringSIMD_ErrorPos is the tier twin of TestStreamString_ErrorPos:
// every error exit of the stringViewAVX* cores must leave Offset() where the
// scalar core does. A consumed prefix makes the window compact first, so an
// exit that skips the rebase reports the pre-compaction cursor — inflated by
// the discarded prefix, past the document on the unterminated row.
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

// Every tier classifies an unterminated escaped string exactly as String does
// — the escape landing at each lane position — and copies nothing to do it.
func TestStringSIMD_UnterminatedEscaped(t *testing.T) {
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
		if allocs := testing.AllocsPerRun(5, func() { tier.fn(data, 0, true) }); allocs != 0 {
			t.Errorf("%s(unterminated escaped) allocates %v per call, want 0", tier.name, allocs)
		}
	}
}

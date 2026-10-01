//go:build goexperiment.simd && unix

package ggen

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"
)

// TestSIMD_NoOverRead checks that no SIMD function reads past the end of its
// input. SIMD code loads 16, 32 or 64 bytes with one CPU instruction, so a
// load near the end can reach beyond the slice. Each probe puts its input
// right before a PROT_NONE page, a page that cannot be read. An over-read
// then crashes with SIGSEGV. That kills the whole process instead of failing
// the test, so the probes run in a child test binary and the parent reports
// the crash. Bytes read past the end are ignored by the result, so comparing
// outputs can never catch this. Only the guard page can.
func TestSIMD_NoOverRead(t *testing.T) {
	t.Parallel()
	if os.Getenv("GGEN_OVERREAD_PROBE") == "1" {
		overReadProbe(t)
		return
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSIMD_NoOverRead$", "-test.count=1")
	cmd.Env = append(os.Environ(), "GGEN_OVERREAD_PROBE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe child: %v\n%s", err, out)
	}
}

// guardPage maps a readable page followed by a PROT_NONE page. place copies
// src so that its last byte is the last readable byte. The returned slice's
// capacity also ends there, so a Stream that reads into it stays right
// against the guard page.
func guardPage(t *testing.T) (place func(src []byte) []byte, done func()) {
	t.Helper()
	page := syscall.Getpagesize()
	mem, err := syscall.Mmap(-1, 0, 2*page, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mprotect(mem[page:], syscall.PROT_NONE); err != nil {
		t.Fatal(err)
	}
	return func(src []byte) []byte {
		if len(src) > page {
			t.Fatalf("probe input of %d bytes exceeds the page", len(src))
		}
		dst := mem[page-len(src) : page : page]
		copy(dst, src)
		return dst
	}, func() { _ = syscall.Munmap(mem) }
}

func overReadProbe(t *testing.T) {
	t.Helper()
	place, done := guardPage(t)
	defer done()

	const maxN = 130
	bodies := func(n int) []string {
		x := strings.Repeat("x", n)
		return []string{
			x,
			strings.Repeat("é", n/2) + x[:n%2],
			strings.Repeat("😀", n/4) + x[:n%4],
			x + "\\n",
			x + "\x01",
			x + "\xff",
			x + "\xe2(",
		}
	}

	// String functions on []byte input, with and without a closing quote,
	// at every length.
	for n := 0; n <= maxN; n++ {
		for _, body := range bodies(n) {
			for _, payload := range []string{`"` + body + `"`, `"` + body} {
				in := place([]byte(payload))
				for _, validate := range []bool{true, false} {
					ws, wp, we := String(in, 0, validate)
					for _, tier := range stringTiers {
						s, p, err := tier.fn(in, 0, validate)
						if s != ws || p != wp || err != we {
							t.Fatalf("%s(%q, validate=%v) = (%q, %d, %v), scalar (%q, %d, %v)", tier.name, payload, validate, s, p, err, ws, wp, we)
						}
					}
				}
			}
		}
	}

	// UTF-8 validators, at lengths below and above the point where
	// validUTF8x64 stops handing off to validUTF8x16. The last two spans
	// end in a cut-off multi-byte character.
	for n := 0; n <= 2*maxN; n++ {
		for _, span := range []string{
			strings.Repeat("é", n/2) + strings.Repeat("a", n%2),
			strings.Repeat("😀", n/4) + strings.Repeat("a", n%4),
			strings.Repeat("a", n) + "\xc3",
			strings.Repeat("a", n) + "\xf0\x9f\x98",
		} {
			in := place([]byte(span))
			want := utf8.Valid(in)
			if got := validUTF8x16(in); got != want {
				t.Fatalf("validUTF8x16(len %d) = %v, want %v", len(in), got, want)
			}
			if got := validUTF8x64(in); got != want {
				t.Fatalf("validUTF8x64(len %d) = %v, want %v", len(in), got, want)
			}
		}
	}

	// Skip and whitespace functions on []byte input.
	skipTiers := []struct {
		name string
		fn   func([]byte, int) (int, error)
	}{{"AVX", SkipValueAVX}, {"AVX2", SkipValueAVX2}, {"AVX512", SkipValueAVX512}}
	spaceTiers := []struct {
		name string
		fn   func([]byte, int) int
	}{{"AVX", SkipSpaceAVX}, {"AVX2", SkipSpaceAVX2}, {"AVX512", SkipSpaceAVX512}}
	for n := 0; n <= maxN; n++ {
		x := strings.Repeat("x", n)
		for _, payload := range []string{
			`"` + x + `"`, `"` + x + `\n"`, `[1,"` + x + `",{"k":"` + x + `"}]`, `{"` + x + `":[` + x + `]}`,
			`"` + x, `[` + x, `[1,2`,
		} {
			in := place([]byte(payload))
			wp, we := SkipValue(in, 0)
			for _, tier := range skipTiers {
				if p, err := tier.fn(in, 0); p != wp || err != we {
					t.Fatalf("SkipValue%s(%q) = (%d, %v), scalar (%d, %v)", tier.name, payload, p, err, wp, we)
				}
			}
		}
		for _, payload := range []string{strings.Repeat(" ", n), strings.Repeat(" \t\n\r", n/4+1) + "x"} {
			in := place([]byte(payload))
			want := SkipSpace(in, 0)
			for _, tier := range spaceTiers {
				if got := tier.fn(in, 0); got != want {
					t.Fatalf("SkipSpace%s(len %d) = %d, scalar %d", tier.name, len(in), got, want)
				}
			}
		}
	}

	// Encode functions. The string points straight at the bytes at the
	// page edge, without a copy.
	encTiers := []struct {
		name   string
		fn     func([]byte, string) []byte
		scalar func([]byte, string) []byte
	}{
		{"AppendStringAVX", AppendStringAVX, AppendString},
		{"AppendStringAVX2", AppendStringAVX2, AppendString},
		{"AppendStringAVX512", AppendStringAVX512, AppendString},
		{"AppendStringNoHTMLAVX", AppendStringNoHTMLAVX, AppendStringNoHTML},
		{"AppendStringNoHTMLAVX2", AppendStringNoHTMLAVX2, AppendStringNoHTML},
		{"AppendStringNoHTMLAVX512", AppendStringNoHTMLAVX512, AppendStringNoHTML},
	}
	for n := 0; n <= maxN; n++ {
		for _, body := range []string{strings.Repeat("x", n), strings.Repeat("x", n) + "\"<&\n"} {
			s := BytesToString(place([]byte(body)))
			want := AppendString(nil, s)
			for _, tier := range encTiers {
				if tier.scalar != nil {
					want = tier.scalar(nil, s)
				}
				if got := tier.fn(nil, s); !bytes.Equal(got, want) {
					t.Fatalf("%s(%q) = %q, scalar %q", tier.name, body, got, want)
				}
			}
		}
	}

	// Stream functions. The Stream's buffer, called the window, ends at the
	// page edge. The first read from the reader fills the whole payload right
	// up against the guard page.
	type streamFn = func(*Stream) (string, error)
	streamTiers := []struct {
		name   string
		fn     streamFn
		scalar streamFn
	}{
		{"StringAVX", func(s *Stream) (string, error) { return s.StringAVX(true) }, func(s *Stream) (string, error) { return s.String(true) }},
		{"StringAVX2", func(s *Stream) (string, error) { return s.StringAVX2(true) }, func(s *Stream) (string, error) { return s.String(true) }},
		{"StringAVX512", func(s *Stream) (string, error) { return s.StringAVX512(true) }, func(s *Stream) (string, error) { return s.String(true) }},
		{"KeyViewAVX", func(s *Stream) (string, error) { return s.KeyViewAVX(true) }, func(s *Stream) (string, error) { return s.KeyView(true) }},
		{"KeyViewAVX2", func(s *Stream) (string, error) { return s.KeyViewAVX2(true) }, func(s *Stream) (string, error) { return s.KeyView(true) }},
		{"KeyViewAVX512", func(s *Stream) (string, error) { return s.KeyViewAVX512(true) }, func(s *Stream) (string, error) { return s.KeyView(true) }},
		{"SkipValueAVX", func(s *Stream) (string, error) { return "", s.SkipValueAVX() }, func(s *Stream) (string, error) { return "", s.SkipValue() }},
		{"SkipValueAVX2", func(s *Stream) (string, error) { return "", s.SkipValueAVX2() }, func(s *Stream) (string, error) { return "", s.SkipValue() }},
		{"SkipValueAVX512", func(s *Stream) (string, error) { return "", s.SkipValueAVX512() }, func(s *Stream) (string, error) { return "", s.SkipValue() }},
		{
			"CaptureValueAVX",
			func(s *Stream) (string, error) { b, err := s.CaptureValueAVX(); return string(b), err },
			func(s *Stream) (string, error) { b, err := s.CaptureValue(); return string(b), err },
		},
		{
			"CaptureValueAVX2",
			func(s *Stream) (string, error) { b, err := s.CaptureValueAVX2(); return string(b), err },
			func(s *Stream) (string, error) { b, err := s.CaptureValue(); return string(b), err },
		},
		{
			"CaptureValueAVX512",
			func(s *Stream) (string, error) { b, err := s.CaptureValueAVX512(); return string(b), err },
			func(s *Stream) (string, error) { b, err := s.CaptureValue(); return string(b), err },
		},
		{"SkipSpaceAVX", func(s *Stream) (string, error) { return "", s.SkipSpaceAVX() }, func(s *Stream) (string, error) { return "", s.SkipSpace() }},
		{"SkipSpaceAVX2", func(s *Stream) (string, error) { return "", s.SkipSpaceAVX2() }, func(s *Stream) (string, error) { return "", s.SkipSpace() }},
		{"SkipSpaceAVX512", func(s *Stream) (string, error) { return "", s.SkipSpaceAVX512() }, func(s *Stream) (string, error) { return "", s.SkipSpace() }},
	}
	run := func(fn streamFn, payload []byte) (string, int, error) {
		var s Stream
		s.Reset(bytes.NewReader(payload), place(payload)[:0])
		v, err := fn(&s)
		return v, s.Pos, err
	}
	for n := 0; n <= maxN; n++ {
		x := strings.Repeat("x", n)
		for _, body := range bodies(n) {
			for _, payload := range []string{`"` + body + `"`, `"` + body, `[1,"` + body + `"]`, strings.Repeat(" ", n) + "x", x + `"`} {
				for _, tier := range streamTiers {
					wv, wp, we := run(tier.scalar, []byte(payload))
					v, p, err := run(tier.fn, []byte(payload))
					if v != wv || err != we || (err == nil && p != wp) {
						t.Fatalf("Stream.%s(%q) = (%q, %d, %v), scalar (%q, %d, %v)", tier.name, payload, v, p, err, wv, wp, we)
					}
				}
			}
		}
	}
}

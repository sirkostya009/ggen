package bench

// The BenchmarkSmall_* benchmarks decode the ValidPayload of about 2.9 KiB.
// They measure parse and streaming overhead at a size where the decoded value
// is too small to hide the cost of per-call buffer management.

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"io"
	"testing"

	"github.com/bytedance/sonic"

	"github.com/sirkostya009/ggen"
)

// BenchmarkSmall_Unmarshal decodes ValidPayload into a Validated value on the
// bytes path. It is the companion of BenchmarkSmall_Reader and measures the
// parse cost of the generated code without streaming buffer management.
func BenchmarkSmall_Unmarshal(b *testing.B) {
	codecs := []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v Validated; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v Validated; return sonic.Unmarshal(p, &v) }},
		{"sonic_fast", func(p []byte) error { var v Validated; return sonic.ConfigFastest.Unmarshal(p, &v) }},
		{"ggen", func(p []byte) error { _, _, err := Validated{}.DecodeFrom(p); return err }},
		// ggen_copy: -copy mode (CopyValidated) copies strings out of the
		// input. The 2800-byte Bio dominates, so this measures the copy cost.
		{"ggen_copy", func(p []byte) error { _, _, err := CopyValidated{}.DecodeFrom(p); return err }},
	}
	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			runBench(b, int64(len(ValidPayload)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if err := c.fn(ValidPayload); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

// BenchmarkSmall_Reader decodes ValidPayload from an io.Reader. There are two
// ggen stream rows. One starts with a 512-byte buffer, which forces the
// buffer to compact and grow. The other starts with a buffer the size of the
// payload, so it never grows. jsonv2, sonic and ggen-readall rows are for
// reference.
func BenchmarkSmall_Reader(b *testing.B) {
	type readerState struct {
		r   bytes.Reader
		buf []byte
	}

	codecs := []struct {
		name   string
		bufCap int // initial buffer capacity for the ggen stream rows, 0 for the others
		fn     func(*readerState) error
	}{
		{"jsonv2", 0, func(s *readerState) error {
			s.r.Reset(ValidPayload)
			var v Validated
			return jsonv2.UnmarshalRead(&s.r, &v)
		}},
		{"sonic", 0, func(s *readerState) error {
			s.r.Reset(ValidPayload)
			var v Validated
			return sonic.ConfigDefault.NewDecoder(&s.r).Decode(&v)
		}},
		{"sonic_fast", 0, func(s *readerState) error {
			s.r.Reset(ValidPayload)
			var v Validated
			return sonic.ConfigFastest.NewDecoder(&s.r).Decode(&v)
		}},
		{"ggen_stream_512", 512, func(s *readerState) error {
			s.r.Reset(ValidPayload)
			var _st ggen.Stream
			_st.Reset(&s.r, make([]byte, 0, 512))
			_, err := Validated{}.DecodeFromStream(&_st)
			return err
		}},
		{"ggen_stream_full", len(ValidPayload), func(s *readerState) error {
			s.r.Reset(ValidPayload)
			var err error
			var _st ggen.Stream
			_st.Reset(&s.r, s.buf[:0])
			_, err = Validated{}.DecodeFromStream(&_st)
			s.buf = _st.Bytes()
			return err
		}},
		{"ggen_readall", 0, func(s *readerState) error {
			s.r.Reset(ValidPayload)
			data, err := io.ReadAll(&s.r)
			if err != nil {
				return err
			}
			_, _, err = Validated{}.DecodeFrom(data)
			return err
		}},
	}

	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			bufCap := c.bufCap
			runBench(b, int64(len(ValidPayload)),
				func() readerState { return readerState{buf: make([]byte, 0, bufCap)} },
				func(s *readerState) {
					if err := c.fn(s); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

// BenchmarkTiny_Unmarshal measures the minimum overhead of decoding, at a
// payload size where the fixed setup cost of each call outweighs the scan
// work.
func BenchmarkTiny_Unmarshal(b *testing.B) {
	codecs := []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v Claim; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v Claim; return sonic.Unmarshal(p, &v) }},
		{"sonic_fast", func(p []byte) error { var v Claim; return sonic.ConfigFastest.Unmarshal(p, &v) }},
		{"easyjson", func(p []byte) error { var v EasyClaim; return v.UnmarshalJSON(p) }},
		{"ggen", func(p []byte) error { _, _, err := Claim{}.DecodeFrom(p); return err }},
		// ggen_copy: -copy mode (CopyClaim). It shows the cost of copying each
		// string when little else is going on.
		{"ggen_copy", func(p []byte) error { _, _, err := CopyClaim{}.DecodeFrom(p); return err }},
	}
	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			runBench(b, int64(len(TinyPayload)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if err := c.fn(TinyPayload); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

// BenchmarkTiny_Marshal is the encode version. With about 150 bytes of
// output, allocating the buffer on each call is a much larger part of the
// total cost than at Mega's size.
func BenchmarkTiny_Marshal(b *testing.B) {
	codecs := []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"jsonv2", func() ([]byte, error) { return jsonv2.Marshal(TinyValue) }},
		{"sonic", func() ([]byte, error) { return sonic.Marshal(TinyValue) }},
		{"sonic_fast", func() ([]byte, error) { return sonic.ConfigFastest.Marshal(TinyValue) }},
		{"easyjson", func() ([]byte, error) { return EasyTinyValue.MarshalJSON() }},
		{"ggen", func() ([]byte, error) { return ggen.Marshal(TinyValue) }},
	}
	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			out, _ := c.fn()
			runBench(b, int64(len(out)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if _, err := c.fn(); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

// BenchmarkValidationHeavy_Unmarshal decodes and checks rules on every field.
// ggen runs about 25 validation checks per payload, while jsonv2 and sonic
// run none. The cost per check is the main number to read.
func BenchmarkValidationHeavy_Unmarshal(b *testing.B) {
	codecs := []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2_noval", func(p []byte) error { var v ValidationHeavy; return jsonv2.Unmarshal(p, &v) }},
		{"sonic_noval", func(p []byte) error { var v ValidationHeavy; return sonic.Unmarshal(p, &v) }},
		{"easyjson_noval", func(p []byte) error { var v EasyValidationHeavy; return v.UnmarshalJSON(p) }},
		{"ggen_noval", func(p []byte) error { _, _, err := NoValidationHeavy{}.DecodeFrom(p); return err }},
		{"ggen_validated", func(p []byte) error { _, _, err := ValidationHeavy{}.DecodeFrom(p); return err }},
	}
	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			runBench(b, int64(len(ValidationHeavyPayload)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if err := c.fn(ValidationHeavyPayload); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

// BenchmarkRuneGated_Unmarshal measures the rune rules alone on long strings
// of about 8 KB. It measures validation cost during decode. No other library
// validates, so there is only a ggen row.
func BenchmarkRuneGated_Unmarshal(b *testing.B) {
	runBench(b, int64(len(RuneGatedPayload)),
		func() struct{} { return struct{}{} },
		func(_ *struct{}) {
			if _, _, err := (RuneGated{}).DecodeFrom(RuneGatedPayload); err != nil {
				b.Fatal(err)
			}
		},
	)
}

// BenchmarkHTMLEscape_MarshalParity compares the htmlescape option, which
// writes `<`, `>` and `&` as \uXXXX escapes, with the default output that
// writes them as is and matches jsonv2.
func BenchmarkHTMLEscape_MarshalParity(b *testing.B) {
	codecs := []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"ggen_noescape", func() ([]byte, error) { return ggen.Marshal(HTMLPlainValue) }},
		{"ggen_htmlescape", func() ([]byte, error) { return ggen.Marshal(HTMLEscapeValue) }},
		{"jsonv2", func() ([]byte, error) { return jsonv2.Marshal(HTMLPlainValue) }},
		{"sonic", func() ([]byte, error) { return sonic.Marshal(HTMLPlainValue) }},
		{"easyjson", func() ([]byte, error) { return EasyHTMLPlainValue.MarshalJSON() }},
	}
	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			out, _ := c.fn()
			runBench(b, int64(len(out)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if _, err := c.fn(); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

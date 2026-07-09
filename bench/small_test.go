//go:build goexperiment.jsonv2

package bench

// BenchmarkSmall_* — small-value benchmarks on the ~2.9 KiB ValidPayload,
// isolating parse / streaming overhead at a size where the decoded value
// is too small to drown out per-call buffer management.

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"io"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/scan"
)

// BenchmarkSmall_Unmarshal — bytes-path decode of ValidPayload into a
// Validated value. Companion to BenchmarkSmall_Reader; isolates the
// codegen+parse overhead from streaming buffer management.
func BenchmarkSmall_Unmarshal(b *testing.B) {
	var codecs = []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v Validated; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v Validated; return sonic.Unmarshal(p, &v) }},
		{"sonic_fast", func(p []byte) error { var v Validated; return sonic.ConfigFastest.Unmarshal(p, &v) }},
		{"ggen", func(p []byte) error { _, _, err := Validated{}.DecodeFrom(p); return err }},
		// ggen_copy: -copy mode (CopyValidated) — copies strings out of the
		// input; the 2800 B Bio dominates, isolating the copy-out cost.
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

// BenchmarkSmall_Reader — Reader-path decode of ValidPayload. Two ggen-stream
// rows: 512-byte initial buf (forces shifts + grows) vs payload-sized buf (no
// growth). Reference rows from jsonv2 / sonic / ggen-readall.
func BenchmarkSmall_Reader(b *testing.B) {
	type readerState struct {
		r   bytes.Reader
		buf []byte
	}

	var codecs = []struct {
		name   string
		bufCap int // initial buf cap for ggen-stream variants; 0 elsewhere
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
			var _st scan.Stream
			_st.Reset(&s.r, make([]byte, 0, 512))
			_, err := Validated{}.DecodeFromStream(&_st)
			return err
		}},
		{"ggen_stream_full", len(ValidPayload), func(s *readerState) error {
			s.r.Reset(ValidPayload)
			var err error
			var _st scan.Stream
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
			cap := c.bufCap
			runBench(b, int64(len(ValidPayload)),
				func() readerState { return readerState{buf: make([]byte, 0, cap)} },
				func(s *readerState) {
					if err := c.fn(s); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

// BenchmarkTiny_Unmarshal — overhead floor of the dispatch path at a
// payload size where per-call setup costs dominate the actual scan work.
func BenchmarkTiny_Unmarshal(b *testing.B) {
	var codecs = []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v Claim; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v Claim; return sonic.Unmarshal(p, &v) }},
		{"sonic_fast", func(p []byte) error { var v Claim; return sonic.ConfigFastest.Unmarshal(p, &v) }},
		{"easyjson", func(p []byte) error { var v EasyClaim; return v.UnmarshalJSON(p) }},
		{"ggen", func(p []byte) error { _, _, err := Claim{}.DecodeFrom(p); return err }},
		// ggen_copy: -copy mode (CopyClaim) — per-string copy tax at the
		// dispatch-overhead floor.
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

// BenchmarkTiny_Marshal — encode-side analogue. At ~150 B output, the
// per-call buffer alloc is a larger fraction of total cost than at mega
// scale.
func BenchmarkTiny_Marshal(b *testing.B) {
	var codecs = []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"jsonv2", func() ([]byte, error) { return jsonv2.Marshal(TinyValue) }},
		{"sonic", func() ([]byte, error) { return sonic.Marshal(TinyValue) }},
		{"sonic_fast", func() ([]byte, error) { return sonic.ConfigFastest.Marshal(TinyValue) }},
		{"easyjson", func() ([]byte, error) { return EasyTinyValue.MarshalJSON() }},
		{"ggen", func() ([]byte, error) { return encode.Marshal(TinyValue) }},
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

// BenchmarkValidationHeavy_Unmarshal — decode + per-field rule check on
// every field. ggen runs ~25 validation checks per payload (vs jsonv2/
// sonic which do zero). The per-check cost is the headline number.
func BenchmarkValidationHeavy_Unmarshal(b *testing.B) {
	var codecs = []struct {
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

// BenchmarkRuneGated_Unmarshal isolates rune-rule validation on long (~8 KB)
// strings. Decode-only validation cost; no other codec validates, so a single
// ggen row.
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

// BenchmarkHTMLEscape_MarshalParity — htmlescape opt-in (\uXXXX expansion of
// `<` / `>` / `&`) vs the default literal output, which matches jsonv2.
func BenchmarkHTMLEscape_MarshalParity(b *testing.B) {
	var codecs = []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"ggen_noescape", func() ([]byte, error) { return encode.Marshal(HTMLPlainValue) }},
		{"ggen_htmlescape", func() ([]byte, error) { return encode.Marshal(HTMLEscapeValue) }},
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

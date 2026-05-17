//go:build goexperiment.jsonv2

package bench

// BenchmarkSmall_* — small-value benchmarks on the ~2.9 KiB ValidPayload.
// Where BenchmarkMega_* exercises the deep-tree case (decoded Node tree
// dominates time and memory), this file isolates parse / streaming
// overhead at a size where the decoded value is too small to drown out
// per-call buffer management.

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"io"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/sirkostya009/ggen/decode"
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
		{"ggen", func(p []byte) error { _, err := decode.Unmarshal[Validated](p); return err }},
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

// BenchmarkSmall_Reader — Reader-path decode of ValidPayload. Two
// ggen-stream rows: 512-byte initial buf (forces shifts + a couple
// grows) vs a payload-sized buf (no growth, no shift). Reference rows
// from jsonv2 / sonic / ggen-readall at the same payload size.
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
			var err error
			_, s.buf, err = decode.UnmarshalStream[Validated](&s.r, s.buf[:0])
			return err
		}},
		{"ggen_stream_full", len(ValidPayload), func(s *readerState) error {
			s.r.Reset(ValidPayload)
			var err error
			_, s.buf, err = decode.UnmarshalStream[Validated](&s.r, s.buf[:0])
			return err
		}},
		{"ggen_readall", 0, func(s *readerState) error {
			s.r.Reset(ValidPayload)
			data, err := io.ReadAll(&s.r)
			if err != nil {
				return err
			}
			_, err = decode.Unmarshal[Validated](data)
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

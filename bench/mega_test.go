//go:build goexperiment.jsonv2

package bench

// -cpu=1 runs serially, -cpu=N with N parallel goroutines. Under RunParallel
// b.SetBytes reports aggregate MB/s; use -cpu=1 for single-thread numbers.

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"io"
	"runtime"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/mailru/easyjson"
	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/scan"
)

func reportGC(b *testing.B, pre, post runtime.MemStats) {
	b.Helper()
	b.ReportMetric(float64(post.NumGC-pre.NumGC), "gc")
}

func runBench[S any](b *testing.B, bytesPerOp int64, setup func() S, body func(*S)) {
	b.Helper()
	b.SetBytes(bytesPerOp)
	b.ReportAllocs()
	var pre, post runtime.MemStats
	runtime.ReadMemStats(&pre)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		s := setup()
		for pb.Next() {
			body(&s)
		}
	})
	b.StopTimer()
	runtime.ReadMemStats(&post)
	reportGC(b, pre, post)
}

func BenchmarkMega_Unmarshal(b *testing.B) {
	var unmarshalCodecs = []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v NodePlain; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v NodePlain; return sonic.Unmarshal(p, &v) }},
		{"sonic_fast", func(p []byte) error { var v NodePlain; return sonic.ConfigFastest.Unmarshal(p, &v) }},
		{"easyjson", func(p []byte) error { var v Node; return v.UnmarshalJSON(p) }},
		{"ggen", func(p []byte) error { _, _, err := Node{}.DecodeFrom(p); return err }},
		// ggen_copy: -copy mode (CopyNode) — bytes path copies strings/raw/any
		// out of the input instead of aliasing. The delta vs ggen is copy cost.
		{"ggen_copy", func(p []byte) error { _, _, err := CopyNode{}.DecodeFrom(p); return err }},
	}

	for _, c := range unmarshalCodecs {
		b.Run(c.name, func(b *testing.B) {
			runBench(b, int64(len(MegaPayload)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if err := c.fn(MegaPayload); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

func BenchmarkMega_Marshal(b *testing.B) {
	var marshalCodecs = []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"jsonv2", func() ([]byte, error) { return jsonv2.Marshal(MegaValuePlain) }},
		{"sonic", func() ([]byte, error) { return sonic.Marshal(MegaValuePlain) }},
		{"sonic_fast", func() ([]byte, error) { return sonic.ConfigFastest.Marshal(MegaValuePlain) }},
		{"easyjson", func() ([]byte, error) { return MegaValue.MarshalJSON() }},
		{"ggen", func() ([]byte, error) { return encode.Marshal(MegaValue) }},
	}

	for _, c := range marshalCodecs {
		b.Run(c.name, func(b *testing.B) {
			// bytesPerOp from first marshal so MB/s reflects output size.
			out, err := c.fn()
			if err != nil {
				b.Fatal(err)
			}
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

	// ggen_presized: ggen marshal reusing a caller-owned pre-grown buffer
	// across calls. No other codec row has caller-owned buffer reuse.
	b.Run("ggen_presized", func(b *testing.B) {
		buf := make([]byte, 0, MegaValue.JSONSize())
		runBench(b, int64(len(MegaPayload)),
			func() []byte { return buf },
			func(buf *[]byte) {
				out, err := MegaValue.AppendJSON((*buf)[:0])
				if err != nil {
					b.Fatal(err)
				}
				*buf = out[:0]
			},
		)
	})
}

func BenchmarkMega_Reader(b *testing.B) {
	type readerState struct {
		r   bytes.Reader
		buf []byte
	}

	var readerCodecs = []struct {
		name string
		fn   func(*readerState) error
	}{
		{"jsonv2", func(s *readerState) error {
			s.r.Reset(MegaPayload)
			var v NodePlain
			return jsonv2.UnmarshalRead(&s.r, &v)
		}},
		{"sonic", func(s *readerState) error {
			s.r.Reset(MegaPayload)
			var v NodePlain
			return sonic.ConfigDefault.NewDecoder(&s.r).Decode(&v)
		}},
		{"sonic_fast", func(s *readerState) error {
			s.r.Reset(MegaPayload)
			var v NodePlain
			return sonic.ConfigFastest.NewDecoder(&s.r).Decode(&v)
		}},
		{"easyjson", func(s *readerState) error {
			s.r.Reset(MegaPayload)
			var v Node
			// easyjson just does io.ReadAll here
			return easyjson.UnmarshalFromReader(&s.r, &v)
		}},
		{"ggen stream", func(s *readerState) error {
			s.r.Reset(MegaPayload)
			var err error
			var _st scan.Stream
			_st.Reset(&s.r, s.buf[:0])
			_, err = Node{}.DecodeFromStream(&_st)
			s.buf = _st.Bytes()
			return err
		}},
		{"ggen readall", func(s *readerState) error {
			s.r.Reset(MegaPayload)
			data, err := io.ReadAll(&s.r)
			if err != nil {
				return err
			}
			_, _, err = Node{}.DecodeFrom(data)
			return err
		}},
	}

	for _, c := range readerCodecs {
		b.Run(c.name, func(b *testing.B) {
			runBench(b, int64(len(MegaPayload)),
				func() readerState { return readerState{buf: make([]byte, 0, 4196)} },
				func(s *readerState) {
					if err := c.fn(s); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

// BenchmarkDeepNested_Unmarshal — single 50-level chain, isolating per-codec
// recursion cost from mega's fanout work. As the maximally depth-sensitive
// bench it's also the one that shows the recursion depth-cap cost (cli
// CLAUDE.md opt #51): min-of-8 floor +4.7% scalar / +10.3% avx512 vs uncapped
// (~13 / ~33 ns per level), from the per-level depth thread + `> scan.MaxDepth`
// compare in Node's decodeFromDepth core. It's noisy at count=1 (scalar swung
// 14.2→22.0µs on one binary). Mega (realistic shallow nesting) is flat — the
// compare hides under memory latency there.
func BenchmarkDeepNested_Unmarshal(b *testing.B) {
	var codecs = []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v NodePlain; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v NodePlain; return sonic.Unmarshal(p, &v) }},
		{"easyjson", func(p []byte) error { var v Node; return v.UnmarshalJSON(p) }},
		{"ggen", func(p []byte) error { _, _, err := Node{}.DecodeFrom(p); return err }},
	}
	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			runBench(b, int64(len(DeepNestedPayload)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if err := c.fn(DeepNestedPayload); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

// BenchmarkMapHeavy_Unmarshal — 1024-entry string→string map. Measures
// per-entry hash + alloc + insert cost.
func BenchmarkMapHeavy_Unmarshal(b *testing.B) {
	var codecs = []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v MapHeavy; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v MapHeavy; return sonic.Unmarshal(p, &v) }},
		{"ggen", func(p []byte) error { _, _, err := MapHeavy{}.DecodeFrom(p); return err }},
	}
	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			runBench(b, int64(len(MapHeavyPayload)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if err := c.fn(MapHeavyPayload); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

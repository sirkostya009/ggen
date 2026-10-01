package bench

// -cpu=1 runs each benchmark on one goroutine, -cpu=N on N parallel
// goroutines. Under RunParallel, b.SetBytes reports the total MB/s of all
// goroutines. Use -cpu=1 for single-thread numbers.

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"io"
	"runtime"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/mailru/easyjson"

	"github.com/sirkostya009/ggen"
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
	unmarshalCodecs := []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v NodePlain; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v NodePlain; return sonic.Unmarshal(p, &v) }},
		{"sonic_fast", func(p []byte) error { var v NodePlain; return sonic.ConfigFastest.Unmarshal(p, &v) }},
		{"easyjson", func(p []byte) error { var v Node; return v.UnmarshalJSON(p) }},
		{"ggen", func(p []byte) error { _, _, err := Node{}.DecodeFrom(p); return err }},
		// ggen_copy: -copy mode (CopyNode). The bytes path copies strings, raw
		// values and any values out of the input instead of aliasing it. The
		// difference from the ggen row is the cost of copying.
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
	marshalCodecs := []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"jsonv2", func() ([]byte, error) { return jsonv2.Marshal(MegaValuePlain) }},
		{"sonic", func() ([]byte, error) { return sonic.Marshal(MegaValuePlain) }},
		{"sonic_fast", func() ([]byte, error) { return sonic.ConfigFastest.Marshal(MegaValuePlain) }},
		{"easyjson", func() ([]byte, error) { return MegaValue.MarshalJSON() }},
		{"ggen", func() ([]byte, error) { return ggen.Marshal(MegaValue) }},
	}

	for _, c := range marshalCodecs {
		b.Run(c.name, func(b *testing.B) {
			// Take the byte count from a first marshal, so MB/s reflects the
			// output size.
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

	// ggen_presized: ggen marshal that reuses a buffer the caller owns and
	// sized in advance, across calls. No other library row reuses a
	// caller-owned buffer.
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

	readerCodecs := []struct {
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
			// easyjson reads the whole input with io.ReadAll here.
			return easyjson.UnmarshalFromReader(&s.r, &v)
		}},
		{"ggen stream", func(s *readerState) error {
			s.r.Reset(MegaPayload)
			var err error
			var _st ggen.Stream
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

// BenchmarkDeepNested_Unmarshal decodes a single chain of 50 nested Nodes. It
// measures each library's recursion cost without Mega's wide fan-out. Because
// it depends on depth the most, it is also the benchmark that shows the cost
// of the recursion depth cap (see opt #51 in cmd/ggen/CLAUDE.md). That cost
// comes from passing the depth down each level and comparing it against the
// runtime maxDepth cap (10000) in Node's decodeFromDepth. On Mega, which
// nests only a few levels, the cost does not show, because memory latency
// hides the compare.
//
// This row is the noise canary. It is tiny (microseconds) and fits in CPU
// cache, so it reacts strongly to machine state such as a capped power
// profile, even on an unchanged binary. Always warm up first and check the
// jsonv2 row in the same run as a control before trusting any change here
// (see "Running benchmarks" in bench/CLAUDE.md).
func BenchmarkDeepNested_Unmarshal(b *testing.B) {
	codecs := []struct {
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

// BenchmarkMapHeavy_Unmarshal decodes a map of 1024 string keys to string
// values. It measures the per-entry cost of hashing, allocation and insertion.
func BenchmarkMapHeavy_Unmarshal(b *testing.B) {
	codecs := []struct {
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

// BenchmarkMapValues_Unmarshal decodes maps whose values own allocations: a
// struct with a slice, and a slice. The ggen_reuse row decodes into a receiver
// that already holds data, and each entry's previous value becomes the target
// of the new decode. The plain ggen row starts from a zero value, where the
// reuse code can only add cost.
func BenchmarkMapValues_Unmarshal(b *testing.B) {
	codecs := []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v MapValues; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v MapValues; return sonic.Unmarshal(p, &v) }},
		{"ggen", func(p []byte) error { _, _, err := MapValues{}.DecodeFrom(p); return err }},
	}
	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			runBench(b, int64(len(MapValuesPayload)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if err := c.fn(MapValuesPayload); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
	b.Run("ggen_reuse", func(b *testing.B) {
		runBench(b, int64(len(MapValuesPayload)),
			func() MapValues {
				v, _, err := MapValues{}.DecodeFrom(MapValuesPayload)
				if err != nil {
					b.Fatal(err)
				}
				return v
			},
			func(prev *MapValues) {
				v, _, err := prev.DecodeFrom(MapValuesPayload)
				if err != nil {
					b.Fatal(err)
				}
				*prev = v
			},
		)
	})
}

// BenchmarkMapValuesHeavy_Unmarshal has the same shape as MapValues but much
// more memory per value, where reuse has the most to gain.
func BenchmarkMapValuesHeavy_Unmarshal(b *testing.B) {
	b.Run("jsonv2", func(b *testing.B) {
		runBench(b, int64(len(MapValuesHeavyPayload)),
			func() struct{} { return struct{}{} },
			func(_ *struct{}) {
				var v MapValuesHeavy
				if err := jsonv2.Unmarshal(MapValuesHeavyPayload, &v); err != nil {
					b.Fatal(err)
				}
			})
	})
	b.Run("ggen", func(b *testing.B) {
		runBench(b, int64(len(MapValuesHeavyPayload)),
			func() struct{} { return struct{}{} },
			func(_ *struct{}) {
				if _, _, err := (MapValuesHeavy{}).DecodeFrom(MapValuesHeavyPayload); err != nil {
					b.Fatal(err)
				}
			})
	})
	b.Run("ggen_reuse", func(b *testing.B) {
		runBench(b, int64(len(MapValuesHeavyPayload)),
			func() MapValuesHeavy {
				v, _, err := MapValuesHeavy{}.DecodeFrom(MapValuesHeavyPayload)
				if err != nil {
					b.Fatal(err)
				}
				return v
			},
			func(prev *MapValuesHeavy) {
				v, _, err := prev.DecodeFrom(MapValuesHeavyPayload)
				if err != nil {
					b.Fatal(err)
				}
				*prev = v
			})
	})
}

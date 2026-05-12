//go:build goexperiment.jsonv2

package bench

// Use -cpu=1 to run serially, -cpu=N to run with N parallel goroutines.
// b.SetBytes under RunParallel reports aggregate MB/s (summed across
// goroutines) — meaningful for saturation throughput; for single-thread
// numbers use -cpu=1.

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"io"
	"runtime"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/mailru/easyjson"
	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
)

func reportGC(b *testing.B, pre, post runtime.MemStats) {
	b.Helper()
	gcs := float64(post.NumGC - pre.NumGC)
	// heap_KB is a snapshot of HeapAlloc at b.StopTimer
	b.ReportMetric(float64(post.HeapAlloc)/1024, "heap_KB")
	// total_KB and gc are deltas across the timed region
	b.ReportMetric(float64(post.TotalAlloc-pre.TotalAlloc)/1024, "total_KB")
	b.ReportMetric(gcs, "gc")
	if b.N > 0 {
		// gc/op is per-iteration GC rate, e.g. 0.5 = one GC every two ops
		b.ReportMetric(gcs/float64(b.N), "gc/op")
	}
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
		{"jsonv2", func(p []byte) error { var v Node; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v Node; return sonic.Unmarshal(p, &v) }},
		{"easyjson", func(p []byte) error { var v Node; return v.UnmarshalJSON(p) }},
		{"ggen", func(p []byte) error { _, err := decode.Unmarshal[Node](p); return err }},
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
		{"jsonv2", func() ([]byte, error) { return jsonv2.Marshal(MegaValue) }},
		{"sonic", func() ([]byte, error) { return sonic.Marshal(MegaValue) }},
		{"easyjson", func() ([]byte, error) { return MegaValue.MarshalJSON() }},
		{"ggen", func() ([]byte, error) { return encode.Marshal(MegaValue) }},
	}

	for _, c := range marshalCodecs {
		b.Run(c.name, func(b *testing.B) {
			// bytesPerOp set after first marshal so MB/s reflects output size.
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
			var v Node
			return jsonv2.UnmarshalRead(&s.r, &v)
		}},
		{"sonic", func(s *readerState) error {
			s.r.Reset(MegaPayload)
			var v Node
			return sonic.ConfigDefault.NewDecoder(&s.r).Decode(&v)
		}},
		{"easyjson", func(s *readerState) error {
			s.r.Reset(MegaPayload)
			var v Node
			// easyjson just does io.ReadAll there
			return easyjson.UnmarshalFromReader(&s.r, &v)
		}},
		{"ggen stream", func(s *readerState) error {
			s.r.Reset(MegaPayload)
			var err error
			_, s.buf, err = decode.UnmarshalStream[Node](&s.r, s.buf[:0])
			return err
		}},
		{"ggen readall", func(s *readerState) error {
			s.r.Reset(MegaPayload)
			data, err := io.ReadAll(&s.r)
			if err != nil {
				return err
			}
			_, err = decode.Unmarshal[Node](data)
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

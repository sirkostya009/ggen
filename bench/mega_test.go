//go:build goexperiment.jsonv2

package bench

// -cpu=1 runs serially, -cpu=N with N parallel goroutines. Under RunParallel
// b.SetBytes reports aggregate MB/s; use -cpu=1 for single-thread numbers.

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/mailru/easyjson"
	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/scan"
)

// memProfileSample holds alloc counters attributed to the top non-runtime
// stack frame.
type memProfileSample struct {
	allocObjects int64
	allocBytes   int64
}

// snapshotMemProfile drains runtime.MemProfile and aggregates records by
// their top non-runtime stack frame. Caller diffs two snapshots for deltas.
// Set MemProfileRate=1 before the region or small per-iter allocs are missed.
func snapshotMemProfile() map[string]memProfileSample {
	records := make([]runtime.MemProfileRecord, 4096)
	for {
		n, ok := runtime.MemProfile(records, true)
		if ok {
			records = records[:n]
			break
		}
		records = make([]runtime.MemProfileRecord, n+128)
	}
	agg := make(map[string]memProfileSample)
	for _, r := range records {
		var site string
		for _, pc := range r.Stack() {
			fn := runtime.FuncForPC(pc)
			if fn == nil {
				continue
			}
			name := fn.Name()
			if strings.HasPrefix(name, "runtime.") {
				continue
			}
			site = name
			break
		}
		if site == "" {
			continue
		}
		s := agg[site]
		s.allocObjects += r.AllocObjects
		s.allocBytes += r.AllocBytes
		agg[site] = s
	}
	return agg
}

// reportTopAllocs diffs two MemProfile snapshots and reports the top-N sites
// as inline `<name>/op` bench metrics. Skips small b.N (warm-up noise).
func reportTopAllocs(b *testing.B, pre, post map[string]memProfileSample, top int) {
	b.Helper()
	if b.N < 100 {
		return
	}
	type diff struct {
		site         string
		allocObjects int64
	}
	var diffs []diff
	for site, postS := range post {
		preS := pre[site]
		dObjs := postS.allocObjects - preS.allocObjects
		if dObjs <= 0 {
			continue
		}
		diffs = append(diffs, diff{site, dObjs})
	}
	slices.SortFunc(diffs, func(a, b diff) int {
		return int(b.allocObjects - a.allocObjects)
	})
	if len(diffs) > top {
		diffs = diffs[:top]
	}
	for _, d := range diffs {
		name := d.site
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		b.ReportMetric(float64(d.allocObjects)/float64(b.N), name+"/op")
	}
}

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

// BenchmarkRetention measures retained heap per held output. HeapInuse delta /
// iters = retain_KB/op (heap cost of one held response); retain_MiB is the
// total live-set. Use a fixed iter count (-benchtime=1000x) for comparable
// numbers. Uses slowPayload (~36 KiB) to keep retained memory tractable.
func BenchmarkRetention(b *testing.B) {
	var retentionCodecs = []struct {
		name string
		// returns the decoded value as any so the sink holds it alive
		// for the HeapInuse measurement regardless of concrete type.
		fn func(*[]byte) any
	}{
		{"stdjson", func(_ *[]byte) any {
			n := new(NodePlain)
			if err := jsonv2.UnmarshalRead(bytes.NewReader(slowPayload), n); err != nil {
				b.Fatal(err)
			}
			return n
		}},
		{"sonic", func(_ *[]byte) any {
			n := new(NodePlain)
			if err := sonic.Unmarshal(slowPayload, n); err != nil {
				b.Fatal(err)
			}
			return n
		}},
		{"sonic_fast", func(_ *[]byte) any {
			n := new(NodePlain)
			if err := sonic.ConfigFastest.Unmarshal(slowPayload, n); err != nil {
				b.Fatal(err)
			}
			return n
		}},
		{"easyjson", func(_ *[]byte) any {
			n := new(Node)
			if err := easyjson.UnmarshalFromReader(bytes.NewReader(slowPayload), n); err != nil {
				b.Fatal(err)
			}
			return n
		}},
		{"ggen_stream", func(buf *[]byte) any {
			var n Node
			var err error
			var _st scan.Stream
			_st.Reset(bytes.NewReader(slowPayload), (*buf)[:0])
			n, err = Node{}.DecodeFromStream(&_st)
			*buf = _st.Bytes()
			if err != nil {
				b.Fatal(err)
			}
			return &n
		}},
		{"ggen_bytes", func(_ *[]byte) any {
			n, _, err := Node{}.DecodeFrom(slowPayload)
			if err != nil {
				b.Fatal(err)
			}
			return &n
		}},
		{"ggen_readall", func(_ *[]byte) any {
			data, err := io.ReadAll(bytes.NewReader(slowPayload))
			if err != nil {
				b.Fatal(err)
			}
			n, _, err := Node{}.DecodeFrom(data)
			if err != nil {
				b.Fatal(err)
			}
			return &n
		}},
	}

	for _, c := range retentionCodecs {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(slowPayload)))
			b.ReportAllocs()

			var mu sync.Mutex
			var sinks [][]any // per-goroutine accumulators, merged post-run

			// Top-allocs breakdown opt-in via GGEN_BENCH_TOPALLOCS=1:
			// MemProfileRate=1 captures every alloc but is ~40× slower.
			topAllocs := os.Getenv("GGEN_BENCH_TOPALLOCS") != ""
			if topAllocs {
				prevRate := runtime.MemProfileRate
				runtime.MemProfileRate = 1
				defer func() { runtime.MemProfileRate = prevRate }()
			}

			runtime.GC()
			runtime.GC()
			var pre runtime.MemStats
			runtime.ReadMemStats(&pre)
			var preProf map[string]memProfileSample
			if topAllocs {
				preProf = snapshotMemProfile()
			}

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				local := make([]any, 0, 64)
				buf := make([]byte, 0, len(slowPayload))
				for pb.Next() {
					local = append(local, c.fn(&buf))
				}
				mu.Lock()
				sinks = append(sinks, local)
				mu.Unlock()
			})
			b.StopTimer()

			runtime.GC()
			runtime.GC()
			var post runtime.MemStats
			runtime.ReadMemStats(&post)
			var postProf map[string]memProfileSample
			if topAllocs {
				postProf = snapshotMemProfile()
			}

			delta := int64(post.HeapInuse) - int64(pre.HeapInuse)
			if b.N > 0 {
				b.ReportMetric(float64(delta)/float64(b.N)/1024, "retain_KB/op")
			}
			b.ReportMetric(float64(delta)/(1024*1024), "retain_MiB")
			b.ReportMetric(float64(delta)/float64(b.N)/float64(len(slowPayload)), "retain×payload")

			// Top-5 alloc sites under the bench line (enable with
			// GGEN_BENCH_TOPALLOCS=1 go test -v).
			if topAllocs {
				reportTopAllocs(b, preProf, postProf, 5)
			}

			// Dump inuse_space pprof while sinks are still live. Set
			// GGEN_RESIDENCY_PROFILE=<dir>; file named per sub-bench. View:
			//   go tool pprof -inuse_space <dir>/<codec>.pprof
			if dir := os.Getenv("GGEN_RESIDENCY_PROFILE"); dir != "" {
				f, err := os.Create(dir + "/" + c.name + ".pprof")
				if err != nil {
					b.Fatal(err)
				}
				if err := pprof.Lookup("heap").WriteTo(f, 0); err != nil {
					b.Fatal(err)
				}
				if err := f.Close(); err != nil {
					b.Fatal(err)
				}
			}

			runtime.KeepAlive(sinks)
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
// recursion cost from mega's fanout work.
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

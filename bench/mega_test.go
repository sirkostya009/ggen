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
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/mailru/easyjson"
	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
)

// memProfileSample is one entry in a flattened MemProfile snapshot —
// alloc counters attributed to the top non-runtime stack frame.
type memProfileSample struct {
	allocObjects int64
	allocBytes   int64
}

// snapshotMemProfile drains runtime.MemProfile and aggregates records
// by their top non-runtime stack frame, returning a map keyed by symbol
// name. Caller diffs two snapshots to get per-region deltas.
//
// Requires runtime.MemProfileRate=1 (or low) before the bench region —
// the default 512 KB sample rate misses small per-iter allocations.
// inuseZero=true includes records for allocations that have already
// been freed, so we capture the full cumulative count (alloc - free).
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

// reportTopAllocs diffs two MemProfile snapshots and reports the top-N
// sites as inline bench metrics — each site contributes one
// `<name>/op` column, landing in the bench output table next to ns/op
// / gc/op / etc. Skips when b.N is small (testing.B's warm-up runN(1)
// is noise).
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
	// Descending — biggest contributors first; truncate to top-N.
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
		{"jsonv2", func(p []byte) error { var v NodePlain; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v NodePlain; return sonic.Unmarshal(p, &v) }},
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
		{"jsonv2", func() ([]byte, error) { return jsonv2.Marshal(MegaValuePlain) }},
		{"sonic", func() ([]byte, error) { return sonic.Marshal(MegaValuePlain) }},
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

// BenchmarkRetention measures retained heap per held output across
// codecs. Each goroutine accumulates decoded *Node into a local sink,
// then sinks are merged after RunParallel so every produced value
// survives until the post-loop GC pair. HeapInuse delta over the
// timed region divided by total iters gives retain_KB/op — the
// "what does my server's heap look like with one held response"
// number. retain_MiB is the total live-set across all iters.
//
// HeapInuse is process-global; works under RunParallel just like
// serial. For stable comparable numbers across codecs use a fixed
// iter count: `-benchtime=1000x`. Default `-benchtime=1s` picks
// different b.N per codec, but per-item retention is roughly
// constant so cross-codec comparison still reads cleanly.
//
// Uses slowPayload (~36 KiB) rather than MegaPayload to keep total
// retained memory tractable at high iter counts.
func BenchmarkRetention(b *testing.B) {
	var retentionCodecs = []struct {
		name string
		// returns the decoded value (as any so stdjson can return a
		// fresh NodePlain whose nested types don't trip easyjson's
		// json.Unmarshaler hooks). Sink holds it alive for the
		// HeapInuse measurement regardless of concrete type.
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
			n, *buf, err = decode.UnmarshalStream[Node](bytes.NewReader(slowPayload), (*buf)[:0])
			if err != nil {
				b.Fatal(err)
			}
			return &n
		}},
		{"ggen_bytes", func(_ *[]byte) any {
			n, err := decode.Unmarshal[Node](slowPayload)
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
			n, err := decode.Unmarshal[Node](data)
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
			var sinks [][]any // every goroutine's accumulator, merged post-run

			// Top-allocs breakdown is opt-in: setting MemProfileRate=1
			// captures every allocation but makes the bench ~40× slower.
			// Enable with GGEN_BENCH_TOPALLOCS=1.
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

			// Top-5 alloc sites under the bench line — saves having to
			// `go tool pprof` to figure out where the bytes came from.
			// Run with `GGEN_BENCH_TOPALLOCS=1 go test -v` to enable
			// (b.Logf only surfaces under verbose mode).
			if topAllocs {
				reportTopAllocs(b, preProf, postProf, 5)
			}

			// Dump inuse_space pprof while sinks are still live so we can
			// see exactly where retained bytes come from per codec. Set
			// GGEN_RESIDENCY_PROFILE=<dir>; the file is named after the
			// sub-bench (jsonv2/sonic/easyjson/ggen_*). View with:
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

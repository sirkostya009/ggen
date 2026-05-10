//go:build goexperiment.jsonv2

package bench

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/mailru/easyjson"
	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
)

// slowPayload — a few-dozen-KiB Node tree, separate from MegaPayload so the
// per-iteration I/O cost stays tractable. Built once at package init with
// a fixed seed for reproducibility. Shape matches a real-world small API
// response (a handful of children, a couple of refs, ~20 props/tags).
var slowPayload []byte

func init() {
	r := rand.New(rand.NewSource(2))
	v := buildNode(r, 3, []int{2, 2, 2, 0})
	out, err := encode.Marshal(v)
	if err != nil {
		panic(err)
	}
	slowPayload = out
}

// slowReader wraps a []byte and serves it with a chunk size + per-Read
// delay that interpolate linearly from a slow start to a fast steady
// state. Models a connection warming up: large initial latency drops to
// near-zero by the end of the response.
//
// Defaults below: read 1 starts at (1500 bytes, 52 ms); after
// rampReads, the reader settles at (800 bytes, 1.2 ms). Latency
// uses geometric decay (>>2 per read) so the floor is hit in
// ~4 reads — models a connection that warms up fast.
type slowReader struct {
	data       []byte
	pos        int
	reads      int
	rampReads  int
	startChunk int
	endChunk   int
	startDelay time.Duration
	endDelay   time.Duration
}

func newSlowReader(data []byte) *slowReader {
	return &slowReader{
		data:       data,
		rampReads:  20,
		startChunk: 1500,
		endChunk:   800,
		startDelay: 52 * time.Millisecond,
		endDelay:   1200 * time.Microsecond,
	}
}

func (s *slowReader) reset() {
	s.pos = 0
	s.reads = 0
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	t := min(s.reads, s.rampReads)
	// Chunk size: linear interp, t==0 → startChunk, t==rampReads → endChunk.
	chunk := s.startChunk + (s.endChunk-s.startChunk)*t/s.rampReads
	// Delay: geometric decay — each read shaves the remaining
	// (current - endDelay) gap by 75%, so the latency collapses to
	// the floor in about 4–5 reads. Models a connection that feels
	// awful for the first packet or two and then settles fast.
	extra := s.startDelay - s.endDelay
	for range s.reads {
		extra = extra >> 2 // multiply by 0.25
	}
	delay := s.endDelay + extra
	time.Sleep(delay)
	if chunk > len(p) {
		chunk = len(p)
	}
	remaining := len(s.data) - s.pos
	if chunk > remaining {
		chunk = remaining
	}
	n := copy(p, s.data[s.pos:s.pos+chunk])
	s.pos += n
	s.reads++
	return n, nil
}

// BenchmarkSlowStream_* — the same payload through an io.Reader that
// simulates a slow-warming connection. Run with a longer benchtime, e.g.
// `go test -bench=BenchmarkSlowStream -benchtime=10s ./bench/`.

func BenchmarkSlowStream_stdjson(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(slowPayload)))
	r := newSlowReader(slowPayload)
	dec := jsontext.NewDecoder(r)
	for b.Loop() {
		dec.Reset(r)
		r.reset()
		var v Node
		if err := jsonv2.UnmarshalDecode(dec, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSlowStream_easyjson(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(slowPayload)))
	r := newSlowReader(slowPayload)
	for b.Loop() {
		r.reset()
		var v Node
		if err := easyjson.UnmarshalFromReader(r, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSlowStream_ggen(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(slowPayload)))
	r := newSlowReader(slowPayload)
	buf := make([]byte, 0, len(slowPayload))
	for b.Loop() {
		r.reset()
		var err error
		if _, buf, err = decode.UnmarshalStream[Node](r, buf[:0]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSlowStream_ggen_Bounded constrains the parse buffer to
// 4 KiB while the payload is ~36 KiB. Exercises the window-shift
// path inside Ensure: as the buffer fills, the in-flight string's
// prefix is dropped and the parser slides over the remaining input.
// `buf[:0:4096]` resets length to 0 and CAPS capacity at 4 KiB —
// any grow attempt allocates a fresh backing, but if the shift
// works as designed the parser stays within the original 4 KiB.
func BenchmarkSlowStream_ggen_Bounded(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(slowPayload)))
	r := newSlowReader(slowPayload)
	buf := make([]byte, 0, 8192) // headroom in case of grow
	for b.Loop() {
		r.reset()
		if _, _, err := decode.UnmarshalStream[Node](r, buf[:0:4096]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSlowStream_ggen_ReadAll — io.ReadAll then bytes-path
// Unmarshal. No I/O / parse overlap; pays the full read latency
// before parsing starts.
func BenchmarkSlowStream_ggen_ReadAll(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(slowPayload)))
	r := newSlowReader(slowPayload)
	for b.Loop() {
		r.reset()
		data, err := io.ReadAll(r)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := decode.Unmarshal[Node](data); err != nil {
			b.Fatal(err)
		}
	}
}

// --- fail-fast: invalid payload, validation triggers early ---
//
// Here streaming has its theoretical advantage: ggen's per-field
// validation rejects on the first invalid field (Email is alphabetically
// first, after Age and Bio). With fail-fast streaming, the parser
// returns an error after reading just enough of the payload to scan
// the bad field — no need to wait for the rest. ReadAll variants pay
// the full read latency before validation can even begin.

func BenchmarkSlowStream_ggen_invalid(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(InvalidPayload)))
	r := newSlowReader(InvalidPayload)
	buf := make([]byte, 0, len(InvalidPayload))
	for b.Loop() {
		r.reset()
		var err error
		_, buf, err = decode.UnmarshalStream[Validated](r, buf[:0])
		if err == nil {
			b.Fatal("expected validation error")
		}
	}
}

func BenchmarkSlowStream_ggen_invalid_ReadAll(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(InvalidPayload)))
	r := newSlowReader(InvalidPayload)
	for b.Loop() {
		r.reset()
		data, err := io.ReadAll(r)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := decode.Unmarshal[Validated](data); err == nil {
			b.Fatal("expected validation error")
		}
	}
}

// jsonv2 baseline — no application-level validation, just reads
// and decodes. Shows the cost of "read everything, no fail-fast".
func BenchmarkSlowStream_jsonv2_invalid(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(InvalidPayload)))
	r := newSlowReader(InvalidPayload)
	dec := jsontext.NewDecoder(r)
	for b.Loop() {
		dec.Reset(r)
		r.reset()
		var v Validated
		if err := jsonv2.UnmarshalDecode(dec, &v); err != nil {
			b.Fatal(err)
		}
	}
}

// Sanity-check the slowReader before the bench harness invokes it under
// the timer — easier to debug a wiring mistake here than as a flaky
// benchmark.
func TestSlowReader_DeliversWholePayload(t *testing.T) {
	r := newSlowReader(slowPayload)
	// Override the delays so the test runs in milliseconds, not seconds.
	r.startDelay = 0
	r.endDelay = 0
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(out, slowPayload) {
		t.Errorf("payload mismatch: got %d bytes, want %d", len(out), len(slowPayload))
	}
}

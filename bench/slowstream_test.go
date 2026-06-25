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

	"github.com/bytedance/sonic"
	"github.com/mailru/easyjson"
	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/scan"
)

// slowPayload — a few-dozen-KiB Node tree, separate from MegaPayload so the
// per-iteration I/O cost stays tractable. Built once at init with a fixed seed.
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

// slowReader serves a []byte with a chunk size + per-Read delay that ramp
// from a slow start to a fast steady state — models a connection warming up.
// Defaults: read 1 at (1500 bytes, 52 ms), settling to (800 bytes, 1.2 ms);
// latency decays geometrically (>>2 per read), hitting the floor in ~4 reads.
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
	// Chunk size: linear interp from startChunk to endChunk over rampReads.
	chunk := s.startChunk + (s.endChunk-s.startChunk)*t/s.rampReads
	// Delay: geometric decay — each read shaves the remaining gap by 75%.
	extra := s.startDelay - s.endDelay
	for range s.reads {
		extra = extra >> 2 // ×0.25
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

// slowState carries per-goroutine reader + decode buffer, so the Sleep stays
// per-goroutine and the Stream alias buffer doesn't race across workers.
type slowState struct {
	r   *slowReader
	buf []byte
}

// BenchmarkSlowStream_Valid — same payload through a slow-warming reader.
// Run with a longer benchtime, e.g.
// `go test -bench=BenchmarkSlowStream -benchtime=10s -cpu=1 .`.
func BenchmarkSlowStream_Valid(b *testing.B) {
	var slowValidCodecs = []struct {
		name string
		fn   func(*slowState) error
	}{
		{"stdjson", func(s *slowState) error {
			s.r.reset()
			var v NodePlain
			return jsonv2.UnmarshalDecode(jsontext.NewDecoder(s.r), &v)
		}},
		{"sonic", func(s *slowState) error {
			s.r.reset()
			var v NodePlain
			return sonic.ConfigDefault.NewDecoder(s.r).Decode(&v)
		}},
		{"sonic_fast", func(s *slowState) error {
			s.r.reset()
			var v NodePlain
			return sonic.ConfigFastest.NewDecoder(s.r).Decode(&v)
		}},
		{"easyjson", func(s *slowState) error {
			s.r.reset()
			var v Node
			return easyjson.UnmarshalFromReader(s.r, &v)
		}},
		{"ggen_stream", func(s *slowState) error {
			s.r.reset()
			var err error
			var _st scan.Stream
			_st.Reset(s.r, s.buf[:0])
			_, err = Node{}.DecodeFromStream(&_st)
			s.buf = _st.Bytes()
			return err
		}},
		{"ggen_readall", func(s *slowState) error {
			s.r.reset()
			data, err := io.ReadAll(s.r)
			if err != nil {
				return err
			}
			_, _, err = Node{}.DecodeFrom(data)
			return err
		}},
	}

	for _, c := range slowValidCodecs {
		b.Run(c.name, func(b *testing.B) {
			runBench(b, int64(len(slowPayload)),
				func() slowState {
					return slowState{
						r:   newSlowReader(slowPayload),
						buf: make([]byte, 0, len(slowPayload)),
					}
				},
				func(s *slowState) {
					if err := c.fn(s); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

func BenchmarkSlowStream_Invalid(b *testing.B) {
	// Streaming's theoretical advantage is being able to reject
	// invalid payloads without draining io.Reader. jsonv2 doesn't
	// have this but is streaming so its also present for a baseline comparison.
	var slowInvalidCodecs = []struct {
		name    string
		fn      func(*slowState) error
		wantErr bool
	}{
		{"ggen_stream", func(s *slowState) error {
			s.r.reset()
			var err error
			var _st scan.Stream
			_st.Reset(s.r, s.buf[:0])
			_, err = Validated{}.DecodeFromStream(&_st)
			s.buf = _st.Bytes()
			return err
		}, true},
		{"ggen_readall", func(s *slowState) error {
			s.r.reset()
			data, err := io.ReadAll(s.r)
			if err != nil {
				return err
			}
			_, _, err = Validated{}.DecodeFrom(data)
			return err
		}, true},
		{"jsonv2", func(s *slowState) error {
			s.r.reset()
			var v Validated
			return jsonv2.UnmarshalDecode(jsontext.NewDecoder(s.r), &v)
		}, false},
		{"sonic", func(s *slowState) error {
			s.r.reset()
			var v Validated
			return sonic.ConfigDefault.NewDecoder(s.r).Decode(&v)
		}, false},
		{"sonic_fast", func(s *slowState) error {
			s.r.reset()
			var v Validated
			return sonic.ConfigFastest.NewDecoder(s.r).Decode(&v)
		}, false},
	}

	for _, c := range slowInvalidCodecs {
		b.Run(c.name, func(b *testing.B) {
			runBench(b, int64(len(InvalidPayload)),
				func() slowState {
					return slowState{
						r:   newSlowReader(InvalidPayload),
						buf: make([]byte, 0, len(InvalidPayload)),
					}
				},
				func(s *slowState) {
					err := c.fn(s)
					if c.wantErr && err == nil {
						b.Fatal("expected validation error")
					}
					if !c.wantErr && err != nil {
						b.Fatal(err)
					}
				},
			)
		})
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

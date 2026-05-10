//go:build goexperiment.jsonv2

package bench

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"testing"

	"github.com/mailru/easyjson"
	"github.com/sirkostya009/ggen/decode"
)

// residency measures bytes retained on the heap after parsing `count`
// copies of slowPayload via decode and holding all of them live. Forces
// two GC cycles before each measurement to settle finalizers / sweeping.
// Set GGEN_RESIDENCY_PROFILE=<path> to dump an inuse_space profile
// while the held slice is still live — useful for tracking down where
// retained bytes are coming from.
func residency(t *testing.T, count int, label string, decodeOne func() *Node) {
	runtime.GC()
	runtime.GC()
	var pre runtime.MemStats
	runtime.ReadMemStats(&pre)

	nodes := make([]*Node, 0, count)
	for range count {
		nodes = append(nodes, decodeOne())
	}

	runtime.GC()
	runtime.GC()
	var post runtime.MemStats
	runtime.ReadMemStats(&post)

	if dir := os.Getenv("GGEN_RESIDENCY_PROFILE"); dir != "" {
		f, err := os.Create(dir + "/" + label + ".pprof")
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.Lookup("heap").WriteTo(f, 0); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	runtime.KeepAlive(nodes)

	delta := int64(post.HeapInuse) - int64(pre.HeapInuse)
	t.Logf("%-12s  %d items → %.2f MiB retained (%.1f KiB/item, %.2fx payload)",
		label, count,
		float64(delta)/(1024*1024),
		float64(delta)/float64(count)/1024,
		float64(delta)/float64(count*len(slowPayload)))
}

// TestResidency runs each codec through the residency probe. Run with
// `go test -run TestResidency -v ./bench/`.
func TestResidency(t *testing.T) {
	const count = 1000

	t.Run("stdjson", func(t *testing.T) {
		residency(t, count, "stdjson", func() *Node {
			n := new(Node)
			if err := jsonv2.UnmarshalRead(bytes.NewReader(slowPayload), n); err != nil {
				t.Fatal(err)
			}
			return n
		})
	})

	t.Run("easyjson", func(t *testing.T) {
		residency(t, count, "easyjson", func() *Node {
			n := new(Node)
			if err := easyjson.UnmarshalFromReader(bytes.NewReader(slowPayload), n); err != nil {
				t.Fatal(err)
			}
			return n
		})
	})

	t.Run("ggen_stream", func(t *testing.T) {
		buf := make([]byte, 0, len(slowPayload))
		residency(t, count, "ggen_stream", func() *Node {
			var n Node
			var err error
			n, buf, err = decode.UnmarshalStream[Node](bytes.NewReader(slowPayload), buf[:0])
			if err != nil {
				t.Fatal(err)
			}
			return &n
		})
	})

	t.Run("ggen_bytes", func(t *testing.T) {
		residency(t, count, "ggen_bytes", func() *Node {
			n, err := decode.Unmarshal[Node](slowPayload)
			if err != nil {
				t.Fatal(err)
			}
			return &n
		})
	})

	t.Run("ggen_readall", func(t *testing.T) {
		residency(t, count, "ggen_readall", func() *Node {
			data, err := io.ReadAll(bytes.NewReader(slowPayload))
			if err != nil {
				t.Fatal(err)
			}
			n, err := decode.Unmarshal[Node](data)
			if err != nil {
				t.Fatal(err)
			}
			return &n
		})
	})

	// Small-initial-cap buffer — caller passes `buf[:0:4096]` so the
	// Stream starts with 4 KiB of caller-provided backing. ReadMore
	// never shifts, only grows, so on the ~36 KiB payload the buffer
	// grows several times past the initial cap; each grow allocates a
	// fresh backing and the orphaned chunks stay live only until the
	// next GC. Useful as a "what does a too-small starting buf cost
	// per retained value?" probe — kept around even though Stream no
	// longer supports true bounded streaming.
	t.Run("ggen_stream_bounded", func(t *testing.T) {
		buf := make([]byte, 0, 8192)
		residency(t, count, "ggen_stream_bounded", func() *Node {
			n, _, err := decode.UnmarshalStream[Node](bytes.NewReader(slowPayload), buf[:0:4096])
			if err != nil {
				t.Fatal(err)
			}
			return &n
		})
	})
}

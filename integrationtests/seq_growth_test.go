package integrationtests

import (
	"bytes"
	"testing"

	"github.com/sirkostya009/ggen/scan"
)

// Seq must keep the Stream's buffer bounded across a long run of GENERATED
// container-bearing values. Node carries []string, map[string]string and
// []Node, whose emitted stream refills are grow-only (ReadMore(0)) — a full
// window doubles rather than reusing the space consumed values leave behind,
// so a 422-byte value through a 1 KiB buffer can ratchet it to 4 KiB unless
// Seq drops those consumed bytes.
func TestSeqBufferStaysBounded(t *testing.T) {
	t.Parallel()
	for _, bufCap := range []int{64, 256, 1024} {
		var sb bytes.Buffer
		for range 200 {
			sb.Write(complexPayload)
			sb.WriteByte('\n')
		}
		var s scan.Stream
		s.Reset(bytes.NewReader(sb.Bytes()), make([]byte, 0, bufCap))
		n := 0
		for v, err := range s.Seq[Node]() {
			if err != nil {
				t.Fatalf("bufCap=%d value %d: %v", bufCap, n, err)
			}
			if v.ID != 42 || v.Name != "hello world" || len(v.Children) != 2 {
				t.Fatalf("bufCap=%d value %d decoded wrong: %+v", bufCap, n, v)
			}
			n++
		}
		if n != 200 {
			t.Errorf("bufCap=%d: yielded %d values, want 200", bufCap, n)
		}
		// The window may need to hold one whole value, never more than a
		// doubling past that.
		if want := max(bufCap, 2*len(complexPayload)); cap(s.Bytes()) > want {
			t.Errorf("bufCap=%d: buffer ratcheted to %d, want <= %d (value is %d B)",
				bufCap, cap(s.Bytes()), want, len(complexPayload))
		}
	}
}

// Array must hold the same bound as Seq over a long array of GENERATED
// container-bearing elements: yielded elements are dropped once the window is
// full, so the grow-only emitted refills cannot ratchet it.
func TestArrayBufferStaysBounded(t *testing.T) {
	t.Parallel()
	for _, bufCap := range []int{64, 256, 1024} {
		var sb bytes.Buffer
		sb.WriteByte('[')
		for i := range 200 {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.Write(complexPayload)
		}
		sb.WriteByte(']')
		var s scan.Stream
		s.Reset(bytes.NewReader(sb.Bytes()), make([]byte, 0, bufCap))
		n := 0
		for v, err := range s.Array[Node]() {
			if err != nil {
				t.Fatalf("bufCap=%d elem %d: %v", bufCap, n, err)
			}
			if v.ID != 42 || len(v.Children) != 2 {
				t.Fatalf("bufCap=%d elem %d decoded wrong: %+v", bufCap, n, v)
			}
			n++
		}
		if n != 200 {
			t.Errorf("bufCap=%d: yielded %d elements, want 200", bufCap, n)
		}
		if want := max(bufCap, 2*len(complexPayload)); cap(s.Bytes()) > want {
			t.Errorf("bufCap=%d: buffer ratcheted to %d, want <= %d (element is %d B)",
				bufCap, cap(s.Bytes()), want, len(complexPayload))
		}
	}
}

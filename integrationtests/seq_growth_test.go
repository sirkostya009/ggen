package integrationtests

import (
	"bytes"
	"testing"

	"github.com/sirkostya009/ggen"
)

// Seq must keep the Stream's buffer bounded across a long run of values whose
// generated decoders hold containers. Node has []string, map[string]string and
// []Node. The generated stream code for those refills with ReadMore(0), which
// only grows the buffer. When the buffer is full, it doubles instead of
// reusing the space that already-decoded values left behind. So a 422-byte
// value read through a 1 KiB buffer could grow it to 4 KiB, unless Seq drops
// the consumed bytes first.
func TestSeqBufferStaysBounded(t *testing.T) {
	t.Parallel()
	for _, bufCap := range []int{64, 256, 1024} {
		var sb bytes.Buffer
		for range 200 {
			sb.Write(complexPayload)
			sb.WriteByte('\n')
		}
		var s ggen.Stream
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
		// The buffer may need to hold one whole value, and never more than
		// one doubling past that.
		if want := max(bufCap, 2*len(complexPayload)); cap(s.Bytes()) > want {
			t.Errorf("bufCap=%d: buffer ratcheted to %d, want <= %d (value is %d B)",
				bufCap, cap(s.Bytes()), want, len(complexPayload))
		}
	}
}

// Slice collects every element, but the Stream's buffer must stay bounded the
// same way it does for Seq and Array. Consumed bytes are dropped once the
// buffer is full.
func TestSliceBufferStaysBounded(t *testing.T) {
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
		var s ggen.Stream
		s.Reset(bytes.NewReader(sb.Bytes()), make([]byte, 0, bufCap))
		got, err := s.Slice[Node]()
		if err != nil {
			t.Fatalf("bufCap=%d: %v", bufCap, err)
		}
		if len(got) != 200 {
			t.Errorf("bufCap=%d: gathered %d elements, want 200", bufCap, len(got))
		}
		for i, v := range got {
			if v.ID != 42 || len(v.Children) != 2 {
				t.Fatalf("bufCap=%d elem %d decoded wrong: %+v", bufCap, i, v)
			}
		}
		if want := max(bufCap, 2*len(complexPayload)); cap(s.Bytes()) > want {
			t.Errorf("bufCap=%d: buffer ratcheted to %d, want <= %d (element is %d B)",
				bufCap, cap(s.Bytes()), want, len(complexPayload))
		}
	}
}

// Array must keep the same bound as Seq over a long array of elements whose
// generated decoders hold containers. Elements already yielded are dropped
// once the buffer is full, so the grow-only refills in generated code cannot
// keep growing it.
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
		var s ggen.Stream
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

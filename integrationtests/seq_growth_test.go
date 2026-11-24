package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"math/big"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
)

//ggen:generate
type SeqShareLeaf struct {
	A int `json:"a"`
}

//ggen:generate
type SeqShareHost struct {
	Name string        `json:"name"`
	User *SeqShareLeaf `json:"user"`
	Sum  big.Int       `json:"sum"`
}

//ggen:generate
type SeqAllocRow struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// With the value reused, Seq and Array allocate no containers per element.
// What is left is one owned copy per string: here the name and two tags.
func TestSeqArray_steadyStateAllocsAreStringCopies(t *testing.T) { //nolint:paralleltest // measures allocations
	const row = `{"name":"row","tags":["a","b"]}`
	const perElem = 3
	payload := func(n int, array bool) []byte {
		var b bytes.Buffer
		if array {
			b.WriteByte('[')
		}
		for i := range n {
			if i > 0 && array {
				b.WriteByte(',')
			}
			b.WriteString(row)
		}
		if array {
			b.WriteByte(']')
		}
		return b.Bytes()
	}
	buf := make([]byte, 0, 1<<16)
	var r bytes.Reader
	var s ggen.Stream
	for _, array := range []bool{false, true} {
		allocs := func(n int) float64 {
			data := payload(n, array)
			return testing.AllocsPerRun(20, func() {
				r.Reset(data)
				s.Reset(&r, buf)
				seq := s.Seq[SeqAllocRow]()
				if array {
					seq = s.Array[SeqAllocRow]()
				}
				for _, err := range seq {
					if err != nil {
						t.Fatal(err)
					}
				}
			})
		}
		if got := allocs(200) - allocs(100); got != 100*perElem {
			t.Errorf("array=%v: %v allocs per 100 extra elements, want %d", array, got, 100*perElem)
		}
	}
}

// Seq and Array decode each step into the previous value, so a yielded
// value shares its pointer pointees and big.Int words with later steps, as
// their docs say. Strings are owned and stay.
func TestSeqArray_yieldedValueSharesPointeesAndBigInt(t *testing.T) {
	t.Parallel()
	const one = `{"name":"one","user":{"a":1},"sum":11111111111111111111111111}`
	const two = `{"name":"two","user":{"a":2},"sum":22222222222222222222222222}`
	check := func(api string, kept []SeqShareHost) {
		if len(kept) != 2 {
			t.Fatalf("%s: yielded %d values, want 2", api, len(kept))
		}
		first := kept[0]
		if first.Name != "one" {
			t.Errorf("%s: kept Name = %q, want one", api, first.Name)
		}
		if first.User != kept[1].User || first.User.A != 2 {
			t.Errorf("%s: kept pointee not shared with the next step: %p %p A=%d", api, first.User, kept[1].User, first.User.A)
		}
		if got := first.Sum.String(); got == "11111111111111111111111111" {
			t.Errorf("%s: kept big.Int did not share its words with the next step", api)
		}
	}
	var kept []SeqShareHost
	for v, err := range ggen.NewStream(strings.NewReader(one+"\n"+two), nil).Seq[SeqShareHost]() {
		if err != nil {
			t.Fatal(err)
		}
		kept = append(kept, v)
	}
	check("Seq", kept)
	kept = kept[:0]
	for v, err := range ggen.NewStream(strings.NewReader("["+one+","+two+"]"), nil).Array[SeqShareHost]() {
		if err != nil {
			t.Fatal(err)
		}
		kept = append(kept, v)
	}
	check("Array", kept)
}

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

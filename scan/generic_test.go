package scan

import (
	"strconv"
	"strings"
	"testing"
)

// intT is a minimal StreamDecoder for the generic-method tests.
type intT int64

func (intT) DecodeFromStream(s *Stream) (intT, error) {
	v, err := s.Int64()
	return intT(v), err
}

// sliceT is a StreamDecoder whose value carries a container, so buffer reuse
// is observable. It threads its own buffer into the nested Slice call, which
// is how a generated decoder with a slice field recycles it.
type sliceT struct{ Vals []intT }

func (recv sliceT) DecodeFromStream(s *Stream) (sliceT, error) {
	result := recv
	v, err := s.Slice(result.Vals)
	result.Vals = v
	return result, err
}

// TestStreamMethods pins the generic Stream methods: Reset chains into them,
// and both leave the cursor positioned so the caller can keep reading — the
// capability the bytes walkers cannot offer.
func TestStreamMethods(t *testing.T) {
	v, err := NewStream(strings.NewReader("42"), nil).Value[intT]()
	if err != nil || v != 42 {
		t.Fatalf("Value = %v, %v; want 42", v, err)
	}

	// Slice does NOT reject trailing data, so a value after the array is
	// still readable from the same Stream.
	s := NewStream(strings.NewReader("[1,2] 7"), nil)
	got, err := s.Slice[intT]()
	if err != nil || len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("Slice = %v, %v; want [1 2]", got, err)
	}
	if err := s.SkipSpace(); err != nil {
		t.Fatalf("SkipSpace: %v", err)
	}
	if tail, err := s.Value[intT](); err != nil || tail != 7 {
		t.Errorf("Value after Slice = %v, %v; want 7", tail, err)
	}

	// An empty array decodes to a NON-nil empty slice, matching the bytes
	// walker and jsonv2.
	if e, err := NewStream(strings.NewReader(" [ ] "), nil).Slice[intT](); err != nil || e == nil || len(e) != 0 {
		t.Errorf("Slice([]) = %#v, %v; want non-nil empty", e, err)
	}

	// Consecutive values off one Stream.
	s = NewStream(strings.NewReader("1 2"), nil)
	first, err := s.Value[intT]()
	if err != nil || first != 1 {
		t.Fatalf("first Value = %v, %v", first, err)
	}
	if err := s.SkipSpace(); err != nil {
		t.Fatalf("SkipSpace: %v", err)
	}
	if second, err := s.Value[intT](); err != nil || second != 2 {
		t.Errorf("second Value = %v, %v; want 2", second, err)
	}

	// A malformed array still errors.
	if _, err := NewStream(strings.NewReader("[1 2]"), nil).Slice[intT](); err == nil {
		t.Error("Slice([1 2]) must fail on the missing comma")
	}
}

// TestStreamSeq pins the iterator: consecutive top-level values, a clean end
// when the reader drains, early break leaving the Stream usable, and the
// one-value reuse that makes a long run allocation-free.
func TestStreamSeq(t *testing.T) {
	var got []intT
	for v, err := range NewStream(strings.NewReader("1 2\n3\n"), nil).Seq[intT]() {
		if err != nil {
			t.Fatalf("Seq yielded error: %v", err)
		}
		got = append(got, v)
	}
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Errorf("Seq = %v, want [1 2 3]", got)
	}

	// Empty / whitespace-only input ends immediately with no error.
	for _, err := range NewStream(strings.NewReader("  \n "), nil).Seq[intT]() {
		t.Fatalf("Seq over blank input yielded %v", err)
	}

	// A malformed value yields exactly one error, then stops.
	var errs, vals int
	for _, err := range NewStream(strings.NewReader("1 x"), nil).Seq[intT]() {
		if err != nil {
			errs++
		} else {
			vals++
		}
	}
	if vals != 1 || errs != 1 {
		t.Errorf("malformed stream: vals=%d errs=%d, want 1/1", vals, errs)
	}

	// Seq reuses one value across iterations, so a long run settles at zero
	// allocations; an rcv seeds that value with containers already to hand.
	var r strings.Reader
	var st Stream
	sbuf := make([]byte, 0, 256)
	drain := func(seed ...sliceT) (n int, last *intT) {
		r.Reset("[1,2,3] [4,5,6] [7,8,9]")
		for v := range st.Reset(&r, sbuf).Seq[sliceT](seed...) {
			n++
			last = &v.Vals[0]
		}
		return n, last
	}
	if n, _ := drain(); n != 3 {
		t.Fatalf("Seq yielded %d values, want 3", n)
	}
	warm := sliceT{Vals: make([]intT, 0, 8)}
	warmPtr := &warm.Vals[:1][0]
	if n, got := drain(warm); n != 3 || got != warmPtr {
		t.Errorf("Seq(rcv) reuse: n=%d ptr-reused=%v", n, got == warmPtr)
	}
	if allocs := testing.AllocsPerRun(50, func() { drain(warm) }); allocs > 0 {
		t.Errorf("Seq(rcv) allocates %.0f per drain; want 0", allocs)
	}

	// The window stays bounded over a long stream. Seq drops consumed values
	// once the buffer is full, so refills reuse that space instead of
	// doubling. (The generated-decoder case, where the emitted refills are
	// grow-only and this actually bites, is TestSeqBufferStaysBounded in
	// integrationtests.)
	{
		const n = 2000
		var sb strings.Builder
		for i := range n {
			sb.WriteString(strconv.Itoa(i % 97))
			sb.WriteByte('\n')
		}
		var lr strings.Reader
		lr.Reset(sb.String())
		var ls Stream
		ls.Reset(&lr, make([]byte, 0, 64))
		count := 0
		for _, err := range ls.Seq[intT]() {
			if err != nil {
				t.Fatalf("long stream yielded error at %d: %v", count, err)
			}
			count++
		}
		if count != n {
			t.Errorf("long stream yielded %d values, want %d", count, n)
		}
		if got := cap(ls.Bytes()); got > 256 {
			t.Errorf("buffer grew to cap %d over %d values (started at 64) — compaction not holding", got, n)
		}
	}

	// Breaking out leaves the Stream positioned for further reads.
	s := NewStream(strings.NewReader("1 2 3"), nil)
	for v := range s.Seq[intT]() {
		if v == 1 {
			break
		}
	}
	if err := s.SkipSpace(); err != nil {
		t.Fatalf("SkipSpace after break: %v", err)
	}
	if next, err := s.Value[intT](); err != nil || next != 2 {
		t.Errorf("after break Value = %v, %v; want 2", next, err)
	}
}

// TestStreamBufferReuse pins that rcv recycles containers — the outer slice
// AND each element's own slice — so a steady-state re-decode stops allocating.
func TestStreamBufferReuse(t *testing.T) {
	const payload = `[[1,2,3],[4,5,6]]`
	// Reuse the reader, the Stream and its buffer so the only allocations left
	// to measure are the decode's own.
	var r strings.Reader
	var s Stream
	sbuf := make([]byte, 0, 256)
	decodeInto := func(buf []sliceT) []sliceT {
		r.Reset(payload)
		got, err := s.Reset(&r, sbuf).Slice(buf)
		if err != nil {
			t.Fatalf("Slice: %v", err)
		}
		return got
	}

	first := decodeInto(nil)
	if len(first) != 2 || len(first[0].Vals) != 3 || first[1].Vals[2] != 6 {
		t.Fatalf("first decode wrong: %+v", first)
	}
	outerPtr, elemPtr := &first[0], &first[0].Vals[0]

	second := decodeInto(first)
	if len(second) != 2 || second[1].Vals[2] != 6 {
		t.Fatalf("second decode wrong: %+v", second)
	}
	if &second[0] != outerPtr {
		t.Error("outer slice backing array not reused")
	}
	if &second[0].Vals[0] != elemPtr {
		t.Error("element container not reused — elements were appended blindly")
	}

	// Steady state must not allocate at all.
	buf := second
	if n := testing.AllocsPerRun(50, func() { buf = decodeInto(buf) }); n > 0 {
		t.Errorf("reuse still allocates %.0f per decode; want 0", n)
	}

	// Without rcv the decode is independent (fresh containers every time).
	fresh := decodeInto(nil)
	if &fresh[0] == outerPtr {
		t.Error("no-rcv decode must not reuse the previous buffer")
	}
}

// TestStreamArray pins the lazy array iterator: same grammar as Slice, cursor
// left past the closing bracket, one error then stop, and no accumulation.
func TestStreamArray(t *testing.T) {
	// Agrees with Slice on element values.
	var got []intT
	for v, err := range NewStream(strings.NewReader("[1, 2,3]"), nil).Array[intT]() {
		if err != nil {
			t.Fatalf("Array yielded error: %v", err)
		}
		got = append(got, v)
	}
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Errorf("Array = %v, want [1 2 3]", got)
	}

	// Empty array yields nothing, no error.
	for _, err := range NewStream(strings.NewReader(" [ ] "), nil).Array[intT]() {
		t.Fatalf("Array over [] yielded %v", err)
	}

	// The closing bracket is consumed, so the Stream reads on.
	s := NewStream(strings.NewReader("[1,2] 9"), nil)
	n := 0
	for range s.Array[intT]() {
		n++
	}
	if n != 2 {
		t.Fatalf("Array yielded %d, want 2", n)
	}
	if err := s.SkipSpace(); err != nil {
		t.Fatalf("SkipSpace: %v", err)
	}
	if tail, err := s.Value[intT](); err != nil || tail != 9 {
		t.Errorf("Value after Array = %v, %v; want 9", tail, err)
	}

	// A malformed array yields exactly one error after the good elements.
	var vals, errs int
	for _, err := range NewStream(strings.NewReader("[1 2]"), nil).Array[intT]() {
		if err != nil {
			errs++
		} else {
			vals++
		}
	}
	if vals != 1 || errs != 1 {
		t.Errorf("malformed array: vals=%d errs=%d, want 1/1", vals, errs)
	}

	// Not an array at all.
	var sawErr bool
	for _, err := range NewStream(strings.NewReader("42"), nil).Array[intT]() {
		sawErr = err != nil
	}
	if !sawErr {
		t.Error("Array over a non-array must yield an error")
	}

	// A long array through a small buffer: nothing accumulates and the window
	// stays bounded.
	const count = 5000
	var sb strings.Builder
	sb.WriteByte('[')
	for i := range count {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.Itoa(i % 97))
	}
	sb.WriteByte(']')
	var lr strings.Reader
	lr.Reset(sb.String())
	var ls Stream
	ls.Reset(&lr, make([]byte, 0, 64))
	seen := 0
	for _, err := range ls.Array[intT]() {
		if err != nil {
			t.Fatalf("long array error at %d: %v", seen, err)
		}
		seen++
	}
	if seen != count {
		t.Errorf("long array yielded %d, want %d", seen, count)
	}
	if c := cap(ls.Bytes()); c > 256 {
		t.Errorf("buffer grew to cap %d over %d elements (started at 64)", c, count)
	}
}

// Array must not allocate per element once the reused value is warm.
func TestStreamArrayNoAlloc(t *testing.T) {
	payload := "[[1,2,3],[4,5,6],[7,8,9]]"
	var r strings.Reader
	var s Stream
	buf := make([]byte, 0, 256)
	warm := sliceT{Vals: make([]intT, 0, 8)}
	drain := func() {
		r.Reset(payload)
		for _, err := range s.Reset(&r, buf).Array(warm) {
			if err != nil {
				t.Fatalf("Array: %v", err)
			}
		}
	}
	drain()
	if n := testing.AllocsPerRun(50, drain); n > 0 {
		t.Errorf("Array(rcv) allocates %.0f per drain; want 0", n)
	}
}

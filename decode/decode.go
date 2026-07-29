// Package decode provides runtime helpers ggen-generated decoders delegate to:
// the Decoder interface every generated type satisfies, plus array-walking
// helpers (UnmarshalSlice / ReadSlice / UnmarshalSliceStream).
//
// For single values, call the generated method directly on a zero-value
// receiver:
//
//	res, _, err := T{}.DecodeFrom(data)
//	res, err := T{}.DecodeFromStream(s)
package decode

import (
	"io"
	"strconv"
	"unsafe"

	"github.com/sirkostya009/ggen/scan"
)

// Decoder is the interface satisfied by every ggen-generated struct.
// DecodeFrom reads one value and returns it, the bytes consumed, and any
// error; callers chaining values advance their own cursor over data[n:].
// DecodeFromStream is the streaming counterpart — the Stream owns the cursor
// (s.Pos), so it returns only (T, error).
//
// Strings inside the returned value alias the caller's bytes — callers MUST
// NOT mutate the input buffer while the value is in use.
type Decoder[T any] interface {
	DecodeFrom(data []byte) (T, int, error)
	DecodeFromStream(s *scan.Stream) (T, error)
}

// UnmarshalSlice decodes a JSON array of T, delegating each element to
// T.DecodeFrom. Errors route through [NewParseErr].
func UnmarshalSlice[T Decoder[T]](data []byte) ([]T, error) {
	i := scan.SkipSpace(data, 0)
	if i >= len(data) || data[i] != '[' {
		return nil, NewParseErr("[]", i, scan.ErrBadArray)
	}
	i++
	i = scan.SkipSpace(data, i)
	var result []T
	if i < len(data) && data[i] == ']' {
		return result, nil
	}
	for {
		var zero T
		v, n, err := zero.DecodeFrom(data[i:])
		if err != nil {
			return nil, NewParseErr(arrField(len(result)), i, err)
		}
		result = append(result, v)
		i = scan.SkipSpace(data, i+n)
		if i >= len(data) {
			return nil, NewParseErr(arrField(len(result)-1), i, scan.ErrBadArray)
		}
		if data[i] == ',' {
			i = scan.SkipSpace(data, i+1)
			continue
		}
		if data[i] == ']' {
			return result, nil
		}
		return nil, NewParseErr(arrField(len(result)-1), i, scan.ErrBadArray)
	}
}

// ReadSlice reads an array from r then decodes it via UnmarshalSlice.
// io.ReadAll failures are surfaced as-is (not wrapped as parse errors).
func ReadSlice[T Decoder[T]](r io.Reader) ([]T, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return UnmarshalSlice[T](data)
}

// UnmarshalSliceStream decodes a JSON array of T lazily from r. buf is a
// reusable working area (nil to allocate fresh). The returned []byte is the
// (possibly grown) buffer — safe to recycle immediately, as decoded values
// own their string content. Errors route through [NewParseErr].
func UnmarshalSliceStream[T Decoder[T]](r io.Reader, buf []byte) ([]T, []byte, error) {
	var s scan.Stream
	s.Reset(r, buf)
	if err := s.ArrayOpen(); err != nil {
		return nil, s.Bytes(), NewParseErr("[]", s.Pos, err)
	}
	if err := s.SkipSpace(); err != nil {
		return nil, s.Bytes(), NewParseErr("[]", s.Pos, err)
	}
	if s.Pos >= len(s.Bytes()) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, s.Bytes(), NewParseErr("[]", s.Pos, err)
		}
		s.Pos = 0
	}
	var result []T
	if s.Bytes()[s.Pos] == ']' {
		s.Pos++
		return result, s.Bytes(), nil
	}
	for {
		var zero T
		v, err := zero.DecodeFromStream(&s)
		if err != nil {
			return nil, s.Bytes(), NewParseErr(arrField(len(result)), s.Pos, err)
		}
		result = append(result, v)
		if err := s.SkipSpace(); err != nil {
			return nil, s.Bytes(), NewParseErr(arrField(len(result)-1), s.Pos, err)
		}
		if s.Pos >= len(s.Bytes()) {
			if err := s.ReadMore(s.Pos); err != nil {
				return nil, s.Bytes(), NewParseErr(arrField(len(result)-1), s.Pos, err)
			}
			s.Pos = 0
		}
		c := s.Bytes()[s.Pos]
		if c == ',' {
			s.Pos++
			continue
		}
		if c == ']' {
			s.Pos++
			return result, s.Bytes(), nil
		}
		return nil, s.Bytes(), NewParseErr(arrField(len(result)-1), s.Pos, scan.ErrBadArray)
	}
}

// arrField renders "[N]" for the path segment on slice-walker errors
// (e.g. "[5].street").
func arrField(n int) string {
	buf := make([]byte, 0, 12)
	buf = append(buf, '[')
	buf = strconv.AppendInt(buf, int64(n), 10)
	buf = append(buf, ']')
	return unsafe.String(unsafe.SliceData(buf), len(buf))
}

// Byte budgets PreallocCap sizes a slice into. Both are properties of the Go
// runtime, not of JSON:
//
//   - fastAllocMax — go1.27 fast-paths allocations below 80 bytes.
//   - spanBudget — the Green Tea collector scans 512-byte spans as a unit, so
//     staying inside one keeps a freshly-decoded slice cheap to mark.
const (
	// fastAllocMax is go1.27's size-specialized-malloc cutoff. The release note
	// prose says "<80 byte", but the gate is INCLUSIVE of 80 on both paths: the
	// compiler bails on `size > specializedMallocMax` (ssagen/ssa.go) and the
	// runtime — which is what make([]T,0,N) reaches via makeslice — indexes
	// `size < len(mallocNoScanTable)` where that table is
	// [specializedMallocMax+1] = [81]. So 80 bytes is specialized; 81 is not. NOT active on go1.26
	// — SizeSpecializedMalloc is absent from the buildcfg baseline there, and
	// opting in via GOEXPERIMENT on 1.26 specializes everything up to 512
	// anyway, so this rung is a no-op distinction until users build with 1.27.
	// Kept because it is also plain memory conservatism: it stops a slice of
	// small elements from speculating a whole span.
	fastAllocMax = 80

	// spanBudget is the runtime's OWN boundary, not a guess — but not a GC one:
	// gc.MinSizeForMallocHeader = goarch.PtrSize * goarch.PtrBits = 8 × PtrSize²
	// (512 on 64-bit, 128 on 32-bit), from the go1.22 allocation-headers work.
	// Its size is bound by writeHeapBitsSmall, not by anything the collector
	// does. What it buys, in order of how much it is worth:
	//
	//   - No malloc header. Above the boundary a POINTERFUL object gets 8 bytes
	//     added before the size-class lookup (runtime/msize.go roundupsize), so
	//     it can jump a whole class: measured here, a []struct{*int; [7]int64}
	//     at 512 B allocates 512, at 576 B allocates 640.
	//   - Inline span mark bits, which is what lets Green Tea batch a span's
	//     objects into one scan (mgcmark_greenteagc.go gcUsesSpanInlineMarkBits
	//     = heapBitsInSpan(size) && size >= 16). Green Tea is the default
	//     collector from go1.26 (buildcfg baseline GreenTeaGC: true) — but this
	//     half does NOT apply to a NOSCAN backing array ([]int, []byte, structs
	//     with no pointers): tryDeferToSpanScan fast-tracks noscan objects and
	//     never scans them. For those, only mark-bit locality remains.
	//
	// The boundary is an EXCLUSIVE minimum chosen so the test is invariant
	// under size-class rounding (mbitmap.go), and 512 is itself exactly a size
	// class — so comparing a raw N*sizeof(E) against it is guaranteed to agree
	// with the runtime, and this code needs no size-class model of its own.
	spanBudget = 8 * unsafe.Sizeof(uintptr(0)) * unsafe.Sizeof(uintptr(0))
)

// PreallocCap returns the capacity a generated decoder gives a slice whose
// element is size bytes wide, when no tag says otherwise. Everything that
// states what the payload actually holds outranks it: `hint:` / `len` /
// `minlen`, and `maxlen=N` too whenever N elements still fit within
// spanBudget — an exact upper bound beats a guess from the element size, and
// the slice can never outgrow it.
//
// The ladder, widest budget that still fits:
//
//  1. as many elements as fit within 80 bytes, if that is at least 2;
//  2. else as many as fit within spanBudget, if that is at least 2;
//  3. else 1 — a single element already blows the span budget, so guessing
//     higher only wastes memory the payload may never use.
//
// Two elements is the floor because one is what the growth chain starts with
// anyway, and a sub-32-byte slice can land on the stack, where the eventual heap
// escape costs a copy. The runtime agrees from the other side: an object below
// 16 bytes is excluded from span inline mark bits too (the `size >= 16` half of
// gcUsesSpanInlineMarkBits), and the ladder's smallest possible allocation is
// 40 bytes, so nothing it emits falls into that hole.
//
// Callers pass `unsafe.Sizeof(*new(T))`, which is a compile-time constant, so
// gc folds the whole thing to a literal — this never runs.
func PreallocCap(size uintptr) int {
	if size == 0 {
		return 2 // zero-width elements: capacity is free
	}
	if n := fastAllocMax / size; n >= 2 {
		return int(n)
	}
	if n := spanBudget / size; n >= 2 {
		return int(n)
	}
	return 1
}

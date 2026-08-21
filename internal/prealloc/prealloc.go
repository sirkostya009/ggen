package prealloc

import "unsafe"

// Byte budgets Cap sizes a slice into. Both are properties of the Go
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

// Cap returns the capacity a generated decoder gives a slice whose
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
func Cap(size uintptr) int {
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

package prealloc

import "unsafe"

// Byte budgets that Cap fits a slice into. Both come from the Go runtime, not
// from JSON:
//
//   - fastAllocMax: go1.27 has a fast allocation path for small sizes.
//   - spanBudget: below this size the runtime adds no malloc header, and the
//     garbage collector can mark the object cheaply.
const (
	// fastAllocMax is the go1.27 cutoff for size-specialized malloc. The
	// release notes say "<80 byte", but 80 itself is included. The compiler
	// gives up on `size > specializedMallocMax` (ssagen/ssa.go). The runtime
	// path that make([]T,0,N) reaches through makeslice checks
	// `size < len(mallocNoScanTable)`, and that table has
	// specializedMallocMax+1 = 81 entries. So 80 bytes is specialized and 81 is
	// not. On go1.26 this rung changes nothing: SizeSpecializedMalloc is off by
	// default there, and turning it on with GOEXPERIMENT specializes every size
	// up to 512. The rung also saves memory: it stops a slice of small elements
	// from reserving a whole span up front.
	fastAllocMax = 80

	// spanBudget is a boundary the runtime defines, not a guess. It is not a
	// garbage collector constant: it equals gc.MinSizeForMallocHeader =
	// goarch.PtrSize * goarch.PtrBits = 8 * PtrSize * PtrSize, which is 512 on
	// 64-bit and 128 on 32-bit. writeHeapBitsSmall sets its size. Staying
	// within it gives two things, the first worth more:
	//
	//   - No malloc header. Above the boundary, an object that contains
	//     pointers gets 8 bytes added before its size class is chosen
	//     (roundupsize in runtime/msize.go). That can push it into the next
	//     class. For example a []struct{*int; [7]int64} of 512 bytes allocates
	//     512, and one of 576 bytes allocates 640.
	//   - Inline span mark bits, which let the Green Tea collector (the default
	//     since go1.26) scan all objects of a span in one batch
	//     (gcUsesSpanInlineMarkBits = heapBitsInSpan(size) && size >= 16 in
	//     mgcmark_greenteagc.go). This does not apply to backing arrays that
	//     hold no pointers, such as []int, []byte, or structs without pointers.
	//     tryDeferToSpanScan never scans those. For them only the locality of
	//     the mark bits helps.
	//
	// The boundary is an exclusive minimum chosen so the test gives the same
	// answer before and after size-class rounding (mbitmap.go). 512 is also a
	// size class itself. So comparing a raw N*sizeof(E) against it always
	// agrees with the runtime, and this code needs no size-class model.
	spanBudget = 8 * unsafe.Sizeof(uintptr(0)) * unsafe.Sizeof(uintptr(0))
)

// Cap returns the initial capacity a generated decoder gives a slice whose
// element is size bytes wide, when no tag says otherwise. Any tag that states
// what the payload holds wins over Cap: `hint:`, `len`, `minlen`, and also
// `maxlen=N` when N elements fit within spanBudget. An exact upper bound is
// better than a guess from the element size, and the slice can never grow
// past it.
//
// Cap picks the first rule that fits:
//
//  1. as many elements as fit in 80 bytes, if that is at least 2;
//  2. else as many as fit in spanBudget, if that is at least 2;
//  3. else 1. A single element already exceeds the span budget, so a larger
//     guess only wastes memory the payload may never use.
//
// The floor is two elements because append starts with one anyway. Also, a
// slice under 32 bytes can be placed on the stack, and moving it to the heap
// later costs a copy. The runtime agrees from the other side: objects below
// 16 bytes get no inline span mark bits (the `size >= 16` part of
// gcUsesSpanInlineMarkBits). Cap never produces an allocation under 40
// bytes, so it never lands below that limit.
//
// Callers pass `unsafe.Sizeof(*new(T))`, a compile-time constant, so the
// compiler folds the call to a literal and this code never runs.
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

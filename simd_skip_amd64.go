//go:build goexperiment.simd

// Vector versions of the skip functions, which step over a JSON value
// without decoding it. SIMD (vector) code processes 16, 32 or 64 bytes with
// one CPU instruction. AVX, AVX2 and AVX-512 are the x86 vector instruction
// sets for those widths. A lane is one byte slot inside a vector. A mask is
// one bit per lane, set where a test matched.
//
// The scalar skip functions walk whitespace one byte at a time and search
// strings with several passes. On pretty-printed input with lots of
// whitespace, those two costs dominate. Each tier here has:
//   - SkipSpace*, which checks the first byte without vectors and then skips
//     a whitespace run with vectors,
//   - skipString*, which finds the end of a string with one vector pass and
//     checks escapes with the same switch as the scalar skipString,
//   - SkipValue*, skipArray* and skipObject*, copies of the scalar functions
//     that differ only in which tier functions they call.
// Behaviour and errors match the scalar functions exactly. Generated code
// calls one tier directly when ggen runs with -simd.
//
// Whitespace test: each lane is compared to ' ', '\t', '\n' and '\r', and the
// results are ORed. The first zero bit in the mask is the first byte that is
// not whitespace. At 512 bits the masks are ORed as uint64 values, because
// Mask.Or moves them back into vector registers and out again.

package ggen

import (
	"math/bits"

	"simd/archsimd"
)

// Each SkipSpace* is a small wrapper that the compiler can inline, around a
// rarely used vector body. The wrapper returns without a call when the next
// byte is not whitespace, which is the common case. Only a real whitespace
// run enters the vector body. The skip functions call SkipSpace* at 8 places
// per tier. On compact JSON with no whitespace, the inlined check saves a
// function call at each of them, and those calls were a large share of skip
// time. A single function with the vector body inside would be too big to
// inline. The scalar and stream SkipSpace use the same early-return shape.

// SkipSpaceAVX is SkipSpace that skips whitespace runs 16 bytes at a time.
func SkipSpaceAVX(data []byte, i int) int {
	if i >= len(data) || data[i] > ' ' {
		return i
	}
	return skipSpaceAVXSlow(data, i)
}

func skipSpaceAVXSlow(data []byte, i int) int {
	sp := archsimd.BroadcastUint8x16(' ')
	tb := archsimd.BroadcastUint8x16('\t')
	nl := archsimd.BroadcastUint8x16('\n')
	cr := archsimd.BroadcastUint8x16('\r')
	for ; i+16 <= len(data); i += 16 {
		v := archsimd.LoadUint8x16(data[i:])
		m := v.Equal(sp).Or(v.Equal(tb)).Or(v.Equal(nl)).Or(v.Equal(cr)).ToBits()
		if m != 0xFFFF {
			return i + bits.TrailingZeros16(^m)
		}
	}
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	return i
}

// SkipSpaceAVX2 is SkipSpace that skips whitespace runs 32 bytes at a time.
func SkipSpaceAVX2(data []byte, i int) int {
	if i >= len(data) || data[i] > ' ' {
		return i
	}
	return skipSpaceAVX2Slow(data, i)
}

func skipSpaceAVX2Slow(data []byte, i int) int {
	sp := archsimd.BroadcastUint8x32(' ')
	tb := archsimd.BroadcastUint8x32('\t')
	nl := archsimd.BroadcastUint8x32('\n')
	cr := archsimd.BroadcastUint8x32('\r')
	for ; i+32 <= len(data); i += 32 {
		v := archsimd.LoadUint8x32(data[i:])
		m := v.Equal(sp).Or(v.Equal(tb)).Or(v.Equal(nl)).Or(v.Equal(cr)).ToBits()
		if m != 0xFFFFFFFF {
			return i + bits.TrailingZeros32(^m)
		}
	}
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	return i
}

// SkipSpaceAVX512 is SkipSpace that skips whitespace runs 64 bytes at a
// time.
func SkipSpaceAVX512(data []byte, i int) int {
	if i >= len(data) || data[i] > ' ' {
		return i
	}
	return skipSpaceAVX512Slow(data, i)
}

func skipSpaceAVX512Slow(data []byte, i int) int {
	sp := archsimd.BroadcastUint8x64(' ')
	tb := archsimd.BroadcastUint8x64('\t')
	nl := archsimd.BroadcastUint8x64('\n')
	cr := archsimd.BroadcastUint8x64('\r')
	for ; i+64 <= len(data); i += 64 {
		v := archsimd.LoadUint8x64(data[i:])
		m := v.Equal(sp).ToBits() | v.Equal(tb).ToBits() | v.Equal(nl).ToBits() | v.Equal(cr).ToBits()
		if m != ^uint64(0) {
			return i + bits.TrailingZeros64(^m)
		}
	}
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	return i
}

// skipStringTail validates the escape at data[bs] and returns the index to
// continue from. Every skipString tier shares it for the rare escape case.
// It uses the same switch as the scalar skipString.
func skipStringTail(data []byte, bs int) (int, error) {
	if bs+1 >= len(data) {
		return len(data), ErrBadString
	}
	switch data[bs+1] {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return bs + 2, nil
	case 'u':
		if bs+6 > len(data) {
			return uEscapeEnd(data, bs), ErrBadString
		}
		if _, ok := parseHex4(data[bs+2 : bs+6]); !ok {
			return bs, ErrBadString
		}
		return bs + 6, nil
	default:
		return bs, ErrBadString
	}
}

// skipStringAVX steps past a JSON string using the AVX scan.
func skipStringAVX(data []byte, i int) (int, error) {
	j := i + 1
	for {
		k := structuralIndexAVX(data[j:])
		if k < 0 {
			return len(data), ErrUnterminated
		}
		switch data[j+k] {
		case '"':
			return j + k + 1, nil
		case '\\':
			nj, err := skipStringTail(data, j+k)
			if err != nil {
				return nj, err
			}
			j = nj
		default:
			// The scan stopped on the control byte itself, which is the
			// position the scalar skipString reports.
			return j + k, ErrBadString
		}
	}
}

// skipStringAVX2 is skipStringAVX using the AVX2 scan.
func skipStringAVX2(data []byte, i int) (int, error) {
	j := i + 1
	for {
		k := structuralIndexAVX2(data[j:])
		if k < 0 {
			return len(data), ErrUnterminated
		}
		switch data[j+k] {
		case '"':
			return j + k + 1, nil
		case '\\':
			nj, err := skipStringTail(data, j+k)
			if err != nil {
				return nj, err
			}
			j = nj
		default:
			// The scan stopped on the control byte itself, which is the
			// position the scalar skipString reports.
			return j + k, ErrBadString
		}
	}
}

// skipStringAVX512 is skipStringAVX using the AVX-512 scan.
func skipStringAVX512(data []byte, i int) (int, error) {
	j := i + 1
	for {
		k := structuralIndexAVX512(data[j:])
		if k < 0 {
			return len(data), ErrUnterminated
		}
		switch data[j+k] {
		case '"':
			return j + k + 1, nil
		case '\\':
			nj, err := skipStringTail(data, j+k)
			if err != nil {
				return nj, err
			}
			j = nj
		default:
			// The scan stopped on the control byte itself, which is the
			// position the scalar skipString reports.
			return j + k, ErrBadString
		}
	}
}

// SkipValueAVX is SkipValue using the AVX skip functions.
func SkipValueAVX(data []byte, i int) (int, error) {
	return skipValueAVX(data, i, 0)
}

func skipValueAVX(data []byte, i, depth int) (int, error) {
	i = SkipSpaceAVX(data, i)
	if i >= len(data) {
		return i, ErrUnexpectedEnd
	}
	switch data[i] {
	case '"':
		return skipStringAVX(data, i)
	case 't', 'f':
		_, j, err := Bool(data, i)
		if err != nil {
			return BoolEnd(data, i), err
		}
		return j, nil
	case 'n':
		if j, ok := Null(data, i); ok {
			return j, nil
		}
		return litEnd(data, i, "null"), ErrBadLiteral
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return skipNumber(data, i)
	case '[':
		return skipArrayAVX(data, i+1, depth+1)
	case '{':
		return skipObjectAVX(data, i+1, depth+1)
	}
	return i, ErrBadValue
}

func skipArrayAVX(data []byte, i, depth int) (int, error) {
	if depth > maxDepth {
		return i, ErrMaxDepth
	}
	i = SkipSpaceAVX(data, i)
	if i < len(data) && data[i] == ']' {
		return i + 1, nil
	}
	for {
		j, err := skipValueAVX(data, i, depth)
		if err != nil {
			return j, err
		}
		i = SkipSpaceAVX(data, j)
		if i >= len(data) {
			return i, ErrBadArray
		}
		if data[i] == ',' {
			i++
			continue
		}
		if data[i] == ']' {
			return i + 1, nil
		}
		return i, ErrBadArray
	}
}

func skipObjectAVX(data []byte, i, depth int) (int, error) {
	if depth > maxDepth {
		return i, ErrMaxDepth
	}
	i = SkipSpaceAVX(data, i)
	if i < len(data) && data[i] == '}' {
		return i + 1, nil
	}
	for {
		if i >= len(data) || data[i] != '"' {
			return i, ErrBadObject
		}
		j, err := skipStringAVX(data, i)
		if err != nil {
			return j, err
		}
		j = SkipSpaceAVX(data, j)
		if j >= len(data) || data[j] != ':' {
			return j, ErrBadObject
		}
		j = SkipSpaceAVX(data, j+1)
		k, err := skipValueAVX(data, j, depth)
		if err != nil {
			return k, err
		}
		i = SkipSpaceAVX(data, k)
		if i >= len(data) {
			return i, ErrBadObject
		}
		if data[i] == ',' {
			i = SkipSpaceAVX(data, i+1)
			continue
		}
		if data[i] == '}' {
			return i + 1, nil
		}
		return i, ErrBadObject
	}
}

// SkipValueAVX2 is SkipValue using the AVX2 skip functions.
func SkipValueAVX2(data []byte, i int) (int, error) {
	return skipValueAVX2(data, i, 0)
}

func skipValueAVX2(data []byte, i, depth int) (int, error) {
	i = SkipSpaceAVX2(data, i)
	if i >= len(data) {
		return i, ErrUnexpectedEnd
	}
	switch data[i] {
	case '"':
		return skipStringAVX2(data, i)
	case 't', 'f':
		_, j, err := Bool(data, i)
		if err != nil {
			return BoolEnd(data, i), err
		}
		return j, nil
	case 'n':
		if j, ok := Null(data, i); ok {
			return j, nil
		}
		return litEnd(data, i, "null"), ErrBadLiteral
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return skipNumber(data, i)
	case '[':
		return skipArrayAVX2(data, i+1, depth+1)
	case '{':
		return skipObjectAVX2(data, i+1, depth+1)
	}
	return i, ErrBadValue
}

func skipArrayAVX2(data []byte, i, depth int) (int, error) {
	if depth > maxDepth {
		return i, ErrMaxDepth
	}
	i = SkipSpaceAVX2(data, i)
	if i < len(data) && data[i] == ']' {
		return i + 1, nil
	}
	for {
		j, err := skipValueAVX2(data, i, depth)
		if err != nil {
			return j, err
		}
		i = SkipSpaceAVX2(data, j)
		if i >= len(data) {
			return i, ErrBadArray
		}
		if data[i] == ',' {
			i++
			continue
		}
		if data[i] == ']' {
			return i + 1, nil
		}
		return i, ErrBadArray
	}
}

func skipObjectAVX2(data []byte, i, depth int) (int, error) {
	if depth > maxDepth {
		return i, ErrMaxDepth
	}
	i = SkipSpaceAVX2(data, i)
	if i < len(data) && data[i] == '}' {
		return i + 1, nil
	}
	for {
		if i >= len(data) || data[i] != '"' {
			return i, ErrBadObject
		}
		j, err := skipStringAVX2(data, i)
		if err != nil {
			return j, err
		}
		j = SkipSpaceAVX2(data, j)
		if j >= len(data) || data[j] != ':' {
			return j, ErrBadObject
		}
		j = SkipSpaceAVX2(data, j+1)
		k, err := skipValueAVX2(data, j, depth)
		if err != nil {
			return k, err
		}
		i = SkipSpaceAVX2(data, k)
		if i >= len(data) {
			return i, ErrBadObject
		}
		if data[i] == ',' {
			i = SkipSpaceAVX2(data, i+1)
			continue
		}
		if data[i] == '}' {
			return i + 1, nil
		}
		return i, ErrBadObject
	}
}

// SkipValueAVX512 is SkipValue using the AVX-512 skip functions.
func SkipValueAVX512(data []byte, i int) (int, error) {
	return skipValueAVX512(data, i, 0)
}

func skipValueAVX512(data []byte, i, depth int) (int, error) {
	i = SkipSpaceAVX512(data, i)
	if i >= len(data) {
		return i, ErrUnexpectedEnd
	}
	switch data[i] {
	case '"':
		return skipStringAVX512(data, i)
	case 't', 'f':
		_, j, err := Bool(data, i)
		if err != nil {
			return BoolEnd(data, i), err
		}
		return j, nil
	case 'n':
		if j, ok := Null(data, i); ok {
			return j, nil
		}
		return litEnd(data, i, "null"), ErrBadLiteral
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return skipNumber(data, i)
	case '[':
		return skipArrayAVX512(data, i+1, depth+1)
	case '{':
		return skipObjectAVX512(data, i+1, depth+1)
	}
	return i, ErrBadValue
}

func skipArrayAVX512(data []byte, i, depth int) (int, error) {
	if depth > maxDepth {
		return i, ErrMaxDepth
	}
	i = SkipSpaceAVX512(data, i)
	if i < len(data) && data[i] == ']' {
		return i + 1, nil
	}
	for {
		j, err := skipValueAVX512(data, i, depth)
		if err != nil {
			return j, err
		}
		i = SkipSpaceAVX512(data, j)
		if i >= len(data) {
			return i, ErrBadArray
		}
		if data[i] == ',' {
			i++
			continue
		}
		if data[i] == ']' {
			return i + 1, nil
		}
		return i, ErrBadArray
	}
}

func skipObjectAVX512(data []byte, i, depth int) (int, error) {
	if depth > maxDepth {
		return i, ErrMaxDepth
	}
	i = SkipSpaceAVX512(data, i)
	if i < len(data) && data[i] == '}' {
		return i + 1, nil
	}
	for {
		if i >= len(data) || data[i] != '"' {
			return i, ErrBadObject
		}
		j, err := skipStringAVX512(data, i)
		if err != nil {
			return j, err
		}
		j = SkipSpaceAVX512(data, j)
		if j >= len(data) || data[j] != ':' {
			return j, ErrBadObject
		}
		j = SkipSpaceAVX512(data, j+1)
		k, err := skipValueAVX512(data, j, depth)
		if err != nil {
			return k, err
		}
		i = SkipSpaceAVX512(data, k)
		if i >= len(data) {
			return i, ErrBadObject
		}
		if data[i] == ',' {
			i = SkipSpaceAVX512(data, i+1)
			continue
		}
		if data[i] == '}' {
			return i + 1, nil
		}
		return i, ErrBadObject
	}
}

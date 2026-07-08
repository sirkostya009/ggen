//go:build goexperiment.simd

// Fused SIMD skip tree. The scalar tree steps byte-at-a-time through
// whitespace and calls the three-pass skipString; on whitespace-rich
// (pretty-printed) payloads both dominate skip cost. Per tier:
// SkipSpace* (vector run skip after a scalar first-byte exit),
// skipString* (fused structural locate, escape switch identical to
// scalar skipString), and SkipValue*/skipArray*/skipObject* copies whose
// only change is the tier callees. Semantics + error identity mirror the
// scalar tree; generated code calls one tier directly (ggen -simd).
//
// Whitespace classify: eq ' ' | eq '\t' | eq '\n' | eq '\r' per lane; the
// first zero bit in the mask is the first non-WS byte. 512-bit ORs mask
// bits in scalar registers (Mask.Or round-trips the vector domain).

package scan

import (
	"math/bits"
	"simd/archsimd"
)

// The SkipSpace* tiers are an inlinable shell over a cold vector body. The
// shell exits on the common non-whitespace byte without a call; only an
// actual whitespace run enters the vector body. The tier skip tree calls
// SkipSpace* at 8 sites per tier, so on compact (whitespace-free) JSON the
// inlined early-out replaces a call + prologue + ret at every site — that
// overhead was 22% flat of SkipHeavy/compact before the split (the whole
// vector body was unreachable there yet still blocked inlining). Same
// no-temp early-out shape as the scalar/stream SkipSpace shells.

// SkipSpaceAVX is SkipSpace with a 16-byte vector run skip.
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
		v := archsimd.LoadUint8x16Slice(data[i:])
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

// SkipSpaceAVX2 is SkipSpace with a 32-byte vector run skip.
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
		v := archsimd.LoadUint8x32Slice(data[i:])
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

// SkipSpaceAVX512 is SkipSpace with a 64-byte vector run skip.
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
		v := archsimd.LoadUint8x64Slice(data[i:])
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

// skipStringTail validates the escape at data[bs] and returns the resume
// index — the shared cold arm of every skipString tier (same switch as
// scalar skipString).
func skipStringTail(data []byte, bs int) (int, error) {
	if bs+1 >= len(data) {
		return 0, ErrBadString
	}
	switch data[bs+1] {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return bs + 2, nil
	case 'u':
		if bs+6 > len(data) {
			return 0, ErrBadString
		}
		if _, ok := parseHex4(data[bs+2 : bs+6]); !ok {
			return 0, ErrBadString
		}
		return bs + 6, nil
	default:
		return 0, ErrBadString
	}
}

// skipStringAVX advances past a JSON string via the fused AVX locate.
func skipStringAVX(data []byte, i int) (int, error) {
	j := i + 1
	for {
		k := structuralIndexAVX(data[j:])
		if k < 0 {
			return 0, ErrUnterminated
		}
		switch data[j+k] {
		case '"':
			return j + k + 1, nil
		case '\\':
			nj, err := skipStringTail(data, j+k)
			if err != nil {
				return 0, err
			}
			j = nj
		default:
			return 0, ctrlHitErr(data[j+k:])
		}
	}
}

// skipStringAVX2 — see skipStringAVX.
func skipStringAVX2(data []byte, i int) (int, error) {
	j := i + 1
	for {
		k := structuralIndexAVX2(data[j:])
		if k < 0 {
			return 0, ErrUnterminated
		}
		switch data[j+k] {
		case '"':
			return j + k + 1, nil
		case '\\':
			nj, err := skipStringTail(data, j+k)
			if err != nil {
				return 0, err
			}
			j = nj
		default:
			return 0, ctrlHitErr(data[j+k:])
		}
	}
}

// skipStringAVX512 — see skipStringAVX.
func skipStringAVX512(data []byte, i int) (int, error) {
	j := i + 1
	for {
		k := structuralIndexAVX512(data[j:])
		if k < 0 {
			return 0, ErrUnterminated
		}
		switch data[j+k] {
		case '"':
			return j + k + 1, nil
		case '\\':
			nj, err := skipStringTail(data, j+k)
			if err != nil {
				return 0, err
			}
			j = nj
		default:
			return 0, ctrlHitErr(data[j+k:])
		}
	}
}

// SkipValueAVX is SkipValue over the fused AVX skip tree.
func SkipValueAVX(data []byte, i int) (int, error) {
	i = SkipSpaceAVX(data, i)
	if i >= len(data) {
		return 0, ErrUnexpectedEnd
	}
	switch data[i] {
	case '"':
		return skipStringAVX(data, i)
	case 't', 'f':
		_, j, err := Bool(data, i)
		return j, err
	case 'n':
		if j, ok := Null(data, i); ok {
			return j, nil
		}
		return 0, ErrBadLiteral
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return skipNumber(data, i)
	case '[':
		return skipArrayAVX(data, i+1)
	case '{':
		return skipObjectAVX(data, i+1)
	}
	return 0, ErrBadValue
}

func skipArrayAVX(data []byte, i int) (int, error) {
	i = SkipSpaceAVX(data, i)
	if i < len(data) && data[i] == ']' {
		return i + 1, nil
	}
	for {
		j, err := SkipValueAVX(data, i)
		if err != nil {
			return 0, err
		}
		i = SkipSpaceAVX(data, j)
		if i >= len(data) {
			return 0, ErrBadArray
		}
		if data[i] == ',' {
			i++
			continue
		}
		if data[i] == ']' {
			return i + 1, nil
		}
		return 0, ErrBadArray
	}
}

func skipObjectAVX(data []byte, i int) (int, error) {
	i = SkipSpaceAVX(data, i)
	if i < len(data) && data[i] == '}' {
		return i + 1, nil
	}
	for {
		if i >= len(data) || data[i] != '"' {
			return 0, ErrBadObject
		}
		j, err := skipStringAVX(data, i)
		if err != nil {
			return 0, err
		}
		j = SkipSpaceAVX(data, j)
		if j >= len(data) || data[j] != ':' {
			return 0, ErrBadObject
		}
		j = SkipSpaceAVX(data, j+1)
		k, err := SkipValueAVX(data, j)
		if err != nil {
			return 0, err
		}
		i = SkipSpaceAVX(data, k)
		if i >= len(data) {
			return 0, ErrBadObject
		}
		if data[i] == ',' {
			i = SkipSpaceAVX(data, i+1)
			continue
		}
		if data[i] == '}' {
			return i + 1, nil
		}
		return 0, ErrBadObject
	}
}

// SkipValueAVX2 is SkipValue over the fused AVX2 skip tree.
func SkipValueAVX2(data []byte, i int) (int, error) {
	i = SkipSpaceAVX2(data, i)
	if i >= len(data) {
		return 0, ErrUnexpectedEnd
	}
	switch data[i] {
	case '"':
		return skipStringAVX2(data, i)
	case 't', 'f':
		_, j, err := Bool(data, i)
		return j, err
	case 'n':
		if j, ok := Null(data, i); ok {
			return j, nil
		}
		return 0, ErrBadLiteral
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return skipNumber(data, i)
	case '[':
		return skipArrayAVX2(data, i+1)
	case '{':
		return skipObjectAVX2(data, i+1)
	}
	return 0, ErrBadValue
}

func skipArrayAVX2(data []byte, i int) (int, error) {
	i = SkipSpaceAVX2(data, i)
	if i < len(data) && data[i] == ']' {
		return i + 1, nil
	}
	for {
		j, err := SkipValueAVX2(data, i)
		if err != nil {
			return 0, err
		}
		i = SkipSpaceAVX2(data, j)
		if i >= len(data) {
			return 0, ErrBadArray
		}
		if data[i] == ',' {
			i++
			continue
		}
		if data[i] == ']' {
			return i + 1, nil
		}
		return 0, ErrBadArray
	}
}

func skipObjectAVX2(data []byte, i int) (int, error) {
	i = SkipSpaceAVX2(data, i)
	if i < len(data) && data[i] == '}' {
		return i + 1, nil
	}
	for {
		if i >= len(data) || data[i] != '"' {
			return 0, ErrBadObject
		}
		j, err := skipStringAVX2(data, i)
		if err != nil {
			return 0, err
		}
		j = SkipSpaceAVX2(data, j)
		if j >= len(data) || data[j] != ':' {
			return 0, ErrBadObject
		}
		j = SkipSpaceAVX2(data, j+1)
		k, err := SkipValueAVX2(data, j)
		if err != nil {
			return 0, err
		}
		i = SkipSpaceAVX2(data, k)
		if i >= len(data) {
			return 0, ErrBadObject
		}
		if data[i] == ',' {
			i = SkipSpaceAVX2(data, i+1)
			continue
		}
		if data[i] == '}' {
			return i + 1, nil
		}
		return 0, ErrBadObject
	}
}

// SkipValueAVX512 is SkipValue over the fused AVX-512 skip tree.
func SkipValueAVX512(data []byte, i int) (int, error) {
	i = SkipSpaceAVX512(data, i)
	if i >= len(data) {
		return 0, ErrUnexpectedEnd
	}
	switch data[i] {
	case '"':
		return skipStringAVX512(data, i)
	case 't', 'f':
		_, j, err := Bool(data, i)
		return j, err
	case 'n':
		if j, ok := Null(data, i); ok {
			return j, nil
		}
		return 0, ErrBadLiteral
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return skipNumber(data, i)
	case '[':
		return skipArrayAVX512(data, i+1)
	case '{':
		return skipObjectAVX512(data, i+1)
	}
	return 0, ErrBadValue
}

func skipArrayAVX512(data []byte, i int) (int, error) {
	i = SkipSpaceAVX512(data, i)
	if i < len(data) && data[i] == ']' {
		return i + 1, nil
	}
	for {
		j, err := SkipValueAVX512(data, i)
		if err != nil {
			return 0, err
		}
		i = SkipSpaceAVX512(data, j)
		if i >= len(data) {
			return 0, ErrBadArray
		}
		if data[i] == ',' {
			i++
			continue
		}
		if data[i] == ']' {
			return i + 1, nil
		}
		return 0, ErrBadArray
	}
}

func skipObjectAVX512(data []byte, i int) (int, error) {
	i = SkipSpaceAVX512(data, i)
	if i < len(data) && data[i] == '}' {
		return i + 1, nil
	}
	for {
		if i >= len(data) || data[i] != '"' {
			return 0, ErrBadObject
		}
		j, err := skipStringAVX512(data, i)
		if err != nil {
			return 0, err
		}
		j = SkipSpaceAVX512(data, j)
		if j >= len(data) || data[j] != ':' {
			return 0, ErrBadObject
		}
		j = SkipSpaceAVX512(data, j+1)
		k, err := SkipValueAVX512(data, j)
		if err != nil {
			return 0, err
		}
		i = SkipSpaceAVX512(data, k)
		if i >= len(data) {
			return 0, ErrBadObject
		}
		if data[i] == ',' {
			i = SkipSpaceAVX512(data, i+1)
			continue
		}
		if data[i] == '}' {
			return i + 1, nil
		}
		return 0, ErrBadObject
	}
}

// Package decode provides runtime helpers ggen-generated decoders delegate to:
// the bytes-path Decoder interface every generated type satisfies, plus the
// array walkers (UnmarshalSlice / ReadSlice).
//
// The STREAMING side lives on the Stream itself — StreamDecoder plus the
// generic methods (*Stream).Value / .Slice:
//
//	res, _, err := T{}.DecodeFrom(data)
//	res, err := NewStream(r, buf).Value[T]()
package ggen

import (
	"github.com/sirkostya009/ggen/internal/prealloc"
	"io"
	"strconv"
	"unsafe"
)

// Decoder is the BYTES-path interface. DecodeFrom reads one value and returns
// it, the bytes consumed, and any error; callers chaining values advance their
// own cursor over data[n:].
//
// Strings inside the returned value alias the caller's bytes — callers MUST
// NOT mutate the input buffer while the value is in use.
type Decoder[T any] interface {
	DecodeFrom(data []byte) (T, int, error)
}

// UnmarshalSlice decodes a JSON array of T, delegating each element to
// T.DecodeFrom. Errors route through [NewParseErr].
func UnmarshalSlice[T Decoder[T]](data []byte) ([]T, error) {
	i := SkipSpace(data, 0)
	if i >= len(data) || data[i] != '[' {
		return nil, NewParseErr("[]", i, ErrBadArray)
	}
	i++
	i = SkipSpace(data, i)
	if i < len(data) && data[i] == ']' {
		// Non-nil empty for [] — generated slice fields and jsonv2 agree.
		return []T{}, trailingCheck(data, i+1)
	}
	// Same width-driven prealloc ladder generated slice fields use.
	result := make([]T, 0, prealloc.Cap(unsafe.Sizeof(*new(T))))
	for {
		var zero T
		v, n, err := zero.DecodeFrom(data[i:])
		if err != nil {
			return nil, NewParseErrShift(arrField(len(result)), i+n, n, err)
		}
		result = append(result, v)
		i = SkipSpace(data, i+n)
		if i >= len(data) {
			return nil, NewParseErr(arrField(len(result)-1), i, ErrBadArray)
		}
		if data[i] == ',' {
			i = SkipSpace(data, i+1)
			continue
		}
		if data[i] == ']' {
			return result, trailingCheck(data, i+1)
		}
		return nil, NewParseErr(arrField(len(result)-1), i, ErrBadArray)
	}
}

// trailingCheck rejects non-whitespace bytes after the closing bracket, so a
// caller can detect a remainder like `[1,2]]]` or `[{}]{"junk":`
// (jsonv2 whole-input parity).
func trailingCheck(data []byte, i int) error {
	if i = SkipSpace(data, i); i < len(data) {
		return NewParseErr("[]", i, ErrTrailingData)
	}
	return nil
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

// arrField renders "[N]" for the path segment on slice-walker errors
// (e.g. "[5].street").
func arrField(n int) string {
	buf := make([]byte, 0, 12)
	buf = append(buf, '[')
	buf = strconv.AppendInt(buf, int64(n), 10)
	buf = append(buf, ']')
	return unsafe.String(unsafe.SliceData(buf), len(buf))
}

// Decoding over a []byte that holds the whole input (the bytes path). This
// file has the Decoder interface every generated type satisfies and the array
// helpers UnmarshalSlice and ReadSlice.
//
// Decoding from an io.Reader (the stream path) lives on Stream: the
// StreamDecoder interface and the generic methods (*Stream).Value and
// (*Stream).Slice.
//
//	res, _, err := T{}.DecodeFrom(data)
//	res, err := NewStream(r, buf).Value[T]()

package ggen

import (
	"io"
	"strconv"
	"unsafe"

	"github.com/sirkostya009/ggen/internal/prealloc"
)

// Decoder is the bytes-path decoding interface. DecodeFrom reads one value
// and returns it, the number of bytes consumed, and any error. A caller that
// reads several values in a row continues from data[n:].
//
// Strings inside the returned value alias data: they point into the caller's
// bytes instead of holding a copy. The caller must not modify data while the
// value is in use.
type Decoder[T any] interface {
	DecodeFrom(data []byte) (T, int, error)
}

// Unmarshaler is a type that decodes on both the bytes path ([Decoder]) and
// the stream path ([StreamDecoder]). Every ggen-generated struct satisfies
// it. Use it as a constraint for code that picks the path at the call site.
type Unmarshaler[T any] interface {
	Decoder[T]
	StreamDecoder[T]
}

// UnmarshalSlice decodes a JSON array of T. Each element is decoded by
// T.DecodeFrom. Errors are wrapped with [NewParseErr].
func UnmarshalSlice[T Decoder[T]](data []byte) ([]T, error) {
	i := SkipSpace(data, 0)
	if i >= len(data) || data[i] != '[' {
		return nil, NewParseErr("[]", i, ErrBadArray)
	}
	i++
	i = SkipSpace(data, i)
	if i < len(data) && data[i] == ']' {
		// [] gives an empty non-nil slice, like generated slice fields and
		// jsonv2.
		return []T{}, trailingCheck(data, i+1)
	}
	// Pick the initial capacity from the element size, like generated slice
	// fields do.
	result := make([]T, 0, prealloc.Cap(unsafe.Sizeof(*new(T))))
	for {
		var zero T
		v, n, err := zero.DecodeFrom(data[i:])
		if err != nil {
			// a multierr element read itself whole, so all of its failures are kept
			if es, ok := err.(Errors); ok {
				es.AddPos(i)
				es.PrependPath(arrField(len(result)))
				return nil, es
			}
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

// trailingCheck rejects anything but whitespace after the closing bracket,
// such as `[1,2]]]` or `[{}]{"junk":`. jsonv2 also requires the whole input
// to be one value.
func trailingCheck(data []byte, i int) error {
	if i = SkipSpace(data, i); i < len(data) {
		return NewParseErr("[]", i, ErrTrailingData)
	}
	return nil
}

// ReadSlice reads all of r and decodes it with UnmarshalSlice. An error from
// io.ReadAll is returned as is, not wrapped as a parse error.
func ReadSlice[T Decoder[T]](r io.Reader) ([]T, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return UnmarshalSlice[T](data)
}

// arrField returns the error path segment "[N]" for element n, as in
// "[5].street".
func arrField(n int) string {
	buf := make([]byte, 0, 12)
	buf = append(buf, '[')
	buf = strconv.AppendInt(buf, int64(n), 10)
	buf = append(buf, ']')
	// buf is local and never mutated after this point.
	return unsafe.String(unsafe.SliceData(buf), len(buf))
}

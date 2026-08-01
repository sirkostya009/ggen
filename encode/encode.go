// Package encode provides JSON marshaling helpers used by ggen-generated
// code. Values implement Marshaler by emitting themselves into a caller-owned
// byte slice via AppendJSON, avoiding intermediate allocations.
package encode

import (
	"errors"
	"io"
	"math"
	"reflect"
	"strconv"
	"sync"
	"unsafe"
)

// Marshaler is satisfied by any type that can append its JSON encoding to dst.
// Generated code implements this for every ggen-annotated struct.
//
// AppendJSON propagates errors from any nested encoder that can fail
// (TextMarshaler, JSONMarshaler, json.Marshal fallback). On error the
// returned dst slice is unspecified; callers must not consume it.
type Marshaler interface {
	AppendJSON(dst []byte) ([]byte, error)
	JSONSize() int
}

var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 512)
		return &b
	},
}

// Marshal returns the JSON encoding of v as a freshly owned []byte.
func Marshal[T Marshaler](v T) ([]byte, error) {
	return v.AppendJSON(make([]byte, 0, v.JSONSize()))
}

// Returned by AppendFloat when v is non-finite — JSON has no NaN/±Inf.
var (
	errNaN  = errors.New("ggen: unsupported value: NaN")
	errPInf = errors.New("ggen: unsupported value: +Inf")
	errNInf = errors.New("ggen: unsupported value: -Inf")
)

// AppendFloat appends v to dst as a JSON number, or returns an error when v
// is NaN / ±Inf. bitSize selects the strconv precision — 32 for float32
// source, 64 for float64. Format matches stdlib (ES6 ToString): 'f' while
// the decimal exponent sits in [-6, 21), 'e' otherwise, with no zero-padded
// negative exponent ("1e-7", not "1e-07").
func AppendFloat(dst []byte, v float64, bitSize int) ([]byte, error) {
	if math.IsNaN(v) {
		return dst, errNaN
	}
	if math.IsInf(v, 1) {
		return dst, errPInf
	}
	if math.IsInf(v, -1) {
		return dst, errNInf
	}
	format := byte('f')
	if abs := math.Abs(v); abs != 0 {
		if bitSize == 64 && (abs < 1e-6 || abs >= 1e21) ||
			bitSize == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
			format = 'e'
		}
	}
	dst = strconv.AppendFloat(dst, v, format, -1, bitSize)
	if format == 'e' {
		// Trim zero-padded exponent: e-09 → e-9.
		if n := len(dst); n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst, nil
}

// MarshalString returns the JSON encoding of v as a string. The returned
// string aliases the freshly allocated buffer via unsafe conversion.
func MarshalString[T Marshaler](v T) (string, error) {
	b, err := v.AppendJSON(make([]byte, 0, v.JSONSize()))
	if err != nil {
		return "", err
	}
	return BytesToString(b), nil
}

// BytesToString returns a string that aliases buf without copying. The caller
// must not mutate buf after this call, and buf must not be pooled/reused.
func BytesToString(buf []byte) string {
	if len(buf) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(buf), len(buf))
}

// WriteTo writes the JSON encoding of v directly to w using a pooled buffer.
// Returns the first non-nil error from AppendJSON or the writer.
func WriteTo[T Marshaler](w io.Writer, v T) error {
	bufp := bufPool.Get().(*[]byte)
	buf, err := v.AppendJSON((*bufp)[:0])
	if err != nil {
		*bufp = buf[:0]
		bufPool.Put(bufp)
		return err
	}
	_, werr := w.Write(buf)
	*bufp = buf[:0]
	bufPool.Put(bufp)
	return werr
}

// ErrEmptyMarshalJSON reports a MarshalJSON that returned (empty, nil) —
// appending nothing would corrupt the output, and stdlib v1/v2 both treat
// it as a broken marshaler rather than inventing a value.
var ErrEmptyMarshalJSON = errors.New("encode: MarshalJSON returned empty output")

// isNilable reports whether T can hold a nil that breaks the value-receiver
// call: a pointer (auto-deref) or an interface (T = encode.Marshaler is
// explicitly supported — a nil element used to panic in JSONSize/AppendJSON
// where stdlib emits null).
func isNilable[T Marshaler]() bool {
	k := reflect.TypeFor[T]().Kind()
	return k == reflect.Pointer || k == reflect.Interface
}

// nilItem reports whether v is nil under a nilable T: a nil interface boxes
// to an invalid reflect.Value; an interface holding a typed nil pointer is
// nil for marshal purposes too (stdlib emits null for both).
func nilItem[T Marshaler](v T) bool {
	rv := reflect.ValueOf(v)
	return !rv.IsValid() || (rv.Kind() == reflect.Pointer && rv.IsNil())
}

// AppendSlice appends a JSON array of items to dst. A nil items slice emits
// `null` and an empty non-nil one `[]` (stdlib parity); nil pointer /
// nil-interface items emit `null`. Returns the first error encountered.
func AppendSlice[T Marshaler](dst []byte, items []T) ([]byte, error) {
	if items == nil {
		return append(dst, "null"...), nil
	}
	dst = append(dst, '[')
	isPtr := isNilable[T]()
	for i := range items {
		if i > 0 {
			dst = append(dst, ',')
		}
		if isPtr && nilItem(items[i]) {
			dst = append(dst, "null"...)
			continue
		}
		var err error
		dst, err = items[i].AppendJSON(dst)
		if err != nil {
			return dst, err
		}
	}
	return append(dst, ']'), nil
}

// MarshalSlice returns the JSON encoding of items as an array (`null` for a
// nil slice, like AppendSlice).
func MarshalSlice[T Marshaler](items []T) ([]byte, error) {
	if items == nil {
		return []byte("null"), nil
	}
	n := 2 + len(items) // brackets + commas
	isPtr := isNilable[T]()
	for i := range items {
		if isPtr && nilItem(items[i]) {
			n += 4 // null
			continue
		}
		n += items[i].JSONSize()
	}
	return AppendSlice(make([]byte, 0, n), items)
}

// MarshalSliceString returns the JSON encoding of items as a string.
func MarshalSliceString[T Marshaler](items []T) (string, error) {
	buf, err := MarshalSlice(items)
	if err != nil {
		return "", err
	}
	return BytesToString(buf), nil
}

// WriteSliceTo writes the JSON encoding of items directly to w.
func WriteSliceTo[T Marshaler](w io.Writer, items []T) error {
	bufp := bufPool.Get().(*[]byte)
	buf, err := AppendSlice((*bufp)[:0], items)
	if err != nil {
		*bufp = buf[:0]
		bufPool.Put(bufp)
		return err
	}
	_, werr := w.Write(buf)
	*bufp = buf[:0]
	bufPool.Put(bufp)
	return werr
}

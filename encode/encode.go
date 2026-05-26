// Package encode provides JSON marshaling helpers used by ggen-generated
// code. Values implement Marshaler by emitting themselves into a caller-owned
// byte slice via AppendJSON, avoiding intermediate allocations.
package encode

import (
	"errors"
	"io"
	"math"
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
func Marshal(v Marshaler) ([]byte, error) {
	return v.AppendJSON(make([]byte, 0, v.JSONSize()))
}

// errNaN / errInf*: returned by AppendFloat when v is non-finite. JSON
// has no representation for NaN / ±Inf and stdlib v1 + v2 both reject
// these values on marshal; ggen follows.
var (
	errNaN  = errors.New("ggen: unsupported value: NaN")
	errPInf = errors.New("ggen: unsupported value: +Inf")
	errNInf = errors.New("ggen: unsupported value: -Inf")
)

// AppendFloat appends v to dst as a JSON number, or returns an error
// when v is NaN / ±Inf (matching encoding/json/v2). bitSize selects the
// strconv precision — 32 for float32 source, 64 for float64.
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
	return strconv.AppendFloat(dst, v, 'g', -1, bitSize), nil
}

// MarshalString returns the JSON encoding of v as a string. The returned
// string aliases the freshly allocated buffer via unsafe conversion.
func MarshalString(v Marshaler) (string, error) {
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

// Write writes the JSON encoding of v directly to w using a pooled buffer.
// Returns the first non-nil error from AppendJSON or the writer.
func Write(w io.Writer, v Marshaler) error {
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

// AppendSlice appends a JSON array of items to dst. Each item emits itself
// via AppendJSON. Returns the first error encountered along the way.
func AppendSlice[T Marshaler](dst []byte, items []T) ([]byte, error) {
	dst = append(dst, '[')
	for i := range items {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		dst, err = items[i].AppendJSON(dst)
		if err != nil {
			return dst, err
		}
	}
	return append(dst, ']'), nil
}

// MarshalSlice returns the JSON encoding of items as an array.
func MarshalSlice[T Marshaler](items []T) ([]byte, error) {
	var zero T
	return AppendSlice(make([]byte, 0, 2+len(items)*zero.JSONSize()), items)
}

// MarshalSliceString returns the JSON encoding of items as a string.
func MarshalSliceString[T Marshaler](items []T) (string, error) {
	var zero T
	buf, err := AppendSlice(make([]byte, 0, 2+len(items)*zero.JSONSize()), items)
	if err != nil {
		return "", err
	}
	return BytesToString(buf), nil
}

// WriteSlice writes the JSON encoding of items directly to w.
func WriteSlice[T Marshaler](w io.Writer, items []T) error {
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

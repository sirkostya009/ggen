// JSON encoding helpers used by generated code. A value implements Marshaler
// by appending its JSON to a byte slice the caller owns, through AppendJSON.
// This avoids intermediate allocations.

package ggen

import (
	"errors"
	"io"
	"math"
	"reflect"
	"strconv"
	"sync"
	"time"
	"unsafe"
)

// Marshaler is a type that can append its JSON encoding to dst. Generated
// code implements it for every ggen-annotated struct. JSONSize returns the
// capacity Marshal reserves for the output.
//
// AppendJSON returns errors from nested encoders that can fail, such as a
// TextMarshaler, a JSONMarshaler or the json.Marshal fallback. On error the
// contents of the returned slice are unspecified and must not be used.
type Marshaler interface {
	AppendJSON(dst []byte) ([]byte, error)
	JSONSize() int
}

var bufPool = sync.Pool{New: func() any {
	b := make([]byte, 0, 512)
	return &b
}}

// Marshal returns the JSON encoding of v as a freshly owned []byte.
func Marshal[T Marshaler](v T) ([]byte, error) {
	return v.AppendJSON(make([]byte, 0, v.JSONSize()))
}

// AppendFloat returns these for non-finite values, which JSON cannot
// represent.
var (
	errNaN  = errors.New("ggen: unsupported value: NaN")
	errPInf = errors.New("ggen: unsupported value: +Inf")
	errNInf = errors.New("ggen: unsupported value: -Inf")
)

// AppendFloat appends v to dst as a JSON number. It returns an error when v
// is NaN, +Inf or -Inf. bitSize is the strconv precision: 32 for a float32
// value, 64 for a float64. The format matches the standard library, which
// follows JavaScript's number-to-string rules. It uses 'f' when the absolute
// value is in [1e-6, 1e21) and 'e' otherwise. A negative exponent has no
// leading zero ("1e-7", not "1e-07").
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
		// Drop the exponent's leading zero: e-09 becomes e-9.
		if n := len(dst); n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst, nil
}

// AppendUnixSeconds appends t as decimal Unix seconds with an exact
// nanosecond fraction. This is the `format:unix` form. It avoids
// float64(UnixNano()), which overflows outside roughly the years 1678 to 2262
// and cannot hold every nanosecond digit.
func AppendUnixSeconds(dst []byte, t time.Time) []byte {
	sec, ns := t.Unix(), t.Nanosecond()
	if ns == 0 {
		return strconv.AppendInt(dst, sec, 10)
	}
	// Nanosecond() is in [0,1e9). So a negative time with a fraction prints
	// as -(|sec+1| + (1e9-ns)/1e9). For example sec=-1, ns=5e8 gives "-0.5".
	if sec < 0 {
		dst = append(dst, '-')
		sec = -(sec + 1)
		ns = 1e9 - ns
	}
	dst = strconv.AppendInt(dst, sec, 10)
	dst = append(dst, '.')
	var frac [9]byte
	for i := 8; i >= 0; i-- {
		frac[i] = byte('0' + ns%10)
		ns /= 10
	}
	n := 9
	for n > 1 && frac[n-1] == '0' {
		n--
	}
	return append(dst, frac[:n]...)
}

// MarshalString returns the JSON encoding of v as a string. The string
// shares memory with a newly allocated buffer, so no copy is made.
func MarshalString[T Marshaler](v T) (string, error) {
	b, err := v.AppendJSON(make([]byte, 0, v.JSONSize()))
	if err != nil {
		return "", err
	}
	return BytesToString(b), nil
}

// BytesToString returns a string that shares buf's memory, without a copy.
// The caller must not modify buf after this call, and must not reuse or pool
// buf.
func BytesToString(buf []byte) string {
	if len(buf) == 0 {
		return ""
	}
	// Safe because the caller never modifies buf afterwards.
	return unsafe.String(unsafe.SliceData(buf), len(buf))
}

// WriteTo writes the JSON encoding of v to w through a pooled buffer. It
// returns the first error from AppendJSON or from the writer.
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

// ErrEmptyMarshalJSON reports a MarshalJSON that returned empty output and a
// nil error. Writing nothing would produce invalid JSON. encoding/json and
// jsonv2 both treat this as a broken marshaler and do not invent a value.
var ErrEmptyMarshalJSON = errors.New("encode: MarshalJSON returned empty output")

// isNilable reports whether T is a pointer or an interface type. Such a T can
// hold nil, and calling a value-receiver method through a nil pointer or nil
// interface panics. T = Marshaler is supported, and a nil element encodes as
// null like in the standard library.
func isNilable[T Marshaler]() bool {
	k := reflect.TypeFor[T]().Kind()
	return k == reflect.Pointer || k == reflect.Interface
}

// nilItem reports whether v, of a nilable T, should encode as null. A nil
// interface gives an invalid reflect.Value. An interface holding a nil
// pointer also counts. The standard library writes null for both.
func nilItem[T Marshaler](v T) bool {
	rv := reflect.ValueOf(v)
	return !rv.IsValid() || (rv.Kind() == reflect.Pointer && rv.IsNil())
}

// AppendSlice appends items to dst as a JSON array. A nil slice encodes as
// null and an empty non-nil slice as [], as in the standard library. A nil
// pointer or nil interface element encodes as null. It returns the first
// error it hits.
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

// MarshalSlice returns the JSON encoding of items as an array. A nil slice
// encodes as null, like in AppendSlice.
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

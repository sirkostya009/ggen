// Package encode provides JSON marshaling helpers used by ggen-generated
// code. Values implement Marshaler by emitting themselves into a caller-owned
// byte slice via AppendJSON, avoiding intermediate allocations.
package encode

import (
	"encoding"
	"encoding/base64"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
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

// AppendString appends the escaped body of s plus a closing `"`. The
// CALLER is responsible for writing the opening `"` — generated code
// folds it into the constant key prefix where possible, or emits an
// explicit `dst = append(dst, '"')` at slice/map/standalone call sites.
//
// Escapes are HTML-safe by default: <, >, & become <, >, &
// (matches stdlib `encoding/json` v1). For raw output use
// AppendStringNoHTML. Zero allocation.
func AppendString(dst []byte, s string) []byte {
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c != '<' && c != '>' && c != '&' {
			continue
		}
		if start < i {
			dst = append(dst, s[start:i]...)
		}
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		default:
			const hex = "0123456789abcdef"
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		}
		start = i + 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

// AppendAny marshals an `any` value into dst as JSON, type-switching on
// the runtime value to avoid the reflection cliff for the common cases
// (`nil`, primitives, `[]any`, `map[string]any`, `json.Number`). Then
// dispatches via well-known interfaces (Marshaler, json.Marshaler,
// TextAppender, TextMarshaler) before reflecting through slices,
// arrays, maps, pointers, and structs. Last resort is
// `encoding/json.Marshal`.
//
// Struct fields honor `json:"name,omitempty,omitzero,string,inline"`
// tags. Anonymous (embedded) struct fields are promoted at parent level
// per stdlib semantics.
func AppendAny(dst []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(dst, 'n', 'u', 'l', 'l'), nil
	case bool:
		return strconv.AppendBool(dst, x), nil
	case string:
		dst = append(dst, '"')
		return AppendString(dst, x), nil
	case float64:
		return strconv.AppendFloat(dst, x, 'g', -1, 64), nil
	case float32:
		return strconv.AppendFloat(dst, float64(x), 'g', -1, 32), nil
	case int:
		return strconv.AppendInt(dst, int64(x), 10), nil
	case int8:
		return strconv.AppendInt(dst, int64(x), 10), nil
	case int16:
		return strconv.AppendInt(dst, int64(x), 10), nil
	case int32:
		return strconv.AppendInt(dst, int64(x), 10), nil
	case int64:
		return strconv.AppendInt(dst, x, 10), nil
	case uint:
		return strconv.AppendUint(dst, uint64(x), 10), nil
	case uint8:
		return strconv.AppendUint(dst, uint64(x), 10), nil
	case uint16:
		return strconv.AppendUint(dst, uint64(x), 10), nil
	case uint32:
		return strconv.AppendUint(dst, uint64(x), 10), nil
	case uint64:
		return strconv.AppendUint(dst, x, 10), nil
	case json.Number:
		// Already a valid JSON numeric literal — emit unquoted.
		return append(dst, x...), nil
	// Interfaces, in cross-pkg-dispatch priority order.
	case Marshaler:
		return x.AppendJSON(dst)
	case json.Marshaler:
		b, err := x.MarshalJSON()
		if err != nil {
			return dst, err
		}
		return append(dst, b...), nil
	case encoding.TextAppender:
		dst = append(dst, '"')
		var err error
		dst, err = x.AppendText(dst)
		if err != nil {
			return dst, err
		}
		return append(dst, '"'), nil
	case encoding.TextMarshaler:
		t, err := x.MarshalText()
		if err != nil {
			return dst, err
		}
		dst = append(dst, '"')
		dst = AppendString(dst, BytesToString(t))
		return dst, nil
	}
	// Reflection-driven path for slices, arrays, maps, pointers, and
	// structs. Recursing through AppendAny on each element keeps nested
	// ggen Marshalers, TextAppenders, etc. on their fast path instead of
	// dropping into json.Marshal's reflection.
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return AppendAny(dst, rv.Elem().Interface())
	// Named primitives (`type MyEnum int`, `type ID string`, etc.) — the
	// type switch above only matches predeclared types exactly, so these
	// cases catch the named-alias variants.
	case reflect.Bool:
		return strconv.AppendBool(dst, rv.Bool()), nil
	case reflect.String:
		dst = append(dst, '"')
		return AppendString(dst, rv.String()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(dst, rv.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.AppendUint(dst, rv.Uint(), 10), nil
	case reflect.Float32:
		return strconv.AppendFloat(dst, rv.Float(), 'g', -1, 32), nil
	case reflect.Float64:
		return strconv.AppendFloat(dst, rv.Float(), 'g', -1, 64), nil
	case reflect.Struct:
		return appendStruct(dst, rv)
	case reflect.Slice, reflect.Array:
		// `[]T` where T's underlying kind is uint8 also routes through
		// base64 — matches stdlib (e.g. `type Bytes []byte`).
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			dst = append(dst, '"')
			if rv.Kind() == reflect.Array {
				// reflect.Value.Bytes panics on arrays — copy via Slice.
				rv = rv.Slice(0, rv.Len())
			}
			dst = base64.StdEncoding.AppendEncode(dst, rv.Bytes())
			return append(dst, '"'), nil
		}
		dst = append(dst, '[')
		for i := range rv.Len() {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = AppendAny(dst, rv.Index(i).Interface())
			if err != nil {
				return dst, err
			}
		}
		return append(dst, ']'), nil
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return dst, &json.UnsupportedTypeError{Type: rv.Type()}
		}
		dst = append(dst, '{')
		iter := rv.MapRange()
		first := true
		for iter.Next() {
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = append(dst, '"')
			dst = AppendString(dst, iter.Key().String())
			dst = append(dst, ':')
			var err error
			dst, err = AppendAny(dst, iter.Value().Interface())
			if err != nil {
				return dst, err
			}
		}
		return append(dst, '}'), nil
	}
	return dst, &json.UnsupportedTypeError{Type: rv.Type()}
}

// fieldInfo describes one JSON-visible field of a struct type, with its
// emit-side flags pre-parsed at type-info build time.
type fieldInfo struct {
	name      string
	index     []int
	omitEmpty bool
	omitZero  bool
	quoted    bool // ,string — wrap primitive value in JSON string
	inline    bool // ,inline — splice map[string]any keys at parent level
}

// structInfo is the cached, flattened field list for a struct type.
// Anonymous embedded structs are walked at build time so emit-side has
// no recursion-into-embeds branch.
type structInfo struct {
	fields []fieldInfo
}

var structInfoCache sync.Map // map[reflect.Type]*structInfo

func cachedStructInfo(t reflect.Type) *structInfo {
	if v, ok := structInfoCache.Load(t); ok {
		return v.(*structInfo)
	}
	info := &structInfo{}
	collectFields(info, t, nil)
	structInfoCache.Store(t, info)
	return info
}

func collectFields(info *structInfo, t reflect.Type, parentIndex []int) {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		idx := append(append([]int(nil), parentIndex...), i)
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		// Anonymous embedded struct (or *struct) with no explicit JSON
		// name: promote its fields up to the parent.
		if sf.Anonymous && name == "" {
			ft := sf.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collectFields(info, ft, idx)
				continue
			}
		}
		if !sf.IsExported() {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		info.fields = append(info.fields, fieldInfo{
			name:      name,
			index:     idx,
			omitEmpty: hasTagOpt(opts, "omitempty"),
			omitZero:  hasTagOpt(opts, "omitzero"),
			quoted:    hasTagOpt(opts, "string"),
			inline:    hasTagOpt(opts, "inline"),
		})
	}
}

func hasTagOpt(opts, want string) bool {
	for opts != "" {
		var o string
		o, opts, _ = strings.Cut(opts, ",")
		if o == want {
			return true
		}
	}
	return false
}

// isJSONEmpty mirrors stdlib's "omitempty" predicate.
func isJSONEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

func appendStruct(dst []byte, rv reflect.Value) ([]byte, error) {
	info := cachedStructInfo(rv.Type())
	dst = append(dst, '{')
	first := true
	for _, f := range info.fields {
		fv := rv.FieldByIndex(f.index)
		if f.omitEmpty && isJSONEmpty(fv) {
			continue
		}
		if f.omitZero && fv.IsZero() {
			continue
		}
		// ,inline catch-all: splice map[string]V entries at this level.
		if f.inline && fv.Kind() == reflect.Map && fv.Type().Key().Kind() == reflect.String {
			iter := fv.MapRange()
			for iter.Next() {
				if !first {
					dst = append(dst, ',')
				}
				first = false
				dst = append(dst, '"')
				dst = AppendString(dst, iter.Key().String())
				dst = append(dst, ':')
				var err error
				dst, err = AppendAny(dst, iter.Value().Interface())
				if err != nil {
					return dst, err
				}
			}
			continue
		}
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = append(dst, '"')
		dst = AppendString(dst, f.name)
		dst = append(dst, ':')
		if f.quoted {
			dst = append(dst, '"')
		}
		var err error
		dst, err = AppendAny(dst, fv.Interface())
		if err != nil {
			return dst, err
		}
		if f.quoted {
			dst = append(dst, '"')
		}
	}
	return append(dst, '}'), nil
}

// AppendStringNoHTML is the no-HTML-escape counterpart of AppendString:
// emits <, >, & literally and only applies the standard JSON escapes.
// Same caller contract: opening quote is the caller's responsibility.
func AppendStringNoHTML(dst []byte, s string) []byte {
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		if start < i {
			dst = append(dst, s[start:i]...)
		}
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		default:
			const hex = "0123456789abcdef"
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		}
		start = i + 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

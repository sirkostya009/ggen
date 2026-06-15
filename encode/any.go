package encode

import (
	"encoding"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
//
// String escaping follows the package default (jsonv2 shape — <, >, &
// literal), matching sibling generated string fields. AppendAnyHTML is
// the HTML-safe variant for `htmlescape` structs.
func AppendAny(dst []byte, v any) ([]byte, error) {
	return appendAny(dst, v, AppendStringNoHTML)
}

// AppendAnyHTML is AppendAny with HTML-safe string escaping (<, >, & →
// \uXXXX, stdlib v1 shape). Generated code routes `any` fields here when
// the struct opts in via `htmlescape` / `-htmlescape`.
func AppendAnyHTML(dst []byte, v any) ([]byte, error) {
	return appendAny(dst, v, AppendString)
}

// escapeFn is AppendString or AppendStringNoHTML, threaded through the
// whole any-walk so nested strings and map keys escape consistently.
type escapeFn = func([]byte, string) []byte

func appendAny(dst []byte, v any, esc escapeFn) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(dst, 'n', 'u', 'l', 'l'), nil
	case bool:
		return strconv.AppendBool(dst, x), nil
	case string:
		dst = append(dst, '"')
		return esc(dst, x), nil
	case float64:
		return AppendFloat(dst, x, 64)
	case float32:
		return AppendFloat(dst, float64(x), 32)
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
	// Concrete container types — type-assert before reflect to avoid the
	// reflect.MapIter / Value boxing that allocates per entry. These
	// shapes (`[]any`, `map[string]any`, plus string→string and []string
	// which are common metadata-bag shapes) dominate the alloc count
	// when an `any` field holds a nested collection.
	case []any:
		dst = append(dst, '[')
		for i, e := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendAny(dst, e, esc)
			if err != nil {
				return dst, err
			}
		}
		return append(dst, ']'), nil
	case []string:
		dst = append(dst, '[')
		for i, s := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, '"')
			dst = esc(dst, s)
		}
		return append(dst, ']'), nil
	// Homogeneous primitive slices. The reflect.Slice path would box
	// the element type for every iteration via reflect.Value; native
	// range over the typed slice is one strconv call per element with
	// no per-element alloc.
	case []int:
		return appendSliceInt(dst, x), nil
	case []int8:
		return appendSliceInt(dst, x), nil
	case []int16:
		return appendSliceInt(dst, x), nil
	case []int32:
		return appendSliceInt(dst, x), nil
	case []int64:
		return appendSliceInt(dst, x), nil
	case []uint:
		return appendSliceUint(dst, x), nil
	// Skip `case []uint8`: that's `[]byte`, which routes through the
	// base64 path in reflect.Slice further down — keep the wire shape.
	case []uint16:
		return appendSliceUint(dst, x), nil
	case []uint32:
		return appendSliceUint(dst, x), nil
	case []uint64:
		return appendSliceUint(dst, x), nil
	case []float32:
		return appendSliceFloat(dst, x, 32)
	case []float64:
		return appendSliceFloat(dst, x, 64)
	case []bool:
		return appendSliceBool(dst, x), nil
	case map[string]any:
		dst = append(dst, '{')
		first := true
		for k, val := range x {
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = append(dst, '"')
			dst = esc(dst, k)
			dst = append(dst, ':')
			var err error
			dst, err = appendAny(dst, val, esc)
			if err != nil {
				return dst, err
			}
		}
		return append(dst, '}'), nil
	case map[string]string:
		dst = append(dst, '{')
		first := true
		for k, val := range x {
			if !first {
				dst = append(dst, ',')
			}
			first = false
			// AppendString writes <body>"<closing-quote> — caller writes
			// the opening `"`. So key emits as `"k"` then we append
			// `:"` to start the value, AppendString closes value with `"`.
			dst = append(dst, '"')
			dst = esc(dst, k)
			dst = append(dst, ':', '"')
			dst = esc(dst, val)
		}
		return append(dst, '}'), nil
	// Homogeneous primitive maps. Same rationale as the typed-slice
	// cases above: reflect.MapIter on a map[string]V allocates per
	// iteration in practice (key+value boxing, even when V is a
	// primitive); native range yields zero-alloc per entry.
	case map[string]int:
		return appendMapInt(dst, x, esc), nil
	case map[string]int8:
		return appendMapInt(dst, x, esc), nil
	case map[string]int16:
		return appendMapInt(dst, x, esc), nil
	case map[string]int32:
		return appendMapInt(dst, x, esc), nil
	case map[string]int64:
		return appendMapInt(dst, x, esc), nil
	case map[string]uint:
		return appendMapUint(dst, x, esc), nil
	case map[string]uint8:
		return appendMapUint(dst, x, esc), nil
	case map[string]uint16:
		return appendMapUint(dst, x, esc), nil
	case map[string]uint32:
		return appendMapUint(dst, x, esc), nil
	case map[string]uint64:
		return appendMapUint(dst, x, esc), nil
	case map[string]float32:
		return appendMapFloat(dst, x, 32, esc)
	case map[string]float64:
		return appendMapFloat(dst, x, 64, esc)
	case map[string]bool:
		return appendMapBool(dst, x, esc), nil
	// Pre-empt the json.Marshaler / TextAppender interface dispatches
	// for two common stdlib types. Both implement json.Marshaler so
	// they'd otherwise hit `case json.Marshaler` and pay the
	// `MarshalJSON() ([]byte, error)` alloc; the concrete cases write
	// straight into dst with no intermediate buffer.
	case json.RawMessage:
		// json.RawMessage is `type RawMessage []byte`. Nil / empty
		// becomes `null` (matches encoding/json v1 behavior); otherwise
		// the bytes are assumed valid JSON and pass through verbatim.
		if len(x) == 0 {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return append(dst, x...), nil
	case time.Time:
		// Use AppendText (Go 1.24+ TextAppender) — same RFC3339Nano
		// wire shape as MarshalJSON, no intermediate alloc.
		return appendTime(dst, x)
	case *time.Time:
		if x == nil {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return appendTime(dst, *x)
	// Pointer-to-primitive shortcuts. The reflect.Pointer path below
	// derefs via rv.Elem().Interface() which boxes the pointee — one
	// alloc per pointer dispatch. Concrete cases skip that. nil → null.
	case *string:
		if x == nil {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		dst = append(dst, '"')
		return esc(dst, *x), nil
	case *bool:
		if x == nil {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return strconv.AppendBool(dst, *x), nil
	case *int:
		return appendPtrInt(dst, x), nil
	case *int8:
		return appendPtrInt(dst, x), nil
	case *int16:
		return appendPtrInt(dst, x), nil
	case *int32:
		return appendPtrInt(dst, x), nil
	case *int64:
		return appendPtrInt(dst, x), nil
	case *uint:
		return appendPtrUint(dst, x), nil
	case *uint8:
		return appendPtrUint(dst, x), nil
	case *uint16:
		return appendPtrUint(dst, x), nil
	case *uint32:
		return appendPtrUint(dst, x), nil
	case *uint64:
		return appendPtrUint(dst, x), nil
	case *float32:
		if x == nil {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return AppendFloat(dst, float64(*x), 32)
	case *float64:
		if x == nil {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return AppendFloat(dst, *x, 64)
	// Interfaces, in cross-pkg-dispatch priority order. Text encoders
	// outrank json.Marshaler so types that implement both route via
	// AppendText (zero alloc, writes straight into dst) instead of
	// paying the `MarshalJSON() ([]byte, error)` heap alloc. Types
	// whose MarshalJSON shape differs from `"<AppendText body>"`
	// must be pre-empted with a concrete case above this block (see
	// `case time.Time:`).
	case Marshaler:
		return x.AppendJSON(dst)
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
		dst = esc(dst, BytesToString(t))
		return dst, nil
	case json.Marshaler:
		b, err := x.MarshalJSON()
		if err != nil {
			return dst, err
		}
		return append(dst, b...), nil
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
		return appendAny(dst, rv.Elem().Interface(), esc)
	// Named primitives (`type MyEnum int`, `type ID string`, etc.) — the
	// type switch above only matches predeclared types exactly, so these
	// cases catch the named-alias variants.
	case reflect.Bool:
		return strconv.AppendBool(dst, rv.Bool()), nil
	case reflect.String:
		dst = append(dst, '"')
		return esc(dst, rv.String()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(dst, rv.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.AppendUint(dst, rv.Uint(), 10), nil
	case reflect.Float32:
		return AppendFloat(dst, rv.Float(), 32)
	case reflect.Float64:
		return AppendFloat(dst, rv.Float(), 64)
	case reflect.Struct:
		return appendStruct(dst, rv, esc)
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
		elemKind := rv.Type().Elem().Kind()
		for i := range rv.Len() {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendReflectValue(dst, rv.Index(i), elemKind, esc)
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
		elemKind := rv.Type().Elem().Kind()
		iter := rv.MapRange()
		first := true
		for iter.Next() {
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = append(dst, '"')
			dst = esc(dst, iter.Key().String())
			dst = append(dst, ':')
			var err error
			dst, err = appendReflectValue(dst, iter.Value(), elemKind, esc)
			if err != nil {
				return dst, err
			}
		}
		return append(dst, '}'), nil
	}
	return dst, &json.UnsupportedTypeError{Type: rv.Type()}
}

// Primitive slice/map fast-path helpers, parameterized by element type
// so all int / uint / float sizes share one body. Reached from the
// concrete-type cases in AppendAny — never via reflect.

func appendSliceInt[V int | int8 | int16 | int32 | int64](dst []byte, s []V) []byte {
	dst = append(dst, '[')
	for i, v := range s {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = strconv.AppendInt(dst, int64(v), 10)
	}
	return append(dst, ']')
}

func appendSliceUint[V uint | uint16 | uint32 | uint64](dst []byte, s []V) []byte {
	dst = append(dst, '[')
	for i, v := range s {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = strconv.AppendUint(dst, uint64(v), 10)
	}
	return append(dst, ']')
}

func appendSliceFloat[V float32 | float64](dst []byte, s []V, bitSize int) ([]byte, error) {
	dst = append(dst, '[')
	for i, v := range s {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		dst, err = AppendFloat(dst, float64(v), bitSize)
		if err != nil {
			return dst, err
		}
	}
	return append(dst, ']'), nil
}

func appendSliceBool(dst []byte, s []bool) []byte {
	dst = append(dst, '[')
	for i, v := range s {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = strconv.AppendBool(dst, v)
	}
	return append(dst, ']')
}

func appendMapInt[V int | int8 | int16 | int32 | int64](dst []byte, m map[string]V, esc escapeFn) []byte {
	dst = append(dst, '{')
	first := true
	for k, v := range m {
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = append(dst, '"')
		dst = esc(dst, k)
		dst = append(dst, ':')
		dst = strconv.AppendInt(dst, int64(v), 10)
	}
	return append(dst, '}')
}

func appendMapUint[V uint | uint8 | uint16 | uint32 | uint64](dst []byte, m map[string]V, esc escapeFn) []byte {
	dst = append(dst, '{')
	first := true
	for k, v := range m {
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = append(dst, '"')
		dst = esc(dst, k)
		dst = append(dst, ':')
		dst = strconv.AppendUint(dst, uint64(v), 10)
	}
	return append(dst, '}')
}

func appendMapFloat[V float32 | float64](dst []byte, m map[string]V, bitSize int, esc escapeFn) ([]byte, error) {
	dst = append(dst, '{')
	first := true
	for k, v := range m {
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = append(dst, '"')
		dst = esc(dst, k)
		dst = append(dst, ':')
		var err error
		dst, err = AppendFloat(dst, float64(v), bitSize)
		if err != nil {
			return dst, err
		}
	}
	return append(dst, '}'), nil
}

func appendMapBool(dst []byte, m map[string]bool, esc escapeFn) []byte {
	dst = append(dst, '{')
	first := true
	for k, v := range m {
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = append(dst, '"')
		dst = esc(dst, k)
		dst = append(dst, ':')
		dst = strconv.AppendBool(dst, v)
	}
	return append(dst, '}')
}

func appendTime(dst []byte, t time.Time) ([]byte, error) {
	dst = append(dst, '"')
	dst, err := t.AppendText(dst)
	if err != nil {
		return dst, err
	}
	return append(dst, '"'), nil
}

func appendPtrInt[V int | int8 | int16 | int32 | int64](dst []byte, p *V) []byte {
	if p == nil {
		return append(dst, 'n', 'u', 'l', 'l')
	}
	return strconv.AppendInt(dst, int64(*p), 10)
}

func appendPtrUint[V uint | uint8 | uint16 | uint32 | uint64](dst []byte, p *V) []byte {
	if p == nil {
		return append(dst, 'n', 'u', 'l', 'l')
	}
	return strconv.AppendUint(dst, uint64(*p), 10)
}

// appendReflectValue emits rv to dst when the value's Kind is already
// known to the caller. Fast-paths primitive kinds (string/bool/numeric)
// by reading directly from the reflect.Value without going through the
// interface-boxing detour `rv.Interface()` would take — `reflect.Value
// .Interface()` allocates a fresh interface header for every value,
// which is the dominant alloc cost when iterating string→string maps,
// []int slices, etc.
//
// Non-primitive kinds fall back to AppendAny via the Interface() path —
// they need full dispatch (Marshaler / TextAppender / nested any).
func appendReflectValue(dst []byte, rv reflect.Value, kind reflect.Kind, esc escapeFn) ([]byte, error) {
	switch kind {
	case reflect.String:
		dst = append(dst, '"')
		return esc(dst, rv.String()), nil
	case reflect.Bool:
		return strconv.AppendBool(dst, rv.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(dst, rv.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.AppendUint(dst, rv.Uint(), 10), nil
	case reflect.Float32:
		return AppendFloat(dst, rv.Float(), 32)
	case reflect.Float64:
		return AppendFloat(dst, rv.Float(), 64)
	}
	return appendAny(dst, rv.Interface(), esc)
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

func appendStruct(dst []byte, rv reflect.Value, esc escapeFn) ([]byte, error) {
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
				dst = esc(dst, iter.Key().String())
				dst = append(dst, ':')
				var err error
				dst, err = appendAny(dst, iter.Value().Interface(), esc)
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
		dst = esc(dst, f.name)
		dst = append(dst, ':')
		if f.quoted {
			dst = append(dst, '"')
		}
		var err error
		dst, err = appendAny(dst, fv.Interface(), esc)
		if err != nil {
			return dst, err
		}
		if f.quoted {
			dst = append(dst, '"')
		}
	}
	return append(dst, '}'), nil
}

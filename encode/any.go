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

// AppendAny marshals an `any` value into dst as JSON. Struct fields honor
// `json:"name,omitempty,omitzero,string,inline"` tags; anonymous embedded
// fields are promoted at parent level. Strings escape without HTML-safety
// (<, >, & literal); AppendAnyHTML is the HTML-safe variant.
func AppendAny(dst []byte, v any) ([]byte, error) {
	return appendAny(dst, v, AppendStringNoHTML)
}

// AppendAnyHTML is AppendAny with HTML-safe string escaping (<, >, & →
// \uXXXX).
func AppendAnyHTML(dst []byte, v any) ([]byte, error) {
	return appendAny(dst, v, AppendString)
}

// escapeFn (AppendString or AppendStringNoHTML) is threaded through the
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
		// Zero value → 0 (v1 parity; the raw append emitted zero bytes —
		// {"n":} from an unset field). Non-empty content is assumed a valid
		// numeric literal and passes verbatim, same trust as RawMessage.
		if len(x) == 0 {
			return append(dst, '0'), nil
		}
		return append(dst, x...), nil
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
	// No `case []uint8`: that's `[]byte`, routed through the base64
	// reflect.Slice path below.
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
	case []time.Time:
		dst = append(dst, '[')
		for i := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			if dst, err = appendTime(dst, x[i]); err != nil {
				return dst, err
			}
		}
		return append(dst, ']'), nil
	case []json.RawMessage:
		dst = append(dst, '[')
		for i, r := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			if len(r) == 0 {
				dst = append(dst, 'n', 'u', 'l', 'l')
			} else {
				dst = append(dst, r...)
			}
		}
		return append(dst, ']'), nil
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
			dst = append(dst, '"')
			dst = esc(dst, k)
			dst = append(dst, ':', '"')
			dst = esc(dst, val)
		}
		return append(dst, '}'), nil
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
	// Concrete stdlib cases must sit before the json.Marshaler dispatch.
	case json.RawMessage:
		// Nil/empty → null; else assumed-valid JSON verbatim.
		if len(x) == 0 {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return append(dst, x...), nil
	case time.Time:
		return appendTime(dst, x)
	case *time.Time:
		if x == nil {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return appendTime(dst, *x)
	// Pointer-to-primitive shortcuts. nil → null.
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
	// Interface dispatch, priority order. Text encoders outrank
	// json.Marshaler; a type whose MarshalJSON shape differs from
	// `"<AppendText body>"` needs a concrete case above (see time.Time).
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
	// Reflection path for types the type switch above doesn't match.
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return appendAny(dst, rv.Elem().Interface(), esc)
	// Named primitives (`type MyEnum int`, …) land here — the type switch
	// matches only predeclared types exactly.
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
		// uint8-elem slices/arrays marshal as base64 (e.g. `type Bytes []byte`).
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			dst = append(dst, '"')
			if rv.Kind() == reflect.Array {
				// The array came through reflect.ValueOf, so it's
				// unaddressable — Bytes AND Slice both panic; copy out.
				b := make([]byte, rv.Len())
				reflect.Copy(reflect.ValueOf(b), rv)
				dst = base64.StdEncoding.AppendEncode(dst, b)
				return append(dst, '"'), nil
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
		// Reused addressable scratch — SetIterKey/SetIterValue copy into these
		// instead of allocating a fresh reflect.Value per entry.
		kv := reflect.New(rv.Type().Key()).Elem()
		vv := reflect.New(rv.Type().Elem()).Elem()
		first := true
		for iter.Next() {
			if !first {
				dst = append(dst, ',')
			}
			first = false
			kv.SetIterKey(iter)
			vv.SetIterValue(iter)
			dst = append(dst, '"')
			dst = esc(dst, kv.String())
			dst = append(dst, ':')
			var err error
			dst, err = appendReflectValue(dst, vv, elemKind, esc)
			if err != nil {
				return dst, err
			}
		}
		return append(dst, '}'), nil
	}
	return dst, &json.UnsupportedTypeError{Type: rv.Type()}
}

// Primitive slice/map helpers, generic over element type so all int /
// uint / float sizes share one body.

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

// appendReflectValue emits rv to dst given its already-known Kind.
// Primitive kinds read straight off the reflect.Value; non-primitive kinds
// fall back to AppendAny via Interface().
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

// fieldInfo describes one JSON-visible field of a struct type.
type fieldInfo struct {
	name      string
	index     []int
	tagged    bool // name came from an explicit json tag
	omitEmpty bool
	omitZero  bool
	quoted    bool // ,string — wrap primitive value in JSON string
	inline    bool // ,inline — splice map[string]any keys at parent level
}

// structInfo is the cached, flattened field list for a struct type.
// Anonymous embedded structs are flattened in at build time.
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
	info.fields = resolveFieldConflicts(info.fields)
	structInfoCache.Store(t, info)
	return info
}

// resolveFieldConflicts applies stdlib's dominant-field rules to the
// flattened set: per JSON name the shallowest field wins; at equal depth a
// single tagged field beats untagged; still-ambiguous names drop entirely.
// Without it a field shadowing an embedded field emitted duplicate keys.
func resolveFieldConflicts(fields []fieldInfo) []fieldInfo {
	byName := make(map[string][]int, len(fields))
	for i, f := range fields {
		byName[f.name] = append(byName[f.name], i)
	}
	if len(byName) == len(fields) {
		return fields
	}
	drop := make(map[int]bool)
	for _, idxs := range byName {
		if len(idxs) == 1 {
			continue
		}
		minD := len(fields[idxs[0]].index)
		for _, i := range idxs[1:] {
			minD = min(minD, len(fields[i].index))
		}
		winner := -1
		taggedAtMin := -1
		nShallow, nTagged := 0, 0
		for _, i := range idxs {
			if len(fields[i].index) != minD {
				continue
			}
			nShallow++
			winner = i
			if fields[i].tagged {
				nTagged++
				taggedAtMin = i
			}
		}
		if nShallow > 1 {
			winner = -1
			if nTagged == 1 {
				winner = taggedAtMin
			}
		}
		for _, i := range idxs {
			if i != winner {
				drop[i] = true
			}
		}
	}
	out := fields[:0]
	for i, f := range fields {
		if !drop[i] {
			out = append(out, f)
		}
	}
	return out
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
		tagged := name != ""
		if !tagged {
			name = sf.Name
		}
		info.fields = append(info.fields, fieldInfo{
			name:      name,
			index:     idx,
			tagged:    tagged,
			omitEmpty: hasTagOpt(opts, "omitempty"),
			omitZero:  hasTagOpt(opts, "omitzero"),
			quoted:    hasTagOpt(opts, "string") && quotableKind(sf.Type),
			inline:    hasTagOpt(opts, "inline"),
		})
	}
}

// quotableKind gates `,string`: numeric kinds only (through one pointer
// level), matching generated code and jsonv2 — a bare quote wrap around a
// string or composite emits invalid JSON, and jsonv2 dropped the v1 bool
// stringification.
func quotableKind(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
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
		var fv reflect.Value
		if len(f.index) == 1 {
			fv = rv.Field(f.index[0])
		} else {
			var err error
			if fv, err = rv.FieldByIndexErr(f.index); err != nil {
				// promoted through a nil embedded pointer — stdlib omits
				continue
			}
		}
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
		// nil pointer-to-number emits bare null even under ,string.
		quoted := f.quoted && !(fv.Kind() == reflect.Pointer && fv.IsNil())
		if quoted {
			dst = append(dst, '"')
		}
		var err error
		dst, err = appendAny(dst, fv.Interface(), esc)
		if err != nil {
			return dst, err
		}
		if quoted {
			dst = append(dst, '"')
		}
	}
	return append(dst, '}'), nil
}

package ggen

import (
	"encoding"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// AppendAny appends v to dst as JSON. It finds the encoding of v at run time,
// using a type switch and then reflect.
//
// Struct fields follow `json:"name,omitempty,omitzero,string,embed"` tags.
// omitempty drops a field whose value encodes as null, "", [] or {}, but never
// a struct field. omitzero uses an IsZero method when the type has one. Fields
// of anonymous embedded structs are promoted to the parent. A marshal method
// declared on *T is also called for a T value, as jsonv2 does. Strings are not
// HTML-escaped, so <, > and & stay as is. AppendAnyHTML is the HTML-safe
// variant.
//
// Nesting deeper than maxDepth returns ErrMaxDepth. A cyclic value ends the
// same way.
func AppendAny(dst []byte, v any) ([]byte, error) {
	return appendAny(dst, v, AppendStringNoHTML, 0)
}

// AppendAnyHTML is AppendAny with HTML-safe string escaping. It writes <, >
// and & as \uXXXX escapes.
func AppendAnyHTML(dst []byte, v any) ([]byte, error) {
	return appendAny(dst, v, AppendString, 0)
}

// escapeFn is AppendString or AppendStringNoHTML. It is passed down the whole
// walk, so every nested string and map key uses the same escaping.
type escapeFn = func([]byte, string) []byte

func appendAny(dst []byte, v any, esc escapeFn, depth int) ([]byte, error) {
	if depth > maxDepth {
		return dst, ErrMaxDepth
	}
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
		// The zero value writes 0, like encoding/json. Writing nothing would
		// give invalid JSON such as {"n":}. Other content is assumed to be a
		// valid number and is copied as is, the same trust as RawMessage.
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
			dst, err = appendAny(dst, e, esc, depth+1)
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
	// There is no []uint8 case. That type is []byte, which the reflect.Slice
	// path below encodes as base64.
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
			dst, err = appendAny(dst, val, esc, depth+1)
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
	// Concrete standard library types must come before the json.Marshaler
	// case.
	case json.RawMessage:
		// Nil or empty writes null. Anything else is assumed to be valid JSON
		// and copied as is.
		if len(x) == 0 {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return append(dst, x...), nil
	// big.Int has an AppendText method, so the TextAppender case below would
	// write its digits as a quoted string. But encoding/json, jsonv2 and
	// generated big.Int fields all write a bare number. This is the case
	// ordering rule from below: a type whose JSON form differs from its quoted
	// AppendText output needs a concrete case first. big.Float and big.Rat
	// need no case. Their quoted AppendText output is their JSON form, and a
	// non-pointer value reaches it through the pointer dispatch in the reflect
	// fallback.
	case big.Int:
		return x.Append(dst, 10), nil
	case *big.Int:
		if x == nil {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return x.Append(dst, 10), nil
	case time.Time:
		return appendTime(dst, x)
	case *time.Time:
		if x == nil {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return appendTime(dst, *x)
	// A Duration field defaults to `format:units`. This path writes the same
	// form, so one document never holds two forms of the same Go type.
	case time.Duration:
		dst = append(dst, '"')
		return esc(dst, x.String()), nil
	// Pointers to primitives. A nil pointer writes null.
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
	// Marshal interfaces, in priority order. Text encoders win over
	// json.Marshaler. So a type whose MarshalJSON output differs from its
	// quoted AppendText output needs a concrete case above, like time.Time.
	case Marshaler:
		if isNilPtr(v) {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return x.AppendJSON(dst)
	case encoding.TextAppender:
		if isNilPtr(v) {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		dst = append(dst, '"')
		from := len(dst)
		var err error
		dst, err = x.AppendText(dst)
		if err != nil {
			return dst, err
		}
		return closeText(dst, from, esc), nil
	case encoding.TextMarshaler:
		if isNilPtr(v) {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		t, err := x.MarshalText()
		if err != nil {
			return dst, err
		}
		dst = append(dst, '"')
		dst = esc(dst, BytesToString(t))
		return dst, nil
	case json.Marshaler:
		if isNilPtr(v) {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		b, err := x.MarshalJSON()
		if err != nil {
			return dst, err
		}
		if len(b) == 0 {
			// An empty result with a nil error would write nothing and leave
			// invalid JSON such as `{"k":`. encoding/json and jsonv2 both
			// return an error here. Only RawMessage writes null for nil, and
			// it does so in its own MarshalJSON.
			return dst, ErrEmptyMarshalJSON
		}
		return append(dst, b...), nil
	}
	// Types the type switch above does not match go through reflect.
	rv := reflect.ValueOf(v)
	// The marshaler is declared only on *T. Call it through a pointer to a
	// copy, because v is never addressable here. jsonv2 also calls such
	// methods whether or not the value is addressable.
	if needsAddr(rv.Type()) {
		return appendAny(dst, addrOf(rv), esc, depth)
	}
	//exhaustive:ignore not every kind applies here
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return appendAny(dst, rv.Elem().Interface(), esc, depth+1)
	// Named primitive types such as `type MyEnum int` end up here. The type
	// switch matches only the exact predeclared types.
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
		return appendStruct(dst, rv, esc, depth)
	case reflect.Slice, reflect.Array:
		// Slices and arrays of uint8 encode as base64, for example
		// `type Bytes []byte`.
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			dst = append(dst, '"')
			if rv.Kind() == reflect.Array {
				// The array came from reflect.ValueOf, so it is not
				// addressable, and both Bytes and Slice would panic. Copy it.
				b := make([]byte, rv.Len())
				reflect.Copy(reflect.ValueOf(b), rv)
				dst = base64.StdEncoding.AppendEncode(dst, b)
				return append(dst, '"'), nil
			}
			dst = base64.StdEncoding.AppendEncode(dst, rv.Bytes())
			return append(dst, '"'), nil
		}
		dst = append(dst, '[')
		et := rv.Type().Elem()
		elemKind, elemAddr := et.Kind(), needsAddr(et)
		for i := range rv.Len() {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendReflectValue(dst, rv.Index(i), elemKind, elemAddr, esc, depth+1)
			if err != nil {
				return dst, err
			}
		}
		return append(dst, ']'), nil
	case reflect.Map:
		kt, et := rv.Type().Key(), rv.Type().Elem()
		// Reusable addressable values. SetIterKey and SetIterValue copy each
		// entry into them, so no reflect.Value is allocated per entry.
		kv := reflect.New(kt).Elem()
		vv := reflect.New(et).Elem()
		// If the key type has a text marshaler, it produces the key strings.
		// Checking *K finds methods with either receiver, and kv holds every
		// key, so its address works.
		var keyApp encoding.TextAppender
		var keyMar encoding.TextMarshaler
		if kt.PkgPath() != "" {
			switch kp := kv.Addr().Interface().(type) {
			case encoding.TextAppender:
				keyApp = kp
			case encoding.TextMarshaler:
				keyMar = kp
			}
		}
		if keyApp == nil && keyMar == nil && kt.Kind() != reflect.String {
			return dst, &json.UnsupportedTypeError{Type: rv.Type()}
		}
		dst = append(dst, '{')
		elemKind, elemAddr := et.Kind(), needsAddr(et)
		iter := rv.MapRange()
		first := true
		for iter.Next() {
			if !first {
				dst = append(dst, ',')
			}
			first = false
			kv.SetIterKey(iter)
			vv.SetIterValue(iter)
			dst = append(dst, '"')
			var err error
			switch {
			case keyApp != nil:
				from := len(dst)
				if dst, err = keyApp.AppendText(dst); err != nil {
					return dst, err
				}
				dst = closeText(dst, from, esc)
			case keyMar != nil:
				var t []byte
				if t, err = keyMar.MarshalText(); err != nil {
					return dst, err
				}
				dst = esc(dst, BytesToString(t))
			default:
				dst = esc(dst, kv.String())
			}
			dst = append(dst, ':')
			dst, err = appendReflectValue(dst, vv, elemKind, elemAddr, esc, depth+1)
			if err != nil {
				return dst, err
			}
		}
		return append(dst, '}'), nil
	}
	return dst, &json.UnsupportedTypeError{Type: rv.Type()}
}

// Helpers for slices and maps of primitives. They are generic over the
// element type, so every int, uint and float size shares one body.

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

// appendReflectValue appends rv to dst. The caller passes the kind of rv and
// addr, which is needsAddr of the element type, both computed once per
// container. Unnamed primitive kinds are read straight from the
// reflect.Value. Every other value goes back through appendAny via
// Interface(). Named primitive types take that route too, because their
// marshalers live in appendAny's type switch. Examples are json.Number, which
// is written unquoted, and a MarshalJSON or AppendText method on
// `type Level int`.
func appendReflectValue(dst []byte, rv reflect.Value, kind reflect.Kind, addr bool, esc escapeFn, depth int) ([]byte, error) {
	if addr {
		return appendAny(dst, addrOf(rv), esc, depth)
	}
	if t := rv.Type(); t.PkgPath() != "" {
		return appendAny(dst, rv.Interface(), esc, depth)
	}
	//exhaustive:ignore not every kind applies here
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
	return appendAny(dst, rv.Interface(), esc, depth)
}

// fieldInfo describes one JSON-visible field of a struct type.
type fieldInfo struct {
	name      string
	index     []int
	tagged    bool // the name comes from an explicit json tag
	omitEmpty bool
	omitZero  bool
	zero      zeroMode
	quoted    bool // ,string: wrap the number in a JSON string
	embed     bool // ,embed: write map[string]V entries as parent members
	addr      bool // the marshaler is only on *T, so pass a field pointer
}

// zeroMode says how omitzero decides whether a field is zero. As in jsonv2,
// an IsZero method on the type, or on the value it holds, wins over the plain
// reflect.Value.IsZero test.
type zeroMode uint8

const (
	zeroStructural zeroMode = iota
	zeroMethod              // T has IsZero. A nil pointer or interface is zero.
	zeroAddrMethod          // only *T implements IsZero
)

type isZeroer interface{ IsZero() bool }

func (f *fieldInfo) isZero(fv reflect.Value) bool {
	//exhaustive:ignore not every kind applies here
	switch f.zero {
	case zeroMethod:
		switch fv.Kind() {
		case reflect.Pointer:
			if fv.IsNil() {
				return true
			}
		case reflect.Interface:
			if fv.IsNil() || (fv.Elem().Kind() == reflect.Pointer && fv.Elem().IsNil()) {
				return true
			}
		default:
			// *T has T's methods, and boxing a pointer avoids copying the
			// value.
			if fv.CanAddr() {
				return fv.Addr().Interface().(isZeroer).IsZero()
			}
		}
		return fv.Interface().(isZeroer).IsZero()
	case zeroAddrMethod:
		return addrOf(fv).(isZeroer).IsZero()
	}
	return fv.IsZero()
}

// structInfo is the cached field list of a struct type. Fields of anonymous
// embedded structs are included directly when the list is built.
type structInfo struct {
	fields   []fieldInfo
	hasEmbed bool // a field has ,embed, so run the second pass for it
}

var structInfoCache sync.Map // map[reflect.Type]*structInfo

func cachedStructInfo(t reflect.Type) *structInfo {
	if v, ok := structInfoCache.Load(t); ok {
		return v.(*structInfo)
	}
	info := &structInfo{}
	collectFields(info, t, nil, map[reflect.Type]struct{}{t: {}})
	info.fields = resolveFieldConflicts(info.fields)
	for i := range info.fields {
		info.hasEmbed = info.hasEmbed || info.fields[i].embed
	}
	structInfoCache.Store(t, info)
	return info
}

// resolveFieldConflicts applies the encoding/json rules for fields that share
// a JSON name. The least deeply embedded field wins. At equal depth, a single
// tagged field beats untagged ones. If that still leaves a tie, all fields
// with that name are dropped. This keeps a field that shadows an embedded
// field from producing duplicate keys.
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

// collectFields appends the JSON-visible fields of t to info. seen holds the
// chain of embedded types from the root down to t. Recursing into a type
// already on the chain would never end, and encoding/json breaks the cycle
// the same way. seen works as a stack: a type is removed after its recursion
// returns. So a type embedded twice through different paths still shows up
// twice, and resolveFieldConflicts decides between the copies.
func collectFields(info *structInfo, t reflect.Type, parentIndex []int, seen map[reflect.Type]struct{}) {
	for i := range t.NumField() {
		sf := t.Field(i)
		idx := append(append([]int(nil), parentIndex...), i)
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		// An anonymous embedded struct or *struct with no JSON name in its
		// tag: promote its fields to the parent.
		if sf.Anonymous && name == "" {
			ft := sf.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				if _, cyclic := seen[ft]; !cyclic {
					seen[ft] = struct{}{}
					collectFields(info, ft, idx, seen)
					delete(seen, ft)
				}
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
		f := fieldInfo{
			name:      name,
			index:     idx,
			tagged:    tagged,
			omitEmpty: hasTagOpt(opts, "omitempty"),
			omitZero:  hasTagOpt(opts, "omitzero"),
			quoted:    hasTagOpt(opts, "string") && quotableKind(sf.Type),
			embed:     hasTagOpt(opts, "embed"),
			addr:      needsAddr(sf.Type),
		}
		if f.omitZero {
			switch {
			case sf.Type.Implements(isZeroerType):
				f.zero = zeroMethod
			case reflect.PointerTo(sf.Type).Implements(isZeroerType):
				f.zero = zeroAddrMethod
			}
		}
		info.fields = append(info.fields, f)
	}
}

var (
	marshalerType     = reflect.TypeFor[Marshaler]()
	textAppenderType  = reflect.TypeFor[encoding.TextAppender]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	isZeroerType      = reflect.TypeFor[isZeroer]()
)

// hasMarshaler reports whether t implements one of the four marshal
// interfaces appendAny checks.
func hasMarshaler(t reflect.Type) bool {
	return t.Implements(marshalerType) || t.Implements(textAppenderType) ||
		t.Implements(textMarshalerType) || t.Implements(jsonMarshalerType)
}

var needsAddrCache sync.Map // map[reflect.Type]bool

// needsAddr reports whether *t has a marshaler that t lacks. A t value must
// then be marshaled through a pointer to reach it. jsonv2 calls
// pointer-receiver methods whether or not the value is addressable, and
// encoding/json skips them on non-addressable values. Unnamed types have no
// methods, so they skip the cache.
func needsAddr(t reflect.Type) bool {
	if t.PkgPath() == "" {
		return false
	}
	if v, ok := needsAddrCache.Load(t); ok {
		return v.(bool)
	}
	need := !hasMarshaler(t) && hasMarshaler(reflect.PointerTo(t))
	needsAddrCache.Store(t, need)
	return need
}

// addrOf returns a pointer to rv as an any. If rv is not addressable, it
// copies rv into new memory first.
func addrOf(rv reflect.Value) any {
	if rv.CanAddr() {
		return rv.Addr().Interface()
	}
	p := reflect.New(rv.Type())
	p.Elem().Set(rv)
	return p.Interface()
}

// quotableKind reports whether `,string` applies to t. Only numeric kinds
// qualify, also behind one pointer, as in generated code and jsonv2. Wrapping
// a string or a composite value in quotes would give invalid JSON. jsonv2
// does not quote bools under `,string`.
func quotableKind(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// json.Number is a named string that encodes as a number. So
	// encoding/json and jsonv2 both quote it under `,string`. It is the only
	// string kind that qualifies. time.Duration is the reverse case: an int64
	// that encodes as a string.
	switch t {
	case numberType:
		return true
	case durationType:
		return false
	}
	//exhaustive:ignore not every kind applies here
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

var (
	numberType   = reflect.TypeFor[json.Number]()
	durationType = reflect.TypeFor[time.Duration]()
)

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

// emptyWire reports whether an encoded value is one that omitempty drops:
// null, "", {} or []. Any other two-byte value is a number, and null is the
// only four-byte value that starts with 'n'.
func emptyWire(b []byte) bool {
	switch len(b) {
	case 2:
		return b[0] == '"' || b[0] == '{' || b[0] == '['
	case 4:
		return b[0] == 'n'
	}
	return false
}

// structWire reports whether v encodes as a struct's own members. omitempty
// never drops a struct field, and the generator rejects the option on one. So
// a struct whose members are all omitted is still written as {}. Pointers and
// interfaces are followed. A nil one is not a struct, so omitempty can still
// drop it.
func structWire(v reflect.Value) bool {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	return v.Kind() == reflect.Struct
}

// fieldValue returns the value of f in rv. ok is false when omitzero drops
// the field, or when the field cannot be reached because it is promoted
// through a nil embedded pointer. encoding/json omits such fields too.
func fieldValue(rv reflect.Value, f *fieldInfo) (reflect.Value, bool) {
	var fv reflect.Value
	if len(f.index) == 1 {
		fv = rv.Field(f.index[0])
	} else {
		var err error
		if fv, err = rv.FieldByIndexErr(f.index); err != nil {
			return fv, false
		}
	}
	if f.omitZero && f.isZero(fv) {
		return fv, false
	}
	return fv, true
}

// embedSplices reports whether f's ,embed option applies to fv. Only a map
// with string keys has its entries written as parent members. Any other type
// is written as an ordinary member.
func embedSplices(f *fieldInfo, fv reflect.Value) bool {
	return f.embed && fv.Kind() == reflect.Map && fv.Type().Key().Kind() == reflect.String
}

func appendStruct(dst []byte, rv reflect.Value, esc escapeFn, depth int) ([]byte, error) {
	info := cachedStructInfo(rv.Type())
	dst = append(dst, '{')
	first := true
	for i := range info.fields {
		f := &info.fields[i]
		fv, ok := fieldValue(rv, f)
		if !ok {
			continue
		}
		if embedSplices(f, fv) {
			continue // written after the named members
		}
		// omitempty is decided on the encoded bytes. The member is written,
		// then removed again if its value turned out empty.
		mark := len(dst)
		if !first {
			dst = append(dst, ',')
		}
		dst = append(dst, '"')
		dst = esc(dst, f.name)
		dst = append(dst, ':')
		// A nil pointer to a number writes a bare null, even with ,string.
		quoted := f.quoted && (fv.Kind() != reflect.Pointer || !fv.IsNil())
		if quoted {
			dst = append(dst, '"')
		}
		val := len(dst)
		var v any
		if f.addr {
			v = addrOf(fv)
		} else {
			v = fv.Interface()
		}
		var err error
		dst, err = appendAny(dst, v, esc, depth+1)
		if err != nil {
			return dst, err
		}
		if quoted {
			dst = append(dst, '"')
		}
		if f.omitEmpty && emptyWire(dst[val:]) && !structWire(fv) {
			dst = dst[:mark]
			continue
		}
		first = false
	}
	if info.hasEmbed {
		// The ,embed map entries come after all named members, wherever the
		// field is declared. jsonv2 and generated code use the same order.
		for i := range info.fields {
			f := &info.fields[i]
			fv, ok := fieldValue(rv, f)
			if !ok || !embedSplices(f, fv) {
				continue
			}
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
				dst, err = appendAny(dst, iter.Value().Interface(), esc, depth+1)
				if err != nil {
					return dst, err
				}
			}
		}
	}
	return append(dst, '}'), nil
}

// closeText closes text that was appended unescaped, such as AppendText
// output, with '"'. If the text holds a byte that either escape table would
// escape, it rewrites the text through esc first. The check uses the HTML
// table because it covers both. Text that needs no escaping is left as is.
func closeText(dst []byte, from int, esc escapeFn) []byte {
	for _, c := range dst[from:] {
		if needEscapeHTML[c] {
			return esc(dst[:from], string(dst[from:]))
		}
	}
	return append(dst, '"')
}

// isNilPtr reports whether v holds a nil pointer of some type *T. The marshal
// interface cases match such a value when T has value-receiver methods,
// because *T's method set includes them. Calling the method would then
// dereference nil and panic. encoding/json writes null instead. The reflect
// fallback cannot catch this, because the type switch matches first.
//
// The fast path reads the interface's data word directly. For a boxed
// pointer the data word is the pointer itself. No non-nil value of any kind
// has a nil data word, so a non-nil data word always means "call the method".
// reflect.ValueOf was measured noticeably slower on this common path. A nil
// data word is ambiguous, though. Nil maps, funcs and chans have one too, and
// calling a value-receiver method on those is valid Go that encoding/json
// does. So the slow path checks the reflect kind and reports true only for a
// real pointer.
func isNilPtr(v any) bool {
	type iface struct{ typ, data unsafe.Pointer }
	// Read the data word of the interface. reflect cannot detect a typed nil
	// this cheaply.
	if (*iface)(unsafe.Pointer(&v)).data != nil {
		return false
	}
	return reflect.ValueOf(v).Kind() == reflect.Pointer
}

// Size budgets per kind, the same ones the generator uses for typed fields.
// So a value adds the same to JSONSize inside an `any` as in a field of its
// own type.
const (
	anySizeBool     = 5
	anySizeInt      = 20
	anySizeFloat    = 25
	anySizeTime     = 37 // quoted RFC3339Nano
	anySizeDuration = 27 // quoted "-2562047h47m16.854775808s"
)

// AnySize returns an upper bound on len(AppendAny(nil, v)), assuming the same
// escape growth for strings as JSONSize. Scalars use the generator's fixed
// budgets. Slices, maps and structs are walked. A generated value reports its
// own JSONSize. A text or JSON marshaler is run to learn its output length.
func AnySize(v any) int { return anySize(v, 2, 0) }

// AnySizeHTML is [AnySize] for [AppendAnyHTML], where one escaped byte can
// take up to 6 bytes.
func AnySizeHTML(v any) int { return anySize(v, 6, 0) }

func anySize(v any, mult, depth int) int {
	if depth > maxDepth {
		return 0
	}
	switch x := v.(type) {
	case nil:
		return 4
	case bool:
		return anySizeBool
	case string:
		return len(x)*mult + 2
	case float64, float32:
		return anySizeFloat
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return anySizeInt
	case json.Number:
		return max(len(x), 1)
	case []any:
		n := listSize(len(x), 0)
		for _, e := range x {
			n += anySize(e, mult, depth+1)
		}
		return n
	case []string:
		n := listSize(len(x), 2)
		for _, s := range x {
			n += len(s) * mult
		}
		return n
	case []int, []int8, []int16, []int32, []int64, []uint, []uint16, []uint32, []uint64:
		return listSize(reflect.ValueOf(v).Len(), anySizeInt)
	case []float32, []float64:
		return listSize(reflect.ValueOf(v).Len(), anySizeFloat)
	case []bool:
		return listSize(len(x), anySizeBool)
	case []time.Time:
		return listSize(len(x), anySizeTime)
	case []json.RawMessage:
		n := listSize(len(x), 0)
		for _, r := range x {
			n += max(len(r), 4)
		}
		return n
	case map[string]any:
		n := 2
		for k, e := range x {
			n += len(k)*mult + 4 + anySize(e, mult, depth+1)
		}
		return n
	case map[string]string:
		n := 2
		for k, e := range x {
			n += (len(k)+len(e))*mult + 6
		}
		return n
	case map[string]int:
		return mapSize(x, mult, anySizeInt)
	case map[string]int8:
		return mapSize(x, mult, anySizeInt)
	case map[string]int16:
		return mapSize(x, mult, anySizeInt)
	case map[string]int32:
		return mapSize(x, mult, anySizeInt)
	case map[string]int64:
		return mapSize(x, mult, anySizeInt)
	case map[string]uint:
		return mapSize(x, mult, anySizeInt)
	case map[string]uint8:
		return mapSize(x, mult, anySizeInt)
	case map[string]uint16:
		return mapSize(x, mult, anySizeInt)
	case map[string]uint32:
		return mapSize(x, mult, anySizeInt)
	case map[string]uint64:
		return mapSize(x, mult, anySizeInt)
	case map[string]float32:
		return mapSize(x, mult, anySizeFloat)
	case map[string]float64:
		return mapSize(x, mult, anySizeFloat)
	case map[string]bool:
		return mapSize(x, mult, anySizeBool)
	case json.RawMessage:
		return max(len(x), 4)
	case big.Int:
		return x.BitLen()*1233>>12 + 3
	case *big.Int:
		if x == nil {
			return 4
		}
		return x.BitLen()*1233>>12 + 3
	case time.Time, *time.Time:
		return anySizeTime
	case time.Duration:
		return anySizeDuration
	case *string:
		if x == nil {
			return 4
		}
		return len(*x)*mult + 2
	case *bool:
		return anySizeBool
	case *int, *int8, *int16, *int32, *int64, *uint, *uint8, *uint16, *uint32, *uint64:
		return anySizeInt
	case *float32, *float64:
		return anySizeFloat
	case Marshaler:
		if isNilPtr(v) {
			return 4
		}
		return x.JSONSize()
	case encoding.TextAppender:
		if isNilPtr(v) {
			return 4
		}
		b, err := x.AppendText(nil)
		if err != nil {
			return 0
		}
		return len(b)*mult + 2
	case encoding.TextMarshaler:
		if isNilPtr(v) {
			return 4
		}
		b, err := x.MarshalText()
		if err != nil {
			return 0
		}
		return len(b)*mult + 2
	case json.Marshaler:
		if isNilPtr(v) {
			return 4
		}
		b, err := x.MarshalJSON()
		if err != nil {
			return 0
		}
		return len(b)
	}
	rv := reflect.ValueOf(v)
	if needsAddr(rv.Type()) {
		return anySize(addrOf(rv), mult, depth)
	}
	//exhaustive:ignore not every kind applies here
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return 4
		}
		return anySize(rv.Elem().Interface(), mult, depth+1)
	case reflect.Bool:
		return anySizeBool
	case reflect.String:
		return rv.Len()*mult + 2
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return anySizeInt
	case reflect.Float32, reflect.Float64:
		return anySizeFloat
	case reflect.Struct:
		return structSize(rv, mult, depth)
	case reflect.Slice, reflect.Array:
		et := rv.Type().Elem()
		if et.Kind() == reflect.Uint8 {
			return (rv.Len()+2)/3*4 + 2
		}
		n := listSize(rv.Len(), 0)
		elemKind, elemAddr := et.Kind(), needsAddr(et)
		for i := range rv.Len() {
			n += reflectValueSize(rv.Index(i), elemKind, elemAddr, mult, depth+1)
		}
		return n
	case reflect.Map:
		return reflectMapSize(rv, mult, depth)
	}
	return 0
}

// listSize returns the size of an n-element list whose elements each take
// per bytes, including the brackets and commas.
func listSize(n, per int) int {
	return 2 + max(n-1, 0) + n*per
}

// mapSize returns the budget for a string-keyed map of fixed-size values.
// Each entry counts the escaped key, its quotes, the colon and the comma.
func mapSize[V any](m map[string]V, mult, per int) int {
	n := 2
	for k := range m {
		n += len(k)*mult + 4 + per
	}
	return n
}

// reflectValueSize is the size counterpart of appendReflectValue.
func reflectValueSize(rv reflect.Value, kind reflect.Kind, addr bool, mult, depth int) int {
	if addr {
		return anySize(addrOf(rv), mult, depth)
	}
	if rv.Type().PkgPath() != "" {
		return anySize(rv.Interface(), mult, depth)
	}
	//exhaustive:ignore not every kind applies here
	switch kind {
	case reflect.String:
		return rv.Len()*mult + 2
	case reflect.Bool:
		return anySizeBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return anySizeInt
	case reflect.Float32, reflect.Float64:
		return anySizeFloat
	}
	return anySize(rv.Interface(), mult, depth)
}

// reflectMapSize is the size counterpart of the reflect map case in
// appendAny, including key text marshalers.
func reflectMapSize(rv reflect.Value, mult, depth int) int {
	kt, et := rv.Type().Key(), rv.Type().Elem()
	kv := reflect.New(kt).Elem()
	vv := reflect.New(et).Elem()
	var keyApp encoding.TextAppender
	var keyMar encoding.TextMarshaler
	if kt.PkgPath() != "" {
		switch kp := kv.Addr().Interface().(type) {
		case encoding.TextAppender:
			keyApp = kp
		case encoding.TextMarshaler:
			keyMar = kp
		}
	}
	if keyApp == nil && keyMar == nil && kt.Kind() != reflect.String {
		return 0
	}
	n := 2
	elemKind, elemAddr := et.Kind(), needsAddr(et)
	iter := rv.MapRange()
	for iter.Next() {
		kv.SetIterKey(iter)
		vv.SetIterValue(iter)
		switch {
		case keyApp != nil:
			b, err := keyApp.AppendText(nil)
			if err != nil {
				return 0
			}
			n += len(b) * mult
		case keyMar != nil:
			b, err := keyMar.MarshalText()
			if err != nil {
				return 0
			}
			n += len(b) * mult
		default:
			n += kv.Len() * mult
		}
		n += 4 + reflectValueSize(vv, elemKind, elemAddr, mult, depth+1)
	}
	return n
}

// structSize is the size counterpart of appendStruct. It counts every member
// that omitempty might drop, because that decision needs the encoded bytes.
func structSize(rv reflect.Value, mult, depth int) int {
	info := cachedStructInfo(rv.Type())
	n := 2
	for i := range info.fields {
		f := &info.fields[i]
		fv, ok := fieldValue(rv, f)
		if !ok {
			continue
		}
		if embedSplices(f, fv) {
			kv := reflect.New(fv.Type().Key()).Elem()
			iter := fv.MapRange()
			for iter.Next() {
				kv.SetIterKey(iter)
				n += kv.Len()*mult + 4 + anySize(iter.Value().Interface(), mult, depth+1)
			}
			continue
		}
		n += len(f.name)*mult + 4
		if f.quoted {
			n += 2
		}
		var v any
		if f.addr {
			v = addrOf(fv)
		} else {
			v = fv.Interface()
		}
		n += anySize(v, mult, depth+1)
	}
	return n
}

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

// AppendAny marshals an `any` value into dst as JSON. Struct fields honor
// `json:"name,omitempty,omitzero,string,embed"` tags (omitempty drops a field
// whose value encodes as null, "" or [], and an empty map, but never a
// struct; omitzero consults an IsZero method); anonymous embedded fields are
// promoted at parent level. A marshal method declared on *T is called for a
// T value too, as jsonv2 does. Strings escape without HTML-safety (<, >, &
// literal); AppendAnyHTML is the HTML-safe variant.
//
// Nesting past maxDepth returns ErrMaxDepth, which is also what a cyclic
// value hits.
func AppendAny(dst []byte, v any) ([]byte, error) {
	return appendAny(dst, v, AppendStringNoHTML, 0)
}

// AppendAnyHTML is AppendAny with HTML-safe string escaping (<, >, & →
// \uXXXX).
func AppendAnyHTML(dst []byte, v any) ([]byte, error) {
	return appendAny(dst, v, AppendString, 0)
}

// escapeFn (AppendString or AppendStringNoHTML) is threaded through the
// whole any-walk so nested strings and map keys escape consistently.
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
	// Concrete stdlib cases must sit before the json.Marshaler dispatch.
	case json.RawMessage:
		// Nil/empty → null; else assumed-valid JSON verbatim.
		if len(x) == 0 {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return append(dst, x...), nil
	// go1.24 gave big.Int an AppendText, so the TextAppender dispatch below
	// would quote its digits — but v1, jsonv2 AND ggen's own KindBigInt field
	// wire all emit a BARE NUMBER. Exactly the hazard the case-ordering rule
	// warns about (a type whose MarshalJSON shape differs from its AppendText
	// body needs a concrete case first). big.Float/big.Rat need no case: their
	// AppendText IS their quoted-string wire, and a VALUE reaches it through
	// the pointer re-dispatch in the reflect fallback.
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
	// A Duration field defaults to `format:units`; the any path carries the
	// same wire so one document never holds two shapes of one Go type.
	case time.Duration:
		dst = append(dst, '"')
		return esc(dst, x.String()), nil
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
			// A (nil, nil) MarshalJSON would emit NOTHING — `{"k":` with a
			// nil error. Stdlib v1/v2 both error here (only RawMessage nulls
			// its nil, via its own MarshalJSON).
			return dst, ErrEmptyMarshalJSON
		}
		return append(dst, b...), nil
	}
	// Reflection path for types the type switch above doesn't match.
	rv := reflect.ValueOf(v)
	// A marshaler declared on *T only: dispatch through a pointer (a copy,
	// v is never addressable here), as jsonv2 does regardless of
	// addressability.
	if needsAddr(rv.Type()) {
		return appendAny(dst, addrOf(rv), esc, depth)
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return appendAny(dst, rv.Elem().Interface(), esc, depth+1)
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
		return appendStruct(dst, rv, esc, depth)
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
		// Reused addressable scratch — SetIterKey/SetIterValue copy into these
		// instead of allocating a fresh reflect.Value per entry.
		kv := reflect.New(kt).Elem()
		vv := reflect.New(et).Elem()
		// A text marshaler on the key type (either receiver: *K's method set
		// covers both, and kv is where every key lands) names the entries.
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

// appendReflectValue emits rv to dst given its already-known Kind and
// whether its marshaler lives on the pointer type (addr, needsAddr of the
// element type, hoisted by the caller). Primitive kinds read straight off
// the reflect.Value; non-primitive kinds fall back to AppendAny via
// Interface(). NAMED primitive types box too: their marshalers
// (json.Number's unquoted case, a MarshalJSON/AppendText on `type Level
// int`) live in the type switch, and the kind fast path was silently
// bypassing them.
func appendReflectValue(dst []byte, rv reflect.Value, kind reflect.Kind, addr bool, esc escapeFn, depth int) ([]byte, error) {
	if addr {
		return appendAny(dst, addrOf(rv), esc, depth)
	}
	if t := rv.Type(); t.PkgPath() != "" {
		return appendAny(dst, rv.Interface(), esc, depth)
	}
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
	tagged    bool // name came from an explicit json tag
	omitEmpty bool
	omitZero  bool
	zero      zeroMode
	quoted    bool // ,string — wrap primitive value in JSON string
	embed     bool // ,embed — splice map[string]V entries at parent level
	addr      bool // marshaler lives on *T only; box a pointer to the field
}

// zeroMode is how omitzero decides, mirroring jsonv2: an IsZero method
// on the type (or on the value it holds) outranks the structural test.
type zeroMode uint8

const (
	zeroStructural zeroMode = iota
	zeroMethod              // T implements IsZero (pointer/interface kinds guard nil first)
	zeroAddrMethod          // only *T implements IsZero
)

type isZeroer interface{ IsZero() bool }

func (f *fieldInfo) isZero(fv reflect.Value) bool {
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
			// *T carries T's methods; boxing the pointer skips the value copy.
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

// structInfo is the cached, flattened field list for a struct type.
// Anonymous embedded structs are flattened in at build time.
type structInfo struct {
	fields   []fieldInfo
	hasEmbed bool // some field carries ,embed — run the catch-all pass
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

// seen holds the embedding chain from the root to t — recursion into a type
// already on the chain would never terminate (stdlib breaks the cycle the
// same way). Stack semantics (delete after recursing) so a diamond-embedded
// type still surfaces twice for resolveFieldConflicts to judge.
func collectFields(info *structInfo, t reflect.Type, parentIndex []int, seen map[reflect.Type]struct{}) {
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

// hasMarshaler reports whether t satisfies one of the four dispatch arms.
func hasMarshaler(t reflect.Type) bool {
	return t.Implements(marshalerType) || t.Implements(textAppenderType) ||
		t.Implements(textMarshalerType) || t.Implements(jsonMarshalerType)
}

var needsAddrCache sync.Map // map[reflect.Type]bool

// needsAddr reports whether *t carries a marshaler t itself lacks, so a t
// value must be dispatched through a pointer to reach it (jsonv2 calls
// pointer-receiver methods regardless of addressability; v1 skipped them
// on values). Unnamed types have no methods and skip the cache.
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

// addrOf boxes a pointer to rv, copying into fresh memory when rv is not
// addressable.
func addrOf(rv reflect.Value) any {
	if rv.CanAddr() {
		return rv.Addr().Interface()
	}
	p := reflect.New(rv.Type())
	p.Elem().Set(rv)
	return p.Interface()
}

// quotableKind gates `,string`: numeric kinds only (through one pointer
// level), matching generated code and jsonv2 — a bare quote wrap around a
// string or composite emits invalid JSON, and jsonv2 dropped the v1 bool
// stringification.
func quotableKind(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// json.Number is a named STRING whose wire shape is a NUMBER, so both
	// stdlib versions DO quote it under `,string` — the one string-kind
	// exception (unlike bool, which jsonv2 deliberately stopped quoting).
	// The converse holds for time.Duration: an int64 whose wire is a string.
	switch t {
	case numberType:
		return true
	case durationType:
		return false
	}
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

// emptyWire reports whether an encoded value is one omitempty drops: null,
// "", {} or []. Two-byte values other than those are numbers, and null is the
// only four-byte value opening with 'n'.
func emptyWire(b []byte) bool {
	switch len(b) {
	case 2:
		return b[0] == '"' || b[0] == '{' || b[0] == '['
	case 4:
		return b[0] == 'n'
	}
	return false
}

// structWire reports whether v marshals as a struct's own members. A struct
// field is the one shape omitempty never drops — the generator refuses the
// option there outright — so an all-omitted struct is written as {}. Pointers
// and interfaces are peeled; a nil one is not a struct, so it still omits.
func structWire(v reflect.Value) bool {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	return v.Kind() == reflect.Struct
}

// fieldValue resolves f in rv. ok is false when the field is unreachable —
// promoted through a nil embedded pointer, which the stdlib omits — or when
// omitzero drops it.
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

// embedSplices reports whether f's ,embed catch-all applies: the splice needs
// a string-keyed map, any other type is written as an ordinary member.
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
			continue // spliced after the named members
		}
		// omitempty is decided on the encoded bytes: the member is written,
		// then unwritten if its value came out empty.
		mark := len(dst)
		if !first {
			dst = append(dst, ',')
		}
		dst = append(dst, '"')
		dst = esc(dst, f.name)
		dst = append(dst, ':')
		// nil pointer-to-number emits bare null even under ,string.
		quoted := f.quoted && !(fv.Kind() == reflect.Pointer && fv.IsNil())
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
		// The catch-all's entries follow every named member whatever the
		// field's declaration position — jsonv2's order, and the emitter's.
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

// closeText closes raw-appended text (an AppendText body) with `"`, or
// re-escapes it through esc when it carries a byte either escape table
// would touch — checked against the HTML table (a superset of both) so the
// clean fast path stays raw.
func closeText(dst []byte, from int, esc escapeFn) []byte {
	for _, c := range dst[from:] {
		if needEscapeHTML[c] {
			return esc(dst[:from], string(dst[from:]))
		}
	}
	return append(dst, '"')
}

// isNilPtr reports a typed-nil POINTER boxed in the interface: the dispatch
// arms match it through a value-receiver method set on *T, and invoking the
// method would nil-deref (stdlib emits null; the reflect fallback never runs
// because the type switch matches first). The fast path is a raw read of the
// interface's data word — a boxed pointer's data word IS the pointer value,
// and no non-nil value of any kind boxes through a nil data word (Go 1.4+
// interface representation), so a NON-nil data word always means "call the
// method". reflect.ValueOf measured +51% on that common non-nil path in this
// hot dispatch arm (in-situ core-pinned A/B, 2026-08). A NIL data word is
// ambiguous, though: nil maps, funcs, and chans box one too, and calling a
// value-receiver method on those is safe Go that stdlib performs — so the
// cold path disambiguates by reflect kind and emits null only for a true
// pointer.
func isNilPtr(v any) bool {
	type iface struct{ typ, data unsafe.Pointer }
	if (*iface)(unsafe.Pointer(&v)).data != nil {
		return false
	}
	return reflect.ValueOf(v).Kind() == reflect.Pointer
}

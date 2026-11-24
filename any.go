package ggen

import (
	"encoding"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/netip"
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
	return appendAny(dst, v, false, 0)
}

// AppendAnyHTML is AppendAny with HTML-safe escaping. It writes <, > and & as
// \uXXXX escapes, also inside json.RawMessage values and MarshalJSON output.
func AppendAnyHTML(dst []byte, v any) ([]byte, error) {
	return appendAny(dst, v, true, 0)
}

// escMode is true for HTML-safe escaping. It is passed down the whole walk, so
// every nested string, map key and raw value uses the same escaping.
type escMode bool

func (html escMode) str(dst []byte, s string) []byte {
	if html {
		return AppendString(dst, s)
	}
	return AppendStringNoHTML(dst, s)
}

func (html escMode) raw(dst, b []byte) []byte {
	if html {
		return AppendRawHTML(dst, b)
	}
	return append(dst, b...)
}

// netKind tells apart the package net structs that encode as strings, the
// way generated fields of those types do. The package does not import net,
// so they are recognized by name. Their exported fields are, in order, IP and
// Mask for net.IPNet, and IP, Port and Zone for net.TCPAddr and net.UDPAddr.
type netKind uint8

const (
	netNone netKind = iota
	netIPNet
	netAddr // net.TCPAddr or net.UDPAddr
)

func netKindOf(t reflect.Type) netKind {
	if t.Kind() != reflect.Struct || t.PkgPath() != "net" {
		return netNone
	}
	switch t.Name() {
	case "IPNet":
		return netIPNet
	case "TCPAddr", "UDPAddr":
		return netAddr
	}
	return netNone
}

// netElemKind is netKindOf of the struct a pointer in rv points to.
func netElemKind(rv reflect.Value) netKind {
	if rv.Kind() != reflect.Pointer {
		return netNone
	}
	return netKindOf(rv.Type().Elem())
}

// appendNetValue writes the net value in rv as AppendIPNet or AppendNetAddr
// does, escaping a zone with esc.
func appendNetValue(dst []byte, rv reflect.Value, k netKind, esc escMode) ([]byte, error) {
	dst = append(dst, '"')
	if k == netIPNet {
		return AppendIPNet(dst, rv.Field(0).Bytes(), rv.Field(1).Bytes())
	}
	from := len(dst)
	zone := rv.Field(2).String()
	dst, err := appendNetAddrRaw(dst, rv.Field(0).Bytes(), int(rv.Field(1).Int()), zone)
	if err != nil || zone == "" {
		return dst, err
	}
	return closeText(dst[:len(dst)-1], from, esc), nil
}

func appendAny(dst []byte, v any, esc escMode, depth int) ([]byte, error) {
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
		return esc.str(dst, x), nil
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
			dst = esc.str(dst, s)
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
				dst = esc.raw(dst, r)
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
			dst = esc.str(dst, k)
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
			dst = esc.str(dst, k)
			dst = append(dst, ':', '"')
			dst = esc.str(dst, val)
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
		// and copied as is, HTML-escaped in HTML mode.
		if len(x) == 0 {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		return esc.raw(dst, x), nil
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
		return esc.str(dst, x.String()), nil
	// Pointers to primitives. A nil pointer writes null.
	case *string:
		if x == nil {
			return append(dst, 'n', 'u', 'l', 'l'), nil
		}
		dst = append(dst, '"')
		return esc.str(dst, *x), nil
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
		dst = esc.str(dst, BytesToString(t))
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
		return esc.raw(dst, b), nil
	}
	// Types the type switch above does not match go through reflect.
	rv := reflect.ValueOf(v)
	// Generated fields write the net structs as strings. They have no
	// marshal methods, so without this they would be reflected into objects.
	if k := netKindOf(rv.Type()); k != netNone {
		return appendNetValue(dst, rv, k, esc)
	}
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
		// A pointer to a net value is written in place instead of boxing a
		// copy of the struct.
		if k := netElemKind(rv); k != netNone {
			return appendNetValue(dst, rv.Elem(), k, esc)
		}
		// A struct with no codec of its own is walked through the pointer,
		// with no boxed copy.
		if e := rv.Elem(); e.Kind() == reflect.Struct {
			if info := cachedStructInfo(e.Type()); info.inPlace {
				if depth >= maxDepth {
					return dst, ErrMaxDepth
				}
				return appendStructInfo(dst, e, info, esc, depth+1)
			}
		}
		return appendAny(dst, rv.Elem().Interface(), esc, depth+1)
	// Named primitive types such as `type MyEnum int` end up here. The type
	// switch matches only the exact predeclared types.
	case reflect.Bool:
		return strconv.AppendBool(dst, rv.Bool()), nil
	case reflect.String:
		dst = append(dst, '"')
		return esc.str(dst, rv.String()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(dst, rv.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.AppendUint(dst, rv.Uint(), 10), nil
	case reflect.Float32:
		return AppendFloat(dst, rv.Float(), 32)
	case reflect.Float64:
		return AppendFloat(dst, rv.Float(), 64)
	case reflect.Struct:
		return appendStructInfo(dst, rv, cachedStructInfo(rv.Type()), esc, depth)
	case reflect.Slice, reflect.Array:
		// Slices and arrays of byte encode as base64, for example
		// `type Bytes []byte`. A named element type such as `type Octet
		// uint8` is an ordinary list, as in jsonv2.
		if et := rv.Type().Elem(); et.Kind() == reflect.Uint8 && et.PkgPath() == "" {
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
		elem := elemInfoOf(rv.Type().Elem())
		for i := range rv.Len() {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendReflectValue(dst, rv.Index(i), &elem, esc, depth+1)
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
		if mayHaveMethods(kt) {
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
		elem := elemInfoOf(et)
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
				dst = esc.str(dst, BytesToString(t))
			default:
				dst = esc.str(dst, kv.String())
			}
			dst = append(dst, ':')
			dst, err = appendReflectValue(dst, vv, &elem, esc, depth+1)
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

func appendMapInt[V int | int8 | int16 | int32 | int64](dst []byte, m map[string]V, esc escMode) []byte {
	dst = append(dst, '{')
	first := true
	for k, v := range m {
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = append(dst, '"')
		dst = esc.str(dst, k)
		dst = append(dst, ':')
		dst = strconv.AppendInt(dst, int64(v), 10)
	}
	return append(dst, '}')
}

func appendMapUint[V uint | uint8 | uint16 | uint32 | uint64](dst []byte, m map[string]V, esc escMode) []byte {
	dst = append(dst, '{')
	first := true
	for k, v := range m {
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = append(dst, '"')
		dst = esc.str(dst, k)
		dst = append(dst, ':')
		dst = strconv.AppendUint(dst, uint64(v), 10)
	}
	return append(dst, '}')
}

func appendMapFloat[V float32 | float64](dst []byte, m map[string]V, bitSize int, esc escMode) ([]byte, error) {
	dst = append(dst, '{')
	first := true
	for k, v := range m {
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = append(dst, '"')
		dst = esc.str(dst, k)
		dst = append(dst, ':')
		var err error
		dst, err = AppendFloat(dst, float64(v), bitSize)
		if err != nil {
			return dst, err
		}
	}
	return append(dst, '}'), nil
}

func appendMapBool(dst []byte, m map[string]bool, esc escMode) []byte {
	dst = append(dst, '{')
	first := true
	for k, v := range m {
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = append(dst, '"')
		dst = esc.str(dst, k)
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

// elemInfo is what a container knows about its element type. It is computed
// once per container, so the element loop does no type inspection.
type elemInfo struct {
	kind   reflect.Kind
	addr   bool        // needsAddr of the element type
	named  bool        // the element type has a package path
	direct bool        // directKind: written from its kind alone
	net    netKind     // a net struct, written in place
	st     *structInfo // a struct that is walked in place, else nil
}

func elemInfoOf(et reflect.Type) elemInfo {
	e := elemInfo{kind: et.Kind(), addr: needsAddr(et), named: et.PkgPath() != ""}
	if e.addr {
		return e
	}
	e.direct = directKind(et)
	e.net = netKindOf(et)
	if e.kind == reflect.Struct {
		if info := cachedStructInfo(et); info.inPlace {
			e.st = info
		}
	}
	return e
}

// appendReflectValue appends the container element rv to dst. A value with a
// marshaler only on its pointer type is passed by address. A bool, string or
// number with no codec of its own is read straight from the reflect.Value, a
// net struct and a struct with no codec are written in place. Every other
// value goes back through appendAny via Interface(), because its marshalers
// and concrete cases live in appendAny's type switch. Examples are
// json.Number, which is written unquoted, and a MarshalJSON or AppendText
// method on `type Level int`.
func appendReflectValue(dst []byte, rv reflect.Value, e *elemInfo, esc escMode, depth int) ([]byte, error) {
	switch {
	case e.addr:
		return appendAny(dst, addrOf(rv), esc, depth)
	case e.direct:
		if e.named && depth > maxDepth {
			return dst, ErrMaxDepth
		}
		return appendKind(dst, rv, e.kind, esc)
	case e.net != netNone:
		return appendNetValue(dst, rv, e.net, esc)
	case e.st != nil:
		if depth > maxDepth {
			return dst, ErrMaxDepth
		}
		return appendStructInfo(dst, rv, e.st, esc, depth)
	}
	return appendAny(dst, rv.Interface(), esc, depth)
}

// directKind reports whether a value of type t is written from its reflect
// kind alone: a bool, string or number with no marshal method on t or *t.
// json.Number and time.Duration are named primitives appendAny writes in a
// form of their own.
func directKind(t reflect.Type) bool {
	//exhaustive:ignore not every kind applies here
	switch t.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
	default:
		return false
	}
	if t.PkgPath() == "" {
		return true
	}
	return t != numberType && t != durationType && !hasMarshaler(t) && !needsAddr(t)
}

// appendKind appends the bool, string or number in rv, read by kind.
func appendKind(dst []byte, rv reflect.Value, kind reflect.Kind, esc escMode) ([]byte, error) {
	//exhaustive:ignore directKind admits only these
	switch kind {
	case reflect.String:
		dst = append(dst, '"')
		return esc.str(dst, rv.String()), nil
	case reflect.Bool:
		return strconv.AppendBool(dst, rv.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(dst, rv.Int(), 10), nil
	case reflect.Float32:
		return AppendFloat(dst, rv.Float(), 32)
	case reflect.Float64:
		return AppendFloat(dst, rv.Float(), 64)
	default: // the unsigned kinds
		return strconv.AppendUint(dst, rv.Uint(), 10), nil
	}
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
	hidden    bool // a named embedding of an unexported struct type, read through its address
	addr      bool // the marshaler is only on *T, so pass a field pointer
	direct    bool // directKind: written from its kind alone
	kind      reflect.Kind
	plain     *structInfo // a struct with no codec of its own, walked without boxing
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
	fields []fieldInfo
	// embed is the catch-all whose entries are written as members, if any.
	// embedConflict is set when the struct declares two of its own, which
	// AppendAny refuses as jsonv2 and the generator do.
	embed         *fieldInfo
	embedConflict bool
	// hasHidden is set when a field is hidden. Such a struct is walked from
	// an addressable value, copied first when it is not one.
	hasHidden bool
	// inPlace is set when the struct has no codec of its own and none of its
	// fields needs Interface() on a value that is not pointer-shaped. Such a
	// struct is walked where it lies, as a slice element, a map value or
	// behind a pointer. Interface() on an addressable value allocates a
	// copy, so any other struct is boxed once and walked from the copy.
	inPlace bool
}

var structInfoCache sync.Map // map[reflect.Type]*structInfo

func cachedStructInfo(t reflect.Type) *structInfo {
	if v, ok := structInfoCache.Load(t); ok {
		return v.(*structInfo)
	}
	info := &structInfo{}
	var embeds []fieldInfo
	collectFields(info, &embeds, t, nil, map[reflect.Type]struct{}{t: {}})
	info.fields = resolveFieldConflicts(info.fields)
	info.embed, info.embedConflict = dominantEmbed(embeds)
	info.inPlace = netKindOf(t) == netNone && !hasMarshaler(t) && !needsAddr(t)
	for i := range info.fields {
		f := &info.fields[i]
		info.hasHidden = info.hasHidden || f.hidden
		switch {
		case f.direct, f.addr:
		case f.plain != nil:
			info.inPlace = info.inPlace && f.plain.inPlace
		default:
			//exhaustive:ignore the kinds listed box without a copy
			switch f.kind {
			case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Interface:
			default:
				info.inPlace = false
			}
		}
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

// dominantEmbed picks the catch-all a struct uses, by jsonv2's rule: the
// shallowest one, none on a tie between promoted ones, and a conflict on a
// tie between the struct's own.
func dominantEmbed(embeds []fieldInfo) (*fieldInfo, bool) {
	if len(embeds) == 0 {
		return nil, false
	}
	win, tie := 0, false
	for i := 1; i < len(embeds); i++ {
		switch d, w := len(embeds[i].index), len(embeds[win].index); {
		case d < w:
			win, tie = i, false
		case d == w:
			tie = true
		}
	}
	if tie {
		return nil, len(embeds[win].index) == 1
	}
	return &embeds[win], false
}

// collectFields appends the JSON-visible fields of t to info, and its
// string-keyed ,embed catch-alls to embeds. Catch-alls take no part in the
// name conflict resolution. seen holds the
// chain of embedded types from the root down to t. Recursing into a type
// already on the chain would never end, and encoding/json breaks the cycle
// the same way. seen works as a stack: a type is removed after its recursion
// returns. So a type embedded twice through different paths still shows up
// twice, and resolveFieldConflicts decides between the copies.
func collectFields(info *structInfo, embeds *[]fieldInfo, t reflect.Type, parentIndex []int, seen map[reflect.Type]struct{}) {
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
					collectFields(info, embeds, ft, idx, seen)
					delete(seen, ft)
				}
				continue
			}
		}
		// An embedded struct is kept under its tag name even when its type
		// is unexported, as in encoding/json and jsonv2.
		hidden := false
		if !sf.IsExported() {
			ft := sf.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if !sf.Anonymous || ft.Kind() != reflect.Struct {
				continue
			}
			hidden = true
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
			hidden:    hidden,
			addr:      needsAddr(sf.Type),
			direct:    directKind(sf.Type),
			kind:      sf.Type.Kind(),
		}
		if f.kind == reflect.Struct && !f.addr && netKindOf(sf.Type) == netNone && !hasMarshaler(sf.Type) {
			f.plain = cachedStructInfo(sf.Type)
		}
		if f.omitZero {
			switch {
			case sf.Type.Implements(isZeroerType):
				f.zero = zeroMethod
			case reflect.PointerTo(sf.Type).Implements(isZeroerType):
				f.zero = zeroAddrMethod
			}
		}
		// A ,embed on any other type is an ordinary member.
		if hasTagOpt(opts, "embed") && f.kind == reflect.Map && sf.Type.Key().Kind() == reflect.String {
			*embeds = append(*embeds, f)
			continue
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
// encoding/json skips them on non-addressable values. Types without methods
// skip the cache.
func needsAddr(t reflect.Type) bool {
	if !mayHaveMethods(t) {
		return false
	}
	if v, ok := needsAddrCache.Load(t); ok {
		return v.(bool)
	}
	need := !hasMarshaler(t) && hasMarshaler(reflect.PointerTo(t))
	needsAddrCache.Store(t, need)
	return need
}

// mayHaveMethods reports whether t can carry methods: a named type, or an
// unnamed struct, which has the methods of its embedded fields.
func mayHaveMethods(t reflect.Type) bool {
	return t.PkgPath() != "" || t.Kind() == reflect.Struct
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
	// A type with its own codec writes what it writes. jsonv2 ignores
	// `,string` there, and quoting a marshaler's string would be invalid JSON.
	if t.PkgPath() != "" && (hasMarshaler(t) || hasMarshaler(reflect.PointerTo(t))) {
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

// structWire reports whether v, which was written as {}, is a struct.
// omitempty never drops a struct field that comes out {}, and the generator
// rejects the option on one. Pointers and interfaces are followed. A struct
// that writes "", such as a zero netip.Addr, is dropped like any "".
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
	if f.hidden {
		// Interface() refuses a value read through an unexported field. The
		// same memory viewed through its address carries no such flag.
		fv = reflect.NewAt(fv.Type(), unsafe.Pointer(fv.UnsafeAddr())).Elem()
	}
	if f.omitZero && f.isZero(fv) {
		return fv, false
	}
	return fv, true
}

// addressable returns rv, or an addressable copy of it.
func addressable(rv reflect.Value) reflect.Value {
	if rv.CanAddr() {
		return rv
	}
	c := reflect.New(rv.Type()).Elem()
	c.Set(rv)
	return c
}

func appendStructInfo(dst []byte, rv reflect.Value, info *structInfo, esc escMode, depth int) ([]byte, error) {
	if info.embedConflict {
		return dst, &json.UnsupportedTypeError{Type: rv.Type()}
	}
	if info.hasHidden {
		rv = addressable(rv)
	}
	dst = append(dst, '{')
	first := true
	for i := range info.fields {
		f := &info.fields[i]
		fv, ok := fieldValue(rv, f)
		if !ok {
			continue
		}
		// omitempty is decided on the encoded bytes. The member is written,
		// then removed again if its value turned out empty.
		mark := len(dst)
		if !first {
			dst = append(dst, ',')
		}
		dst = append(dst, '"')
		dst = esc.str(dst, f.name)
		dst = append(dst, ':')
		// A nil pointer to a number writes a bare null, even with ,string.
		quoted := f.quoted && (fv.Kind() != reflect.Pointer || !fv.IsNil())
		if quoted {
			dst = append(dst, '"')
		}
		val := len(dst)
		var err error
		switch {
		case depth >= maxDepth:
			err = ErrMaxDepth
		case f.direct:
			dst, err = appendKind(dst, fv, f.kind, esc)
		case f.plain != nil && (f.plain.inPlace || !fv.CanAddr()):
			dst, err = appendStructInfo(dst, fv, f.plain, esc, depth+1)
		case f.addr:
			dst, err = appendAny(dst, addrOf(fv), esc, depth+1)
		default:
			dst, err = appendAny(dst, fv.Interface(), esc, depth+1)
		}
		if err != nil {
			return dst, err
		}
		if quoted {
			dst = append(dst, '"')
		}
		if f.omitEmpty && emptyWire(dst[val:]) && (dst[val] != '{' || !structWire(fv)) {
			dst = dst[:mark]
			continue
		}
		first = false
	}
	// The catch-all entries come after all named members, wherever the field
	// is declared. jsonv2 and generated code use the same order.
	if info.embed != nil {
		if fv, ok := fieldValue(rv, info.embed); ok {
			iter := fv.MapRange()
			for iter.Next() {
				if !first {
					dst = append(dst, ',')
				}
				first = false
				dst = append(dst, '"')
				dst = esc.str(dst, iter.Key().String())
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
func closeText(dst []byte, from int, esc escMode) []byte {
	for _, c := range dst[from:] {
		if needEscapeHTML[c] {
			return esc.str(dst[:from], string(dst[from:]))
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
	anySizeIPNet4   = 20 // quoted "255.255.255.255/32"
	anySizeIPNet6   = 45 // quoted IPv6 address and "/128"
	anySizeAddr4    = 23 // quoted "255.255.255.255:65535"
	anySizeAddr6    = 49 // quoted "[IPv6 address]:65535"
)

// netipAddrSize is the budget of a quoted netip.Addr. A zone adds its '%'
// separator and its escaped bytes. The zero Addr writes "".
func netipAddrSize(a netip.Addr, mult int) int {
	n := len(`"255.255.255.255"`)
	if !a.Is4() {
		n = len(`"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"`)
	}
	if z := len(a.Zone()); z > 0 {
		n += 1 + z*mult
	}
	return n
}

// netSize is the budget of the net value in rv. A zone adds its '%'
// separator and its escaped bytes.
func netSize(rv reflect.Value, k netKind, mult int) int {
	v4 := ipv4(rv.Field(0).Bytes()) != nil
	if k == netIPNet {
		if v4 {
			return anySizeIPNet4
		}
		return anySizeIPNet6
	}
	n := anySizeAddr6
	if v4 {
		n = anySizeAddr4
	}
	if z := rv.Field(2).Len(); z > 0 {
		n += 1 + z*mult
	}
	return n
}

// AnySize returns an upper bound on len(AppendAny(nil, v)), assuming the same
// escape growth for strings as JSONSize. Scalars use the generator's fixed
// budgets. Slices, maps and structs are walked. A generated value reports its
// own JSONSize. A text or JSON marshaler is run to learn its output length.
// A value nested deeper than maxDepth, which AppendAny refuses with
// ErrMaxDepth, sizes as 0.
func AnySize(v any) (n int) {
	defer recoverOverDepth(&n)
	return anySize(v, 2, 0)
}

// AnySizeHTML is [AnySize] for [AppendAnyHTML], where one escaped byte can
// take up to 6 bytes.
func AnySizeHTML(v any) (n int) {
	defer recoverOverDepth(&n)
	return anySize(v, 6, 0)
}

// sizeOverDepth is panicked by anySize past maxDepth so the whole walk ends
// there, as appendAny's does. Summing on past one pruned node would cost
// fan-out^maxDepth steps on a cyclic value.
type sizeOverDepth struct{}

// rawMult is the growth of a raw JSON value under the string multiplier mult.
// Raw bytes are escaped only by AnySizeHTML's AppendAnyHTML, up to 6 bytes
// each.
func rawMult(mult int) int {
	if mult == 6 {
		return 6
	}
	return 1
}

func recoverOverDepth(n *int) {
	if r := recover(); r != nil {
		if _, ok := r.(sizeOverDepth); !ok {
			panic(r)
		}
		*n = 0
	}
}

func anySize(v any, mult, depth int) int {
	if depth > maxDepth {
		panic(sizeOverDepth{})
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
			n += max(len(r)*rawMult(mult), 4)
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
		return max(len(x)*rawMult(mult), 4)
	case big.Int:
		return x.BitLen()/3 + 4
	case *big.Int:
		if x == nil {
			return 4
		}
		return x.BitLen()/3 + 4
	case time.Time, *time.Time:
		return anySizeTime
	case time.Duration:
		return anySizeDuration
	// The netip types are TextAppenders. Budgeting them like generated
	// fields avoids running AppendText into a fresh buffer to measure it.
	case netip.Addr:
		return netipAddrSize(x, mult)
	case netip.AddrPort:
		return netipAddrSize(x.Addr(), mult) + len(":65535") + 2
	case netip.Prefix:
		return netipAddrSize(x.Addr(), mult) + len("/128")
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
		return len(b) * rawMult(mult)
	}
	rv := reflect.ValueOf(v)
	if k := netKindOf(rv.Type()); k != netNone {
		return netSize(rv, k, mult)
	}
	if needsAddr(rv.Type()) {
		return anySize(addrOf(rv), mult, depth)
	}
	//exhaustive:ignore not every kind applies here
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return 4
		}
		if k := netElemKind(rv); k != netNone {
			return netSize(rv.Elem(), k, mult)
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
		if et.Kind() == reflect.Uint8 && et.PkgPath() == "" {
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
	if t := rv.Type(); t.PkgPath() != "" {
		if k := netKindOf(t); k != netNone {
			return netSize(rv, k, mult)
		}
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
	if mayHaveMethods(kt) {
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

// structSize is the size counterpart of appendStructInfo. It counts every member
// that omitempty might drop, because that decision needs the encoded bytes.
func structSize(rv reflect.Value, mult, depth int) int {
	info := cachedStructInfo(rv.Type())
	if info.hasHidden {
		rv = addressable(rv)
	}
	n := 2
	for i := range info.fields {
		f := &info.fields[i]
		fv, ok := fieldValue(rv, f)
		if !ok {
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
	if info.embed != nil {
		if fv, ok := fieldValue(rv, info.embed); ok {
			kv := reflect.New(fv.Type().Key()).Elem()
			iter := fv.MapRange()
			for iter.Next() {
				kv.SetIterKey(iter)
				n += kv.Len()*mult + 4 + anySize(iter.Value().Interface(), mult, depth+1)
			}
		}
	}
	return n
}

package ggen

import "reflect"

// AnyIsEmpty reports whether v encodes as an empty JSON value: null, "", []
// or {}. Those are the values omitempty skips on an `any` field. Common
// dynamic types are checked directly. Other types go through reflect, so a
// typed nil pointer, an empty typed slice and an empty []byte (base64 "")
// also count as empty. A struct never counts, because its encoding is not
// inspected. The exceptions are net.IPNet, net.TCPAddr and net.UDPAddr,
// which encode as strings and count when they write "".
func AnyIsEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return false
	}
	rv := reflect.ValueOf(v)
	//exhaustive:ignore not every kind applies here
	switch rv.Kind() {
	case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
		return rv.Len() == 0
	case reflect.Pointer:
		if rv.IsNil() {
			return true
		}
		// A pointer to a net value is read in place instead of boxing a copy
		// of the struct.
		if k := netElemKind(rv); k != netNone {
			return netEmpty(rv.Elem(), k)
		}
		return AnyIsEmpty(rv.Elem().Interface())
	case reflect.Struct:
		if k := netKindOf(rv.Type()); k != netNone {
			return netEmpty(rv, k)
		}
	}
	return false
}

// netEmpty reports whether the net value in rv writes "".
func netEmpty(rv reflect.Value, k netKind) bool {
	if k == netIPNet {
		return rv.Field(0).Len() == 0 && rv.Field(1).Len() == 0
	}
	return rv.Field(0).Len() == 0 && rv.Field(1).Int() == 0 && rv.Field(2).Len() == 0
}

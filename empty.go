package ggen

import "reflect"

// AnyIsEmpty reports whether v encodes as an empty JSON value: null, "", []
// or {}. Those are the values omitempty skips on an `any` field. Common
// dynamic types are checked directly. Other types go through reflect, so a
// typed nil pointer, an empty typed slice and an empty []byte (base64 "")
// also count as empty. A struct never counts, because its encoding is not
// inspected.
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
		return rv.IsNil() || AnyIsEmpty(rv.Elem().Interface())
	}
	return false
}

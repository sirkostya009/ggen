package ggen

import "reflect"

// AnyIsEmpty reports whether v encodes as a JSON-empty value — null, "", []
// or {} — which is what omitempty skips on an `any` field. The common
// dynamic types are matched directly; anything else consults reflect, so a
// typed nil pointer, an empty typed slice or an empty []byte (base64 "")
// count too. Structs never do: their encoding is not inspected.
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

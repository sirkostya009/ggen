package ggen

import (
	"encoding/json"
	"net/netip"
	"reflect"
)

// RawIsEmpty reports whether raw JSON text encodes as a value omitempty
// drops: nil or empty, which is written as null, or null, "", [] or {}.
func RawIsEmpty(b []byte) bool {
	return len(b) == 0 || emptyWire(b)
}

// AnyIsEmpty reports whether v encodes as an empty JSON value: null, "", []
// or {}. Those are the values omitempty skips on an `any` field. Common
// dynamic types are checked directly. Other types go through reflect, so a
// typed nil pointer, an empty typed slice and an empty []byte (base64 "")
// also count as empty. A struct never counts, because its encoding is not
// inspected. The exceptions are net.IPNet, net.TCPAddr, net.UDPAddr and the
// netip types, which encode as strings and count when they write "".
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
	case json.RawMessage:
		return RawIsEmpty(x)
	}
	rv := reflect.ValueOf(v)
	// Pointers are followed in place. Boxing the pointee to recurse would
	// copy it to the heap.
	for range maxDepth {
		//exhaustive:ignore not every kind applies here
		switch rv.Kind() {
		case reflect.Slice:
			if rv.Type() == rawMessageType {
				return RawIsEmpty(rv.Bytes())
			}
			return rv.Len() == 0
		case reflect.String, reflect.Map, reflect.Array:
			return rv.Len() == 0
		case reflect.Pointer, reflect.Interface:
			if rv.IsNil() {
				return true
			}
			rv = rv.Elem()
		case reflect.Struct:
			if k := netKindOf(rv.Type()); k != netNone {
				return netEmpty(rv, k)
			}
			return netipEmpty(rv)
		default:
			return false
		}
	}
	// A pointer that reaches itself. AppendAny reports it as ErrMaxDepth.
	return false
}

var (
	rawMessageType    = reflect.TypeFor[json.RawMessage]()
	netipAddrType     = reflect.TypeFor[netip.Addr]()
	netipAddrPortType = reflect.TypeFor[netip.AddrPort]()
	netipPrefixType   = reflect.TypeFor[netip.Prefix]()
)

// netipEmpty reports whether rv is a netip value that writes "": a zero
// Addr or Prefix, or an AddrPort with a zero Addr.
func netipEmpty(rv reflect.Value) bool {
	switch rv.Type() {
	case netipAddrType, netipPrefixType:
		return rv.IsZero()
	case netipAddrPortType:
		return rv.Field(0).IsZero()
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

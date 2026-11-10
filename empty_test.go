package ggen

import (
	"net"
	"testing"
	"time"
)

func TestAnyIsEmpty(t *testing.T) {
	t.Parallel()
	var nilPtr *int
	cases := []struct {
		v    any
		want bool
	}{
		{nil, true},
		{"", true},
		{"x", false},
		{[]any{}, true},
		{[]any{1}, false},
		{map[string]any{}, true},
		{map[string]any{"a": 1}, false},
		{0, false},
		{false, false},
		{0.0, false},
		{uint8(0), false},
		{[]int{}, true},
		{[]int(nil), true},
		{[]int{1}, false},
		{[]byte{}, true},
		{[]byte("x"), false},
		{map[string]int{}, true},
		{map[string]int{"a": 1}, false},
		{[0]int{}, true},
		{[1]int{}, false},
		{nilPtr, true},
		{new(""), true},
		{new("x"), false},
		{new(0), false},
		{new(new("")), true},
		{time.Time{}, false},
		// The net structs AppendAny writes as strings are empty when the
		// string is.
		{net.IPNet{}, true},
		{net.IPNet{IP: net.IP{}, Mask: net.IPMask{}}, true},
		{net.IPNet{IP: net.IPv4(10, 0, 0, 0).To4(), Mask: net.CIDRMask(8, 32)}, false},
		{net.TCPAddr{}, true},
		{&net.UDPAddr{IP: net.IP{}}, true},
		{(*net.TCPAddr)(nil), true},
		{net.TCPAddr{Port: 1}, false},
		{net.UDPAddr{IP: net.ParseIP("::1")}, false},
	}
	for _, c := range cases {
		if got := AnyIsEmpty(c.v); got != c.want {
			t.Errorf("AnyIsEmpty(%#v) = %v, want %v", c.v, got, c.want)
		}
		// AnyIsEmpty must agree with what AppendAny writes.
		out, err := AppendAny(nil, c.v)
		if err != nil {
			t.Fatalf("AppendAny(%#v): %v", c.v, err)
		}
		switch s := string(out); s {
		case "null", `""`, "[]", "{}":
			if !c.want {
				t.Errorf("AppendAny(%#v) = %s, JSON-empty, but AnyIsEmpty says false", c.v, s)
			}
		default:
			if c.want {
				t.Errorf("AppendAny(%#v) = %s, not JSON-empty, but AnyIsEmpty says true", c.v, s)
			}
		}
	}
	// AppendAny refuses these, so omitempty must keep them to report the
	// error. Each sets only one of the parts the emptiness check reads.
	for _, v := range []any{
		net.IPNet{Mask: net.CIDRMask(8, 32)},
		&net.IPNet{IP: net.IPv4(10, 0, 0, 0).To4()},
		net.TCPAddr{Zone: "eth0"},
		&net.UDPAddr{Zone: "eth0"},
	} {
		_, err := AppendAny(nil, v)
		if empty := AnyIsEmpty(v); err == nil || empty {
			t.Errorf("%#v: AppendAny error %v, AnyIsEmpty %v; want an error and false", v, err, empty)
		}
	}
}

// A net value behind a pointer is read in place, without boxing a copy.
func TestAnyIsEmpty_netNoAlloc(t *testing.T) { //nolint:paralleltest // measures allocations
	for _, v := range []any{
		&net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 1, Zone: "eth0"},
		&net.UDPAddr{},
		&net.IPNet{IP: net.IPv4(10, 0, 0, 0).To4(), Mask: net.CIDRMask(8, 32)},
		net.TCPAddr{Port: 1},
	} {
		if n := testing.AllocsPerRun(100, func() { _ = AnyIsEmpty(v) }); n != 0 {
			t.Errorf("AnyIsEmpty(%#v): %v allocs, want 0", v, n)
		}
	}
}

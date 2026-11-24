package ggen

import (
	"encoding/json"
	"errors"
	"net"
	"net/netip"
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
		{json.RawMessage(nil), true},
		{json.RawMessage("null"), true},
		{json.RawMessage(`""`), true},
		{json.RawMessage("[]"), true},
		{json.RawMessage("{}"), true},
		{json.RawMessage("0"), false},
		{json.RawMessage("[0]"), false},
		{json.RawMessage("true"), false},
		{map[string]int{}, true},
		{map[string]int{"a": 1}, false},
		{[0]int{}, true},
		{[1]int{}, false},
		{nilPtr, true},
		{new(""), true},
		{new("x"), false},
		{new(0), false},
		{new(new("")), true},
		{new(json.RawMessage("null")), true},
		{new(json.RawMessage("1")), false},
		{new(any(new(""))), true},
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

// A netip value counts as empty exactly when AppendAny writes "".
func TestAnyIsEmpty_netip(t *testing.T) {
	t.Parallel()
	a := netip.MustParseAddr("10.0.0.1")
	for _, v := range []any{
		netip.Addr{},
		a, &a,
		netip.AddrPort{},
		netip.AddrPortFrom(netip.Addr{}, 80), netip.AddrPortFrom(a, 0),
		netip.Prefix{},
		netip.PrefixFrom(a, 8), netip.PrefixFrom(a, 99),
	} {
		b, err := AppendAny(nil, v)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := AnyIsEmpty(v), RawIsEmpty(b); got != want {
			t.Errorf("AnyIsEmpty(%#v) = %v, AppendAny wrote %s", v, got, b)
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

// Every pointee is read in place, whatever it is.
func TestAnyIsEmpty_pointerNoAlloc(t *testing.T) { //nolint:paralleltest // measures allocations
	type payload struct {
		Name string
		At   time.Time
		Tags []string
	}
	p, s, l := &payload{Name: "n"}, "s", []int{1}
	var boxed any = p
	for _, v := range []any{p, &p, &s, &l, &boxed} {
		if AnyIsEmpty(v) {
			t.Errorf("AnyIsEmpty(%T) = true", v)
		}
		if n := testing.AllocsPerRun(100, func() { _ = AnyIsEmpty(v) }); n != 0 {
			t.Errorf("AnyIsEmpty(%T): %v allocs, want 0", v, n)
		}
	}
}

// A pointer that reaches itself must end. AppendAny then reports the cycle.
func TestAnyIsEmpty_PointerCycle(t *testing.T) {
	t.Parallel()
	var a any
	a = &a
	if AnyIsEmpty(a) {
		t.Error("a pointer cycle counts as empty")
	}
	if _, err := AppendAny(nil, a); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("AppendAny: want ErrMaxDepth, got %v", err)
	}
}

type benchEmptyPayload struct {
	ID      int64
	Name    string
	Tags    []string
	Score   float64
	Created time.Time
}

// BenchmarkAnyIsEmpty covers the dynamic types an `any,omitempty` field
// usually holds. The control rows never reach the pointer arm.
func BenchmarkAnyIsEmpty(b *testing.B) {
	p := &benchEmptyPayload{ID: 1, Name: "Ada"}
	s := "text"
	for _, c := range []struct {
		name string
		v    any
	}{
		{"control_string", "text"},
		{"control_map", map[string]int{"a": 1}},
		{"control_struct", *p},
		{"ptr_struct", p},
		{"ptr_ptr_struct", &p},
		{"ptr_string", &s},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if AnyIsEmpty(c.v) {
					b.Fatal("empty")
				}
			}
		})
	}
}

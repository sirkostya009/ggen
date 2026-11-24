package ggen

import (
	"net/netip"
	"strings"
	"testing"
)

func TestAnySize_Netip(t *testing.T) { //nolint:paralleltest // measures allocations
	zones := []string{"", "eth0", strings.Repeat(`<"\&`, 8)}
	var addrs []netip.Addr
	for _, s := range []string{"0.0.0.0", "255.255.255.255", "::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "::ffff:255.255.255.255", "1:2::3"} {
		addrs = append(addrs, netip.MustParseAddr(s))
	}
	var vals []any
	vals = append(vals, netip.Addr{}, netip.AddrPort{}, netip.Prefix{})
	for _, a := range addrs {
		for _, z := range zones {
			za := a
			if z != "" && a.Is6() && !a.Is4In6() {
				za = a.WithZone(z)
			}
			vals = append(vals, za, netip.AddrPortFrom(za, 65535))
			for _, bits := range []int{0, 32, 128} {
				if p, err := a.Prefix(bits); err == nil {
					vals = append(vals, p)
				}
			}
		}
	}
	for _, v := range vals {
		for _, html := range []bool{false, true} {
			size, appendFn := AnySize(v), AppendAny
			if html {
				size, appendFn = AnySizeHTML(v), AppendAnyHTML
			}
			out, err := appendFn(nil, v)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) > size {
				t.Errorf("%#v html=%v: wrote %d (%s), AnySize %d", v, html, len(out), out, size)
			}
		}
		if a := testing.AllocsPerRun(10, func() { _ = AnySize(v) }); a != 0 {
			t.Errorf("%v: AnySize allocates %v", v, a)
		}
	}
}

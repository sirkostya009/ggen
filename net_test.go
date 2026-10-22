package ggen

import (
	"net"
	"reflect"
	"strings"
	"testing"
)

func TestAppendIPNet_matchesString(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"10.0.0.0/8", "0.0.0.0/0", "255.255.255.255/32", "10.1.2.3/8",
		"::/0", "2001:db8::/32", "fe80::1/128", "::ffff:1.2.3.0/120",
	} {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		// Unmasked too: String keeps host bits, and so must the writer.
		for _, v := range []net.IPNet{*n, {IP: ip, Mask: n.Mask}, {IP: ip.To16(), Mask: n.Mask}} {
			out, err := AppendIPNet(nil, v)
			if err != nil {
				t.Fatalf("%s %v: %v", s, v, err)
			}
			if want := v.String() + `"`; string(out) != want {
				t.Errorf("%s: wrote %s, String %s", s, out, want)
			}
		}
		got, err := ParseIPNet(s)
		if err != nil || got.String() != n.String() || !reflect.DeepEqual(got.Mask, n.Mask) {
			t.Errorf("parse %s: %v (%v), ParseCIDR %v", s, got, err, n)
		}
	}
}

func TestAppendNetAddr_matchesString(t *testing.T) {
	t.Parallel()
	for _, a := range []net.TCPAddr{
		{IP: net.ParseIP("1.2.3.4"), Port: 80},
		{IP: net.IPv4(1, 2, 3, 4).To4(), Port: 0},
		{IP: net.ParseIP("::1"), Port: 65535},
		{IP: net.ParseIP("fe80::1"), Port: 1, Zone: "eth0"},
		{Port: 8080},
	} {
		out, err := AppendNetAddr(nil, a)
		if err != nil {
			t.Fatalf("%v: %v", a, err)
		}
		if want := a.String() + `"`; string(out) != want {
			t.Errorf("wrote %s, String %s", out, want)
		}
		got, err := ParseNetAddr[net.UDPAddr](a.String())
		if err != nil || got.String() != a.String() {
			t.Errorf("parse %s: %v (%v)", a.String(), got, err)
		}
	}
}

func TestParseNetAddr_mappedZoneRejected(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"[::ffff:1.2.3.4%eth0]:80", "[0:0:0:0:0:ffff:102:304%x]:1"} {
		if a, err := ParseNetAddr[net.TCPAddr](s); err == nil {
			t.Errorf("%s: parsed as %v, which AppendNetAddr refuses", s, a)
		}
	}
	a, err := ParseNetAddr[net.TCPAddr]("[::ffff:1.2.3.4]:80")
	if err != nil || len(a.IP) != net.IPv4len {
		t.Errorf("mapped without zone: %v (%v), want IPv4", a, err)
	}
}

func TestParseIPNet_oneAlloc(t *testing.T) { //nolint:paralleltest // measures allocations
	for _, s := range []string{"10.0.0.0/8", "2001:db8::/33"} {
		if n := testing.AllocsPerRun(100, func() { _, _ = ParseIPNet(s) }); n != 1 {
			t.Errorf("%s: %v allocs, want 1", s, n)
		}
	}
	// IP is capped, so appending to it cannot overwrite Mask.
	n, _ := ParseIPNet("10.0.0.0/8")
	_ = append(n.IP, 0xaa)
	if n.Mask.String() != "ff000000" {
		t.Errorf("mask clobbered: %v", n.Mask)
	}
}

func TestAppendAny_netTypes(t *testing.T) {
	t.Parallel()
	_, n, _ := net.ParseCIDR("10.0.0.0/8")
	for _, c := range []struct {
		v    any
		want string
	}{
		{*n, `"10.0.0.0/8"`},
		{n, `"10.0.0.0/8"`},
		{(*net.IPNet)(nil), `null`},
		{net.IPNet{}, `""`},
		{net.TCPAddr{IP: net.ParseIP("::1"), Port: 1}, `"[::1]:1"`},
		{&net.UDPAddr{Port: 53}, `":53"`},
		{[]net.UDPAddr{{Port: 1}, {}}, `[":1",""]`},
		// A pointer to a zero value writes "", which omitempty drops.
		{struct {
			A *net.TCPAddr `json:"a,omitempty"`
			B net.IPNet    `json:"b"`
		}{A: &net.TCPAddr{}, B: *n}, `{"b":"10.0.0.0/8"}`},
		{net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 1, Zone: "<"}, `"[fe80::1%<]:1"`},
	} {
		out, err := AppendAny(nil, c.v)
		if err != nil || string(out) != c.want {
			t.Errorf("%T: %s (%v), want %s", c.v, out, err, c.want)
		}
	}
	h, err := AppendAnyHTML(nil, net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 1, Zone: "<"})
	if err != nil || !strings.Contains(string(h), `\u003c`) {
		t.Errorf("html: %s, %v", h, err)
	}
	if _, err := AppendAny(nil, net.TCPAddr{Port: 70000}); err == nil {
		t.Error("port out of range accepted")
	}
}

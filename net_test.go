package ggen

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net"
	"net/netip"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestAppendIPNet_matchesString(t *testing.T) {
	t.Parallel()
	cidrs := []string{"0.0.0.0/0", "255.255.255.255/32", "::/0", "fe80::1/128"}
	for n := range 33 {
		cidrs = append(cidrs, "10.200.33.17/"+strconv.Itoa(n))
	}
	for n := range 129 {
		cidrs = append(cidrs, "2001:db8:ffff:1234:5678:9abc:def0:1111/"+strconv.Itoa(n), "::ffff:1.2.3.4/"+strconv.Itoa(n))
	}
	for _, s := range cidrs {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		// Unmasked too: String keeps host bits, and so must the writer.
		for _, v := range []net.IPNet{*n, {IP: ip, Mask: n.Mask}, {IP: ip.To16(), Mask: n.Mask}} {
			out, err := AppendIPNet(nil, v.IP, v.Mask)
			if err != nil {
				t.Fatalf("%s %v: %v", s, v, err)
			}
			if want := v.String() + `"`; string(out) != want {
				t.Errorf("%s: wrote %s, String %s", s, out, want)
			}
		}
		gotIP, gotMask, err := ParseIPNet(s)
		if err != nil || !bytes.Equal(gotIP, n.IP) || !bytes.Equal(gotMask, n.Mask) || len(gotIP) != len(n.IP) {
			t.Errorf("parse %s: %v %v (%v), ParseCIDR %v %v", s, gotIP, gotMask, err, n.IP, n.Mask)
		}
	}
}

// Each refused value prints without a prefix length, or not at all, so
// ParseIPNet could not read it back.
func TestAppendIPNet_refusals(t *testing.T) {
	t.Parallel()
	v6 := net.ParseIP("2001:db8::")
	for name, v := range map[string]net.IPNet{
		"ipv6_ip_ipv4_mask": {IP: v6, Mask: net.CIDRMask(8, 32)},
		"noncanonical":      {IP: net.IPv4(10, 0, 0, 0).To4(), Mask: net.IPv4Mask(255, 0, 255, 0)},
		"noncanonical_tail": {IP: v6, Mask: append(net.CIDRMask(64, 128)[:15:15], 1)},
		"mask_missing":      {IP: net.IPv4(10, 0, 0, 0)},
		"mask_odd_length":   {IP: net.IPv4(10, 0, 0, 0).To4(), Mask: net.IPMask{255, 0, 0}},
		"ip_missing":        {Mask: net.CIDRMask(8, 32)},
		"ip_missing_v6mask": {Mask: net.CIDRMask(8, 128)},
		"ip_short":          {IP: net.IP{1, 2, 3}, Mask: net.CIDRMask(8, 128)},
		"ip_long":           {IP: make(net.IP, 17), Mask: net.CIDRMask(8, 128)},
	} {
		out, err := AppendIPNet([]byte("x"), v.IP, v.Mask)
		if !errors.Is(err, errIPNet) || string(out) != "x" {
			t.Errorf("%s: wrote %q (%v), want errIPNet", name, out, err)
		}
		if _, _, err := ParseIPNet(v.String()); err == nil {
			t.Errorf("%s: String %q parses", name, v.String())
		}
	}
}

func TestParseIPNet_masksEveryLength(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ addr string }{{"255.255.255.255"}, {"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"}, {"::ffff:1.2.3.4"}} {
		bits := 32
		if strings.Contains(c.addr, ":") {
			bits = 128
		}
		for n := range bits + 1 {
			s := c.addr + "/" + strconv.Itoa(n)
			ip, mask, err := ParseIPNet(s)
			if err != nil {
				t.Fatalf("%s: %v", s, err)
			}
			want := netip.MustParsePrefix(s).Masked().Addr().AsSlice()
			if !bytes.Equal(mask, net.CIDRMask(n, bits)) || !bytes.Equal(ip, want) {
				t.Errorf("%s: %x/%x, want %x/%x", s, ip, mask, want, []byte(net.CIDRMask(n, bits)))
			}
		}
	}
}

func TestParseIPNet_oneAlloc(t *testing.T) { //nolint:paralleltest // measures allocations
	for _, s := range []string{"10.0.0.0/8", "2001:db8::/33"} {
		if n := testing.AllocsPerRun(100, func() { _, _, _ = ParseIPNet(s) }); n != 1 {
			t.Errorf("%s: %v allocs, want 1", s, n)
		}
	}
	// IP is capped, so appending to it cannot overwrite Mask.
	ip, mask, _ := ParseIPNet("10.0.0.0/8")
	_ = append(ip, 0xaa)
	if net.IPMask(mask).String() != "ff000000" {
		t.Errorf("mask clobbered: %x", mask)
	}
}

// netip interns a zone through package unique, which allocates for every zone
// it has not seen. The parsers never hand it one, so text with a new zone on
// every call costs at most two allocations.
func TestNetParsers_zoneNotInterned(t *testing.T) { //nolint:paralleltest // measures allocations
	const runs = 100
	for _, c := range []struct {
		// Each shape has its own zone names, so none is interned already.
		prefix, suffix string
		parse          func(string)
	}{
		{"[fe80::1%za", "]:80", func(s string) { _, _, _, _ = ParseNetAddr(s) }},
		{"fe80::1%zb", ":80", func(s string) { _, _, _, _ = ParseNetAddr(s) }},
		{"fe80::%zc", "/10", func(s string) { _, _, _ = ParseIPNet(s) }},
	} {
		// One more than runs, for AllocsPerRun's warm-up call.
		in := make([]string, runs+1)
		for i := range in {
			in[i] = c.prefix + strconv.Itoa(i) + c.suffix
		}
		i := 0
		n := testing.AllocsPerRun(runs, func() {
			c.parse(in[i])
			i++
		})
		if n > 2 {
			t.Errorf("%s: %v allocs, want at most 2", in[0], n)
		}
	}
}

func TestAppendNetAddr_matchesString(t *testing.T) {
	t.Parallel()
	for _, a := range []net.TCPAddr{
		{IP: net.ParseIP("1.2.3.4"), Port: 80},
		{IP: net.IPv4(255, 255, 255, 255).To4(), Port: 0},
		{IP: net.ParseIP("::1"), Port: 65535},
		{IP: net.ParseIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"), Port: 65535},
		{IP: net.ParseIP("fe80::1"), Port: 1, Zone: "eth0"},
		{IP: net.ParseIP("fe80::1"), Port: 1, Zone: "a]b:c%d"},
		{Port: 8080},
		{Port: 65535},
	} {
		out, err := AppendNetAddr(nil, a.IP, a.Port, a.Zone)
		if err != nil {
			t.Fatalf("%v: %v", a, err)
		}
		if want := a.String() + `"`; string(out) != want {
			t.Errorf("wrote %s, String %s", out, want)
		}
		ip, port, zone, err := ParseNetAddr(a.String())
		if got := (net.TCPAddr{IP: ip, Port: port, Zone: zone}); err != nil || got.String() != a.String() {
			t.Errorf("parse %s: %v (%v)", a.String(), got, err)
		}
	}
}

// Each refused value prints text that ParseNetAddr rejects.
func TestAppendNetAddr_refusals(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		a    net.TCPAddr
		want error
	}{
		"port_high":     {net.TCPAddr{Port: 65536}, errNetAddrPort},
		"port_negative": {net.TCPAddr{IP: net.ParseIP("::1"), Port: -1}, errNetAddrPort},
		"ip_length":     {net.TCPAddr{IP: net.IP{1, 2, 3}, Port: 1}, errNetAddrIP},
		"zone_no_ip":    {net.TCPAddr{Zone: "eth0"}, errNetAddrZone},
		"zone_ipv4":     {net.TCPAddr{IP: net.IPv4(1, 2, 3, 4).To4(), Port: 1, Zone: "eth0"}, errNetAddrZone},
		"zone_mapped":   {net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 1, Zone: "eth0"}, errNetAddrZone},
	} {
		for _, app := range []func([]byte, []byte, int, string) ([]byte, error){AppendNetAddr, AppendNetAddrHTML} {
			out, err := app([]byte("x"), c.a.IP, c.a.Port, c.a.Zone)
			if !errors.Is(err, c.want) || string(out) != "x" {
				t.Errorf("%s: wrote %q (%v), want %v", name, out, err, c.want)
			}
		}
		if _, _, _, err := ParseNetAddr(c.a.String()); err == nil {
			t.Errorf("%s: String %q parses", name, c.a.String())
		}
	}
}

func TestAppendNetAddr_escapesZone(t *testing.T) {
	t.Parallel()
	ip := net.ParseIP("fe80::1")
	for _, zone := range []string{"plain", `a"b`, `a\b`, "a\x01b", "<script>", "a&b"} {
		a := net.TCPAddr{IP: ip, Port: 1, Zone: zone}
		for _, html := range []bool{false, true} {
			app := AppendNetAddr
			if html {
				app = AppendNetAddrHTML
			}
			out, err := app([]byte(`"`), a.IP, a.Port, a.Zone)
			if err != nil {
				t.Fatal(err)
			}
			var s string
			if err := json.Unmarshal(out, &s); err != nil || s != a.String() {
				t.Errorf("zone %q html=%v: wrote %s, reads %q (%v), want %q", zone, html, out, s, err, a.String())
			}
			// Only the HTML variant escapes <, > and &.
			if raw := bytes.ContainsAny(out, "<>&"); raw != (!html && strings.ContainsAny(zone, "<>&")) {
				t.Errorf("zone %q html=%v: wrote %s", zone, html, out)
			}
		}
	}
}

// refParseNetAddr is ParseNetAddr's documented grammar spelled with
// netip.ParseAddrPort.
func refParseNetAddr(s string) (net.TCPAddr, bool) {
	if s == "" {
		return net.TCPAddr{}, true
	}
	if s[0] == ':' {
		p, err := strconv.ParseUint(s[1:], 10, 16)
		return net.TCPAddr{Port: int(p)}, err == nil
	}
	ap, err := netip.ParseAddrPort(s)
	a := ap.Addr()
	if err != nil || a.Zone() != "" && a.Is4In6() {
		return net.TCPAddr{}, false
	}
	return net.TCPAddr{IP: a.WithZone("").Unmap().AsSlice(), Port: int(ap.Port()), Zone: a.Zone()}, true
}

// netTextCorpus is every short string over an alphabet of the separators,
// hand-picked edge cases, and seeded random strings.
func netTextCorpus(alphabet string, n int, extra ...string) []string {
	out := []string{""}
	level := []string{""}
	for range n {
		var next []string
		for _, p := range level {
			for _, c := range alphabet {
				next = append(next, p+string(c))
			}
		}
		out = append(out, next...)
		level = next
	}
	r := rand.New(rand.NewPCG(1, 2))
	const pool = "0123456789abcdefABCDEF:.[]%/x"
	for range 50000 {
		b := make([]byte, 1+r.IntN(48))
		for i := range b {
			b[i] = pool[r.IntN(len(pool))]
		}
		out = append(out, string(b))
	}
	return append(out, extra...)
}

func TestParseNetAddr_matchesNetip(t *testing.T) {
	t.Parallel()
	for _, s := range netTextCorpus("1:[]%.f", 5,
		"1.2.3.4:80", "1.2.3.4:65535", "1.2.3.4:65536", "1.2.3.4:080", "1.2.3.4:+80", "1.2.3.4:",
		"[::1]:80", "[::1]", "::1:80", "[1.2.3.4]:80", "[::ffff:1.2.3.4]:80", "[fe80::1%eth0]:80",
		"[fe80::1%]:80", "1.2.3.4%eth0:80", "fe80::1%eth0:80", "[fe80::1%a:b]:1", "[fe80::1%a]b]:1",
		":80", ":", ":99999", ":65536", ":0", ":080", "::80", "[]:80", "[:80", "[%eth0]:80",
		"localhost:80", "1.2.3.4:http", "01.2.3.4:1", "[::ffff:1.2.3.4%eth0]:80", "[1.2.3.4%eth0]:80",
		"[0:0:0:0:0:ffff:102:304%x]:1", "[::1:ffff:1.2.3.4%x]:1", "[ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff%z]:65535",
	) {
		ip, port, zone, err := ParseNetAddr(s)
		want, ok := refParseNetAddr(s)
		if (err == nil) != ok {
			t.Fatalf("%q: err %v, reference accepts=%v", s, err, ok)
		}
		if !ok {
			continue
		}
		if !bytes.Equal(ip, want.IP) || port != want.Port || zone != want.Zone {
			t.Fatalf("%q: %v %d %q, want %v", s, ip, port, zone, want)
		}
		// Whatever parses must write back to text that parses the same.
		out, err := AppendNetAddr([]byte(`"`), ip, port, zone)
		var text string
		if err == nil {
			err = json.Unmarshal(out, &text)
		}
		ip2, port2, zone2, err2 := ParseNetAddr(text)
		if err != nil || err2 != nil || !bytes.Equal(ip, ip2) || port != port2 || zone != zone2 {
			t.Fatalf("%q: wrote %s (%v), reads %v %d %q (%v)", s, out, err, ip2, port2, zone2, err2)
		}
	}
}

func TestParseIPNet_matchesNetip(t *testing.T) {
	t.Parallel()
	for _, s := range netTextCorpus("1:./%f", 5,
		"10.0.0.0/8", "10.1.2.3/8", "10.0.0.0/08", "10.0.0.0/33", "fe80::/10%eth0", "fe80::%eth0/10",
		"::ffff:1.2.3.4/96", "::ffff:1.2.3.4/120", "2001:db8::/33", "::/0", "::/129", "1.2.3.4/32/1",
	) {
		ip, mask, err := ParseIPNet(s)
		p, perr := netip.ParsePrefix(s)
		if s == "" {
			perr = nil
		}
		if (err == nil) != (perr == nil) {
			t.Fatalf("%q: err %v, netip %v", s, err, perr)
		}
		if err != nil || s == "" {
			continue
		}
		bits := p.Addr().BitLen()
		if want := p.Masked().Addr().AsSlice(); !bytes.Equal(ip, want) || !bytes.Equal(mask, net.CIDRMask(p.Bits(), bits)) {
			t.Fatalf("%q: %x/%x", s, ip, mask)
		}
		// Whatever parses writes back what String prints, and that parses to
		// the same network. A mapped prefix comes back in IPv4 form, as
		// String prints it.
		out, err := AppendIPNet([]byte(`"`), ip, mask)
		var text string
		if err == nil {
			err = json.Unmarshal(out, &text)
		}
		ip2, mask2, err2 := ParseIPNet(text)
		n, n2 := net.IPNet{IP: ip, Mask: mask}, net.IPNet{IP: ip2, Mask: mask2}
		if err != nil || err2 != nil || text != n.String() || n2.String() != n.String() {
			t.Fatalf("%q: wrote %s (%v), reads %v (%v), String %v", s, out, err, n2, err2, n)
		}
	}
}

// The error quotes the rejected text and holds its own copy of it, from
// every place the parsers build one.
func TestAddrError_quotesAndDetaches(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"10.0.0.0/99\n\x1b[2J", "fe80::%eth0/10\n", ":99999\n", "1.2.3.4:http\x1b", "[fe80::1%]:1\r"} {
		buf := []byte(s)
		in := BytesToString(buf)
		var err error
		if strings.Contains(s, "/") {
			_, _, err = ParseIPNet(in)
		} else {
			_, _, _, err = ParseNetAddr(in)
		}
		ae, ok := errors.AsType[*AddrError](err)
		if !ok || ae.Text != s {
			t.Fatalf("%q: %v, want an *AddrError holding the text", s, err)
		}
		msg := err.Error()
		if strings.ContainsAny(msg, "\n\r\x1b") || !strings.Contains(msg, strconv.Quote(s)) {
			t.Errorf("%q: Error() = %q", s, msg)
		}
		for i := range buf {
			buf[i] = 'X'
		}
		if ae.Text != s {
			t.Errorf("%q: Text changed with the input buffer: %q", s, ae.Text)
		}
	}
}

func TestAppendAny_netTypes(t *testing.T) {
	t.Parallel()
	_, n, _ := net.ParseCIDR("10.0.0.0/8")
	tcp := &net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 80}
	type myAddr net.TCPAddr
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
		{&tcp, `"1.2.3.4:80"`},
		{[]net.UDPAddr{{Port: 1}, {}}, `[":1",""]`},
		{[1]*net.IPNet{n}, `["10.0.0.0/8"]`},
		{map[string]net.TCPAddr{"a": *tcp}, `{"a":"1.2.3.4:80"}`},
		{[]any{*n, tcp}, `["10.0.0.0/8","1.2.3.4:80"]`},
		// A pointer to a zero value writes "", which omitempty drops.
		{struct {
			A *net.TCPAddr `json:"a,omitempty"`
			B net.IPNet    `json:"b"`
		}{A: &net.TCPAddr{}, B: *n}, `{"b":"10.0.0.0/8"}`},
		{net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 1, Zone: "<"}, `"[fe80::1%<]:1"`},
		{net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 1, Zone: `"`}, `"[fe80::1%\"]:1"`},
		// A named type over one is a struct of its own, written as one.
		{myAddr{Port: 1}, `{"IP":"","Port":1,"Zone":""}`},
	} {
		out, err := AppendAny(nil, c.v)
		if err != nil || string(out) != c.want {
			t.Errorf("%T: %s (%v), want %s", c.v, out, err, c.want)
		}
	}
	h, err := AppendAnyHTML(nil, net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 1, Zone: "<"})
	if err != nil || strings.Contains(string(h), "<") || !strings.Contains(string(h), "u003c") {
		t.Errorf("html: %s, %v", h, err)
	}
	if _, err := AppendAny(nil, net.TCPAddr{Port: 70000}); err == nil {
		t.Error("port out of range accepted")
	}
}

// The reflect walk writes and sizes net values in place instead of boxing a
// copy of each struct, and a zone that needs no escaping is not copied.
func TestAppendAny_netNoAlloc(t *testing.T) { //nolint:paralleltest // measures allocations
	v4 := net.TCPAddr{IP: net.IPv4(1, 2, 3, 4).To4(), Port: 80}
	zoned := net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 1, Zone: "eth0"}
	_, n, _ := net.ParseCIDR("10.0.0.0/8")
	dst := make([]byte, 0, 512)
	walks := []struct {
		name string
		run  func(any)
	}{
		{"AppendAny", func(v any) { _, _ = AppendAny(dst[:0], v) }},
		{"AppendAnyHTML", func(v any) { _, _ = AppendAnyHTML(dst[:0], v) }},
		{"AnySize", func(v any) { _ = AnySize(v) }},
		{"AnySizeHTML", func(v any) { _ = AnySizeHTML(v) }},
	}
	values := []struct {
		name string
		v    any
	}{
		{"[]TCPAddr", []net.TCPAddr{v4, zoned, v4, zoned}},
		{"[]*TCPAddr", []*net.TCPAddr{&v4, &zoned, &v4, &zoned}},
		{"*IPNet", n},
		{"*TCPAddr", &v4},
		{"zoned TCPAddr", zoned},
		{"zoned *UDPAddr", &net.UDPAddr{IP: zoned.IP, Port: 1, Zone: "eth0"}},
	}
	// A map allocates its key and value holders once, whatever its size.
	addrs, nets := map[string]net.TCPAddr{}, map[string]*net.IPNet{}
	for i := range 8 {
		addrs[strconv.Itoa(i)] = zoned
		nets[strconv.Itoa(i)] = n
	}
	maps := []struct {
		name       string
		one, eight any
	}{
		{"map[string]TCPAddr", map[string]net.TCPAddr{"a": zoned}, addrs},
		{"map[string]*IPNet", map[string]*net.IPNet{"a": n}, nets},
	}
	for _, w := range walks {
		for _, c := range values {
			if got := testing.AllocsPerRun(100, func() { w.run(c.v) }); got != 0 {
				t.Errorf("%s(%s): %v allocs, want 0", w.name, c.name, got)
			}
		}
		for _, m := range maps {
			one := testing.AllocsPerRun(100, func() { w.run(m.one) })
			eight := testing.AllocsPerRun(100, func() { w.run(m.eight) })
			if one != eight {
				t.Errorf("%s(%s): %v allocs for one entry, %v for eight", w.name, m.name, one, eight)
			}
		}
	}
}

// The runtime reads these fields by index, because it does not import net.
func TestNetLayout(t *testing.T) {
	t.Parallel()
	for typ, want := range map[reflect.Type][]string{
		reflect.TypeFor[net.IPNet]():   {"IP:slice", "Mask:slice"},
		reflect.TypeFor[net.TCPAddr](): {"IP:slice", "Port:int", "Zone:string"},
		reflect.TypeFor[net.UDPAddr](): {"IP:slice", "Port:int", "Zone:string"},
	} {
		var got []string
		for f := range typ.Fields() {
			got = append(got, f.Name+":"+f.Type.Kind().String())
		}
		if !slices.Equal(got, want) || netKindOf(typ) == netNone {
			t.Errorf("%v: fields %v, want %v", typ, got, want)
		}
	}
	if k := netKindOf(reflect.TypeFor[net.IPAddr]()); k != netNone {
		t.Errorf("net.IPAddr has no string form, got kind %d", k)
	}
}

// Importing net would link it, with its cgo resolver, into every program that
// uses the runtime, whether or not it has a net field.
func TestRuntimeDoesNotImportNet(t *testing.T) {
	t.Parallel()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	out, err := exec.CommandContext(t.Context(), gobin, "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for dep := range strings.FieldsSeq(string(out)) {
		if dep == "net" || dep == "runtime/cgo" {
			t.Errorf("the runtime package depends on %s", dep)
		}
	}
}

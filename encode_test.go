package ggen

import (
	"bytes"
	"encoding/json"
	"math"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestAppendFloatStdlibParity checks that float output matches the standard
// library. encoding/json v1 and v2 both use JavaScript (ES6) style. They use
// plain decimal notation while the decimal exponent is in [-6, 21) and
// exponent notation otherwise. A negative exponent is not zero-padded, so it
// is "1e-7", not "1e-07". Every row is also checked against encoding/json v1
// so the table cannot go wrong unnoticed.
func TestAppendFloatStdlibParity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v    float64
		bits int
		want string
	}{
		{1e6, 64, "1000000"},
		{123456789, 64, "123456789"},
		{1e20, 64, "100000000000000000000"},
		{1e21, 64, "1e+21"},
		{1e-6, 64, "0.000001"},
		{1e-7, 64, "1e-7"},
		{-1e-7, 64, "-1e-7"},
		{0.1, 64, "0.1"},
		{0, 64, "0"},
		{math.MaxFloat64, 64, "1.7976931348623157e+308"},
		{5e-324, 64, "5e-324"},
		{float64(float32(3.4e38)), 32, "3.4e+38"},
		{float64(float32(1e7)), 32, "10000000"},
		{float64(float32(1e-7)), 32, "1e-7"},
	}
	for _, c := range cases {
		got, err := AppendFloat(nil, c.v, c.bits)
		if err != nil {
			t.Errorf("AppendFloat(%v, %d): %v", c.v, c.bits, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("AppendFloat(%v, %d) = %s, want %s", c.v, c.bits, got, c.want)
		}
		var sv []byte
		if c.bits == 32 {
			sv, _ = json.Marshal(float32(c.v))
		} else {
			sv, _ = json.Marshal(c.v)
		}
		if string(sv) != c.want {
			t.Errorf("table drift: stdlib emits %s for %v/%d, table says %s", sv, c.v, c.bits, c.want)
		}
	}
}

// fatItem is a minimal Marshaler whose JSONSize depends on its content. The
// zero value reports 2 bytes and a filled item reports much more. This
// catches MarshalSlice sizing its buffer from the zero value.
type fatItem struct{ s string }

func (f fatItem) JSONSize() int { return 2 + 2*len(f.s) }
func (f fatItem) AppendJSON(dst []byte) ([]byte, error) {
	dst = append(dst, '"')
	return AppendStringNoHTML(dst, f.s), nil
}

// TestMarshalSlicePointerElems checks that MarshalSlice works with a pointer
// element type. It must not call JSONSize on a nil *T, which would panic. Nil
// elements must marshal as JSON null.
func TestMarshalSlicePointerElems(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("MarshalSlice([]*T) panicked: %v", r)
		}
	}()
	a, b := fatItem{s: "a"}, fatItem{s: "b"}
	got, err := MarshalSlice([]*fatItem{&a, nil, &b})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `["a",null,"b"]` {
		t.Errorf("MarshalSlice = %s, want [\"a\",null,\"b\"]", got)
	}
}

// TestMarshalSliceSingleAlloc checks that the output buffer is sized from
// the sum of the items' real JSONSize, not the zero value's. Then marshaling
// does one allocation instead of growing the buffer again and again.
func TestMarshalSliceSingleAlloc(t *testing.T) { //nolint:paralleltest // measures allocations
	items := make([]fatItem, 64)
	for i := range items {
		items[i] = fatItem{s: strings.Repeat("x", 100)}
	}
	allocs := testing.AllocsPerRun(50, func() {
		if _, err := MarshalSlice(items); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 1 {
		t.Errorf("MarshalSlice did %v allocs, want 1 (presized from zero-value JSONSize?)", allocs)
	}
}

// refEscapeAt, refNoHTML and refHTML are a simple reference escaper written
// with plain comparisons. AppendString and AppendStringNoHTML use a lookup
// table instead. TestAppendString_TableParity checks that both give the same
// bytes.
func refEscapeAt(dst []byte, s string, i, start int) (int, []byte) {
	if start < i {
		dst = append(dst, s[start:i]...)
	}
	switch c := s[i]; c {
	case '"':
		dst = append(dst, '\\', '"')
	case '\\':
		dst = append(dst, '\\', '\\')
	case '\n':
		dst = append(dst, '\\', 'n')
	case '\r':
		dst = append(dst, '\\', 'r')
	case '\t':
		dst = append(dst, '\\', 't')
	case '\b':
		dst = append(dst, '\\', 'b')
	case '\f':
		dst = append(dst, '\\', 'f')
	default:
		const hex = "0123456789abcdef"
		dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
	}
	return i + 1, dst
}

func refNoHTML(dst []byte, s string) []byte {
	start := 0
	for i := range len(s) {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		start, dst = refEscapeAt(dst, s, i, start)
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

func refHTML(dst []byte, s string) []byte {
	start := 0
	for i := range len(s) {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c != '<' && c != '>' && c != '&' {
			continue
		}
		start, dst = refEscapeAt(dst, s, i, start)
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

// TestAppendString_TableParity checks the table-based escapers against the
// reference for every single byte value and a few typical strings.
func TestAppendString_TableParity(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"", "hello world", "a\"b\\c", "tab\tnl\n", "<a>&</a>",
		"\x00\x1f\x7f", "ünïcödé", strings.Repeat("x", 200),
	}
	for b := range 256 {
		inputs = append(inputs, string([]byte{byte(b)}))
	}
	for _, in := range inputs {
		if got, want := AppendStringNoHTML(nil, in), refNoHTML(nil, in); !bytes.Equal(got, want) {
			t.Errorf("NoHTML(%q) = %q, want %q", in, got, want)
		}
		if got, want := AppendString(nil, in), refHTML(nil, in); !bytes.Equal(got, want) {
			t.Errorf("HTML(%q) = %q, want %q", in, got, want)
		}
	}
}

// MarshalSlice with an interface element type must write a nil element as
// null, like the standard library. It must not call a method on it and
// panic.
func TestMarshalSlice_NilInterfaceElem(t *testing.T) {
	t.Parallel()
	items := []Marshaler{nil, mtItem{}}
	out, err := MarshalSlice(items)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `[null,{}]` {
		t.Errorf("got %s, want [null,{}]", out)
	}
}

type mtItem struct{}

func (mtItem) AppendJSON(dst []byte) ([]byte, error) { return append(dst, '{', '}'), nil }
func (mtItem) JSONSize() int                         { return 2 }

// A nil slice marshals as null, like the standard library. An empty non-nil
// slice marshals as [].
func TestSliceWalkers_NilVsEmpty(t *testing.T) {
	t.Parallel()
	out, err := AppendSlice[mtItem]([]byte("x:"), nil)
	if err != nil || string(out) != "x:null" {
		t.Errorf("AppendSlice(nil) = %q, %v", out, err)
	}
	out, err = MarshalSlice[mtItem](nil)
	if err != nil || string(out) != "null" {
		t.Errorf("MarshalSlice(nil) = %q, %v", out, err)
	}
	out, err = MarshalSlice([]mtItem{})
	if err != nil || string(out) != "[]" {
		t.Errorf("MarshalSlice(empty) = %q, %v", out, err)
	}
}

// AppendUnixSeconds must be exact in the cases where float64(UnixNano())/1e9
// is not. That is outside the years UnixNano can hold as int64 (about 1678 to
// 2262) and at precision finer than 100ns.
func TestAppendUnixSeconds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		t    time.Time
		want string
	}{
		{time.Unix(0, 0), "0"},
		{time.Unix(978307200, 0), "978307200"},
		{time.Unix(978307200, 1), "978307200.000000001"},
		{time.Unix(978307200, 500000000), "978307200.5"},
		{time.Unix(-1, 500000000), "-0.5"},
		{time.Unix(-2, 250000000), "-1.75"},
		{time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), "-30610224000"},
		{time.Date(3000, 1, 1, 0, 0, 0, 123, time.UTC), "32503680000.000000123"},
	}
	for _, c := range cases {
		if got := string(AppendUnixSeconds(nil, c.t)); got != c.want {
			t.Errorf("%v: got %s, want %s", c.t, got, c.want)
		}
	}
}

// AppendNetipAddr writes the address and the closing quote. The caller
// writes the opening quote. The zone after '%' can hold any bytes, since
// ParseAddr accepts `%q"z`. Those bytes must be escaped, or the output is not
// valid JSON. Addresses without a zone are written as is. The output must
// match json.Marshal byte for byte, which escapes the same set of bytes.
func TestAppendNetipAddr(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"10.0.0.1",
		"2001:db8::1",
		"fe80::1%eth0",
		`fe80::1%q"z`,
		`fe80::1%a\b`,
		"fe80::1%z\x01n", // a control byte in the zone must be escaped
	} {
		a, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", raw, err)
		}
		got := string(AppendNetipAddr([]byte{'"'}, a))
		want, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("json.Marshal(%q): %v", raw, err)
		}
		if got != string(want) {
			t.Errorf("AppendNetipAddr(%q) = %s, want %s", raw, got, want)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("invalid JSON: %s", got)
		}
	}
}

// TestAppendURL checks AppendURL against (*url.URL).String for many URL
// shapes. The output must equal String() byte for byte, plus the closing
// quote that AppendURL writes. Generated code calls AppendURL instead of
// String() to avoid an allocation per call. Any difference would silently
// change the JSON output.
func TestAppendURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"scheme_host", "https://example.com"},
		{"scheme_host_port", "https://example.com:8443"},
		{"scheme_host_path", "https://example.com/api/v1"},
		{"scheme_host_path_query", "https://example.com/api?key=value&k2=v2"},
		{"with_fragment", "https://example.com/page#section"},
		{"force_query", "https://example.com/api?"},
		{"username_only", "https://alice@example.com/inbox"},
		{"user_pass", "https://alice:s3cret@example.com/inbox"},
		{"creds_with_percent", "https://us%20er:p%40ss@host.example/api"},
		{"unicode_path_frag", "https://приклад.укр/шлях/розділ?запит=значення#якір"},
		{"opaque_mailto", "mailto:user@example.com?subject=hi"},
		{"opaque_news", "news:comp.lang.go"},
		{"ipv6_host", "http://[2001:db8::1]:8080/v6"},
		{"path_dots", "https://example.com/./a/../b"},
		{"trailing_slash", "https://example.com/"},
		{"scheme_only", "scheme:"},
		{"query_with_space", "https://example.com/?q=hello+world&x=a%20b"},
		{"fragment_only", "https://example.com/#frag-with-dashes_and.dots"},
		{"plus_in_path", "https://example.com/a+b/c+d"},
		{"colon_in_path", "https://example.com/foo:bar"},
		{"at_in_path", "https://example.com/foo@bar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var u url.URL
			if c.raw != "" {
				p, err := url.Parse(c.raw)
				if err != nil {
					t.Fatalf("parse %q: %v", c.raw, err)
				}
				u = *p
			}
			want := u.String() + `"`
			got := string(AppendURL(nil, u))
			if got != want {
				t.Errorf("AppendURL(%q) mismatch\n want: %q\n  got: %q", c.raw, want, got)
			}
		})
	}
}

// TestAppendURL_AppendsNotOverwrites checks that AppendURL appends after the
// existing bytes in dst and keeps them. Generated code writes many fields
// into the same buffer.
func TestAppendURL_AppendsNotOverwrites(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("https://example.com/x")
	dst := []byte("prefix:")
	got := AppendURL(dst, *u)
	want := "prefix:" + u.String() + `"`
	if string(got) != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

// TestAppendURL_Construction covers URL values built by hand, not by Parse.
// They reach code paths that parsed URLs usually do not: empty Scheme and
// Host, ForceQuery without RawQuery, OmitHost, Opaque with a fragment, and a
// Fragment that needs escaping.
func TestAppendURL_Construction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		u    url.URL
	}{
		{"empty_url", url.URL{}},
		{"force_query_no_value", url.URL{Scheme: "https", Host: "ex.com", ForceQuery: true}},
		{"omit_host", url.URL{Scheme: "file", OmitHost: true, Path: "/tmp/x"}},
		{"opaque_with_fragment", url.URL{Scheme: "tel", Opaque: "+1-555-0100", Fragment: "main"}},
		{"raw_fragment_set", url.URL{Scheme: "https", Host: "ex.com", Fragment: "a b", RawFragment: "a%20b"}},
		{"fragment_with_special", url.URL{Scheme: "https", Host: "ex.com", Fragment: "<script>"}},
		{"path_no_host_with_colon_first_seg", url.URL{Path: "this:that"}},
		// A RawPath that is valid but does not encode Path is ignored. The
		// escaped Path is used instead, like EscapedPath does.
		{"stale_rawpath", url.URL{Scheme: "https", Host: "ex.com", Path: "/new", RawPath: "/old%20path"}},
		{"consistent_rawpath", url.URL{Scheme: "https", Host: "ex.com", Path: "/a b", RawPath: "/a%20b"}},
		{"malformed_rawpath", url.URL{Scheme: "https", Host: "ex.com", Path: "/a b", RawPath: "/a%2"}},
		// With a host, a '/' is added before a path that lacks one. This
		// also applies when RawPath or "*" is used, not only the escaped
		// Path.
		{"rawpath_no_leading_slash", url.URL{Scheme: "https", Host: "ex.com", Path: "a b", RawPath: "a%20b"}},
		{"asterisk_with_host", url.URL{Scheme: "http", Host: "ex.com", Path: "*"}},
		{"asterisk_no_host", url.URL{Path: "*"}},
		// Whether a path is present decides the '/' between host and path,
		// not its first byte. A path starting with NUL is still a path.
		{"nul_leading_path", url.URL{Scheme: "http", Host: "h", Path: "\x00x"}},
		{"nul_leading_rawpath", url.URL{Scheme: "http", Host: "h", Path: "\x00x", RawPath: "%00x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			want := c.u.String() + `"`
			got := string(AppendURL(nil, c.u))
			if got != want {
				t.Errorf("mismatch\n want: %q\n  got: %q", want, got)
			}
			// The output must be the same when dst already holds bytes.
			// The "./" prefix rule from RFC 3986 section 4.2 and every
			// length check must count from where AppendURL started, not
			// from the start of dst.
			pgot := string(AppendURL([]byte(`{"u":"`), c.u))
			if pgot != `{"u":"`+want {
				t.Errorf("prefixed mismatch\n want: %q\n  got: %q", `{"u":"`+want, pgot)
			}
		})
	}
}

// TestAppendURL_LongRandomized checks AppendURL on a few long URLs. For each
// parsed URL, AppendURL must write the same bytes as String().
func TestAppendURL_LongRandomized(t *testing.T) {
	t.Parallel()
	corpus := []string{
		"https://example.com/" + strings.Repeat("seg/", 32),
		"https://example.com/?" + strings.Repeat("k=v&", 32) + "last=1",
		"https://example.com/#" + strings.Repeat("frag-", 32),
		"https://user:" + strings.Repeat("a", 64) + "@example.com/",
	}
	for i, raw := range corpus {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("case %d parse: %v", i, err)
		}
		want := u.String() + `"`
		got := string(AppendURL(nil, *u))
		if got != want {
			t.Errorf("case %d mismatch\n want: %q\n  got: %q", i, want, got)
		}
	}
}

// A URL that ggen accepts can carry bytes that break JSON. String() copies
// RawQuery and Opaque as is, and Host may contain `"`. A RawFragment that
// does not match Fragment must lose to Fragment. AppendURL must write a valid
// JSON string, and its decoded value must still equal String().
func TestAppendURL_JSONSafety(t *testing.T) {
	t.Parallel()
	parse := func(raw string) url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return *u
	}
	cases := []struct {
		name string
		u    url.URL
	}{
		{"quote_in_query", parse(`http://x/?q="`)},
		{"backslash_in_query", parse(`http://x/?q=\a`)},
		{"quote_in_opaque", url.URL{Scheme: "tel", Opaque: `+1"555`}},
		{"quote_in_host", url.URL{Scheme: "https", Host: `a"b`}},
		{"stale_rawfragment", url.URL{Scheme: "https", Host: "x", Path: "/", Fragment: "new", RawFragment: "old%20frag"}},
		{"consistent_rawfragment", url.URL{Scheme: "https", Host: "x", Path: "/", Fragment: "a b", RawFragment: "a%20b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := append([]byte{'"'}, AppendURL(nil, c.u)...)
			if !json.Valid(got) {
				t.Fatalf("invalid JSON: %s", got)
			}
			var body string
			if err := json.Unmarshal(got, &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if want := c.u.String(); body != want {
				t.Errorf("body = %q, want String() %q", body, want)
			}
		})
	}
}

// Named map and func types with value-receiver marshal methods. Inside an
// interface, their nil value has a nil data pointer, just like a typed nil
// pointer. Calling the method on them is still safe, and the standard
// library does call it.
type nilMapJSON map[string]int

func (m nilMapJSON) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte(`"nil-map"`), nil
	}
	return []byte(`"map"`), nil
}

type nilFuncText func()

func (nilFuncText) MarshalText() ([]byte, error) { return []byte("fn"), nil }

// TestAppendAny_NilMapFuncNotNull checks that AppendAny calls the marshal
// method of a nil named map or func with a value receiver, like the standard
// library. isNilPtr must not treat their nil data pointer as a typed nil
// pointer. A typed nil pointer must still write null.
func TestAppendAny_NilMapFuncNotNull(t *testing.T) {
	t.Parallel()
	cases := []any{nilMapJSON(nil), nilFuncText(nil), (*nilMapJSON)(nil)}
	for _, v := range cases {
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("stdlib json.Marshal(%T): %v", v, err)
		}
		got, err := AppendAny(nil, v)
		if err != nil {
			t.Fatalf("AppendAny(%T): %v", v, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("AppendAny(%T) = %s, stdlib %s", v, got, want)
		}
	}
}

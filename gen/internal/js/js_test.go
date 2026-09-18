package js

import (
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand/v2"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

func TestIntRange(t *testing.T) {
	for _, c := range []struct {
		bits     int
		unsigned bool
		lo, hi   string
		ok       bool
	}{
		{8, false, "-128", "127", true},
		{16, true, "0", "65535", true},
		{32, false, "-2147483648", "2147483647", true},
		{64, true, "0", "9007199254740991", true},
		{64, false, "", "", false},
	} {
		lo, hi, ok := IntRange(c.bits, c.unsigned)
		if lo != c.lo || hi != c.hi || ok != c.ok {
			t.Errorf("IntRange(%d, %v) = %s %s %v", c.bits, c.unsigned, lo, hi, ok)
		}
	}
}

// matchGo evaluates expr, a JavaScript expression over the input s, for every
// input in Node and compares its string form with want.
func matchGo(t *testing.T, inputs []string, expr string, want func(s string) string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	quoted, _ := json.Marshal(inputs)
	script := Runtime() + "const out: string[] = [];\nfor (const s of " + string(quoted) + ") out.push(String(" + expr + "));\nconsole.log(JSON.stringify(out));\n"
	file := filepath.Join(t.TempDir(), "check.ts")
	if err := os.WriteFile(file, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, file)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	bad := 0
	for i, s := range inputs {
		if w := want(s); got[i] != w {
			if bad++; bad <= 30 {
				t.Errorf("%q: %s = %s, Go says %s", s, expr, got[i], w)
			}
		}
	}
}

func TestIPHelpersMatchGo(t *testing.T) {
	inputs := []string{
		"1.2.3.4", "255.255.255.255", "256.1.1.1", "01.2.3.4", "1.2.3", "1.2.3.4.5", "0.0.0.0",
		"::", "::1", "1::", "1:2:3:4:5:6:7:8", "1:2:3:4:5:6:7::", "1:2:3:4:5:6:7:8:9", "1::2::3",
		":1::2", "1:2:3:4:5:6:7:8::", "::ffff:1.2.3.4", "::ffff:01.2.3.4", "1:2:3:4:5:6:1.2.3.4",
		"1:2:3:4:5:6:7:1.2.3.4", "::1.2.3.4", "1.2.3.4::", "fe80::1%eth0", "fe80::1%", "1.2.3.4%eth0",
		"12345::1", "g::1", "FFFF::1", ":", ":::", "1:2:3:4:5:6::7:8", "1:2:3:4:5:6:7:8%zone",
		"::ffff:1.2.3.4%eth0", "10.0.0.0/8", "10.0.0.0/33", "::1/128", "::1/129", "1.2.3.4/08",
		"1.2.3.4/", "fe80::1%eth0/64", "", "1.2.3.-4", "::1:", "1:::2",
	}
	matchGo(t, inputs, "`${ggenIsIP(s)} ${ggenIsAddr(s)} ${ggenIsPrefix(s)}`", func(s string) string {
		_, addrErr := netip.ParseAddr(s)
		_, prefErr := netip.ParsePrefix(s)
		return fmt.Sprintf("%v %v %v", net.ParseIP(s) != nil, addrErr == nil, prefErr == nil)
	})
}

// enumerate returns every string over alphabet up to n characters long.
func enumerate(alphabet string, n int) []string {
	out := []string{""}
	for prev := out; n > 0; n-- {
		var next []string
		for _, p := range prev {
			for _, c := range alphabet {
				next = append(next, p+string(c))
			}
		}
		out = append(out, next...)
		prev = next
	}
	return out
}

var bigEdges = []string{
	"1e1000000", "1e1000001", "1.5e1000001", "1e-1000001", "1p10000000", "1p10000001", "0x1.8p10000000",
	"0x1.8p-9999998", "0b1.1p-9999999", "1e9223372036854775807", "1e9223372036854775808",
	"0e9223372036854775807", "0e9223372036854775808", "1e2147483646", "1e2147483647", "1e-2147483649",
	"1e-2147483650", "123456789e2147483620", "123456789e2147483621", "0.001e2147483649", "+Inf", "-inf",
	"INF", "Infinity", "1/0x0", "1/0_7", "1_000/3", "0x_1p4", "1__0", "0b102",
}

func TestBigFloatHelperMatchesGo(t *testing.T) {
	inputs := append(enumerate("01.eEpP+-_xIinf", 4), bigEdges...)
	matchGo(t, inputs, "ggenIsBigFloat(s)", func(s string) string {
		_, _, err := new(big.Float).Parse(s, 10)
		return fmt.Sprint(err == nil)
	})
}

func TestRationalHelperMatchesGo(t *testing.T) {
	inputs := append(enumerate("018a_x.+-pe/o", 4), bigEdges...)
	matchGo(t, inputs, "ggenIsRational(s)", func(s string) string {
		_, ok := new(big.Rat).SetString(s)
		return fmt.Sprint(ok)
	})
}

func TestURLHelperMatchesGo(t *testing.T) {
	inputs := append(enumerate("h:/%2[]@?#1.f", 4),
		"https://a.b/c", "http://%zz", "http://a:1:2", "mongodb://a:1,b:2", "http://[fe80::1%25en0]:80/p",
		"http://[fe80::1%25%65n0]", "http://[fe80::1%25%0a]", "http://[1.2.3.4]", "http://[::ffff:1.2.3.4]",
		"http://u:p@ss@h", "http://u%zz@h", "http://u^@h", "http://h%41", "http://h%c3%a9", "http://a b",
		"//h/p", "///p", "a:b/c", "a/b:c", "*", "x#%zz", "x#a b", "?q=%zz", "mailto:%zz", "http://h/%zz",
		"http://[::1]x", "http://[::1", "http://a[::1]", "http://h:80x", "\u007f", "a	b", "http://h/",
		"cache_object:foo/bar", "1a:b", ":x", "http://ü/", "http://[fe80::1%25ü]",
	)
	matchGo(t, inputs, "ggenParsesURL(s)", func(s string) string {
		_, err := url.Parse(s)
		return fmt.Sprint(err == nil)
	})
}

// TestTimeHelperMatchesGo formats random times in each layout, mutates them,
// and compares ggenIsTime with time.Parse.
func TestTimeHelperMatchesGo(t *testing.T) {
	layouts := []string{
		time.DateOnly, time.TimeOnly, time.DateTime, time.RFC3339, time.RFC3339Nano, time.Kitchen,
		time.ANSIC, time.UnixDate, time.RubyDate, time.RFC822, time.RFC850, time.RFC1123Z, time.Stamp,
		time.StampMicro, "2006-002", "__2 2006", "Jan _2 15:04:05.999", "02/01/06 03:04:05PM -07",
		"2006-01-02T15:04:05Z070000", "Monday January 2 2006 MST", "15:04,000", "x_2006", "1/2 3pm -07:00:00",
		"January 02, 2006 15:04:05.000000000 Z07:00:00", "Mon, 2 Jan 06",
	}
	rng := rand.New(rand.NewPCG(1, 2))
	zones := []*time.Location{time.UTC, time.FixedZone("CEST", 7200), time.FixedZone("", -12600), time.FixedZone("GMT+3", 10800)}
	const chars = "0123456789 :.,-+/ZTAPMamGUCSTJanFebxY_"
	var pairs [][2]string
	for _, layout := range layouts {
		for range 300 {
			tm := time.Date(rng.IntN(3000), time.Month(1+rng.IntN(12)), 1+rng.IntN(31), rng.IntN(24), rng.IntN(60), rng.IntN(60), rng.IntN(1e9), zones[rng.IntN(len(zones))])
			b := []byte(tm.Format(layout))
			for range rng.IntN(3) {
				i := rng.IntN(len(b) + 1)
				switch rng.IntN(3) {
				case 0:
					b = append(b[:i], append([]byte{chars[rng.IntN(len(chars))]}, b[i:]...)...)
				case 1:
					if i < len(b) {
						b = append(b[:i], b[i+1:]...)
					}
				default:
					if i < len(b) {
						b[i] = chars[rng.IntN(len(chars))]
					}
				}
			}
			pairs = append(pairs, [2]string{layout, string(b)})
		}
	}
	for _, s := range []string{"2020-02-29", "2021-02-29", "2020-13-01", "2020-00-10", "2020-04-31", "0000-02-29", "1900-02-29"} {
		pairs = append(pairs, [2]string{time.DateOnly, s})
	}
	for _, s := range []string{"2020-060", "2021-060", "2020-366", "2021-366", "2020-000", "2020-367"} {
		pairs = append(pairs, [2]string{"2006-002", s})
	}
	inputs := make([]string, len(pairs))
	for i, p := range pairs {
		inputs[i] = p[0] + "\x00" + p[1]
	}
	matchGo(t, inputs, `ggenIsTime(...(s.split("\0") as [string, string]))`, func(s string) string {
		layout, value, _ := strings.Cut(s, "\x00")
		_, err := time.Parse(layout, value)
		return fmt.Sprint(err == nil)
	})
}

func TestDurationHelperMatchesGo(t *testing.T) {
	inputs := []string{
		"0", "1h", "1h2m", "1h2m3s", "1.5h", ".5s", "1.s", "-1.5s", "+1h", "1ns", "1us", "1µs", "1μs",
		"1ms", "1m", "300ms", "-1.5h2m", "1d", "1w", "h", "s", "1", "", "-", "+", "1h ", " 1h", "1H",
		"1S", "01h", "1e3s", "9223372036854775807ns", "9223372036854775808ns", "2562047h47m16.854775807s",
		"2562047h47m16.854775808s", "-2562047h47m16.854775808s", "100000h", "0s", "0ms", "1.h", "1.0h",
		"1h0m0s", "1m1m", "1s1s", "1h-1m", "--1h", "1..5h", "1e1h",
	}
	matchGo(t, inputs, "ggenIsDuration(s)", func(s string) string {
		_, err := time.ParseDuration(s)
		return fmt.Sprint(err == nil)
	})
}

func TestBinaryHelpersMatchGo(t *testing.T) {
	inputs := append(enumerate("AQ=a1", 3),
		"AQID", "AQIDBA==", "AQIDBA", "AQIDB===", "-_8=", "_-8=", "-_8", "+/8=", "MFRGG===", "mfrgg===",
		"MFRGGZDF", "0102", "0x02", "abcdef", "ABCDEF", "0g", "012", "CO======", "CPNG====", "", "=",
		"AA==AA==", "A", "AA", "AAA", "AAAA", "AAAAA", "AAAAAA", "AAAAAAA", "AAAAAAAA", "V0======",
	)
	matchGo(t, inputs, "`${ggenBase64.test(s)} ${ggenBase64URL.test(s)} ${ggenBase32.test(s)} ${ggenBase32Hex.test(s)} ${ggenHex.test(s)} ${ggenDecoded(s, 6)} ${ggenDecoded(s, 5)}`",
		func(s string) string {
			ok := func(err error) bool { return err == nil }
			_, b64 := base64.StdEncoding.DecodeString(s)
			_, b64u := base64.URLEncoding.DecodeString(s)
			_, b32 := base32.StdEncoding.DecodeString(s)
			_, b32h := base32.HexEncoding.DecodeString(s)
			_, hexErr := hex.DecodeString(s)
			trimmed := strings.TrimRight(s, "=")
			return fmt.Sprintf("%v %v %v %v %v %d %d", ok(b64), ok(b64u), ok(b32), ok(b32h), ok(hexErr),
				len(trimmed)*6/8, len(trimmed)*5/8)
		})
}

func TestLengthHelpersMatchGo(t *testing.T) {
	inputs := []string{
		"", "a", "ab", "héé", "日本", "\U0001f600", "a\U0001f600b", "\u0085ab", "\ufeffab",
		"ΟΔΟΣ", "İ", "ß", "\u0000", "\u007f", "\u0080", "\u07ff", "\u0800", "\uffff",
	}
	matchGo(t, inputs, "`${ggenBytes(s)} ${ggenRunes(s)}`", func(s string) string {
		return fmt.Sprintf("%d %d", len(s), utf8.RuneCountInString(s))
	})
}

func TestCaseHelpersMatchGo(t *testing.T) {
	inputs := []string{
		"", "a", "A", "aB", "ΟΔΟΣ", "οδος", "ΣΣ",
		"ß", "İ", "ı", "I", "i", "  ab  ", "\u0085ab", "\ufeffab", "\u00a0a\u00a0",
		"\u2000a\u3000", "\ta\n", "日本", "\U0001f600", "Straße", "ǅ", "ǆ",
	}
	matchGo(t, inputs, "`${ggenToLower(s)}|${ggenToUpper(s)}|${ggenTrim(s)}`", func(s string) string {
		return strings.Map(unicode.ToLower, s) + "|" + strings.Map(unicode.ToUpper, s) + "|" + strings.TrimSpace(s)
	})
}

func TestNumberHelpersMatchGo(t *testing.T) {
	inputs := []string{
		"0", "-0", "1", "-1", "1.5", "1e2", "1E+2", "1e-2", "01", "1.", ".5", "+1", "1e", "e1", "",
		" 1", "1 ", "0.0", "-0.0", "1e400", "-1e400", "1e-400", "9007199254740993", "18446744073709551616",
		"9223372036854775807", "-9223372036854775808", "0x1f", "Infinity", "NaN", "1_000", "1e+308",
	}
	// The grammars themselves are pinned in situ by the differential lanes,
	// against what the generated decoder accepts; the range check is what has
	// a Go answer here. It runs only on what the grammar check already
	// accepted, so the corpus is the integers among the inputs.
	inputs = slices.DeleteFunc(inputs, func(s string) bool { return !decimal.MatchString(s) })
	matchGo(t, inputs, "ggenFits(s, -128n, 127n)", func(s string) string {
		n, ok := new(big.Int).SetString(s, 10)
		return fmt.Sprint(ok && n.Cmp(big.NewInt(-128)) >= 0 && n.Cmp(big.NewInt(127)) <= 0)
	})
}

func TestQuote(t *testing.T) {
	for _, c := range [][2]string{
		{"", `""`}, {"a", `"a"`}, {`a"b`, `"a\"b"`}, {`a\b`, `"a\\b"`}, {"a\nb", `"a\nb"`},
		{"a\tb", `"a\tb"`}, {"\u0000", `"\u0000"`}, {"\u001f", `"\u001f"`}, {"日本", `"日本"`},
	} {
		if got := Quote(c[0]); got != c[1] {
			t.Errorf("Quote(%q) = %s, want %s", c[0], got, c[1])
		}
	}
}

func TestIdents(t *testing.T) {
	for _, c := range []struct {
		in           string
		ident        bool
		safe, key    string
		prototypeMem bool
	}{
		{"user", true, "user", "user", false},
		{"User", true, "User", "User", false},
		{"_x$", true, "_x$", "_x$", false},
		{"$", true, "$", "$", false},
		{"", false, "_", `""`, false},
		{"1a", false, "_1a", `"1a"`, false},
		{"a-b", false, "a_b", `"a-b"`, false},
		{"a b", false, "a_b", `"a b"`, false},
		{"a,b", false, "a_b", `"a,b"`, false},
		{"class", false, "class", "class", false},
		{"if", false, "if", "if", false},
		{"Ünicode", true, "__nicode", "Ünicode", false},
		{"日本", true, "______", "日本", false},
		{"toString", true, "toString", "toString", true},
		{"constructor", true, "constructor", "constructor", true},
		{"hasOwnProperty", true, "hasOwnProperty", "hasOwnProperty", true},
	} {
		if got := IsIdent(c.in); got != c.ident {
			t.Errorf("IsIdent(%q) = %v", c.in, got)
		}
		if got := Ident(c.in); got != c.safe {
			t.Errorf("Ident(%q) = %q, want %q", c.in, got, c.safe)
		}
		if got := PropKey(c.in); got != c.key {
			t.Errorf("PropKey(%q) = %s, want %s", c.in, got, c.key)
		}
		if got := PrototypeMember(c.in); got != c.prototypeMem {
			t.Errorf("PrototypeMember(%q) = %v", c.in, got)
		}
	}
}

func TestDoc(t *testing.T) {
	for _, c := range [][3]string{
		{"", "", ""},
		{"one line", "", "/** one line */\n"},
		{"one line", "  ", "  /** one line */\n"},
		{"two\nlines", "", "/**\n * two\n * lines\n */\n"},
	} {
		if got := Doc(c[0], c[1]); got != c[2] {
			t.Errorf("Doc(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestFileStemAndCount(t *testing.T) {
	for _, c := range [][2]string{
		{"a/b/c.ts", "c"}, {"c.ts", "c"}, {"a/b.c.ts", "b"},
	} {
		if got := FileStem(c[0]); got != c[1] {
			t.Errorf("FileStem(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	for _, c := range [][3]string{
		{"1", "byte", "1 byte"}, {"2", "byte", "2 bytes"}, {"0", "rune", "0 runes"}, {"n", "entry", "n entries"},
	} {
		if got := Count(c[0], c[1]); got != c[2] {
			t.Errorf("Count(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
	if got := UTF16Len("\U0001f600a"); got != 3 {
		t.Errorf("UTF16Len = %d, want 3", got)
	}
}

// decimal is the integer spelling ggenUnsigned and ggenInteger accept.
var decimal = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)

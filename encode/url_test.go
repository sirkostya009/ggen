package encode

import (
	"net/url"
	"strings"
	"testing"
)

// TestAppendURL pins AppendURL byte-for-byte against
// (*url.URL).String for a representative cross-section of URL shapes.
// The two outputs must match exactly — ggen-emitted code uses
// AppendURL in place of String() to avoid the per-call alloc, so any
// divergence is a silent wire-format regression.
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
			want := u.String()
			got := string(AppendURL(nil, u))
			if got != want {
				t.Errorf("AppendURL(%q) mismatch\n want: %q\n  got: %q", c.raw, want, got)
			}
		})
	}
}

// TestAppendURL_AppendsNotOverwrites verifies the function appends to
// the existing dst tail rather than replacing it — generated codegen
// reuses the same buffer across many field emits.
func TestAppendURL_AppendsNotOverwrites(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("https://example.com/x")
	dst := []byte("prefix:")
	got := AppendURL(dst, *u)
	want := "prefix:" + u.String()
	if string(got) != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

// TestAppendURL_Construction covers values built programmatically
// (not via Parse) — exercises code paths the parser doesn't normally
// reach: empty Scheme + Host, ForceQuery without RawQuery, OmitHost,
// Opaque with fragment, raw Fragment that needs escaping.
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			want := c.u.String()
			got := string(AppendURL(nil, c.u))
			if got != want {
				t.Errorf("mismatch\n want: %q\n  got: %q", want, got)
			}
		})
	}
}

// TestAppendURL_LongRandomized stresses the appender against a
// generated batch of URLs (every parse-round-trip output of String
// should be appended back to the same bytes).
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
		want := u.String()
		got := string(AppendURL(nil, *u))
		if got != want {
			t.Errorf("case %d mismatch\n want: %q\n  got: %q", i, want, got)
		}
	}
}

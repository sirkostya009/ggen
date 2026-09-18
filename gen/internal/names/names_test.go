package names

import (
	"slices"
	"strings"
	"testing"
)

func TestLowerCamel(t *testing.T) {
	for _, c := range [][2]string{
		{"ID", "id"},
		{"URLPath", "urlPath"},
		{"CreatedAt", "createdAt"},
		{"A", "a"},
		{"AB", "ab"},
		{"ABc", "aBc"},
		{"X509Cert", "x509Cert"},
		{"", ""},
		{"lower", "lower"},
		{"HTTPSProxy", "httpsProxy"},
	} {
		if got := LowerCamel(c[0]); got != c[1] {
			t.Errorf("LowerCamel(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestWords(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"Admin", []string{"Admin"}},
		{"InProgress", []string{"In", "Progress"}},
		{"HTTPServer", []string{"HTTP", "Server"}},
		{"in_progress", []string{"in", "progress"}},
		{"in-progress", []string{"in", "progress"}},
		{"in progress", []string{"in", "progress"}},
		// A digit is not a word boundary: X509Cert stays one word.
		{"v2Beta", []string{"v2Beta"}},
		{"", nil},
		{"__", nil},
		{"A1B2", []string{"A1B2"}},
	} {
		if got := Words(c.in); !slices.Equal(got, c.want) {
			t.Errorf("Words(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIdent(t *testing.T) {
	for _, c := range []struct {
		in string
		ok bool
	}{
		{"a", true}, {"_a", true}, {"a1", true}, {"a_b", true}, {"Ünicode", true},
		{"", false}, {"1a", false}, {"a-b", false}, {"a b", false}, {"a.b", false},
	} {
		if got := Ident(c.in); got != c.ok {
			t.Errorf("Ident(%q) = %v", c.in, got)
		}
	}
}

func TestTitleAndJoin(t *testing.T) {
	for _, c := range [][2]string{{"", ""}, {"a", "A"}, {"ab", "Ab"}, {"Ab", "Ab"}, {"ünicode", "Ünicode"}} {
		if got := Title(c[0]); got != c[1] {
			t.Errorf("Title(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	if got := Join(Words("InProgress"), "_", strings.ToUpper); got != "IN_PROGRESS" {
		t.Errorf("Join = %q, want IN_PROGRESS", got)
	}
	if got := Join(nil, "_", strings.ToUpper); got != "" {
		t.Errorf("Join(nil) = %q", got)
	}
}

//go:build goexperiment.simd

package scan

import (
	"math/rand"
	"strings"
	"testing"
)

var stringTiers = []struct {
	name string
	fn   func([]byte, int) (string, int, error)
}{
	{"AVX", StringAVX},
	{"AVX2", StringAVX2},
	{"AVX512", StringAVX512},
}

// TestStringSIMD_Parity pins every tier byte-identical to scalar String:
// same value, same position, same error identity — across escape placement,
// control bytes, truncation, and vector-width phase alignment.
func TestStringSIMD_Parity(t *testing.T) {
	cases := [][]byte{
		[]byte(`""`), []byte(`"a"`), []byte(`"ab"`), []byte(`not a string`), {},
		[]byte(`"unterminated`), []byte(`"trailing\`), []byte(`"bad\u12`),
		[]byte(`"esc\nape"`), []byte(`"A😀"`), []byte(`"\q"`),
	}
	for _, n := range []int{1, 7, 15, 16, 17, 31, 32, 33, 63, 64, 65, 127, 500} {
		body := strings.Repeat("x", n)
		cases = append(cases,
			[]byte(`"`+body+`"`),
			[]byte(`"`+body),               // unterminated
			[]byte(`"`+body+`\n"`),         // escape at phase boundary
			[]byte(`"`+body+"\x01"+`"`),    // ctrl at phase boundary
			[]byte(`"`+body+`\n`+body+`"`), // escape mid-string
			[]byte(`"`+"\x1f"+body+`"`),    // ctrl first
			[]byte(`"a`+"\x01"+`b\nc"`),    // ctrl before escape
		)
	}
	rng := rand.New(rand.NewSource(1))
	for range 2000 {
		n := rng.Intn(200)
		b := make([]byte, n+2)
		b[0] = '"'
		for i := 1; i <= n; i++ {
			b[i] = byte(rng.Intn(130)) // bias into ctrl/quote/backslash space
		}
		b[n+1] = '"'
		cases = append(cases, b)
	}
	for _, tier := range stringTiers {
		for _, c := range cases {
			s1, p1, e1 := String(c, 0)
			s2, p2, e2 := tier.fn(c, 0)
			if e1 != e2 {
				t.Fatalf("%s %q: err %v vs %v", tier.name, c, e1, e2)
			}
			if s1 != s2 || p1 != p2 {
				t.Fatalf("%s %q: (%q,%d) vs (%q,%d)", tier.name, c, s1, p1, s2, p2)
			}
		}
	}
}

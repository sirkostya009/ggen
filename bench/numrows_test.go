package bench

import (
	jsonv2 "encoding/json/v2"
	"testing"

	"github.com/sirkostya009/ggen"
)

// BenchmarkNumRows_Marshal marshals NumRows. B/op of the ggen row is the
// buffer JSONSize reserves, so it shows how tight the number budgets are.
func BenchmarkNumRows_Marshal(b *testing.B) {
	codecs := []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"jsonv2", func() ([]byte, error) { return jsonv2.Marshal(NumRowsValue) }},
		{"ggen", func() ([]byte, error) { return ggen.Marshal(NumRowsValue) }},
	}
	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			out, err := c.fn()
			if err != nil {
				b.Fatal(err)
			}
			runBench(b, int64(len(out)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if _, err := c.fn(); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

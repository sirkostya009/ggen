package bench

import (
	jsonv2 "encoding/json/v2"
	"testing"

	"github.com/sirkostya009/ggen"
)

// BenchmarkCatchAll_Marshal encodes a struct whose `json:",embed"` catch-all
// holds 32 numeric values. It watches the per-entry cost of the catch-all
// loop. The jsonv2 rows are the control.
func BenchmarkCatchAll_Marshal(b *testing.B) {
	rows := []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"jsonv2_ints", func() ([]byte, error) { return jsonv2.Marshal(CatchAllIntsValue) }},
		{"ggen_ints", func() ([]byte, error) { return ggen.Marshal(CatchAllIntsValue) }},
		{"jsonv2_floats", func() ([]byte, error) { return jsonv2.Marshal(CatchAllFloatsValue) }},
		{"ggen_floats", func() ([]byte, error) { return ggen.Marshal(CatchAllFloatsValue) }},
	}
	for _, r := range rows {
		out, err := r.fn()
		if err != nil {
			b.Fatalf("%s: %v", r.name, err)
		}
		b.Run(r.name, func(b *testing.B) {
			runBench(b, int64(len(out)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if _, err := r.fn(); err != nil {
						b.Fatal(err)
					}
				})
		})
	}
}

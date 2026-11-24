package bench

import (
	jsonv2 "encoding/json/v2"
	"net/netip"
	"testing"

	"github.com/sirkostya009/ggen"
)

// BenchmarkAnyEncode_Marshal marshals a []any of 64 netip values through
// ggen.AppendAny, sizing the buffer with AnySize first, the way a generated
// `any` field is marshaled. jsonv2 is the control row.
func BenchmarkAnyEncode_Marshal(b *testing.B) {
	addrs := make([]any, 64)
	for i := range addrs {
		addrs[i] = netip.AddrFrom4([4]byte{10, 0, byte(i), 1})
		if i%2 == 0 {
			addrs[i] = netip.AddrPortFrom(netip.MustParseAddr("2001:db8::1"), uint16(8000+i))
		}
	}
	var v any = addrs
	codecs := []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"jsonv2", func() ([]byte, error) { return jsonv2.Marshal(v) }},
		{"ggen", func() ([]byte, error) { return ggen.AppendAny(make([]byte, 0, ggen.AnySize(v)), v) }},
	}
	for _, c := range codecs {
		b.Run("netip/"+c.name, func(b *testing.B) {
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

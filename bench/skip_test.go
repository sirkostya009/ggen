//go:build goexperiment.jsonv2

package bench

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/sirkostya009/ggen/scan"
)

// BenchmarkSkipHeavy_Unmarshal — decode of SkipEnvelope (one matched field)
// against a payload that is ~100% skipped content (a Mega-sized blob under
// an unknown key). The pretty variant is the same envelope json.Indent-ed —
// the whitespace-rich shape where a byte-stepping skip loop is detrimental.
// Reflection codecs skip unknown keys by default, so every row measures the
// codec's skip machinery.
func BenchmarkSkipHeavy_Unmarshal(b *testing.B) {
	var codecs = []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v SkipEnvelope; return jsonv2.Unmarshal(p, &v) }},
		// sonic skips via a simdjson-style structural bitmap. ConfigFastest
		// does not grammar-validate skipped content (missing colons/commas,
		// bad escapes, raw ctrl bytes, `truu` all pass); ConfigDefault checks
		// structure but still passes bad escapes + ctrl bytes in skipped
		// strings. ggen's skip validates all of it.
		{"sonic", func(p []byte) error { var v SkipEnvelope; return sonic.ConfigDefault.Unmarshal(p, &v) }},
		{"sonic_fast", func(p []byte) error { var v SkipEnvelope; return sonic.ConfigFastest.Unmarshal(p, &v) }},
		{"ggen", func(p []byte) error { _, _, err := SkipEnvelope{}.DecodeFrom(p); return err }},
		// Stream path: fresh 4 KiB buffer per decode so the skipped blob
		// genuinely streams through refill + compaction.
		{"ggen_stream", func(p []byte) error {
			var st scan.Stream
			st.Reset(bytes.NewReader(p), make([]byte, 0, 4096))
			_, err := SkipEnvelope{}.DecodeFromStream(&st)
			return err
		}},
	}
	payloads := []struct {
		name string
		data []byte
	}{
		{"compact", SkipPayload},
		{"pretty", SkipPayloadPretty},
	}
	for _, p := range payloads {
		for _, c := range codecs {
			b.Run(p.name+"/"+c.name, func(b *testing.B) {
				for range 8 {
					if err := c.fn(p.data); err != nil {
						b.Fatal(err)
					}
				}
				runBench(b, int64(len(p.data)),
					func() struct{} { return struct{}{} },
					func(_ *struct{}) {
						if err := c.fn(p.data); err != nil {
							b.Fatal(err)
						}
					},
				)
			})
		}
	}
}

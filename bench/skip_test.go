package bench

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"testing"

	"github.com/bytedance/sonic"

	"github.com/sirkostya009/ggen"
)

// BenchmarkSkipHeavy_Unmarshal decodes SkipEnvelope, which matches one field,
// from a payload that is almost all skipped content: a Mega-sized blob under
// an unknown key. The pretty variant is the same payload indented with
// json.Indent. That shape is full of whitespace, where a skip loop that steps
// one byte at a time is slow. The reflection-based libraries skip unknown
// keys by default, so every row measures that library's skip code.
func BenchmarkSkipHeavy_Unmarshal(b *testing.B) {
	codecs := []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v SkipEnvelope; return jsonv2.Unmarshal(p, &v) }},
		// sonic skips by building a bitmap of structural characters with
		// vector instructions (process 16, 32 or 64 bytes with one CPU
		// instruction), like simdjson. ConfigFastest does not check the JSON
		// grammar of skipped content. Missing colons or commas, bad escapes,
		// raw control bytes and `truu` all pass. ConfigDefault checks the
		// structure but still passes bad escapes and control bytes in skipped
		// strings. ggen's skip checks all of it.
		{"sonic", func(p []byte) error { var v SkipEnvelope; return sonic.ConfigDefault.Unmarshal(p, &v) }},
		{"sonic_fast", func(p []byte) error { var v SkipEnvelope; return sonic.ConfigFastest.Unmarshal(p, &v) }},
		{"ggen", func(p []byte) error { _, _, err := SkipEnvelope{}.DecodeFrom(p); return err }},
		// Stream path: a fresh 4 KiB buffer per decode, so the skipped blob
		// really streams through refills and buffer compaction.
		{"ggen_stream", func(p []byte) error {
			var st ggen.Stream
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

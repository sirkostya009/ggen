package bench

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sirkostya009/ggen"
)

// BenchmarkJSONNumber decodes and encodes a page of 256 quotes whose prices
// are json.Number fields. The encoding/json rows are the control: the type
// has no jsonv2 arshaler of its own.
func BenchmarkJSONNumber_Unmarshal(b *testing.B) {
	rows := []struct {
		name string
		fn   func([]byte) error
	}{
		{"stdjson", func(p []byte) error { var v QuoteBook; return json.Unmarshal(p, &v) }},
		{"ggen", func(p []byte) error { _, _, err := QuoteBook{}.DecodeFrom(p); return err }},
		{"ggen_stream", func(p []byte) error {
			var st ggen.Stream
			st.Reset(bytes.NewReader(p), make([]byte, 0, 4096))
			_, err := QuoteBook{}.DecodeFromStream(&st)
			return err
		}},
	}
	for _, r := range rows {
		if err := r.fn(JSONNumberPayload); err != nil {
			b.Fatalf("%s: %v", r.name, err)
		}
		b.Run(r.name, func(b *testing.B) {
			runBench(b, int64(len(JSONNumberPayload)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if err := r.fn(JSONNumberPayload); err != nil {
						b.Fatal(err)
					}
				})
		})
	}
}

func BenchmarkJSONNumber_Marshal(b *testing.B) {
	rows := []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"stdjson", func() ([]byte, error) { return json.Marshal(JSONNumberValue) }},
		{"ggen", func() ([]byte, error) { return ggen.Marshal(JSONNumberValue) }},
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

package bench

import (
	jsonv2 "encoding/json/v2"
	"testing"

	"github.com/sirkostya009/ggen"
)

// BenchmarkStamps_Unmarshal decodes an array of 256 Stamped values, three
// RFC 3339 times each. The utc rows carry 30-byte times and the offset rows
// 35-byte ones, which is the longest RFC 3339 text. jsonv2 decodes the same
// type through reflection and is the control row for each payload.
func BenchmarkStamps_Unmarshal(b *testing.B) {
	rows := []struct {
		name    string
		payload []byte
		fn      func([]byte) error
	}{
		{"utc/jsonv2", StampsUTCPayload, func(p []byte) error { var v []Stamped; return jsonv2.Unmarshal(p, &v) }},
		{"utc/ggen", StampsUTCPayload, func(p []byte) error { _, err := ggen.UnmarshalSlice[Stamped](p); return err }},
		{"offset/jsonv2", StampsOffsetPayload, func(p []byte) error { var v []Stamped; return jsonv2.Unmarshal(p, &v) }},
		{"offset/ggen", StampsOffsetPayload, func(p []byte) error { _, err := ggen.UnmarshalSlice[Stamped](p); return err }},
	}
	for _, c := range rows {
		b.Run(c.name, func(b *testing.B) {
			runBench(b, int64(len(c.payload)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if err := c.fn(c.payload); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

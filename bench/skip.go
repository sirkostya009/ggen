// SkipHeavy benchmark family (skip_test.go).

//go:generate ../ggen $GOFILE
package bench

import (
	"bytes"
	"encoding/json"
)

// SkipEnvelope is the type for BenchmarkSkipHeavy_Unmarshal. It matches one
// field, and ignoreunknown skips the rest of the payload, a Mega-sized blob.
// This measures ggen.SkipValue alone, on compact and on pretty-printed input
// that is full of whitespace.
//
//ggen:generate ignoreunknown
type SkipEnvelope struct {
	ID int64 `json:"id"`
}

// SkipPayload puts MegaPayload under an unknown key next to a matched one.
// SkipPayloadPretty is the same payload indented with json.Indent (2 spaces).
// That shape is full of whitespace, where a skip loop that steps one byte at
// a time is slow.
var (
	SkipPayload       []byte
	SkipPayloadPretty []byte
)

// This init needs MegaPayload, which mega.go's init sets. Init functions run
// in file name order, and mega.go sorts before skip.go.
func init() {
	SkipPayload = []byte(`{"id":123,"blob":` + string(MegaPayload) + `}`)
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, SkipPayload, "", "  "); err != nil {
		panic(err)
	}
	SkipPayloadPretty = pretty.Bytes()
}

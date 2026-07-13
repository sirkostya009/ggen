// SkipHeavy bench family (skip_test.go).

//go:generate ../ggen $GOFILE
package bench

import (
	"bytes"
	"encoding/json"
)

// SkipEnvelope drives BenchmarkSkipHeavy_Unmarshal: one matched field, the
// rest of the payload (a Mega-sized blob) is skipped via ignoreunknown —
// isolates scan.SkipValue over compact vs pretty-printed (whitespace-rich)
// input.
//
//ggen:generate ignoreunknown
type SkipEnvelope struct {
	ID int64 `json:"id"`
}

// SkipPayload wraps MegaPayload as an unknown key next to a matched one;
// SkipPayloadPretty is the same envelope json.Indent-ed (2-space) — the
// whitespace-rich shape where byte-stepping skip loops are detrimental.
var (
	SkipPayload       []byte
	SkipPayloadPretty []byte
)

// Depends on mega.go's init having set MegaPayload — init order follows
// lexical file order, and mega.go sorts before skip.go.
func init() {
	SkipPayload = []byte(`{"id":123,"blob":` + string(MegaPayload) + `}`)
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, SkipPayload, "", "  "); err != nil {
		panic(err)
	}
	SkipPayloadPretty = pretty.Bytes()
}

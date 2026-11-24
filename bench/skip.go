// SkipHeavy benchmark family (skip_test.go).

//go:generate ../ggen $GOFILE
package bench

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"

	"github.com/sirkostya009/ggen"
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

// SkipKeysDoc is the type for BenchmarkSkipKeys_Unmarshal. It matches two
// members of a wide flat object, and ignoreunknown skips the other 48, each a
// short key with a short value. This measures the cost of one skipped member,
// where SkipEnvelope measures the cost of one skipped byte.
//
//ggen:generate ignoreunknown
type SkipKeysDoc struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// SkipKeysPayload is 64 SkipKeysDoc objects in an array.
var SkipKeysPayload []byte

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
	doc := []byte(`{"id":123`)
	for i := range 48 {
		doc = append(doc, `,"attribute_`...)
		doc = strconv.AppendInt(doc, int64(i), 10)
		doc = append(doc, `":`...)
		doc = strconv.AppendInt(doc, int64(i*37), 10)
		if i == 23 {
			doc = append(doc, `,"name":"skipkeys"`...)
		}
	}
	doc = append(doc, '}')
	SkipKeysPayload = append(SkipKeysPayload, '[')
	for i := range 64 {
		if i > 0 {
			SkipKeysPayload = append(SkipKeysPayload, ',')
		}
		SkipKeysPayload = append(SkipKeysPayload, doc...)
	}
	SkipKeysPayload = append(SkipKeysPayload, ']')
	SkipPayload = []byte(`{"id":123,"blob":` + string(MegaPayload) + `}`)
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, SkipPayload, "", "  "); err != nil {
		panic(err)
	}
	SkipPayloadPretty = pretty.Bytes()
}

// Stamped is the type for BenchmarkStamps_Unmarshal. Three of its five fields
// are time.Time, so the time text scan and ParseRFC3339 dominate the decode.
//
//ggen:generate
type Stamped struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	SeenAt    time.Time `json:"seenAt"`
	Kind      string    `json:"kind"`
}

// StampsUTCPayload and StampsOffsetPayload are arrays of 256 Stamped values
// with nanosecond timestamps. In UTC each time is 30 bytes of text
// ("2026-11-16T17:26:00.123456789Z"). With a numeric zone it is 35
// ("2026-11-16T17:26:00.123456789+02:00"), the form time.Now() marshals to
// on a host that is not on UTC.
var (
	StampsUTCPayload    []byte
	StampsOffsetPayload []byte
)

func stampsPayload(loc *time.Location) []byte {
	evs := make([]Stamped, 256)
	for i := range evs {
		t := time.Date(2026, 11, 16, 17, 26, 0, 123456789+i, loc).Add(time.Duration(i) * time.Second)
		evs[i] = Stamped{ID: int64(i), CreatedAt: t, UpdatedAt: t.Add(time.Hour), SeenAt: t.Add(2 * time.Hour), Kind: "login"}
	}
	out, err := ggen.MarshalSlice(evs)
	if err != nil {
		panic(err)
	}
	return out
}

func init() {
	StampsUTCPayload = stampsPayload(time.UTC)
	StampsOffsetPayload = stampsPayload(time.FixedZone("", 2*3600))
}

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"errors"
	"strconv"
	"testing"

	"github.com/sirkostya009/ggen"
)

// Octet is an `=` alias of byte, so a field spelled Octet is a uint8.
type Octet = byte

// NarrowByteRune spells its narrow integers byte and rune, the builtin
// aliases of uint8 and int32, at every generated site.
//
//ggen:generate
type NarrowByteRune struct {
	B     byte            `json:"b"                pipe:"lte=10"`
	R     rune            `json:"r"`
	PB    *byte           `json:"pb"`
	PPR   **rune          `json:"ppr"`
	Runes []rune          `json:"runes"`
	Pair  [2]rune         `json:"pair"`
	RM    map[string]rune `json:"rm"`
	BM    map[string]byte `json:"bm"`
	RGB   [3]byte         `json:"rgb,format:array"`
	O     Octet           `json:"o"`
	QB    byte            `json:"qb,string"`
}

//ggen:generate
type NarrowByte byte

//ggen:generate
type NarrowRune rune

// NarrowPlatformInts holds the platform-sized int and uint, which are 32 bits
// wide on 386, arm and mips.
//
//ggen:generate
type NarrowPlatformInts struct {
	I  int             `json:"i"`
	U  uint            `json:"u"`
	PI *int            `json:"pi"`
	SI []int           `json:"si"`
	MU map[string]uint `json:"mu"`
	AI [2]int          `json:"ai"`
	QI int             `json:"qi,string"`
}

//ggen:generate
type NarrowCount int

type narrowCase struct {
	name, payload string
	reject        bool
}

func checkNarrow[T interface {
	ggen.Decoder[T]
	ggen.StreamDecoder[T]
}](t *testing.T, cases []narrowCase) {
	t.Helper()
	for _, c := range cases {
		for _, chunk := range []int{1, 7} {
			bErr, sErr := decodeBothChunked[T](c.payload, chunk)
			for path, err := range map[string]error{"bytes": bErr, "stream": sErr} {
				if c.reject && !errors.Is(err, ggen.ErrNumberOverflow) {
					t.Errorf("%s %s chunk=%d: err=%v, want ErrNumberOverflow", c.name, path, chunk, err)
				}
				if !c.reject && err != nil {
					t.Errorf("%s %s chunk=%d: unexpected err=%v", c.name, path, chunk, err)
				}
			}
		}
	}
}

// TestNarrowIntOverflow_ByteRune pins the overflow check on the byte and rune
// spellings, which take the uint8 and int32 bounds.
func TestNarrowIntOverflow_ByteRune(t *testing.T) {
	t.Parallel()
	checkNarrow[NarrowByteRune](t, []narrowCase{
		{"b_over", `{"b":266}`, true},
		{"b_ok", `{"b":10}`, false},
		{"r_over", `{"r":2147483648}`, true},
		{"r_under", `{"r":-2147483649}`, true},
		{"r_wrap", `{"r":4294967297}`, true},
		{"r_ok", `{"r":-2147483648}`, false},
		{"pb_over", `{"pb":256}`, true},
		{"ppr_over", `{"ppr":4294967297}`, true},
		{"runes_over", `{"runes":[1,4294967298]}`, true},
		{"pair_over", `{"pair":[1,2147483648]}`, true},
		{"rm_over", `{"rm":{"k":4294967299}}`, true},
		{"bm_over", `{"bm":{"k":257}}`, true},
		{"rgb_over", `{"rgb":[1,2,256]}`, true},
		{"rgb_ok", `{"rgb":[1,2,255]}`, false},
		{"o_over", `{"o":300}`, true},
		{"qb_over", `{"qb":"256"}`, true},
	})
	checkNarrow[NarrowByte](t, []narrowCase{{"alias_over", `1000`, true}, {"alias_ok", `255`, false}})
	checkNarrow[NarrowRune](t, []narrowCase{{"alias_over", `4294967297`, true}, {"alias_ok", `-5`, false}})
}

// TestNarrowIntOverflow_PlatformInts pins the overflow check on int and uint.
// A value past 32 bits is rejected only where int is 32 bits wide.
func TestNarrowIntOverflow_PlatformInts(t *testing.T) {
	t.Parallel()
	narrow := strconv.IntSize == 32
	checkNarrow[NarrowPlatformInts](t, []narrowCase{
		{"i_wrap", `{"i":4294967297}`, narrow},
		{"i_over", `{"i":2147483648}`, narrow},
		{"i_under", `{"i":-2147483649}`, narrow},
		{"i_max64", `{"i":9223372036854775807}`, narrow},
		{"i_ok", `{"i":-2147483648}`, false},
		{"u_over", `{"u":4294967296}`, narrow},
		{"u_ok", `{"u":4294967295}`, false},
		{"pi_over", `{"pi":4294967297}`, narrow},
		{"si_over", `{"si":[1,4294967298]}`, narrow},
		{"mu_over", `{"mu":{"k":4294967299}}`, narrow},
		{"ai_over", `{"ai":[4294967301,1]}`, narrow},
		{"qi_over", `{"qi":"4294967300"}`, narrow},
	})
	checkNarrow[NarrowCount](t, []narrowCase{{"alias_over", `4294967297`, narrow}, {"alias_ok", `7`, false}})
}

// EscapeHeavy bench family (escape_test.go).

//go:generate ../ggen $GOFILE
package bench

import "strings"

// EscapeDoc drives BenchmarkEscapeHeavy_Unmarshal: string fields whose payload
// values are escape-dense (two-char escapes, \uXXXX, surrogate pairs), so the
// decode exercises the escape/unescape path (scan.stringSlow, \uXXXX + surrogate
// assembly) — the arm the asciiLetters-only Mega/Small/Account payloads never
// touch. ggen's DecodeFrom is not a stdlib interface, so jsonv2/sonic decode the
// same type via reflection (no easyjson-style method leakage).
//
//ggen:generate
type EscapeDoc struct {
	A string `json:"a"`
	B string `json:"b"`
	C string `json:"c"`
	D string `json:"d"`
}

// CopyEscapeDoc is EscapeDoc under -copy (`//ggen:generate copy`) — the
// ggen_copy row of BenchmarkEscapeHeavy_Unmarshal. On escaped strings copy mode
// runs scan.String → stringSlow (owned scratch) then strings.Clone (a second
// alloc): the escape path already owns its bytes, so the clone is redundant but
// documented -copy overhead. The row quantifies that double-copy vs the aliasing
// ggen row.
//
//ggen:generate copy
type CopyEscapeDoc struct {
	A string `json:"a"`
	B string `json:"b"`
	C string `json:"c"`
	D string `json:"d"`
}

// EscapeHeavyPayload — EscapeDoc with escape-dense string values (~12% of
// bytes are escapes: \n \" \\ \uXXXX + a surrogate pair). Forces the
// unescape path on every string field.
var EscapeHeavyPayload []byte

func init() {
	// Escape-dense string — the raw-string backslashes ARE JSON escape sequences:
	// two-char (\n \" \\), a BMP \uXXXX (é), and a surrogate pair (😀). ~5 escapes
	// per ~44 bytes → the unescape path (stringSlow, \uXXXX + surrogate assembly)
	// runs on every field. ~5 KiB/field, cache-resident.
	escUnit := "word\\ntext\\\"quo\\\\te\\u00e9x\\ud83d\\ude00yz"
	esc := strings.Repeat(escUnit, 120)
	EscapeHeavyPayload = []byte(`{"a":"` + esc + `","b":"` + esc + `","c":"` + esc + `","d":"` + esc + `"}`)
}

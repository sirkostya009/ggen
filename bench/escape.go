// EscapeHeavy benchmark family (escape_test.go).

//go:generate ../ggen $GOFILE
package bench

import "strings"

// EscapeDoc is the type for BenchmarkEscapeHeavy_Unmarshal. Its string values
// are full of escapes: two-char escapes, \uXXXX escapes and surrogate pairs
// (two \uXXXX escapes that together encode one character above U+FFFF). So
// the decode runs the unescape path (scan.stringSlow, including \uXXXX and
// surrogate pair handling). The Mega, Small and Account payloads use only
// letters and digits and never reach it. ggen's DecodeFrom is not a standard
// library interface, so jsonv2 and sonic decode the same type through
// reflection and cannot call ggen's methods by accident.
//
//ggen:generate
type EscapeDoc struct {
	A string `json:"a"`
	B string `json:"b"`
	C string `json:"c"`
	D string `json:"d"`
}

// CopyEscapeDoc is EscapeDoc under -copy (`//ggen:generate copy`), for the
// ggen_copy row of BenchmarkEscapeHeavy_Unmarshal. Copy mode copies a string
// only when it aliases the input (points into it, no copy), through
// ggen.Detach. An escaped string is already a new allocation from stringSlow,
// so it is not copied again. On this payload the row should cost the same as
// the aliasing ggen row.
//
//ggen:generate copy
type CopyEscapeDoc struct {
	A string `json:"a"`
	B string `json:"b"`
	C string `json:"c"`
	D string `json:"d"`
}

// EscapeHeavyPayload is an EscapeDoc whose strings are full of escapes. About
// 12% of the bytes are escapes: \n, \", \\, \uXXXX and a surrogate pair. Every
// string field goes through the unescape path.
var EscapeHeavyPayload []byte

// EscapeSparsePayload is an EscapeDoc with strings shaped like prose. Runs of
// about 90 bytes without escapes are separated by single escapes. That is the
// density real documents have, a quote or newline every sentence or two,
// unlike EscapeHeavy's artificial 12%. Both payloads are needed, because the
// unescape loop's cost depends on how long the runs of plain bytes are. A
// change can speed up one shape and slow down the other.
var EscapeSparsePayload []byte

func init() {
	// A string full of escapes. The backslashes in this Go string are JSON
	// escape sequences: two-char ones (\n, \", \\), a single \uXXXX (é), and
	// a surrogate pair (😀). With about 5 escapes per 44 bytes, every field
	// goes through the unescape path. Each field is about 5 KiB and stays in
	// CPU cache.
	escUnit := "word\\ntext\\\"quo\\\\te\\u00e9x\\ud83d\\ude00yz"
	esc := strings.Repeat(escUnit, 120)
	EscapeHeavyPayload = []byte(`{"a":"` + esc + `","b":"` + esc + `","c":"` + esc + `","d":"` + esc + `"}`)

	// About 90 plain bytes per escape (one \" and one \n per two sentences).
	// Each field is about 5 KiB, like EscapeHeavy, so the two rows can be
	// compared directly.
	sparseUnit := `The quick brown fox jumps over the lazy dog near the riverbank at dawn, ` +
		`and it said \"hello\" to nobody in particular.\n`
	sparse := strings.Repeat(sparseUnit, 42)
	EscapeSparsePayload = []byte(`{"a":"` + sparse + `","b":"` + sparse + `","c":"` + sparse + `","d":"` + sparse + `"}`)
}

package bench

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"testing"

	"github.com/bytedance/sonic"

	"github.com/sirkostya009/ggen"
)

// BenchmarkEscapeHeavy_Unmarshal decodes EscapeDoc from strings full of
// escapes. About 12% of the bytes are escapes: \n, \", \\, \uXXXX and
// surrogate pairs. This runs the unescape path (scan.stringSlow, \uXXXX and
// surrogate pair handling, the scratch buffer allocation), which the
// escape-free Mega, Small and Account payloads never reach. Without this
// row, a slowdown in the unescape path, or a broken copy-once change for
// escaped strings, would go unnoticed.
//
// Reading the rows: this benchmark mainly guards ggen's unescape path. It is
// not a race between libraries. The checks below compare ggen's output on
// the bytes and stream paths against jsonv2, so the unescape path is verified,
// not just timed. The sonic rows are only context. sonic handles escapes
// differently and ConfigFastest skips some validation, so a fast sonic number
// here does not mean it did the same work. The same caveat applies to the
// skip benchmarks. jsonv2 is the fair baseline, because like ggen it must
// fully unescape every string to fill the fields.
func BenchmarkEscapeHeavy_Unmarshal(b *testing.B) {
	var want EscapeDoc
	if err := jsonv2.Unmarshal(EscapeHeavyPayload, &want); err != nil {
		b.Fatalf("jsonv2 reference decode: %v", err)
	}
	// ggen must unescape exactly like the jsonv2 reference. Broken escape
	// handling fails here instead of reporting a misleadingly fast number.
	if g, _, err := (EscapeDoc{}).DecodeFrom(EscapeHeavyPayload); err != nil || g != want {
		b.Fatalf("ggen bytes escape decode wrong: err=%v equal=%v", err, g == want)
	}
	var vs ggen.Stream
	vs.Reset(bytes.NewReader(EscapeHeavyPayload), make([]byte, 0, 512))
	if g, err := (EscapeDoc{}).DecodeFromStream(&vs); err != nil || g != want {
		b.Fatalf("ggen stream escape decode wrong: err=%v equal=%v", err, g == want)
	}
	// ggen_copy: same unescaped values, and no link to the input. After the
	// decode the input is overwritten, and the decoded strings must not
	// change.
	cpSrc := append([]byte(nil), EscapeHeavyPayload...)
	cg, _, err := CopyEscapeDoc{}.DecodeFrom(cpSrc)
	if err != nil || EscapeDoc(cg) != want {
		b.Fatalf("ggen_copy escape decode wrong: err=%v equal=%v", err, EscapeDoc(cg) == want)
	}
	for i := range cpSrc {
		cpSrc[i] = 'Z'
	}
	if EscapeDoc(cg) != want {
		b.Fatal("ggen_copy escape strings aliased input — corrupted after source scribble")
	}

	codecs := []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v EscapeDoc; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v EscapeDoc; return sonic.ConfigDefault.Unmarshal(p, &v) }},
		{"sonic_fast", func(p []byte) error { var v EscapeDoc; return sonic.ConfigFastest.Unmarshal(p, &v) }},
		{"ggen", func(p []byte) error { _, _, err := EscapeDoc{}.DecodeFrom(p); return err }},
		{"ggen_copy", func(p []byte) error { _, _, err := CopyEscapeDoc{}.DecodeFrom(p); return err }},
		{"ggen_stream", func(p []byte) error {
			var s ggen.Stream
			s.Reset(bytes.NewReader(p), make([]byte, 0, 512))
			_, err := EscapeDoc{}.DecodeFromStream(&s)
			return err
		}},
	}
	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			runBench(b, int64(len(EscapeHeavyPayload)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if err := c.fn(EscapeHeavyPayload); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

// BenchmarkEscapeSparse_Unmarshal is the EscapeHeavy benchmark at the escape
// density of prose. Plain runs between escapes are about 90 bytes long
// instead of about 7. The unescape loop's cost depends on run length, so the
// two benchmarks cover both ends. A change that helps one can slow the other,
// so judge a change on both.
func BenchmarkEscapeSparse_Unmarshal(b *testing.B) {
	var want EscapeDoc
	if err := jsonv2.Unmarshal(EscapeSparsePayload, &want); err != nil {
		b.Fatalf("jsonv2 reference decode: %v", err)
	}
	if g, _, err := (EscapeDoc{}).DecodeFrom(EscapeSparsePayload); err != nil || g != want {
		b.Fatalf("ggen bytes escape decode wrong: err=%v equal=%v", err, g == want)
	}
	var vs ggen.Stream
	vs.Reset(bytes.NewReader(EscapeSparsePayload), make([]byte, 0, 512))
	if g, err := (EscapeDoc{}).DecodeFromStream(&vs); err != nil || g != want {
		b.Fatalf("ggen stream escape decode wrong: err=%v equal=%v", err, g == want)
	}

	codecs := []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v EscapeDoc; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v EscapeDoc; return sonic.ConfigDefault.Unmarshal(p, &v) }},
		{"sonic_fast", func(p []byte) error { var v EscapeDoc; return sonic.ConfigFastest.Unmarshal(p, &v) }},
		{"ggen", func(p []byte) error { _, _, err := EscapeDoc{}.DecodeFrom(p); return err }},
		{"ggen_copy", func(p []byte) error { _, _, err := CopyEscapeDoc{}.DecodeFrom(p); return err }},
		{"ggen_stream", func(p []byte) error {
			var s ggen.Stream
			s.Reset(bytes.NewReader(p), make([]byte, 0, 512))
			_, err := EscapeDoc{}.DecodeFromStream(&s)
			return err
		}},
	}
	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			runBench(b, int64(len(EscapeSparsePayload)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if err := c.fn(EscapeSparsePayload); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

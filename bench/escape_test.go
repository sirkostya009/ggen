//go:build goexperiment.jsonv2

package bench

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/sirkostya009/ggen/scan"
)

// BenchmarkEscapeHeavy_Unmarshal decodes EscapeDoc from escape-dense strings
// (~12% of bytes are escapes: \n \" \\ \uXXXX + surrogate pairs), exercising the
// unescape path — scan.stringSlow, \uXXXX + surrogate assembly, scratch alloc —
// that the escape-free Mega/Small/Account payloads never touch. Without
// this row an escape-path regression (or a broken single-copy escape change)
// ships silently.
//
// Reading the rows: this is primarily a ggen escape-path GUARD, not a race. The
// correctness block below pins ggen's own output (bytes + stream) against the
// jsonv2 reference, so the escape path is verified, not just timed. The sonic
// rows are context only — sonic's escape handling differs (ConfigFastest is lax
// on validation), so a fast sonic number here does NOT mean it did equivalent
// work (same caveat as the skip tier). jsonv2 is the honest baseline: it, like
// ggen, must fully unescape to fill the fields.
func BenchmarkEscapeHeavy_Unmarshal(b *testing.B) {
	var want EscapeDoc
	if err := jsonv2.Unmarshal(EscapeHeavyPayload, &want); err != nil {
		b.Fatalf("jsonv2 reference decode: %v", err)
	}
	// ggen must unescape identically to the stdlib reference — a broken escape
	// arm would fail here rather than post a misleadingly-fast number.
	if g, _, err := (EscapeDoc{}).DecodeFrom(EscapeHeavyPayload); err != nil || g != want {
		b.Fatalf("ggen bytes escape decode wrong: err=%v equal=%v", err, g == want)
	}
	var vs scan.Stream
	vs.Reset(bytes.NewReader(EscapeHeavyPayload), make([]byte, 0, 512))
	if g, err := (EscapeDoc{}).DecodeFromStream(&vs); err != nil || g != want {
		b.Fatalf("ggen stream escape decode wrong: err=%v equal=%v", err, g == want)
	}
	// ggen_copy: same unescaped values AND decoupled from the source — scribble
	// the input after decode, the retained escape-path strings must survive.
	cpSrc := append([]byte(nil), EscapeHeavyPayload...)
	cg, _, err := CopyEscapeDoc{}.DecodeFrom(cpSrc)
	if err != nil || (EscapeDoc{cg.A, cg.B, cg.C, cg.D}) != want {
		b.Fatalf("ggen_copy escape decode wrong: err=%v equal=%v", err, EscapeDoc{cg.A, cg.B, cg.C, cg.D} == want)
	}
	for i := range cpSrc {
		cpSrc[i] = 'Z'
	}
	if (EscapeDoc{cg.A, cg.B, cg.C, cg.D}) != want {
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
			var s scan.Stream
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

// BenchmarkEscapeSparse_Unmarshal is EscapeHeavy's counterpart at prose escape
// density — long escape-free runs between escapes (~90 B) instead of ~7 B. The
// unescape loop picks its per-byte vs bulk-copy arm by run length, so the two
// rows bracket the tradeoff; a change that helps one can cost the other, and
// neither row alone is the verdict.
func BenchmarkEscapeSparse_Unmarshal(b *testing.B) {
	var want EscapeDoc
	if err := jsonv2.Unmarshal(EscapeSparsePayload, &want); err != nil {
		b.Fatalf("jsonv2 reference decode: %v", err)
	}
	if g, _, err := (EscapeDoc{}).DecodeFrom(EscapeSparsePayload); err != nil || g != want {
		b.Fatalf("ggen bytes escape decode wrong: err=%v equal=%v", err, g == want)
	}
	var vs scan.Stream
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
			var s scan.Stream
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

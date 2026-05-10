//go:build goexperiment.jsonv2

package bench

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"io"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/mailru/easyjson"
	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
)

func BenchmarkMega_jsonv2_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(MegaPayload)))
	for b.Loop() {
		var v Node
		if err := jsonv2.Unmarshal(MegaPayload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMega_Sonic_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(MegaPayload)))
	for b.Loop() {
		var v Node
		if err := sonic.Unmarshal(MegaPayload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMega_easyjson_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(MegaPayload)))
	for b.Loop() {
		var v Node
		if err := v.UnmarshalJSON(MegaPayload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMega_ggen_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(MegaPayload)))
	for b.Loop() {
		if _, err := decode.Unmarshal[Node](MegaPayload); err != nil {
			b.Fatal(err)
		}
	}
}

// --- marshal ----------------------------------------------------------------

func BenchmarkMega_jsonv2_Marshal(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		out, err := jsonv2.Marshal(MegaValue)
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(out)))
	}
}

func BenchmarkMega_Sonic_Marshal(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		out, err := sonic.Marshal(MegaValue)
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(out)))
	}
}

func BenchmarkMega_easyjson_Marshal(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		out, err := MegaValue.MarshalJSON()
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(out)))
	}
}

func BenchmarkMega_ggen_Marshal(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		out, err := encode.Marshal(MegaValue)
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(out)))
	}
}

// --- streaming (io.Reader input) -------------------------------------------

func BenchmarkMega_jsonv2_UnmarshalRead(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(MegaPayload)))
	var r bytes.Reader
	for b.Loop() {
		r.Reset(MegaPayload)
		var v Node
		if err := jsonv2.UnmarshalRead(&r, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMega_Sonic_Reader(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(MegaPayload)))
	var r bytes.Reader
	for b.Loop() {
		r.Reset(MegaPayload)
		dec := sonic.ConfigDefault.NewDecoder(&r)
		var v Node
		if err := dec.Decode(&v); err != nil {
			b.Fatal(err)
		}
	}
}

// easyjson.UnmarshalFromReader is io.ReadAll + decode — not true streaming,
// but it's the library's recommended io.Reader entry point, so it goes head
// to head with our FastStream here.
func BenchmarkMega_easyjson_UnmarshalFromReader(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(MegaPayload)))
	var r bytes.Reader
	for b.Loop() {
		r.Reset(MegaPayload)
		var v Node
		if err := easyjson.UnmarshalFromReader(&r, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMega_ggen_UnmarshalStream(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(MegaPayload)))
	var r bytes.Reader
	buf := make([]byte, 0, 4196)
	for b.Loop() {
		r.Reset(MegaPayload)
		var err error
		if _, buf, err = decode.UnmarshalStream[Node](&r, buf[:0]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMega_ggen_ReadAllUnmarshal — io.ReadAll into a fresh slice
// then bytes-path Unmarshal. Same shape as easyjson.UnmarshalFromReader
// internally. Aliases strings into the read buffer (zero-copy bytes path),
// no overlap of I/O with parse.
func BenchmarkMega_ggen_ReadAllUnmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(MegaPayload)))
	var r bytes.Reader
	for b.Loop() {
		r.Reset(MegaPayload)
		data, err := io.ReadAll(&r)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := decode.Unmarshal[Node](data); err != nil {
			b.Fatal(err)
		}
	}
}

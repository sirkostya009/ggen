//go:build goexperiment.jsonv2

package main

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/sirkostya009/ggen/decode"
)

// --- complex payload ---------------------------------------------------------

func BenchmarkJSONv2_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(complexPayload)))
	for b.Loop() {
		var v SomePayloadRequestStruct
		if err := jsonv2.Unmarshal(complexPayload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSonic_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(complexPayload)))
	for b.Loop() {
		var v SomePayloadRequestStruct
		if err := sonic.Unmarshal(complexPayload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSonicFastest_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(complexPayload)))
	for b.Loop() {
		var v SomePayloadRequestStruct
		if err := sonic.ConfigFastest.Unmarshal(complexPayload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJSONv2_UnmarshalAndValidate(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(complexPayload)))
	for b.Loop() {
		var v SomePayloadRequestStruct
		if err := jsonv2.Unmarshal(complexPayload, &v); err != nil {
			b.Fatal(err)
		}
		if err := validateComplex(&v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGenerated_Read(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(complexPayload)))
	var r bytes.Reader
	for b.Loop() {
		r.Reset(complexPayload)
		if _, err := decode.Read[SomePayloadRequestStruct](&r); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGenerated_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(complexPayload)))
	for b.Loop() {
		if _, err := decode.Unmarshal[SomePayloadRequestStruct](complexPayload); err != nil {
			b.Fatal(err)
		}
	}
}

// --- simple payload ----------------------------------------------------------

func BenchmarkSimple_JSONv2_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(simplePayload)))
	for b.Loop() {
		var v AnotherStruct
		if err := jsonv2.Unmarshal(simplePayload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSimple_Sonic_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(simplePayload)))
	for b.Loop() {
		var v AnotherStruct
		if err := sonic.Unmarshal(simplePayload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSimple_JSONv2_UnmarshalAndValidate(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(simplePayload)))
	for b.Loop() {
		var v AnotherStruct
		if err := jsonv2.Unmarshal(simplePayload, &v); err != nil {
			b.Fatal(err)
		}
		if err := validateSimple(&v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSimple_Generated_Read(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(simplePayload)))
	var r bytes.Reader
	for b.Loop() {
		r.Reset(simplePayload)
		if _, err := decode.Read[AnotherStruct](&r); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSimple_Generated_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(simplePayload)))
	for b.Loop() {
		if _, err := decode.Unmarshal[AnotherStruct](simplePayload); err != nil {
			b.Fatal(err)
		}
	}
}

// --- mega payload: ~1 MB deep Node tree ------------------------------------

func BenchmarkMega_JSONv2_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(megaPayload)))
	for b.Loop() {
		var v Node
		if err := jsonv2.Unmarshal(megaPayload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMega_Sonic_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(megaPayload)))
	for b.Loop() {
		var v Node
		if err := sonic.Unmarshal(megaPayload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMega_Generated_Unmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(megaPayload)))
	for b.Loop() {
		if _, err := decode.Unmarshal[Node](megaPayload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMega_Generated_Read(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(megaPayload)))
	var r bytes.Reader
	for b.Loop() {
		r.Reset(megaPayload)
		if _, err := decode.Read[Node](&r); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMega_Generated_UnmarshalStream(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(megaPayload)))
	var r bytes.Reader
	buf := make([]byte, 0, 4196)
	for b.Loop() {
		r.Reset(megaPayload)
		var err error
		if _, buf, err = decode.UnmarshalStream[Node](&r, buf); err != nil {
			b.Fatal(err)
		}
	}
}

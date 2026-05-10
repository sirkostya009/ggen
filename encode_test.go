//go:build goexperiment.jsonv2

package main

import (
	jsonv2 "encoding/json/v2"
	"io"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/sirkostya009/ggen/encode"
)

func BenchmarkMarshal_JSONv2(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		out, err := jsonv2.Marshal(complexValue)
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(out)))
	}
}

func BenchmarkMarshal_Sonic(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		out, err := sonic.Marshal(complexValue)
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(out)))
	}
}

func BenchmarkMarshal_SonicFastest(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		out, err := sonic.ConfigFastest.Marshal(complexValue)
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(out)))
	}
}

func BenchmarkMarshal_Generated_Bytes(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		out, _ := encode.Marshal(complexValue)
		b.SetBytes(int64(len(out)))
	}
}

func BenchmarkMarshal_Generated_String(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		out, _ := encode.MarshalString(complexValue)
		b.SetBytes(int64(len(out)))
	}
}

func BenchmarkMarshal_Generated_Write(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if err := encode.Write(io.Discard, complexValue); err != nil {
			b.Fatal(err)
		}
	}
}

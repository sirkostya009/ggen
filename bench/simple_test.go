package bench

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"io"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/mailru/easyjson"

	"github.com/sirkostya009/ggen"
)

// BenchmarkNoAlloc_Unmarshal decodes a wide, flat record of scalar fields
// (Account) on the bytes path. No field forces an allocation, so ggen decodes
// with zero allocations. This measures only the scan, key dispatch and
// assignment loop.
func BenchmarkNoAlloc_Unmarshal(b *testing.B) {
	codecs := []struct {
		name string
		fn   func([]byte) error
	}{
		{"jsonv2", func(p []byte) error { var v Account; return jsonv2.Unmarshal(p, &v) }},
		{"sonic", func(p []byte) error { var v Account; return sonic.Unmarshal(p, &v) }},
		{"sonic_fast", func(p []byte) error { var v Account; return sonic.ConfigFastest.Unmarshal(p, &v) }},
		{"easyjson", func(p []byte) error { var v EasyAccount; return v.UnmarshalJSON(p) }},
		{"ggen", func(p []byte) error { _, _, err := Account{}.DecodeFrom(p); return err }},
		// ggen_copy: -copy mode (CopyAccount). The bytes path copies strings
		// out of the input, so it allocates. This measures the copy cost.
		{"ggen_copy", func(p []byte) error { _, _, err := CopyAccount{}.DecodeFrom(p); return err }},
	}
	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			for range 64 {
				if err := c.fn(AccountPayload); err != nil {
					b.Fatal(err)
				}
			}
			runBench(b, int64(len(AccountPayload)),
				func() struct{} { return struct{}{} },
				func(_ *struct{}) {
					if err := c.fn(AccountPayload); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

// BenchmarkNoAlloc_Reader decodes the Account payload from an io.Reader. The
// stream path copies strings out of its reused buffer, so it allocates.
// ggen_stream starts with a fresh 512-byte buffer, which forces refills and
// compaction. ggen_readall reads everything with io.ReadAll and then uses the
// bytes path.
func BenchmarkNoAlloc_Reader(b *testing.B) {
	type readerState struct{ r bytes.Reader }
	codecs := []struct {
		name string
		fn   func(*readerState) error
	}{
		{"jsonv2", func(s *readerState) error {
			s.r.Reset(AccountPayload)
			var v Account
			return jsonv2.UnmarshalRead(&s.r, &v)
		}},
		{"sonic", func(s *readerState) error {
			s.r.Reset(AccountPayload)
			var v Account
			return sonic.ConfigDefault.NewDecoder(&s.r).Decode(&v)
		}},
		{"easyjson", func(s *readerState) error {
			s.r.Reset(AccountPayload)
			var v EasyAccount
			return easyjson.UnmarshalFromReader(&s.r, &v)
		}},
		// A fresh 512-byte buffer per iteration, smaller than the payload, so
		// the stream really refills and compacts. A buffer kept across
		// iterations would grow to the payload size and act like the bytes
		// path.
		{"ggen_stream", func(s *readerState) error {
			s.r.Reset(AccountPayload)
			var st ggen.Stream
			st.Reset(&s.r, make([]byte, 0, 512))
			_, err := Account{}.DecodeFromStream(&st)
			return err
		}},
		{"ggen_readall", func(s *readerState) error {
			s.r.Reset(AccountPayload)
			data, err := io.ReadAll(&s.r)
			if err != nil {
				return err
			}
			_, _, err = Account{}.DecodeFrom(data)
			return err
		}},
	}
	for _, c := range codecs {
		b.Run(c.name, func(b *testing.B) {
			var warm readerState
			for range 64 {
				if err := c.fn(&warm); err != nil {
					b.Fatal(err)
				}
			}
			runBench(b, int64(len(AccountPayload)),
				func() readerState { return readerState{} },
				func(s *readerState) {
					if err := c.fn(s); err != nil {
						b.Fatal(err)
					}
				},
			)
		})
	}
}

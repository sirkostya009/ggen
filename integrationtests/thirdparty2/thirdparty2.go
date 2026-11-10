// Package thirdparty2 holds ggen-annotated structs in their own package. It
// tests the cross-package fast path: code generated in the main package calls
// this package's generated methods directly instead of using encoding/json.
package thirdparty2

import "time"

//go:generate ../../ggen .

// External2 is generated in a separate ggen run. The main package finds its
// generated methods through type information and calls DecodeFrom and
// AppendJSON directly.
//
//ggen:generate marshal unmarshal
type External2 struct {
	Key   string `json:"key"   pipe:"required minlen=1"`
	Value int    `json:"value" pipe:"gte=0"`
}

// Wrap has a hand-written UnmarshalJSON that calls External2's generated
// decoder, validation included. Wrap has no DecodeFrom of its own, so
// another package's generated code decodes it through its UnmarshalJSON
// method.
type Wrap struct{ In External2 }

func (w *Wrap) UnmarshalJSON(b []byte) error {
	v, _, err := External2{}.DecodeFrom(b)
	w.In = v
	return err
}

// Lease has no marshal hooks, and its Duration is written as a units string
// that only its generated decoder reads.
//
//ggen:generate
type Lease struct {
	TTL time.Duration `json:"ttl"`
}

// Grant and Quota each appear in one host type only (LeaseGrid and
// LeaseNulls), so nothing else in that package registers their methods.
//
//ggen:generate
type Grant struct {
	TTL time.Duration `json:"ttl"`
}

//ggen:generate
type Quota struct {
	TTL time.Duration `json:"ttl"`
}

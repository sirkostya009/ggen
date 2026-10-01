// Package thirdparty2 holds a ggen-annotated struct in its own package. It
// tests the cross-package fast path: code generated in the main package calls
// this package's generated methods directly instead of using encoding/json.
package thirdparty2

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

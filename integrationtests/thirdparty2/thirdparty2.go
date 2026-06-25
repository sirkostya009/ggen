// Package thirdparty2 holds a ggen-annotated struct in its own package to
// exercise the cross-package fast path: its generated decoder is picked up via
// the TryDecodeFast probe from the main package.
package thirdparty2

//go:generate ../../ggen .

// External2 lives in a different generation pass, so the main package emits the
// TryDecodeFast probe (not a direct DecodeFrom call) and finds the method.
//
//ggen:generate marshal unmarshal
type External2 struct {
	Key   string `json:"key" pipe:"required minlen=1"`
	Value int    `json:"value" pipe:"gte=0"`
}

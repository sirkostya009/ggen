// Package thirdparty2 holds a ggen-annotated struct in its own package so the
// main test suite can exercise the cross-package fast path: generated decoders
// here implement decode.TryDecodeFast's interface, so the main-package
// generator's fallback path picks them up at runtime.
package thirdparty2

// External2 is reachable from the main package via import, but lives in a
// different generation pass. The main package's generator can't emit a
// direct DecodeFrom call for it (isGenerated returns false there), so it
// emits the TryDecodeFast probe instead — which finds the method.
//
//ggen:generate marshal unmarshal
type External2 struct {
	Key   string `json:"key" ggen:"required,minlen=1"`
	Value int    `json:"value" ggen:"gte=0"`
}

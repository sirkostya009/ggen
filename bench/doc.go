// Package bench hosts the macro-benchmark types and payloads. Types live in
// non-test files so easyjson's bootstrap (which compiles the non-test build)
// can see them; each bench family keeps its types + values + payloads in the
// file matching its _test.go (mega.go ↔ mega_test.go, …).
//
// All values are built deterministically at init — no randomness, no clock —
// so every run parses byte-identical payloads. Multi-entry maps exist only
// in MapHeavy, whose payload is canonicalized after encode; every other map
// is single-entry, immune to Go's randomized iteration order.
package bench

import (
	"bytes"
	"encoding/json"

	"github.com/sirkostya009/ggen/encode"
)

func mustMarshal[T encode.Marshaler](v T) []byte {
	out, err := encode.Marshal(v)
	if err != nil {
		panic(err)
	}
	return out
}

// canonicalize round-trips a payload through encoding/json v1, which emits
// map keys sorted — freezing the entry order that encode.Marshal leaves to
// Go's randomized map iteration. UseNumber keeps numeric text verbatim.
// Needed only by MapHeavy (the one multi-entry-map payload); the cost is
// that ALL object keys sort (struct fields too), trading natural field
// order for byte-identical runs.
func canonicalize(b []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		panic(err)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'})
}

// mix is a fixed 64-bit bit-mixer (splitmix64 finalizer). Payload values keep
// full-entropy digits / blob bytes — 18-19-digit ints, 17-significant-digit
// floats — while being a pure function of the counter, identical every run.
func mix(n uint64) uint64 {
	n = (n ^ (n >> 30)) * 0xbf58476d1ce4e5b9
	n = (n ^ (n >> 27)) * 0x94d049bb133111eb
	return n ^ (n >> 31)
}

// gen hands out deterministic values off an incrementing counter. Distinct
// payloads seed distinct counter offsets so their contents don't collide.
type gen struct{ n uint64 }

func (g *gen) next() uint64 { g.n++; return mix(g.n) }

func (g *gen) intn(n int) int { return int(g.next() % uint64(n)) }

// f64 is uniform [0,1) with a full 53-bit mantissa (like rand.Float64), so
// float fields keep their 17-significant-digit wire form.
func (g *gen) f64() float64 { return float64(g.next()>>11) / (1 << 53) }

const asciiLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func (g *gen) str(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = asciiLetters[g.intn(len(asciiLetters))]
	}
	return string(b)
}

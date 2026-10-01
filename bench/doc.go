// Package bench holds the types and payloads for the end-to-end benchmarks.
// The types live in non-test files so easyjson's bootstrap step can see them,
// because it compiles only the non-test build. Each benchmark family keeps
// its types, values and payloads in the file that matches its _test.go file,
// for example mega.go and mega_test.go.
//
// All values are built the same way on every run at init, with no randomness
// and no clock, so every run parses byte-identical payloads. Only MapHeavy
// has maps with more than one entry, and its payload is put in a fixed order
// after encoding. Every other map has a single entry, so Go's random map
// iteration order cannot change it.
package bench

import (
	"bytes"
	"encoding/json"

	"github.com/sirkostya009/ggen"
)

func mustMarshal[T ggen.Marshaler](v T) []byte {
	out, err := ggen.Marshal(v)
	if err != nil {
		panic(err)
	}
	return out
}

// canonicalize round-trips a payload through encoding/json v1, which writes
// map keys in sorted order. This fixes the entry order that ggen.Marshal
// leaves to Go's random map iteration. UseNumber keeps number text exactly as
// written. Only MapHeavy needs this, because it is the only payload with
// multi-entry maps. The cost is that all object keys get sorted, struct
// fields included, so the natural field order is lost in exchange for
// byte-identical runs.
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

// mix is a fixed 64-bit bit mixer (the splitmix64 finalizer). Payload values
// look random, with 18-19 digit ints and floats with 17 significant digits,
// but they depend only on the counter, so they are the same on every run.
func mix(n uint64) uint64 {
	n = (n ^ (n >> 30)) * 0xbf58476d1ce4e5b9
	n = (n ^ (n >> 27)) * 0x94d049bb133111eb
	return n ^ (n >> 31)
}

// gen returns repeatable values derived from an incrementing counter.
// Different payloads start the counter at different offsets so their contents
// do not repeat each other.
type gen struct{ n uint64 }

func (g *gen) next() uint64 { g.n++; return mix(g.n) }

func (g *gen) intn(n int) int { return int(g.next() % uint64(n)) }

// f64 returns a uniform value in [0,1) with a full 53-bit mantissa, like
// rand.Float64, so float fields encode with 17 significant digits.
func (g *gen) f64() float64 { return float64(g.next()>>11) / (1 << 53) }

const asciiLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func (g *gen) str(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = asciiLetters[g.intn(len(asciiLetters))]
	}
	return string(b)
}

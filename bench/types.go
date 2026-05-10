// Package bench hosts the macro-benchmark types that need to be importable
// by non-test codegens (easyjson in particular — its bootstrap compiles the
// non-test build so test-only types are invisible).
//
// The mega payload used in these benchmarks is generated from Node at package
// init with a fixed seed — ~1 MiB, 6 levels deep, similar shape to what
// sonic & jsoniter benchmark against.
package bench

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/sirkostya009/ggen/encode"
)

// Addr is a small inline struct used as the pointed-to type for the
// `Refs []*Addr` and `Parent *Addr` fields on Node. Matches what real
// API responses tend to nest at depth-1.
//
//ggen:generate
//
//easyjson:json
type Addr struct {
	Street string `json:"street"`
	City   string `json:"city"`
}

// Node is the deep-tree benchmark target. The original shape mirrored
// sonic's bench struct (scalars + slices + maps + recursion). Expanded
// to exercise the breadth of ggen features so the benchmark stresses
// the codegen paths that real-world API responses actually hit:
// tuples, slices of pointers, nested slices, pointer fields,
// time/bytes/raw/any, and embedded validation. All shapes are also
// supported by jsonv2/sonic/easyjson for apples-to-apples comparison.
//
//ggen:generate
//
//easyjson:json
type Node struct {
	ID        int64             `json:"id" ggen:"required,gte=0"`
	Name      string            `json:"name" ggen:"required,minlen=1,maxlen=128"`
	Score     float64           `json:"score" ggen:"gte=0,lte=100"`
	Active    bool              `json:"active"`
	Tags      []string          `json:"tags" ggen:"maxlen=64,dive:minlen=1,maxlen=64"`
	Props     map[string]string `json:"props" ggen:"maxlen=64"`
	Children  []Node            `json:"children" ggen:"maxlen=16"`
	Coords    [2]float64        `json:"coords"`
	Refs      []*Addr           `json:"refs" ggen:"maxlen=16"`
	Matrix    [][]int           `json:"matrix" ggen:"maxlen=16,dive:maxlen=32"`
	Parent    *Addr             `json:"parent,omitzero"`
	CreatedAt time.Time         `json:"createdAt"`
	Blob      []byte            `json:"blob"`
	Extra     any               `json:"extra"`
	Raw       json.RawMessage   `json:"raw"`
}

// Validated exercises ggen's per-field validation rules. Designed for
// fail-fast streaming benchmarks: the alphabetically-first JSON field
// (Email after sort) is the one we corrupt to force early rejection.
//
//ggen:generate
type Validated struct {
	Email string `json:"email" ggen:"required,email"`
	Name  string `json:"name"  ggen:"required,minlen=1,maxlen=64"`
	Age   int    `json:"age"   ggen:"gte=0,lte=150"`
	Tags  []string `json:"tags" ggen:"dive:notempty,minlen=1,maxlen=32"`
	Bio   string `json:"bio"   ggen:"maxlen=4096"`
}

var (
	MegaValue   Node
	MegaPayload []byte

	// ValidPayload + InvalidPayload — short JSON bodies for fail-fast
	// streaming benchmarks. Both about the same size; the invalid one
	// fails on the first decoded field (email), so a streaming
	// decoder with per-field validation can reject after reading just
	// the prefix of the input.
	ValidPayload   []byte
	InvalidPayload []byte
)

func init() {
	r := rand.New(rand.NewSource(1))
	MegaValue = buildNode(r, 6, []int{5, 4, 3, 3, 3, 3, 0})
	var err error
	MegaPayload, err = encode.Marshal(MegaValue)
	if err != nil {
		panic(err)
	}

	// ~3 KiB body — typical small POST payload. Bio padded with random
	// content so the wire bytes meaningfully amount to something a
	// slow reader has to deliver in chunks.
	bio := randString(rand.New(rand.NewSource(3)), 2800)
	tags := []string{"alpha", "beta", "gamma", "delta"}
	ValidPayload, err = encode.Marshal(Validated{
		Email: "alice@example.com",
		Name:  "alice",
		Age:   30,
		Tags:  tags,
		Bio:   bio,
	})
	if err != nil {
		panic(err)
	}
	// Same shape, but Email is malformed — fails the `email` rule on
	// the very first decoded field (keys are emitted in sorted order,
	// "age" comes before "bio" before "email" alphabetically).
	// "age" passes, "bio" passes (just maxlen), then "email" trips.
	InvalidPayload, err = encode.Marshal(Validated{
		Email: "not-an-email",
		Name:  "alice",
		Age:   30,
		Tags:  tags,
		Bio:   bio,
	})
	if err != nil {
		panic(err)
	}
}

func buildNode(r *rand.Rand, depth int, fanout []int) Node {
	n := Node{
		ID:        r.Int63(),
		Name:      randString(r, 8+r.Intn(56)),
		Score:     r.Float64() * 100,
		Active:    r.Intn(2) == 0,
		Tags:      randTags(r, 6+r.Intn(20)),
		Props:     randProps(r, 6+r.Intn(20)),
		Coords:    [2]float64{r.Float64()*180 - 90, r.Float64()*360 - 180},
		Refs:      randAddrPtrs(r, 4+r.Intn(8)),
		Matrix:    randMatrix(r, 4+r.Intn(8)),
		Parent:    randAddrPtr(r, r.Intn(3) != 0), // ~2/3 of nodes have a parent
		CreatedAt: time.Unix(r.Int63n(1<<31), r.Int63n(1<<30)).UTC(),
		Blob:      randBytes(r, 32+r.Intn(192)),
		Extra:     randAny(r),
		Raw:       randRaw(r),
	}
	if depth > 0 && depth < len(fanout) {
		kids := fanout[len(fanout)-1-depth]
		if kids > 0 {
			n.Children = make([]Node, kids)
			for i := range n.Children {
				n.Children[i] = buildNode(r, depth-1, fanout)
			}
		}
	}
	return n
}

func randAddr(r *rand.Rand) Addr {
	return Addr{Street: randString(r, 8+r.Intn(16)), City: randString(r, 6+r.Intn(10))}
}

func randAddrPtr(r *rand.Rand, present bool) *Addr {
	if !present {
		return nil
	}
	a := randAddr(r)
	return &a
}

func randAddrPtrs(r *rand.Rand, n int) []*Addr {
	out := make([]*Addr, n)
	for i := range out {
		// every 4th element is nil to exercise the null branch in the slab path
		out[i] = randAddrPtr(r, i%4 != 0)
	}
	return out
}

func randMatrix(r *rand.Rand, rows int) [][]int {
	out := make([][]int, rows)
	for i := range out {
		cols := 4 + r.Intn(20)
		row := make([]int, cols)
		for j := range row {
			row[j] = r.Intn(1_000_000) - 500_000
		}
		out[i] = row
	}
	return out
}

func randBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Intn(256))
	}
	return b
}

// randAny synthesizes an `any` value. Mostly scalars or nil — real
// API "metadata" / "context" fields are scalar-dominated; nested any
// shapes show up but aren't the common case.
func randAny(r *rand.Rand) any {
	switch r.Intn(10) {
	case 0, 1, 2, 3:
		return nil
	case 4:
		return randString(r, 16+r.Intn(64))
	case 5:
		return r.Float64() * 1000
	case 6:
		return r.Int63n(1 << 40)
	case 7:
		return r.Intn(2) == 0
	case 8:
		// Small homogeneous string list — common for tags/labels.
		n := 2 + r.Intn(4)
		out := make([]string, n)
		for i := range out {
			out[i] = randString(r, 8+r.Intn(16))
		}
		return out
	default:
		// Small flat object — id/name/value triples.
		n := 2 + r.Intn(3)
		out := make(map[string]string, n)
		for range n {
			out[randString(r, 4+r.Intn(8))] = randString(r, 8+r.Intn(24))
		}
		return out
	}
}

// randRaw synthesizes JSON snippets aliased as RawMessage — objects with
// random key sets, arrays of mixed scalars, and standalone scalars.
// Sizes are bigger than typical scalar fields since RawMessage is the
// "stuff anything in here" escape hatch.
func randRaw(r *rand.Rand) json.RawMessage {
	switch r.Intn(6) {
	case 0:
		return rawObject(r, 4+r.Intn(8))
	case 1:
		return rawArray(r, 4+r.Intn(12))
	case 2:
		return rawObject(r, 8+r.Intn(16))
	case 3:
		// Long quoted string.
		return json.RawMessage(fmt.Sprintf(`%q`, randString(r, 32+r.Intn(96))))
	case 4:
		// Big integer.
		return json.RawMessage(fmt.Sprintf(`%d`, r.Int63n(1<<60)))
	default:
		return json.RawMessage(`null`)
	}
}

// rawObject builds a JSON object literal with n random string→scalar
// entries. Keys are randomized so the resulting wire bytes don't
// compress to a tiny set of repeated shapes.
func rawObject(r *rand.Rand, n int) json.RawMessage {
	var b []byte
	b = append(b, '{')
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '"')
		b = append(b, randString(r, 4+r.Intn(12))...)
		b = append(b, '"', ':')
		b = appendRawScalar(b, r)
	}
	b = append(b, '}')
	return b
}

func rawArray(r *rand.Rand, n int) json.RawMessage {
	var b []byte
	b = append(b, '[')
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendRawScalar(b, r)
	}
	b = append(b, ']')
	return b
}

func appendRawScalar(b []byte, r *rand.Rand) []byte {
	switch r.Intn(6) {
	case 0:
		return append(b, fmt.Sprintf(`%q`, randString(r, 8+r.Intn(48)))...)
	case 1:
		return append(b, fmt.Sprintf(`%d`, r.Int63n(1<<48))...)
	case 2:
		return append(b, fmt.Sprintf(`%.4f`, r.Float64()*10_000)...)
	case 3:
		if r.Intn(2) == 0 {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case 4:
		return append(b, "null"...)
	default:
		// Nested array of ints to add wire-shape variety.
		k := 2 + r.Intn(6)
		b = append(b, '[')
		for i := range k {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, fmt.Sprintf(`%d`, r.Int63n(1<<32))...)
		}
		return append(b, ']')
	}
}

const asciiLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randString(r *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = asciiLetters[r.Intn(len(asciiLetters))]
	}
	return string(b)
}

func randTags(r *rand.Rand, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = randString(r, 4+r.Intn(10))
	}
	return out
}

func randProps(r *rand.Rand, n int) map[string]string {
	out := make(map[string]string, n)
	for range n {
		out[randString(r, 4+r.Intn(8))] = randString(r, 8+r.Intn(24))
	}
	return out
}

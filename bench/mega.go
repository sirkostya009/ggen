// Mega bench family (mega_test.go): the deep Node tree, plus the DeepNested
// and MapHeavy payloads that share its benchmarks file.

//go:generate ../ggen $GOFILE
package bench

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Addr is the pointed-to type for Node's Refs []*Addr and Parent *Addr.
//
//ggen:generate
//easyjson:json
type Addr struct {
	Street string `json:"street"`
	City   string `json:"city"`
}

// Node is the deep-tree benchmark target. Exercises the breadth of ggen
// kinds: scalars, slices, maps, tuples, slices of pointers, nested slices,
// pointer fields, time/bytes/raw/any, and validation. All shapes are also
// supported by jsonv2/sonic/easyjson for apples-to-apples comparison.
//
//ggen:generate
//easyjson:json
type Node struct {
	ID        int64             `json:"id" pipe:"required gte=0"`
	Name      string            `json:"name" pipe:"required minlen=1 maxlen=128"`
	Score     float64           `json:"score" pipe:"gte=0 lte=100"`
	Active    bool              `json:"active"`
	Tags      []string          `json:"tags" pipe:"maxlen=64 inner:(minlen=1 maxlen=64)"`
	Props     map[string]string `json:"props" pipe:"maxlen=64"`
	Children  []Node            `json:"children" pipe:"maxlen=16"`
	Coords    [2]float64        `json:"coords"`
	Refs      []*Addr           `json:"refs" pipe:"maxlen=16"`
	Matrix    [][]int           `json:"matrix" pipe:"maxlen=16 inner:maxlen=32"`
	Parent    *Addr             `json:"parent,omitzero"`
	CreatedAt time.Time         `json:"createdAt"`
	Blob      []byte            `json:"blob"`
	Extra     any               `json:"extra"`
	Raw       json.RawMessage   `json:"raw"`
}

// AddrPlain strips easyjson's methods off Addr (`type T U` drops U's methods)
// so NodePlain never falls back to easyjson via the json.Unmarshaler hook.
type AddrPlain Addr

// NodePlain mirrors Node with a self-referential shape free of easyjson's
// generated methods, so the stdjson/jsonv2 rows measure the reflection path
// rather than easyjson via the json.Marshaler/Unmarshaler hooks.
type NodePlain struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Score     float64           `json:"score"`
	Active    bool              `json:"active"`
	Tags      []string          `json:"tags"`
	Props     map[string]string `json:"props"`
	Children  []NodePlain       `json:"children"`
	Coords    [2]float64        `json:"coords"`
	Refs      []*AddrPlain      `json:"refs"`
	Matrix    [][]int           `json:"matrix"`
	Parent    *AddrPlain        `json:"parent,omitzero"`
	CreatedAt time.Time         `json:"createdAt"`
	Blob      []byte            `json:"blob"`
	Extra     any               `json:"extra"`
	Raw       json.RawMessage   `json:"raw"`
}

// CopyAddr mirrors Addr under -copy: its string fields are copied out of the
// input instead of aliasing it. Pointed to by CopyNode's Refs / Parent.
//
//ggen:generate copy
type CopyAddr struct {
	Street string `json:"street"`
	City   string `json:"city"`
}

// CopyNode is Node generated under -copy (`//ggen:generate copy`): the bytes
// path copies every retained string, map key/value, json.RawMessage, and
// any-embedded string out of the input rather than aliasing it. Wire-identical
// to Node, so it decodes the same MegaPayload — the `ggen_copy` Unmarshal row
// measures the copy-mode cost against the aliasing `ggen` row.
//
//ggen:generate copy
type CopyNode struct {
	ID        int64             `json:"id" pipe:"required gte=0"`
	Name      string            `json:"name" pipe:"required minlen=1 maxlen=128"`
	Score     float64           `json:"score" pipe:"gte=0 lte=100"`
	Active    bool              `json:"active"`
	Tags      []string          `json:"tags" pipe:"maxlen=64 inner:(minlen=1 maxlen=64)"`
	Props     map[string]string `json:"props" pipe:"maxlen=64"`
	Children  []CopyNode        `json:"children" pipe:"maxlen=16"`
	Coords    [2]float64        `json:"coords"`
	Refs      []*CopyAddr       `json:"refs" pipe:"maxlen=16"`
	Matrix    [][]int           `json:"matrix" pipe:"maxlen=16 inner:maxlen=32"`
	Parent    *CopyAddr         `json:"parent,omitzero"`
	CreatedAt time.Time         `json:"createdAt"`
	Blob      []byte            `json:"blob"`
	Extra     any               `json:"extra"`
	Raw       json.RawMessage   `json:"raw"`
}

// nodeToPlain deep-converts a Node tree into NodePlain for the marshal
// benches. One-shot at init.
func nodeToPlain(n Node) NodePlain {
	p := NodePlain{
		ID:        n.ID,
		Name:      n.Name,
		Score:     n.Score,
		Active:    n.Active,
		Tags:      n.Tags,
		Props:     n.Props,
		Coords:    n.Coords,
		Matrix:    n.Matrix,
		CreatedAt: n.CreatedAt,
		Blob:      n.Blob,
		Extra:     n.Extra,
		Raw:       n.Raw,
	}
	if n.Parent != nil {
		ap := AddrPlain(*n.Parent)
		p.Parent = &ap
	}
	if n.Refs != nil {
		p.Refs = make([]*AddrPlain, len(n.Refs))
		for i, r := range n.Refs {
			if r != nil {
				ap := AddrPlain(*r)
				p.Refs[i] = &ap
			}
		}
	}
	if n.Children != nil {
		p.Children = make([]NodePlain, len(n.Children))
		for i, c := range n.Children {
			p.Children[i] = nodeToPlain(c)
		}
	}
	return p
}

// MapHeavy holds one big string-keyed map (1K+ entries) where map alloc,
// hash fill, and iteration dominate — a different bottleneck from mega.
//
//ggen:generate
type MapHeavy struct {
	Labels map[string]string `json:"labels"`
}

var (
	MegaValue      Node
	MegaValuePlain NodePlain // converted copy for stdjson/jsonv2/sonic marshal rows
	MegaPayload    []byte

	// DeepNestedPayload — single 50-level Node chain, isolating recursion cost.
	DeepNestedPayload []byte

	// MapHeavyPayload — 1024-entry string→string map.
	MapHeavyPayload []byte
)

func init() {
	var g gen
	MegaValue = buildNode(&g, 6, []int{5, 4, 3, 3, 3, 3, 0})
	MegaValuePlain = nodeToPlain(MegaValue)
	// No canonicalize: every map in the tree is single-entry, so the encode
	// is already byte-deterministic and keys keep declaration order.
	MegaPayload = mustMarshal(MegaValue)

	// Deep-nested 50-level chain.
	var deep Node
	deep.ID = 1
	deep.Name = "leaf"
	for i := range 50 {
		deep = Node{
			ID:       int64(i + 1),
			Name:     "level-" + strconv.Itoa(i),
			Children: []Node{deep},
		}
	}
	DeepNestedPayload = mustMarshal(deep)

	// Map-heavy 1024-entry string map.
	m := make(map[string]string, 1024)
	for i := range 1024 {
		m["key"+strconv.Itoa(i)] = "value" + strconv.Itoa(i)
	}
	MapHeavyPayload = canonicalize(mustMarshal(MapHeavy{Labels: m}))
}

func buildNode(g *gen, depth int, fanout []int) Node {
	n := Node{
		ID:     int64(g.next() >> 1),
		Name:   g.str(8 + g.intn(56)),
		Score:  g.f64() * 100,
		Active: g.intn(2) == 0,
		Tags:   genTags(g, 6+g.intn(20)),
		// Single entry: keeps the map arm in the generated code, but one
		// entry can't shuffle under Go's randomized map iteration (payload
		// stays byte-deterministic without canonicalization) and map cost
		// stays out of the ns/op signal — MapHeavy owns map measurement.
		Props:     genProps(g, 1),
		Coords:    [2]float64{g.f64()*180 - 90, g.f64()*360 - 180},
		Refs:      genAddrPtrs(g, 4+g.intn(8)),
		Matrix:    genMatrix(g, 4+g.intn(8)),
		Parent:    genAddrPtr(g, g.intn(3) != 0),
		CreatedAt: time.Unix(int64(g.next()%(1<<31)), int64(g.next()%(1<<30))).UTC(),
		Blob:      genBytes(g, 32+g.intn(192)),
		Extra:     genAny(g),
		Raw:       genRaw(g),
	}
	if depth > 0 && depth < len(fanout) {
		kids := fanout[len(fanout)-1-depth]
		if kids > 0 {
			n.Children = make([]Node, kids)
			for i := range n.Children {
				n.Children[i] = buildNode(g, depth-1, fanout)
			}
		}
	}
	return n
}

func genAddr(g *gen) Addr {
	return Addr{Street: g.str(8 + g.intn(16)), City: g.str(6 + g.intn(10))}
}

func genAddrPtr(g *gen, present bool) *Addr {
	if !present {
		return nil
	}
	a := genAddr(g)
	return &a
}

func genAddrPtrs(g *gen, n int) []*Addr {
	out := make([]*Addr, n)
	for i := range out {
		// every 4th element nil — exercises the slab path's null branch
		out[i] = genAddrPtr(g, i%4 != 0)
	}
	return out
}

func genMatrix(g *gen, rows int) [][]int {
	out := make([][]int, rows)
	for i := range out {
		cols := 4 + g.intn(20)
		row := make([]int, cols)
		for j := range row {
			row[j] = g.intn(1_000_000) - 500_000
		}
		out[i] = row
	}
	return out
}

func genBytes(g *gen, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(g.next())
	}
	return b
}

// genAny synthesizes an `any` value — mostly scalars or nil, occasionally
// a small list or object.
func genAny(g *gen) any {
	switch g.intn(10) {
	case 0, 1, 2, 3:
		return nil
	case 4:
		return g.str(16 + g.intn(64))
	case 5:
		return g.f64() * 1000
	case 6:
		return int64(g.next() % (1 << 40))
	case 7:
		return g.intn(2) == 0
	case 8:
		// Small string list.
		n := 2 + g.intn(4)
		out := make([]string, n)
		for i := range out {
			out[i] = g.str(8 + g.intn(16))
		}
		return out
	default:
		// Single-entry flat object — deterministic (see the Props note).
		return map[string]string{g.str(4 + g.intn(8)): g.str(8 + g.intn(24))}
	}
}

// genRaw synthesizes JSON snippets aliased as RawMessage — objects,
// arrays of mixed scalars, and standalone scalars.
func genRaw(g *gen) json.RawMessage {
	switch g.intn(6) {
	case 0:
		return rawObject(g, 4+g.intn(8))
	case 1:
		return rawArray(g, 4+g.intn(12))
	case 2:
		return rawObject(g, 8+g.intn(16))
	case 3:
		return json.RawMessage(fmt.Sprintf(`%q`, g.str(32+g.intn(96))))
	case 4:
		return json.RawMessage(fmt.Sprintf(`%d`, g.next()%(1<<60)))
	default:
		return json.RawMessage(`null`)
	}
}

// rawObject builds a JSON object literal with n string→scalar entries.
func rawObject(g *gen, n int) json.RawMessage {
	var b []byte
	b = append(b, '{')
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '"')
		b = append(b, g.str(4+g.intn(12))...)
		b = append(b, '"', ':')
		b = appendRawScalar(b, g)
	}
	b = append(b, '}')
	return b
}

func rawArray(g *gen, n int) json.RawMessage {
	var b []byte
	b = append(b, '[')
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendRawScalar(b, g)
	}
	b = append(b, ']')
	return b
}

func appendRawScalar(b []byte, g *gen) []byte {
	switch g.intn(6) {
	case 0:
		return append(b, fmt.Sprintf(`%q`, g.str(8+g.intn(48)))...)
	case 1:
		return append(b, fmt.Sprintf(`%d`, g.next()%(1<<48))...)
	case 2:
		return append(b, fmt.Sprintf(`%.4f`, g.f64()*10_000)...)
	case 3:
		if g.intn(2) == 0 {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case 4:
		return append(b, "null"...)
	default:
		// Nested array of ints.
		k := 2 + g.intn(6)
		b = append(b, '[')
		for i := range k {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, fmt.Sprintf(`%d`, g.next()%(1<<32))...)
		}
		return append(b, ']')
	}
}

func genTags(g *gen, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = g.str(4 + g.intn(10))
	}
	return out
}

func genProps(g *gen, n int) map[string]string {
	out := make(map[string]string, n)
	for range n {
		out[g.str(4+g.intn(8))] = g.str(8 + g.intn(24))
	}
	return out
}

// Mega benchmark family (mega_test.go): the deep Node tree, plus the
// DeepNested, MapHeavy and MapValues payloads that share its benchmark file.

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

// Node is the deep-tree benchmark type. It covers a wide range of ggen kinds:
// scalars, slices, maps, fixed arrays, slices of pointers, nested slices,
// pointer fields, time, bytes, raw JSON, any, and validation. jsonv2, sonic
// and easyjson support all of these shapes too, so the comparison is fair.
//
//ggen:generate
//easyjson:json
type Node struct {
	ID        int64             `json:"id"              pipe:"required gte=0"`
	Name      string            `json:"name"            pipe:"required minlen=1 maxlen=128"`
	Score     float64           `json:"score"           pipe:"gte=0 lte=100"`
	Active    bool              `json:"active"`
	Tags      []string          `json:"tags"            pipe:"maxlen=64 inner:(minlen=1 maxlen=64)"`
	Props     map[string]string `json:"props"           pipe:"maxlen=64"`
	Children  []Node            `json:"children"        pipe:"maxlen=16"`
	Coords    [2]float64        `json:"coords"`
	Refs      []*Addr           `json:"refs"            pipe:"maxlen=16"`
	Matrix    [][]int           `json:"matrix"          pipe:"maxlen=16 inner:maxlen=32"`
	Parent    *Addr             `json:"parent,omitzero"`
	CreatedAt time.Time         `json:"createdAt"`
	Blob      []byte            `json:"blob"`
	Extra     any               `json:"extra"`
	Raw       json.RawMessage   `json:"raw"`
}

// AddrPlain is Addr without easyjson's methods (`type T U` does not inherit
// U's methods). So NodePlain never calls into easyjson through the
// json.Unmarshaler interface.
type AddrPlain Addr

// NodePlain has the same recursive shape as Node but none of easyjson's
// generated methods. So the stdjson and jsonv2 rows measure their reflection
// code, not easyjson called through json.Marshaler and json.Unmarshaler.
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

// CopyAddr is Addr under -copy. Its string fields are copied out of the input
// instead of aliasing it (pointing into it without a copy). CopyNode's Refs
// and Parent point to it.
//
//ggen:generate copy
type CopyAddr struct {
	Street string `json:"street"`
	City   string `json:"city"`
}

// CopyNode is Node generated under -copy (`//ggen:generate copy`). The bytes
// path copies every retained string, map key and value, json.RawMessage, and
// string inside an any out of the input instead of aliasing it. It has the
// same JSON form as Node, so it decodes the same MegaPayload. The `ggen_copy`
// Unmarshal row measures the cost of copy mode against the aliasing `ggen`
// row.
//
//ggen:generate copy
type CopyNode struct {
	ID        int64             `json:"id"              pipe:"required gte=0"`
	Name      string            `json:"name"            pipe:"required minlen=1 maxlen=128"`
	Score     float64           `json:"score"           pipe:"gte=0 lte=100"`
	Active    bool              `json:"active"`
	Tags      []string          `json:"tags"            pipe:"maxlen=64 inner:(minlen=1 maxlen=64)"`
	Props     map[string]string `json:"props"           pipe:"maxlen=64"`
	Children  []CopyNode        `json:"children"        pipe:"maxlen=16"`
	Coords    [2]float64        `json:"coords"`
	Refs      []*CopyAddr       `json:"refs"            pipe:"maxlen=16"`
	Matrix    [][]int           `json:"matrix"          pipe:"maxlen=16 inner:maxlen=32"`
	Parent    *CopyAddr         `json:"parent,omitzero"`
	CreatedAt time.Time         `json:"createdAt"`
	Blob      []byte            `json:"blob"`
	Extra     any               `json:"extra"`
	Raw       json.RawMessage   `json:"raw"`
}

// nodeToPlain converts a whole Node tree into NodePlain for the marshal
// benchmarks. It runs once at init.
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
		for i := range n.Children {
			p.Children[i] = nodeToPlain(n.Children[i])
		}
	}
	return p
}

// MapValues holds maps whose values own allocations: a struct with a slice,
// and a slice. For these shapes the decode reads the receiver's existing map,
// and each entry's previous value becomes the target the new value is decoded
// into.
//
//ggen:generate
type MapValues struct {
	Entries map[string]MapEntry `json:"entries"`
	Lists   map[string][]int    `json:"lists"`
}

// MapHeavyEntry is the same shape with much more memory per value. It tests
// whether reusing a bigger allocation is worth more than building a new map.
//
//ggen:generate
type MapHeavyEntry struct {
	ID   int64             `json:"id"`
	Tags []string          `json:"tags"`
	Meta map[string]string `json:"meta"`
}

//ggen:generate
type MapValuesHeavy struct {
	Entries map[string]MapHeavyEntry `json:"entries"`
}

//ggen:generate
type MapEntry struct {
	ID   int64    `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// MapHeavy holds one big string-keyed map with over 1000 entries. Map
// allocation, hashing and iteration dominate, which is a different bottleneck
// from Mega.
//
//ggen:generate
type MapHeavy struct {
	Labels map[string]string `json:"labels"`
}

var (
	MegaValue      Node
	MegaValuePlain NodePlain // converted copy for the stdjson, jsonv2 and sonic marshal rows
	MegaPayload    []byte

	// DeepNestedPayload is a single chain of 50 nested Nodes. It measures
	// recursion cost alone.
	DeepNestedPayload []byte

	// MapHeavyPayload is a map of 1024 string keys to string values.
	MapHeavyPayload []byte

	// MapValuesPayload has 256 entries per map, with values that own
	// allocations.
	MapValuesPayload []byte

	// MapValuesHeavyPayload has 256 entries whose values own much more
	// memory.
	MapValuesHeavyPayload []byte
)

func init() {
	var g gen
	MegaValue = buildNode(&g, 6, []int{5, 4, 3, 3, 3, 3, 0})
	MegaValuePlain = nodeToPlain(MegaValue)
	// No canonicalize here. Every map in the tree has a single entry, so the
	// encoded bytes are already the same on every run, and keys keep their
	// declaration order.
	MegaPayload = mustMarshal(MegaValue)

	// A chain of 50 nested Nodes.
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

	// The 1024-entry string map.
	m := make(map[string]string, 1024)
	for i := range 1024 {
		m["key"+strconv.Itoa(i)] = "value" + strconv.Itoa(i)
	}
	MapHeavyPayload = canonicalize(mustMarshal(MapHeavy{Labels: m}))

	// Maps whose values own allocations: 256 entries each.
	entries := make(map[string]MapEntry, 256)
	lists := make(map[string][]int, 256)
	for i := range 256 {
		k := "key" + strconv.Itoa(i)
		entries[k] = MapEntry{
			ID:   int64(i),
			Name: "name" + strconv.Itoa(i),
			Tags: []string{"a" + strconv.Itoa(i), "b" + strconv.Itoa(i), "c"},
		}
		lists[k] = []int{i, i + 1, i + 2, i + 3}
	}
	MapValuesPayload = canonicalize(mustMarshal(MapValues{Entries: entries, Lists: lists}))

	heavy := make(map[string]MapHeavyEntry, 256)
	for i := range 256 {
		tags := make([]string, 24)
		for j := range tags {
			tags[j] = "tag" + strconv.Itoa(i) + "-" + strconv.Itoa(j)
		}
		meta := make(map[string]string, 8)
		for j := range 8 {
			meta["m"+strconv.Itoa(j)] = "v" + strconv.Itoa(i*8+j)
		}
		heavy["key"+strconv.Itoa(i)] = MapHeavyEntry{ID: int64(i), Tags: tags, Meta: meta}
	}
	MapValuesHeavyPayload = canonicalize(mustMarshal(MapValuesHeavy{Entries: heavy}))
}

func buildNode(g *gen, depth int, fanout []int) Node {
	n := Node{
		ID:     int64(g.next() >> 1),
		Name:   g.str(8 + g.intn(56)),
		Score:  g.f64() * 100,
		Active: g.intn(2) == 0,
		Tags:   genTags(g, 6+g.intn(20)),
		// A single entry still runs the map code in the generated decoder.
		// But one entry cannot be reordered by Go's random map iteration, so
		// the payload stays the same on every run without canonicalize. It
		// also keeps map cost out of the ns/op number. MapHeavy is the
		// benchmark that measures maps.
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
		// Every 4th element is nil, to exercise the null branch of the slab
		// path (pointees decoded into one shared backing slice).
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

// genAny builds an `any` value. It is mostly a scalar or nil, sometimes a
// small list or object.
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
		// A flat object with one entry, so its order is fixed (see the note
		// on Props).
		return map[string]string{g.str(4 + g.intn(8)): g.str(8 + g.intn(24))}
	}
}

// genRaw builds JSON snippets as RawMessage values: objects, arrays of mixed
// scalars, and single scalars.
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
		return json.RawMessage(strconv.FormatUint(g.next()%(1<<60), 10))
	default:
		return json.RawMessage(`null`)
	}
}

// rawObject builds a JSON object with n entries, each a string key with a
// scalar value.
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
		return append(b, strconv.FormatUint(g.next()%(1<<48), 10)...)
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
			b = append(b, strconv.FormatUint(g.next()%(1<<32), 10)...)
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

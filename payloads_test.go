package main

import "math/rand"

// --- complex payload: small handcrafted Node -------------------------------
//
// Used by the roundtrip / read / scan-decode test families to exercise the
// dispatch surface (scalars + slices + maps + a couple of children) without
// the noise of the multi-megabyte mega payload. Field values are fixed so
// tests can assert on them directly.

var complexPayload = []byte(`{
    "id": 42,
    "name": "hello world",
    "score": 9.5,
    "active": true,
    "tags": ["alpha","beta","gamma"],
    "props": {"k1":"v1","k2":"v2"},
    "children": [
        {"id":1, "name": "child-1", "score":1.5, "active": false, "tags": ["x"], "props": {"a":"1"}, "children": null},
        {"id":2, "name": "child-2", "score":2.5, "active": true, "tags": ["y","z"], "props": {"b":"2"}, "children": null}
    ]
}`)

// complexValue is the struct form of complexPayload, used by marshal benches.
var complexValue = Node{
	ID:     42,
	Name:   "hello world",
	Score:  9.5,
	Active: true,
	Tags:   []string{"alpha", "beta", "gamma"},
	Props:  map[string]string{"k1": "v1", "k2": "v2"},
	Children: []Node{
		{ID: 1, Name: "child-1", Score: 1.5, Active: false, Tags: []string{"x"}, Props: map[string]string{"a": "1"}},
		{ID: 2, Name: "child-2", Score: 2.5, Active: true, Tags: []string{"y", "z"}, Props: map[string]string{"b": "2"}},
	},
}

// --- mega value: deep Node tree ≈ 1 MB ------------------------------------

// megaValue is a pseudorandom, 6-level-deep tree of Node values built
// once at init with a fixed seed so results are reproducible across runs.
// Shape (scalars + slices + maps + recursive children) mirrors the kind
// of input other JSON libs benchmark against. Used by stdcompat to catch
// float-formatting / ordering drift that only shows up at scale.
var megaValue Node

func init() {
	r := rand.New(rand.NewSource(1))
	// Fanout per level chosen so the resulting payload lands near 1 MiB. Tune
	// here if content/Node shape changes.
	megaValue = buildNode(r, 6, []int{4, 4, 4, 4, 3, 3, 0})
}

func buildNode(r *rand.Rand, depth int, fanout []int) Node {
	n := Node{
		ID:     r.Int63(),
		Name:   randString(r, 8+r.Intn(24)),
		Score:  float64(r.Intn(10000)) / 100.0,
		Active: r.Intn(2) == 0,
		Tags:   randTags(r, 3+r.Intn(4)),
		Props:  randProps(r, 2+r.Intn(4)),
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

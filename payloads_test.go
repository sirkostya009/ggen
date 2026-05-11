package main

import (
	"math/rand"

	"github.com/sirkostya009/ggen/encode"
)

// --- complex payload: SomePayloadRequestStruct -------------------------------

var complexPayload = []byte(`{
    "field1": "hello world",
    "array": [1,2,3,4,5],
    "address": {"street":"Main 1","city":"Lviv","zipCode":"79000"},
    "contacts": [
        {"street":"S1","city":"C1","zipCode":"00001"},
        {"street":"S2","city":"C2","zipCode":"00002"}
    ],
    "email": "foo@bar.com",
    "website": "https://example.com/x",
    "userId": "550e8400-e29b-41d4-a716-446655440000",
    "role": "admin",
    "age": 30,
    "quota": 5
}`)

// complexValue is the struct form of complexPayload, used by marshal benches.
var complexValue = SomePayloadRequestStruct{
	Field1:   "hello world",
	Slice:    []int{1, 2, 3, 4, 5},
	Address:  Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"},
	Contacts: []Address{{Street: "S1", City: "C1", ZipCode: "00001"}, {Street: "S2", City: "C2", ZipCode: "00002"}},
	Email:    "foo@bar.com",
	Website:  "https://example.com/x",
	UserID:   "550e8400-e29b-41d4-a716-446655440000",
	Role:     "admin",
	Age:      30,
	Quota:    5,
}

// --- mega payload: deep Node tree ≈ 1 MB ------------------------------------

// megaPayload is a pseudorandom, 6-level-deep tree of Node values serialised
// to JSON. Generated once at init with a fixed seed so results are
// reproducible across runs. Shape (scalars + slices + maps + recursive
// children) mirrors the kind of input other JSON libs benchmark against.
var (
	megaValue   Node
	megaPayload []byte
)

func init() {
	r := rand.New(rand.NewSource(1))
	// Fanout per level chosen so the resulting payload lands near 1 MiB. Tune
	// here if content/Node shape changes.
	megaValue = buildNode(r, 6, []int{4, 4, 4, 4, 3, 3, 0})
	var err error
	megaPayload, err = encode.Marshal(megaValue)
	if err != nil {
		panic(err)
	}
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

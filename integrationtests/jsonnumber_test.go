package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
)

// NumberShapes holds json.Number in every field position.
//
//ggen:generate nosortkeys
type NumberShapes struct {
	N    json.Number            `json:"n"`
	P    *json.Number           `json:"p"`
	List []json.Number          `json:"list"`
	Map  map[string]json.Number `json:"map"`
	Pair [2]json.Number         `json:"pair"`
	PL   []*json.Number         `json:"pl"`
	OE   json.Number            `json:"oe,omitempty"`
	OZ   json.Number            `json:"oz,omitzero"`
}

// NumberPair is the allocation probe: two json.Number fields.
//
//ggen:generate
type NumberPair struct {
	A json.Number `json:"a"`
	B json.Number `json:"b"`
}

// NumberCopy is NumberPair under -copy.
//
//ggen:generate copy
type NumberCopy struct {
	A json.Number   `json:"a"`
	L []json.Number `json:"l"`
}

// A json.Number decodes what encoding/json decodes, at every position, on
// both paths, and marshals to encoding/json's bytes.
func TestJSONNumber_matchesEncodingJSON(t *testing.T) {
	t.Parallel()
	// One map key per document: nosortkeys writes map keys in iteration order.
	docs := []string{
		`{"n":12,"p":-3.5e7,"list":[1,2.25,-0],"map":{"b":1e-3},"pair":[1,2],"pl":[5,null],"oe":3,"oz":4}`,
		`{"n":"12","p":"7","list":["1",2],"map":{"a":"9"},"pair":["3","4"],"pl":["6"]}`,
		`{"n":null,"p":null,"list":null,"map":null,"pl":null,"oe":null}`,
		`{"n":0,"oe":0,"oz":0}`,
		`{"n":123456789012345678901234567890.5e-300}`,
	}
	for _, doc := range docs {
		var want NumberShapes
		if err := json.Unmarshal([]byte(doc), &want); err != nil {
			t.Fatalf("%s: encoding/json: %v", doc, err)
		}
		got, _, err := NumberShapes{}.DecodeFrom([]byte(doc))
		if err != nil {
			t.Fatalf("%s: bytes: %v", doc, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: bytes\n got  %+v\n want %+v", doc, got, want)
		}
		for _, chunk := range []int{1, 2, 3, 64} {
			var s ggen.Stream
			s.Reset(&chunkReader{data: []byte(doc), max: chunk}, make([]byte, 0, 16))
			sv, err := s.Value[NumberShapes]()
			if err != nil {
				t.Fatalf("%s: stream chunk %d: %v", doc, chunk, err)
			}
			if !reflect.DeepEqual(sv, want) {
				t.Errorf("%s: stream chunk %d\n got  %+v\n want %+v", doc, chunk, sv, want)
			}
		}
		out, err := ggen.Marshal(got)
		if err != nil {
			t.Fatalf("%s: marshal: %v", doc, err)
		}
		std, _ := json.Marshal(want)
		if !bytes.Equal(out, std) {
			t.Errorf("%s: marshal\n got  %s\n want %s", doc, out, std)
		}
		if n := got.JSONSize(); n < len(out) {
			t.Errorf("%s: JSONSize %d < %d written", doc, n, len(out))
		}
	}
}

// Malformed values fail where encoding/json fails, with the same sentinel and
// Pos on both paths at every chunk size.
func TestJSONNumber_rejectsParity(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{
		`{"n":-}`, `{"n":01}`, `{"n":1.}`, `{"n":"abc"}`, `{"n":true}`, `{"n":{}}`,
		`{"list":[1,"x"]}`, `{"map":{"a":-a}}`, `{"pair":[1,2e]}`, `{"p":nul}`, `{"n":`, `{"list":[`,
	} {
		if json.Unmarshal([]byte(doc), new(NumberShapes)) == nil {
			t.Fatalf("%s: encoding/json accepts it", doc)
		}
		for _, chunk := range []int{1, 3, 64} {
			be, se := decodeBothChunked[NumberShapes](doc, chunk)
			if be == nil || se == nil {
				t.Fatalf("%s chunk %d: bytes %v, stream %v", doc, chunk, be, se)
			}
			var bp, sp *ggen.ParseError
			if !errors.As(be, &bp) || !errors.As(se, &sp) {
				t.Fatalf("%s chunk %d: not ParseErrors: %v / %v", doc, chunk, be, se)
			}
			if bp.Pos != sp.Pos || be.Error() != se.Error() {
				t.Errorf("%s chunk %d: bytes %v, stream %v", doc, chunk, be, se)
			}
		}
	}
}

// Text that is not a JSON number fails to marshal, as in encoding/json.
func TestJSONNumber_marshalRejectsInvalid(t *testing.T) {
	t.Parallel()
	for _, n := range []json.Number{"abc", "1 ", "01", "1.", "+1", "-"} {
		if _, err := ggen.Marshal(NumberPair{A: n}); err == nil {
			t.Errorf("%q marshalled", n)
		}
	}
	out, err := ggen.Marshal(NumberPair{})
	if err != nil || string(out) != `{"a":0,"b":0}` {
		t.Errorf("zero NumberPair = %s, %v", out, err)
	}
}

// A json.Number field costs what a string field costs.
func TestJSONNumber_noAlloc(t *testing.T) { //nolint:paralleltest // measures allocations
	doc := []byte(`{"a":12.5,"b":-7}`)
	if n := testing.AllocsPerRun(100, func() { _, _, _ = NumberPair{}.DecodeFrom(doc) }); n != 0 {
		t.Errorf("DecodeFrom allocs = %v, want 0", n)
	}
	v := NumberPair{A: "12.5", B: "-7"}
	buf := make([]byte, 0, v.JSONSize())
	if n := testing.AllocsPerRun(100, func() { _, _ = v.AppendJSON(buf[:0]) }); n != 0 {
		t.Errorf("AppendJSON allocs = %v, want 0", n)
	}
	if n := testing.AllocsPerRun(100, func() { _, _ = ggen.Marshal(v) }); n != 1 {
		t.Errorf("Marshal allocs = %v, want 1", n)
	}
}

// Under -copy the bytes path detaches the number text from the input.
func TestJSONNumber_copyDetaches(t *testing.T) {
	t.Parallel()
	doc := []byte(`{"a":12345,"l":[678,9]}`)
	got, _, err := NumberCopy{}.DecodeFrom(doc)
	if err != nil {
		t.Fatal(err)
	}
	copy(doc, strings.Repeat("x", len(doc)))
	if got.A != "12345" || len(got.L) != 2 || got.L[0] != "678" || got.L[1] != "9" {
		t.Errorf("after overwrite: %+v", got)
	}
}

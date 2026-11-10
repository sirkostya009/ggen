package integrationtests

//go:generate ../ggen $GOFILE

import (
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sirkostya009/ggen"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty2"
)

// EmbedStruct exercises the json:",embed" catch-all. Decode absorbs unknown
// keys into Extra, and marshal splices them back out as top-level keys.
//
//ggen:generate
type EmbedStruct struct {
	Name  string         `json:"name"`
	Extra map[string]any `json:",embed"`
}

//ggen:generate
type EmbedStringsStruct struct {
	Name  string            `json:"name"`
	Extra map[string]string `json:",embed"`
}

//ggen:generate
type EmbedStructsStruct struct {
	Name  string                 `json:"name"`
	Extra map[string]EmbedStruct `json:",embed"`
}

//ggen:generate
type EmbedRawStruct struct {
	Name  string                     `json:"name"`
	Extra map[string]json.RawMessage `json:",embed"`
}

// EmbedLease is the generated struct the catch-alls below hold behind a
// pointer and in a slice.
//
//ggen:generate
type EmbedLease struct {
	Addr net.TCPAddr   `json:"addr"`
	TTL  time.Duration `json:"ttl"`
}

// The catch-alls below hold values written in a form encoding/json does not
// read: the net kinds, Duration, a byte array and generated structs, alone,
// behind a pointer and in containers, plus a plain and a generated struct
// from other packages. A struct has one catch-all, so each value type gets
// its own.

//ggen:generate
type EmbedIPNets struct {
	Name  string               `json:"name"`
	Extra map[string]net.IPNet `json:",embed"`
}

//ggen:generate
type EmbedIPNetPtrs struct {
	Name  string                `json:"name"`
	Extra map[string]*net.IPNet `json:",embed"`
}

//ggen:generate
type EmbedTCPAddrs struct {
	Name  string                 `json:"name"`
	Extra map[string]net.TCPAddr `json:",embed"`
}

//ggen:generate
type EmbedTCPAddrPtrs struct {
	Name  string                  `json:"name"`
	Extra map[string]*net.TCPAddr `json:",embed"`
}

//ggen:generate
type EmbedUDPAddrs struct {
	Name  string                 `json:"name"`
	Extra map[string]net.UDPAddr `json:",embed"`
}

//ggen:generate
type EmbedUDPAddrPtrs struct {
	Name  string                  `json:"name"`
	Extra map[string]*net.UDPAddr `json:",embed"`
}

//ggen:generate
type EmbedDurations struct {
	Name  string                   `json:"name"`
	Extra map[string]time.Duration `json:",embed"`
}

//ggen:generate
type EmbedDurationPtrs struct {
	Name  string                    `json:"name"`
	Extra map[string]*time.Duration `json:",embed"`
}

//ggen:generate
type EmbedByteArrays struct {
	Name  string              `json:"name"`
	Extra map[string][16]byte `json:",embed"`
}

//ggen:generate
type EmbedLeasePtrs struct {
	Name  string                 `json:"name"`
	Extra map[string]*EmbedLease `json:",embed"`
}

//ggen:generate
type EmbedLeaseSlices struct {
	Name  string                  `json:"name"`
	Extra map[string][]EmbedLease `json:",embed"`
}

//ggen:generate
type EmbedUDPAddrPtrSlices struct {
	Name  string                    `json:"name"`
	Extra map[string][]*net.UDPAddr `json:",embed"`
}

//ggen:generate
type EmbedEndpoints struct {
	Name  string                         `json:"name"`
	Extra map[string]thirdparty.Endpoint `json:",embed"`
}

//ggen:generate
type EmbedForeignLeases struct {
	Name  string                       `json:"name"`
	Extra map[string]thirdparty2.Lease `json:",embed"`
}

// EmbedFlaggedUDPAddrPtrs runs a typed catch-all under the flags that change
// its code: htmlescape escapes the zone, and copy clones the kept key.
//
//ggen:generate copy htmlescape allowinvalidutf8 multierr
type EmbedFlaggedUDPAddrPtrs struct {
	Name  string                  `json:"name"`
	Extra map[string]*net.UDPAddr `json:",embed"`
}

func TestEmbed_decodeAbsorbsUnknown(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"alice","age":30,"city":"Lviv","tags":["a","b"]}`)
	got, _, err := EmbedStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != "alice" {
		t.Errorf("Name = %q", got.Name)
	}
	if len(got.Extra) != 3 {
		t.Fatalf("Extra len = %d, want 3: %+v", len(got.Extra), got.Extra)
	}
	// Bare numbers decode into float64 for any.
	if age, _ := got.Extra["age"].(float64); age != 30 {
		t.Errorf("Extra[age] = %v", got.Extra["age"])
	}
	if city, _ := got.Extra["city"].(string); city != "Lviv" {
		t.Errorf("Extra[city] = %v", got.Extra["city"])
	}
	tags, ok := got.Extra["tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Errorf("Extra[tags] = %#v", got.Extra["tags"])
	}
}

func TestEmbed_emptyDecode(t *testing.T) {
	t.Parallel()
	// With no unknown keys, Extra stays nil.
	got, _, err := EmbedStruct{}.DecodeFrom([]byte(`{"name":"bob"}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Extra != nil {
		t.Errorf("Extra should be nil, got %+v", got.Extra)
	}
}

func TestEmbed_marshalSpreads(t *testing.T) {
	t.Parallel()
	s := EmbedStruct{
		Name:  "alice",
		Extra: map[string]any{"age": 30, "city": "Lviv"},
	}
	out, _ := ggen.MarshalString(s)
	for _, want := range []string{`"name":"alice"`, `"age":30`, `"city":"Lviv"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	// No nested object. The extras must be at the top level.
	if strings.Contains(out, `"Extra"`) || strings.Contains(out, `{"age"`) && !strings.HasPrefix(out, `{"`) {
		t.Errorf("inline shouldn't be nested: %q", out)
	}
	// Must be a single top-level object.
	if !strings.HasPrefix(out, "{") || !strings.HasSuffix(out, "}") {
		t.Errorf("bad framing: %q", out)
	}
}

func TestEmbed_roundtrip(t *testing.T) {
	t.Parallel()
	orig := EmbedStruct{
		Name:  "alice",
		Extra: map[string]any{"age": float64(30), "city": "Lviv", "active": true},
	}
	out, _ := ggen.Marshal(orig)
	got, _, err := EmbedStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Name != orig.Name {
		t.Errorf("Name mismatch: %q vs %q", got.Name, orig.Name)
	}
	if len(got.Extra) != len(orig.Extra) {
		t.Fatalf("Extra len: got %d want %d\n%+v", len(got.Extra), len(orig.Extra), got.Extra)
	}
	for k, v := range orig.Extra {
		if got.Extra[k] != v {
			t.Errorf("Extra[%q] = %v want %v", k, got.Extra[k], v)
		}
	}
}

func TestEmbed_marshalEmpty(t *testing.T) {
	t.Parallel()
	s := EmbedStruct{Name: "alice"}
	out, _ := ggen.MarshalString(s)
	if out != `{"name":"alice"}` {
		t.Errorf("empty-extra marshal = %q", out)
	}
}

func TestEmbed_marshalOnlyExtras(t *testing.T) {
	t.Parallel()
	// An empty fixed field plus catch-all entries. The comma logic must hold.
	s := EmbedStruct{Extra: map[string]any{"k": "v"}}
	out, _ := ggen.MarshalString(s)
	if !strings.Contains(out, `"k":"v"`) {
		t.Errorf("missing k:v in %q", out)
	}
	if !strings.HasPrefix(out, "{") || !strings.HasSuffix(out, "}") {
		t.Errorf("bad framing: %q", out)
	}
}

// The fixed field appears in every marshal regardless of map iteration order.
func TestEmbed_FixedFieldOrderStable(t *testing.T) {
	t.Parallel()
	s := EmbedStruct{
		Name:  "alice",
		Extra: map[string]any{"age": 30, "city": "Lviv", "active": true, "score": 9.5},
	}
	for i := range 20 {
		out, _ := ggen.Marshal(s)
		if !strings.Contains(string(out), `"name":"alice"`) {
			t.Errorf("iter %d: missing name field: %s", i, out)
		}
	}
}

// A typed catch-all: map[string]string, with no boxing into any.
func TestEmbed_TypedString_Decode(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"alice","city":"Lviv","role":"admin"}`)
	got, _, err := EmbedStringsStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "alice" {
		t.Errorf("Name = %q", got.Name)
	}
	if len(got.Extra) != 2 {
		t.Fatalf("Extra len = %d, want 2: %+v", len(got.Extra), got.Extra)
	}
	if got.Extra["city"] != "Lviv" || got.Extra["role"] != "admin" {
		t.Errorf("Extra = %+v", got.Extra)
	}
}

func TestEmbed_TypedString_EmptyExtra(t *testing.T) {
	t.Parallel()
	got, _, err := EmbedStringsStruct{}.DecodeFrom([]byte(`{"name":"bob"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Extra != nil {
		t.Errorf("Extra should be nil, got %+v", got.Extra)
	}
}

// A non-string value into a string-typed catch-all must error.
func TestEmbed_TypedString_RejectsNonString(t *testing.T) {
	t.Parallel()
	_, _, err := EmbedStringsStruct{}.DecodeFrom([]byte(`{"name":"alice","age":30}`))
	if err == nil {
		t.Fatal("expected error decoding number into string-typed inline")
	}
}

func TestEmbed_TypedString_MarshalSpreads(t *testing.T) {
	t.Parallel()
	s := EmbedStringsStruct{
		Name:  "alice",
		Extra: map[string]string{"city": "Lviv", "role": "admin"},
	}
	out, _ := ggen.MarshalString(s)
	for _, want := range []string{`"name":"alice"`, `"city":"Lviv"`, `"role":"admin"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if !strings.HasPrefix(out, "{") || !strings.HasSuffix(out, "}") {
		t.Errorf("bad framing: %q", out)
	}
}

func TestEmbed_TypedString_Roundtrip(t *testing.T) {
	t.Parallel()
	orig := EmbedStringsStruct{
		Name:  "alice",
		Extra: map[string]string{"city": "Lviv", "role": "admin", "lang": "uk"},
	}
	out, _ := ggen.Marshal(orig)
	got, _, err := EmbedStringsStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if got.Name != orig.Name {
		t.Errorf("Name mismatch: %q vs %q", got.Name, orig.Name)
	}
	if len(got.Extra) != len(orig.Extra) {
		t.Fatalf("Extra len: got %d want %d\n%+v", len(got.Extra), len(orig.Extra), got.Extra)
	}
	for k, v := range orig.Extra {
		if got.Extra[k] != v {
			t.Errorf("Extra[%q] = %q want %q", k, got.Extra[k], v)
		}
	}
}

// A typed catch-all: map[string]EmbedStruct. Each value decodes through the
// element type's DecodeFrom, not the json.Unmarshal fallback.
func TestEmbed_TypedStruct_Decode(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"root","kid":{"name":"alice","age":30},"sib":{"name":"bob"}}`)
	got, _, err := EmbedStructsStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "root" {
		t.Errorf("Name = %q", got.Name)
	}
	if len(got.Extra) != 2 {
		t.Fatalf("Extra len = %d, want 2: %+v", len(got.Extra), got.Extra)
	}
	kid := got.Extra["kid"]
	if kid.Name != "alice" {
		t.Errorf("kid.Name = %q", kid.Name)
	}
	// The inner EmbedStruct has its own catch-all, so age:30 lands there.
	if age, _ := kid.Extra["age"].(float64); age != 30 {
		t.Errorf("kid.Extra[age] = %v", kid.Extra["age"])
	}
	if got.Extra["sib"].Name != "bob" {
		t.Errorf("sib.Name = %q", got.Extra["sib"].Name)
	}
}

func TestEmbed_TypedStruct_Roundtrip(t *testing.T) {
	t.Parallel()
	orig := EmbedStructsStruct{
		Name: "root",
		Extra: map[string]EmbedStruct{
			"kid": {Name: "alice", Extra: map[string]any{"age": float64(30)}},
			"sib": {Name: "bob"},
		},
	}
	out, _ := ggen.Marshal(orig)
	got, _, err := EmbedStructsStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if got.Name != orig.Name {
		t.Errorf("Name mismatch: %q vs %q", got.Name, orig.Name)
	}
	if len(got.Extra) != len(orig.Extra) {
		t.Fatalf("Extra len: got %d want %d\n%+v", len(got.Extra), len(orig.Extra), got.Extra)
	}
	for k, want := range orig.Extra {
		gotEntry := got.Extra[k]
		if gotEntry.Name != want.Name {
			t.Errorf("Extra[%q].Name = %q want %q", k, gotEntry.Name, want.Name)
		}
		if want.Extra != nil {
			if age := gotEntry.Extra["age"]; age != want.Extra["age"] {
				t.Errorf("Extra[%q].Extra[age] = %v want %v", k, age, want.Extra["age"])
			}
		}
	}
}

// With an empty Extra, the typed catch-all marshals only the fixed fields.
func TestEmbed_TypedString_MarshalEmpty(t *testing.T) {
	t.Parallel()
	out, _ := ggen.MarshalString(EmbedStringsStruct{Name: "alice"})
	if out != `{"name":"alice"}` {
		t.Errorf("empty-extra marshal = %q", out)
	}
}

func TestEmbed_TypedStruct_MarshalEmpty(t *testing.T) {
	t.Parallel()
	out, _ := ggen.MarshalString(EmbedStructsStruct{Name: "root"})
	if out != `{"name":"root"}` {
		t.Errorf("empty-extra marshal = %q", out)
	}
}

// An empty RawMessage entry in the catch-all map must marshal as null.
// Appending its zero bytes would emit `"k":` with no value, which is corrupt
// JSON. The field-level raw emit and AppendAny also write null for empties.
func TestEmbed_EmptyRawMessageMarshalsNull(t *testing.T) {
	t.Parallel()
	out, err := ggen.Marshal(EmbedRawStruct{
		Name:  "x",
		Extra: map[string]json.RawMessage{"a": nil, "b": {}, "c": json.RawMessage(`{"n":1}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not valid JSON: %v\nwire: %s", err, out)
	}
	if string(m["a"]) != "null" || string(m["b"]) != "null" {
		t.Errorf("empty raw entries → a=%s b=%s, want null/null", m["a"], m["b"])
	}
	if string(m["c"]) != `{"n":1}` {
		t.Errorf("non-empty raw entry mangled: %s", m["c"])
	}
	// Round-trip: decode absorbs unknown keys back into the raw map.
	back, _, err := EmbedRawStruct{}.DecodeFrom(out)
	if err != nil || string(back.Extra["c"]) != `{"n":1}` {
		t.Fatalf("decode: %+v %v", back, err)
	}
}

// The catch-all map is emptied at decode entry like any other container: keys
// from a previous decode must not survive into the next one.
func TestEmbed_carriedMapDropsStaleKeys(t *testing.T) {
	t.Parallel()
	first, _, err := EmbedStructsStruct{}.DecodeFrom([]byte(`{"name":"n","a":{"name":"A"},"b":{"name":"B"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Extra) != 2 {
		t.Fatalf("first decode: %v", first.Extra)
	}
	got, _, err := first.DecodeFrom([]byte(`{"name":"n","a":{"name":"A2"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Extra) != 1 || got.Extra["a"].Name != "A2" {
		t.Errorf("stale catch-all keys survived: %v", got.Extra)
	}
	// The same for values decoded by the field emitters, on both paths.
	carried := func() EmbedTCPAddrPtrs {
		v, _, err := EmbedTCPAddrPtrs{}.DecodeFrom([]byte(`{"name":"n","a":":1","b":":2"}`))
		if err != nil || len(v.Extra) != 2 {
			t.Fatalf("first decode: %v %v", v.Extra, err)
		}
		return v
	}
	const second = `{"name":"n","a":":3"}`
	bgot, _, err := carried().DecodeFrom([]byte(second))
	if err != nil {
		t.Fatal(err)
	}
	sgot, err := ggen.NewStream(strings.NewReader(second), nil).Value(carried())
	if err != nil {
		t.Fatal(err)
	}
	for path, v := range map[string]EmbedTCPAddrPtrs{"bytes": bgot, "stream": sgot} {
		if len(v.Extra) != 1 || v.Extra["a"] == nil || v.Extra["a"].Port != 3 {
			t.Errorf("%s: stale catch-all keys survived: %v", path, v.Extra)
		}
	}
}

// embedRoundTrip runs a subtest that marshals v and requires the output to
// decode, on the bytes path and on a stream fed 1, 3 and 64 bytes per read,
// into a value that marshals to the same members.
func embedRoundTrip[T interface {
	ggen.Marshaler
	ggen.Decoder[T]
	ggen.StreamDecoder[T]
}](t *testing.T, name string, v T) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Parallel()
		out, err := ggen.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		same := func(path string, got T) {
			again, err := ggen.Marshal(got)
			if err != nil || !reflect.DeepEqual(objectKeys(t, again), objectKeys(t, out)) {
				t.Errorf("%s:\n got  %s (%v)\n want %s", path, again, err, out)
			}
		}
		var zero T
		got, _, err := zero.DecodeFrom(out)
		if err != nil {
			t.Fatalf("bytes: %v\nwire: %s", err, out)
		}
		same("bytes", got)
		for _, chunk := range []int{1, 3, 64} {
			var s ggen.Stream
			s.Reset(&chunkReader{data: out, max: chunk}, make([]byte, 0, 16))
			sgot, err := s.Value[T]()
			if err != nil {
				t.Fatalf("stream chunk %d: %v\nwire: %s", chunk, err, out)
			}
			same("stream", sgot)
		}
	})
}

// A catch-all reads back what it writes for values whose written form
// encoding/json does not read.
func TestEmbed_typedValuesRoundTrip(t *testing.T) {
	t.Parallel()
	cidr := mustCIDR("2001:db8::/32")
	tcp := net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 80}
	udp := net.UDPAddr{IP: net.ParseIP("fe80::1"), Port: 53, Zone: "eth0"}
	ttl := 90 * time.Second
	lease := EmbedLease{Addr: tcp, TTL: ttl}
	embedRoundTrip(t, "IPNet", EmbedIPNets{Name: "n", Extra: map[string]net.IPNet{"a": cidr, "zero": {}}})
	embedRoundTrip(t, "*IPNet", EmbedIPNetPtrs{Name: "n", Extra: map[string]*net.IPNet{"a": &cidr, "nil": nil}})
	embedRoundTrip(t, "TCPAddr", EmbedTCPAddrs{Name: "n", Extra: map[string]net.TCPAddr{"a": tcp, "port": {Port: 1}}})
	embedRoundTrip(t, "*TCPAddr", EmbedTCPAddrPtrs{Name: "n", Extra: map[string]*net.TCPAddr{"a": &tcp, "nil": nil}})
	embedRoundTrip(t, "UDPAddr", EmbedUDPAddrs{Name: "n", Extra: map[string]net.UDPAddr{"a": udp, "zero": {}}})
	embedRoundTrip(t, "*UDPAddr", EmbedUDPAddrPtrs{Name: "n", Extra: map[string]*net.UDPAddr{"a": &udp, "nil": nil}})
	embedRoundTrip(t, "Duration", EmbedDurations{Name: "n", Extra: map[string]time.Duration{"a": ttl, "neg": -time.Millisecond}})
	embedRoundTrip(t, "*Duration", EmbedDurationPtrs{Name: "n", Extra: map[string]*time.Duration{"a": &ttl, "nil": nil}})
	embedRoundTrip(t, "[16]byte", EmbedByteArrays{Name: "n", Extra: map[string][16]byte{"a": {1, 2, 255}, "zero": {}}})
	embedRoundTrip(t, "*generated", EmbedLeasePtrs{Name: "n", Extra: map[string]*EmbedLease{"a": &lease, "nil": nil}})
	embedRoundTrip(t, "[]generated", EmbedLeaseSlices{Name: "n", Extra: map[string][]EmbedLease{"a": {lease, {}}}})
	embedRoundTrip(t, "[]*UDPAddr", EmbedUDPAddrPtrSlices{Name: "n", Extra: map[string][]*net.UDPAddr{"a": {&udp, nil}}})
	embedRoundTrip(t, "foreign", EmbedEndpoints{Name: "n", Extra: map[string]thirdparty.Endpoint{"a": {Addr: tcp, Timeout: ttl}}})
	embedRoundTrip(t, "foreign generated", EmbedForeignLeases{Name: "n", Extra: map[string]thirdparty2.Lease{"a": {TTL: ttl}}})
	embedRoundTrip(t, "flagged", EmbedFlaggedUDPAddrPtrs{Name: "n", Extra: map[string]*net.UDPAddr{"a<": {IP: udp.IP, Port: 1, Zone: "a<b"}}})
}

// A struct ggen did not generate is written in a catch-all the way a
// map[string]V field writes it, here through encoding/json.
func TestEmbed_foreignStructWire(t *testing.T) {
	t.Parallel()
	v := EmbedEndpoints{Name: "n", Extra: map[string]thirdparty.Endpoint{"a": {
		Addr: net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 80}, Timeout: time.Second,
	}}}
	const want = `{"name":"n","a":{"Addr":{"IP":"1.2.3.4","Port":80,"Zone":""},"Timeout":1000000000}}`
	if out, err := ggen.Marshal(v); err != nil || string(out) != want {
		t.Errorf("got  %s (%v)\nwant %s", out, err, want)
	}
}

// A malformed catch-all value fails at the same position on both paths, and
// its error path starts at the member's key.
func TestEmbed_typedValueErrorPath(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		payload string
		run     func(string, int) (error, error)
		keys    []string
	}{
		{`{"name":"n","edge":"1.2.3.4"}`, decodeBothChunked[EmbedTCPAddrs], []string{"edge"}},
		{`{"name":"n","edge":"10.0.0.0/33"}`, decodeBothChunked[EmbedIPNetPtrs], []string{"edge"}},
		{`{"name":"n","edge":"soon"}`, decodeBothChunked[EmbedDurations], []string{"edge"}},
		{`{"name":"n","edge":[":1","x"]}`, decodeBothChunked[EmbedUDPAddrPtrSlices], []string{"edge[]"}},
		{`{"name":"n","edge":{"ttl":"soon"}}`, decodeBothChunked[EmbedLeasePtrs], []string{"edge", "ttl"}},
	} {
		for _, chunk := range []int{1, 3, 64} {
			bErr, sErr := c.run(c.payload, chunk)
			bp, bok := errors.AsType[*ggen.ParseError](bErr)
			sp, sok := errors.AsType[*ggen.ParseError](sErr)
			if !bok || !sok {
				t.Errorf("%s chunk %d: want ParseError on both paths, got %v / %v", c.payload, chunk, bErr, sErr)
				continue
			}
			if !slices.Equal(bp.Keys, c.keys) || !slices.Equal(sp.Keys, c.keys) || bp.Pos != sp.Pos {
				t.Errorf("%s chunk %d: keys %q / %q pos %d / %d, want keys %q on both at one pos",
					c.payload, chunk, bp.Keys, sp.Keys, bp.Pos, sp.Pos, c.keys)
			}
		}
	}
}

// Under copy, the key and the value of a typed catch-all entry do not change
// when the input is overwritten after decode.
func TestEmbed_typedValueCopyOutlivesInput(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"n","edge":"[fe80::1%eth0]:1"}`)
	got, _, err := EmbedFlaggedUDPAddrPtrs{}.DecodeFrom(in)
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		in[i] = 'X'
	}
	if a := got.Extra["edge"]; a == nil || a.Zone != "eth0" || a.Port != 1 {
		t.Errorf("entry changed with the input: %v", got.Extra)
	}
}

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	jsonv1 "encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
)

// HookedStruct exercises the opt-in MarshalJSON/UnmarshalJSON hooks.
//
//ggen:generate marshal unmarshal
type HookedStruct struct {
	Name string `json:"name" pipe:"required minlen=1 maxlen=20"`
	N    int    `json:"n"    pipe:"gte=0 lte=100"`
}

// Compile-time checks that the []byte-based MarshalJSON and UnmarshalJSON
// hooks satisfy the stdlib interfaces.
var (
	_ jsonv1.Marshaler = HookedStruct{}
	_ jsonv2.Marshaler = HookedStruct{}

	_ jsonv1.Unmarshaler = (*HookedStruct)(nil)
	_ jsonv2.Unmarshaler = (*HookedStruct)(nil)
)

// Validation errors from the hooks surface through both jsonv1.Unmarshal and
// jsonv2.Unmarshal.
func TestHookedValidationPropagates(t *testing.T) {
	t.Parallel()
	// N is out of range (lte=100), so generated validation fails.
	bad := []byte(`{"name":"ok","n":200}`)

	// jsonv1 path
	{
		var got HookedStruct
		err := jsonv1.Unmarshal(bad, &got)
		if err == nil {
			t.Fatal("v1 unmarshal: expected validation error, got nil")
		}
		if !strings.Contains(err.Error(), "exceeds maximum") {
			t.Errorf("v1 error = %q, want to contain 'exceeds maximum'", err.Error())
		}
	}

	// jsonv2 path
	{
		var got HookedStruct
		err := jsonv2.Unmarshal(bad, &got)
		if err == nil {
			t.Fatal("v2 unmarshal: expected validation error, got nil")
		}
		if !strings.Contains(err.Error(), "exceeds maximum") {
			t.Errorf("v2 error = %q, want to contain 'exceeds maximum'", err.Error())
		}
	}
}

// Valid data round-trips through both stdlib entry points.
func TestHookedRoundtrip(t *testing.T) {
	t.Parallel()
	in := HookedStruct{Name: "ok", N: 42}

	for _, tc := range []struct {
		name      string
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
	}{
		{"jsonv1", jsonv1.Marshal, jsonv1.Unmarshal},
		{
			"jsonv2",
			func(v any) ([]byte, error) { return jsonv2.Marshal(v) },
			func(b []byte, v any) error { return jsonv2.Unmarshal(b, v) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := tc.marshal(in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got HookedStruct
			if err := tc.unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got != in {
				t.Errorf("roundtrip: got %+v want %+v", got, in)
			}
		})
	}
}

// HookedOwned keeps a string and a raw span through the UnmarshalJSON hook.
//
//ggen:generate unmarshal
type HookedOwned struct {
	Name string            `json:"name"`
	Raw  jsonv1.RawMessage `json:"raw"`
}

// HookedOwnedCopy is HookedOwned under copy, where DecodeFrom already owns
// what it returns.
//
//ggen:generate unmarshal copy
type HookedOwnedCopy struct {
	Name string            `json:"name"`
	Raw  jsonv1.RawMessage `json:"raw"`
}

// The decoders reuse the buffer they hand to UnmarshalJSON, so values kept
// from a stream must not point into it.
func TestHookedValuesOutliveDecoderBuffer(t *testing.T) {
	t.Parallel()
	const n = 300
	var sb strings.Builder
	want := make([]string, n)
	for i := range want {
		want[i] = fmt.Sprintf("record-%04d-%s", i, strings.Repeat("x", 20))
		fmt.Fprintf(&sb, `{"name":%q,"raw":[%d]}`+"\n", want[i], i)
	}
	in := sb.String()
	check := func(t *testing.T, names []string, raws []string) {
		t.Helper()
		for i := range want {
			if names[i] != want[i] || raws[i] != fmt.Sprintf("[%d]", i) {
				t.Fatalf("value %d: name %q raw %q, want %q [%d]", i, names[i], raws[i], want[i], i)
			}
		}
	}
	t.Run("v1 decoder", func(t *testing.T) {
		t.Parallel()
		dec := jsonv1.NewDecoder(strings.NewReader(in))
		var names, raws []string
		var kept []HookedOwned
		for range n {
			var v HookedOwned
			if err := dec.Decode(&v); err != nil {
				t.Fatal(err)
			}
			kept = append(kept, v)
		}
		for _, v := range kept {
			names, raws = append(names, v.Name), append(raws, string(v.Raw))
		}
		check(t, names, raws)
	})
	t.Run("v2 read", func(t *testing.T) {
		t.Parallel()
		var kept []HookedOwned
		if err := jsonv2.UnmarshalRead(strings.NewReader("["+strings.ReplaceAll(strings.TrimSpace(in), "\n", ",")+"]"), &kept); err != nil {
			t.Fatal(err)
		}
		var names, raws []string
		for _, v := range kept {
			names, raws = append(names, v.Name), append(raws, string(v.Raw))
		}
		check(t, names, raws)
	})
	t.Run("v1 decoder copy", func(t *testing.T) {
		t.Parallel()
		dec := jsonv1.NewDecoder(strings.NewReader(in))
		var kept []HookedOwnedCopy
		for range n {
			var v HookedOwnedCopy
			if err := dec.Decode(&v); err != nil {
				t.Fatal(err)
			}
			kept = append(kept, v)
		}
		var names, raws []string
		for _, v := range kept {
			names, raws = append(names, v.Name), append(raws, string(v.Raw))
		}
		check(t, names, raws)
	})
}

// HookedCount is a primitive alias with the hook.
//
//ggen:generate unmarshal
type HookedCount int

// A direct UnmarshalJSON call takes one value and rejects anything after it
// but whitespace, as json.Unmarshal does.
func TestHookedRejectsTrailingData(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`{"name":"a"}{"name":"b"}`, `{"name":"a"} x`, `{"name":"a"}]`} {
		var v HookedOwned
		if err := v.UnmarshalJSON([]byte(in)); !errors.Is(err, ggen.ErrTrailingData) {
			t.Errorf("%q: err = %v, want ErrTrailingData", in, err)
		}
	}
	for _, in := range []string{"12x", "12 3"} {
		var c HookedCount
		if err := c.UnmarshalJSON([]byte(in)); !errors.Is(err, ggen.ErrTrailingData) {
			t.Errorf("%q: err = %v, want ErrTrailingData", in, err)
		}
	}
	var v HookedOwned
	if err := v.UnmarshalJSON([]byte(" {\"name\":\"a\",\"raw\":1} \n")); err != nil || v.Name != "a" {
		t.Errorf("trailing whitespace: %+v, %v", v, err)
	}
}

// HookedLeaf is decoded under copy.
//
//ggen:generate copy
type HookedLeaf struct {
	Name string `json:"name"`
}

// HookedLooseLeaf points into its input.
//
//ggen:generate
type HookedLooseLeaf struct {
	Name string `json:"name"`
}

// HookedTree decodes under copy all the way down, so its hook reads the
// caller's bytes directly.
//
//ggen:generate unmarshal copy
type HookedTree struct {
	Leaf   HookedLeaf            `json:"leaf"`
	Ptr    *HookedLeaf           `json:"ptr"`
	List   []HookedLeaf          `json:"list"`
	ByKey  map[string]HookedLeaf `json:"byKey"`
	Rows   [][]string            `json:"rows"`
	Groups map[string][]string   `json:"groups"`
	Num    jsonv1.Number         `json:"num"`
	Any    any                   `json:"any"`
	Kids   []HookedTree          `json:"kids"`
}

// HookedMixed is under copy but reaches a decoder that is not, so its hook
// clones.
//
//ggen:generate unmarshal copy
type HookedMixed struct {
	Name  string                       `json:"name"`
	Loose map[string][]HookedLooseLeaf `json:"loose"`
}

// A hook's result, and its error, stay intact when the caller overwrites the
// bytes it passed in, whether the hook clones them or reads them directly.
func TestHookedCopyOwnsNestedValues(t *testing.T) {
	t.Parallel()
	const leaf = `{"name":"leaf-name-value"}`
	scribble := func(buf []byte) {
		for i := range buf {
			buf[i] = '#'
		}
	}
	for _, c := range []struct {
		v  jsonv1.Unmarshaler
		in string
	}{
		{new(HookedTree), `{"leaf":` + leaf + `,"ptr":` + leaf + `,"list":[` + leaf + `],"byKey":{"key-one":` + leaf + `},` +
			`"rows":[["cell-a","cell-b"]],"groups":{"group-key":["member"]},"num":1234.5,"any":{"k":["deep-string"]},` +
			`"kids":[{"leaf":` + leaf + `,"kids":[{"rows":[["grandkid"]]}]}]}`},
		{new(HookedMixed), `{"name":"mixed-name","loose":{"loose-key":[` + leaf + `]}}`},
		{new(HookedOwned), `{"name":"owned-name","raw":{"k":[1,2]}}`},
		{new(HookedOwnedCopy), `{"name":"owned-name","raw":{"k":[1,2]}}`},
		{new(HookedCount), `42`},
	} {
		buf := []byte(c.in)
		if err := c.v.UnmarshalJSON(buf); err != nil {
			t.Fatalf("%T: %v", c.v, err)
		}
		want, err := jsonv1.Marshal(c.v)
		if err != nil {
			t.Fatal(err)
		}
		scribble(buf)
		if got, _ := jsonv1.Marshal(c.v); !bytes.Equal(got, want) {
			t.Errorf("%T changed with its input:\n got  %s\n want %s", c.v, got, want)
		}
	}
	for _, in := range []string{`{"unknown-key-name":1}`, `{"num":"not-a-number"}`, `{"leaf":{"name":7}}`} {
		buf := []byte(in)
		err := new(HookedTree).UnmarshalJSON(buf)
		if err == nil {
			t.Fatalf("%s: no error", in)
		}
		want := err.Error()
		scribble(buf)
		if got := err.Error(); got != want {
			t.Errorf("%s: error changed with its input:\n got  %s\n want %s", in, got, want)
		}
	}
}

package integrationtests

import (
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
)

// OmitStruct exercises the json tag options: omitempty (skip JSON-empty on
// marshal), omitzero (skip Go-zero on marshal), and string (wrap primitive as
// JSON string on both marshal and unmarshal).
//
//ggen:generate
type OmitStruct struct {
	Name     string            `json:"name"`
	Bio      string            `json:"bio,omitempty"`
	Score    float64           `json:"score,omitzero"`
	StrCount int               `json:"count,string"`
	Tags     []string          `json:"tags,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Meta     map[string]string `json:"meta,omitzero"`
	Extra    []string          `json:"extra,omitzero"`
}

func TestOmitEmpty_marshal(t *testing.T) {
	// Empty Bio + nil Tags → both omitted
	s := OmitStruct{Name: "alice", Score: 0, StrCount: 42}
	out, _ := encode.MarshalString(s)
	if strings.Contains(out, "bio") {
		t.Errorf("expected bio omitted, got %q", out)
	}
	if strings.Contains(out, "tags") {
		t.Errorf("expected tags omitted, got %q", out)
	}
	if !strings.Contains(out, `"name":"alice"`) {
		t.Errorf("name missing: %q", out)
	}
}

func TestOmitEmpty_present(t *testing.T) {
	s := OmitStruct{Name: "a", Bio: "hello", Tags: []string{"x"}, StrCount: 1}
	out, _ := encode.MarshalString(s)
	if !strings.Contains(out, `"bio":"hello"`) {
		t.Errorf("bio missing: %q", out)
	}
	if !strings.Contains(out, `"tags":["x"]`) {
		t.Errorf("tags missing: %q", out)
	}
}

func TestOmitZero_marshal(t *testing.T) {
	// Score=0 → omitted via omitzero
	s := OmitStruct{Name: "x", Score: 0, StrCount: 1}
	out, _ := encode.MarshalString(s)
	if strings.Contains(out, "score") {
		t.Errorf("expected score omitted, got %q", out)
	}

	s.Score = 3.14
	out, _ = encode.MarshalString(s)
	if !strings.Contains(out, `"score":3.14`) {
		t.Errorf("score missing: %q", out)
	}
}

func TestStringTag_marshal(t *testing.T) {
	s := OmitStruct{Name: "x", StrCount: 42}
	out, _ := encode.MarshalString(s)
	// StrCount must be JSON-string-wrapped, not a bare number
	if !strings.Contains(out, `"count":"42"`) {
		t.Errorf("expected quoted count, got %q", out)
	}
}

func TestStringTag_unmarshal(t *testing.T) {
	input := []byte(`{"name":"x","count":"99"}`)
	got, err := decode.Unmarshal[OmitStruct](input)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.StrCount != 99 {
		t.Errorf("StrCount = %d, want 99", got.StrCount)
	}
}

func TestStringTag_unmarshalBadString(t *testing.T) {
	input := []byte(`{"name":"x","count":"abc"}`)
	if _, err := decode.Unmarshal[OmitStruct](input); err == nil {
		t.Error("expected parse error for non-numeric string")
	}
}

func TestStringTag_unmarshalExpectsString(t *testing.T) {
	// count is plain number, not a string — must error
	input := []byte(`{"name":"x","count":99}`)
	if _, err := decode.Unmarshal[OmitStruct](input); err == nil {
		t.Error("expected error when count is bare number instead of string-wrapped")
	}
}

func TestOmit_roundtrip(t *testing.T) {
	orig := OmitStruct{Name: "alice", Bio: "dev", Score: 9.5, StrCount: 42, Tags: []string{"go", "rust"}}
	out, _ := encode.Marshal(orig)
	got, err := decode.Unmarshal[OmitStruct](out)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Name != orig.Name || got.Bio != orig.Bio || got.Score != orig.Score ||
		got.StrCount != orig.StrCount || len(got.Tags) != len(orig.Tags) {
		t.Errorf("roundtrip: got %+v want %+v", got, orig)
	}
}

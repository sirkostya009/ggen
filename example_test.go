//go:build goexperiment.jsonv2

package schema

import (
	"strings"
	"testing"
)

func TestParseFrom_valid(t *testing.T) {
	input := `{"field1": "hello", "array": [1, 2, 3]}`
	result, err := SomePayloadRequestStruct{}.ParseFrom(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if result.Field1 != "hello" {
		t.Errorf("Field1 = %q, want %q", result.Field1, "hello")
	}
	if len(result.Slice) != 3 || result.Slice[0] != 1 || result.Slice[1] != 2 || result.Slice[2] != 3 {
		t.Errorf("Slice = %v, want [1 2 3]", result.Slice)
	}
}

func TestParseFrom_optionalField1(t *testing.T) {
	input := `{"array": [5]}`
	result, err := SomePayloadRequestStruct{}.ParseFrom(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if result.Field1 != "" {
		t.Errorf("Field1 = %q, want empty", result.Field1)
	}
	if len(result.Slice) != 1 || result.Slice[0] != 5 {
		t.Errorf("Slice = %v, want [5]", result.Slice)
	}
}

func TestParseFrom_missingRequiredField(t *testing.T) {
	input := `{"field1": "hello"}`
	_, err := SomePayloadRequestStruct{}.ParseFrom(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for missing required field")
	}
	if !strings.Contains(err.Error(), "missing required field") {
		t.Errorf("error = %q, want 'missing required field'", err.Error())
	}
}

func TestParseFrom_minlen(t *testing.T) {
input := `{"field1": "a", "array": [1]}`
	_, err := SomePayloadRequestStruct{}.ParseFrom(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for minlen violation")
	}
	if !strings.Contains(err.Error(), "below minimum") {
		t.Errorf("error = %q, want 'below minimum'", err.Error())
	}
}

func TestParseFrom_maxlen(t *testing.T) {
	input := `{"field1": "` + strings.Repeat("x", 24) + `", "array": [1]}`
	_, err := SomePayloadRequestStruct{}.ParseFrom(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for maxlen violation")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("error = %q, want 'exceeds maximum'", err.Error())
	}
}

func TestParseFrom_sliceMinlen(t *testing.T) {
	input := `{"array": []}`
	_, err := SomePayloadRequestStruct{}.ParseFrom(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for empty slice (minlen=1)")
	}
	if !strings.Contains(err.Error(), "below minimum") {
		t.Errorf("error = %q, want 'below minimum'", err.Error())
	}
}

func TestParseFrom_sliceMaxlen(t *testing.T) {
	elems := make([]string, 11)
	for i := range elems {
		elems[i] = "1"
	}
	input := `{"array": [` + strings.Join(elems, ",") + `]}`
	_, err := SomePayloadRequestStruct{}.ParseFrom(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for slice exceeding maxlen")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("error = %q, want 'exceeds maximum'", err.Error())
	}
}

func TestParseFrom_unknownFields(t *testing.T) {
	input := `{"field1": "hi", "array": [1], "unknown": "value", "another": 42}`
	result, err := SomePayloadRequestStruct{}.ParseFrom(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unknown fields should be skipped, got: %v", err)
	}
	if result.Field1 != "hi" {
		t.Errorf("Field1 = %q, want %q", result.Field1, "hi")
	}
}

func TestParseFrom_wrongType(t *testing.T) {
	input := `{"field1": 123, "array": [1]}`
	_, err := SomePayloadRequestStruct{}.ParseFrom(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for wrong type")
	}
	if !strings.Contains(err.Error(), "expected string") {
		t.Errorf("error = %q, want 'expected string'", err.Error())
	}
}

func TestParseFrom_wrongSliceElemType(t *testing.T) {
	input := `{"array": ["not", "numbers"]}`
	_, err := SomePayloadRequestStruct{}.ParseFrom(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for wrong slice element type")
	}
	if !strings.Contains(err.Error(), "expected number") {
		t.Errorf("error = %q, want 'expected number'", err.Error())
	}
}

func TestParseFrom_invalidJSON(t *testing.T) {
	input := `not json`
	_, err := SomePayloadRequestStruct{}.ParseFrom(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseFrom_notObject(t *testing.T) {
	input := `[1, 2, 3]`
	_, err := SomePayloadRequestStruct{}.ParseFrom(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for non-object JSON")
	}
	if !strings.Contains(err.Error(), "expected '{'") {
		t.Errorf("error = %q, want \"expected '{'\"", err.Error())
	}
}

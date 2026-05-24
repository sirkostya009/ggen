package integrationtests

//go:generate ../ggen $GOFILE

import (
	"strings"
	"testing"
)

// IgnoreUnknownStruct tests that `//ggen:generate ignoreunknown` suppresses
// the default UnknownKey validation error and silently skips extra JSON keys.
//
//ggen:generate ignoreunknown
type IgnoreUnknownStruct struct {
	Name string `json:"name"`
}

const validAddress = `{"street": "Main 1", "city": "Lviv", "zipCode": "79000"}`

func TestRead_valid(t *testing.T) {
	got, _, err := Address{}.DecodeFrom([]byte(validAddress))
	if err != nil {
		t.Fatal(err)
	}
	if got.Street != "Main 1" || got.City != "Lviv" || got.ZipCode != "79000" {
		t.Errorf("got %+v", got)
	}
}

func TestRead_missingRequired(t *testing.T) {
	// city omitted — Address.city is `required,notempty`.
	_, _, err := Address{}.DecodeFrom([]byte(`{"street":"a","zipCode":"12345"}`))
	if err == nil {
		t.Fatal("expected missing-required error")
	}
	if !strings.Contains(err.Error(), "city") {
		t.Errorf("error = %q, want 'city'", err.Error())
	}
}

func TestRead_notempty(t *testing.T) {
	// city has notempty; empty string fails.
	bad := `{"street":"s","city":"","zipCode":"12345"}`
	_, _, err := Address{}.DecodeFrom([]byte(bad))
	if err == nil {
		t.Fatal("expected notempty error")
	}
	if !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("error = %q, want 'must not be empty'", err.Error())
	}
}

func TestRead_len(t *testing.T) {
	// zipCode len=5 exact; length 6 fails.
	bad := `{"street":"s","city":"c","zipCode":"123456"}`
	_, _, err := Address{}.DecodeFrom([]byte(bad))
	if err == nil {
		t.Fatal("expected len error")
	}
	if !strings.Contains(err.Error(), "length") {
		t.Errorf("error = %q, want 'length'", err.Error())
	}
}

func TestRead_unknownFields_errorByDefault(t *testing.T) {
	input := `{"street":"s","city":"c","zipCode":"12345","xx":"y"}`
	_, _, err := Address{}.DecodeFrom([]byte(input))
	if err == nil {
		t.Fatal("expected error on unknown key")
	}
	if !strings.Contains(err.Error(), `unknown key "xx"`) {
		t.Errorf("error = %q, want unknown-key message", err.Error())
	}
}

func TestRead_unknownFields_ignoreOptIn(t *testing.T) {
	// IgnoreUnknownStruct has `//ggen:generate ignoreunknown` — extras silently skipped.
	input := []byte(`{"name":"alice","extra":42,"also":"ignored"}`)
	got, _, err := IgnoreUnknownStruct{}.DecodeFrom(input)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Name != "alice" {
		t.Errorf("Name = %q", got.Name)
	}
}

func TestRead_wrongType(t *testing.T) {
	// street is a string; integer should error.
	input := `{"street":123,"city":"c","zipCode":"12345"}`
	_, _, err := Address{}.DecodeFrom([]byte(input))
	if err == nil {
		t.Fatal("expected wrong-type error")
	}
	if !strings.Contains(err.Error(), "expected string") {
		t.Errorf("error = %q, want 'expected string'", err.Error())
	}
}

func TestRead_notObject(t *testing.T) {
	_, _, err := Address{}.DecodeFrom([]byte(`[1,2,3]`))
	if err == nil {
		t.Fatal("expected not-object error")
	}
}

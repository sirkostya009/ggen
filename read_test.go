package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/decode"
)

// IgnoreUnknownStruct tests that `//ggen:generate ignoreunknown` suppresses
// the default UnknownKey validation error and silently skips extra JSON keys.
//
//ggen:generate ignoreunknown
type IgnoreUnknownStruct struct {
	Name string `json:"name"`
}

const validAddress = `{"street": "Main 1", "city": "Lviv", "zipCode": "79000"}`

func parsePayload(t *testing.T, body string) (SomePayloadRequestStruct, error) {
	t.Helper()
	return decode.Read[SomePayloadRequestStruct](strings.NewReader(body))
}

func TestRead_valid(t *testing.T) {
	input := `{"field1": "hello", "array": [1, 2, 3], "address": ` + validAddress + `}`
	result, err := parsePayload(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Field1 != "hello" {
		t.Errorf("Field1 = %q, want %q", result.Field1, "hello")
	}
	if len(result.Slice) != 3 {
		t.Errorf("Slice = %v, want [1 2 3]", result.Slice)
	}
	if result.Address.City != "Lviv" {
		t.Errorf("Address.City = %q, want Lviv", result.Address.City)
	}
}

func TestRead_nestedStructValidation(t *testing.T) {
	// zipCode length 4, must be 5
	input := `{"array": [1], "address": {"street": "a", "city": "b", "zipCode": "1234"}}`
	_, err := parsePayload(t, input)
	if err == nil {
		t.Fatal("expected nested validation error")
	}
	if !strings.Contains(err.Error(), "zipCode") {
		t.Errorf("error = %q, want 'zipCode' context", err.Error())
	}
}

func TestRead_missingRequiredNested(t *testing.T) {
	// missing address
	_, err := parsePayload(t, `{"array": [1]}`)
	if err == nil {
		t.Fatal("expected missing required field error")
	}
	if !strings.Contains(err.Error(), "address") {
		t.Errorf("error = %q, want 'address'", err.Error())
	}
}

func TestRead_sliceOfStructs(t *testing.T) {
	input := `{"array": [1], "address": ` + validAddress + `, "contacts": [` + validAddress + `, ` + validAddress + `]}`
	res, err := parsePayload(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Contacts) != 2 {
		t.Errorf("Contacts len = %d, want 2", len(res.Contacts))
	}
}

func TestRead_email(t *testing.T) {
	cases := []struct {
		addr    string
		wantErr bool
	}{
		{"foo@bar.com", false},
		{"not-an-email", true},
		{"a@b", true},
	}
	for _, tc := range cases {
		input := `{"array":[1],"address":` + validAddress + `,"email":"` + tc.addr + `"}`
		_, err := parsePayload(t, input)
		if tc.wantErr && err == nil {
			t.Errorf("addr %q: expected error", tc.addr)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("addr %q: unexpected error: %v", tc.addr, err)
		}
	}
}

func TestRead_url(t *testing.T) {
	input := `{"array":[1],"address":` + validAddress + `,"website":"https://example.com/path"}`
	if _, err := parsePayload(t, input); err != nil {
		t.Errorf("valid url: unexpected error: %v", err)
	}

	bad := `{"array":[1],"address":` + validAddress + `,"website":"not a url"}`
	if _, err := parsePayload(t, bad); err == nil {
		t.Error("bad url: expected error")
	}
}

func TestRead_oneofString(t *testing.T) {
	ok := `{"array":[1],"address":` + validAddress + `,"role":"admin"}`
	if _, err := parsePayload(t, ok); err != nil {
		t.Errorf("allowed role: unexpected error: %v", err)
	}

	bad := `{"array":[1],"address":` + validAddress + `,"role":"superuser"}`
	_, err := parsePayload(t, bad)
	if err == nil {
		t.Fatal("disallowed role: expected error")
	}
	if !strings.Contains(err.Error(), "not in allowed set") {
		t.Errorf("error = %q, want 'not in allowed set'", err.Error())
	}
}

func TestRead_oneofNumber(t *testing.T) {
	ok := `{"title":"x","kind":2}`
	if _, err := decode.Read[AnotherStruct](strings.NewReader(ok)); err != nil {
		t.Errorf("allowed kind: unexpected error: %v", err)
	}

	bad := `{"title":"x","kind":7}`
	if _, err := decode.Read[AnotherStruct](strings.NewReader(bad)); err == nil {
		t.Error("disallowed kind: expected error")
	}
}

func TestRead_gte_lte(t *testing.T) {
	cases := []struct {
		age     int
		wantErr bool
	}{
		{0, false}, {150, false},
		{-1, true}, {151, true},
	}
	for _, tc := range cases {
		input := `{"array":[1],"address":` + validAddress + `,"age":` + strconv.Itoa(tc.age) + `}`
		_, err := parsePayload(t, input)
		if tc.wantErr && err == nil {
			t.Errorf("age %d: expected error", tc.age)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("age %d: unexpected error: %v", tc.age, err)
		}
	}
}

func TestRead_gtZero(t *testing.T) {
	ok := `{"array":[1],"address":` + validAddress + `,"quota":5}`
	if _, err := parsePayload(t, ok); err != nil {
		t.Errorf("positive quota: unexpected error: %v", err)
	}

	bad := `{"array":[1],"address":` + validAddress + `,"quota":0}`
	if _, err := parsePayload(t, bad); err == nil {
		t.Error("zero quota: expected error")
	}
}

func TestRead_notempty(t *testing.T) {
	// Address.City has notempty; empty string fails
	bad := `{"array":[1],"address":{"street":"s","city":"","zipCode":"12345"}}`
	_, err := parsePayload(t, bad)
	if err == nil {
		t.Fatal("expected notempty error")
	}
	if !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("error = %q, want 'must not be empty'", err.Error())
	}
}

func TestRead_len(t *testing.T) {
	// zipCode len=5 exact; length 6 fails
	bad := `{"array":[1],"address":{"street":"s","city":"c","zipCode":"123456"}}`
	_, err := parsePayload(t, bad)
	if err == nil {
		t.Fatal("expected len error")
	}
	if !strings.Contains(err.Error(), "length") {
		t.Errorf("error = %q, want 'length'", err.Error())
	}
}

func TestRead_unknownFields_errorByDefault(t *testing.T) {
	input := `{"array":[1],"address":` + validAddress + `,"xx":"y"}`
	_, err := parsePayload(t, input)
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
	got, err := decode.Unmarshal[IgnoreUnknownStruct](input)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Name != "alice" {
		t.Errorf("Name = %q", got.Name)
	}
}

func TestRead_wrongType(t *testing.T) {
	input := `{"array":[1],"address":` + validAddress + `,"field1":123}`
	_, err := parsePayload(t, input)
	if err == nil {
		t.Fatal("expected wrong-type error")
	}
	if !strings.Contains(err.Error(), "expected string") {
		t.Errorf("error = %q, want 'expected string'", err.Error())
	}
}

func TestRead_notObject(t *testing.T) {
	_, err := parsePayload(t, `[1,2,3]`)
	if err == nil {
		t.Fatal("expected not-object error")
	}
}

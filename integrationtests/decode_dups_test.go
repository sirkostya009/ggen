//go:build goexperiment.jsonv2

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"strings"
	"testing"
)

func TestDuplicateKey_rejected(t *testing.T) {
	// Duplicate-key confusion attack. jsontext's default rejects at the
	// decoder level; if that's ever relaxed, our generated `seen<Field>`
	// guard is the fallback.
	in := []byte(`{"name":"guest","n":1,"n":999}`)
	_, _, err := HookedStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected duplicate-key error")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q, want 'duplicate' substring", err.Error())
	}
}

func TestDuplicateKey_firstFieldAlsoRejected(t *testing.T) {
	in := []byte(`{"name":"a","name":"b","n":1}`)
	_, _, err := HookedStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected duplicate-key error")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q", err.Error())
	}
}

// AllowDupsStruct opts out of the default duplicate-key error. The wire
// contract becomes first-wins: the first key-value pair is parsed,
// subsequent occurrences of the same key are skipped via scan.SkipValue
// without being decoded into the field.
//
//ggen:generate allowdups
type AllowDupsStruct HookedStruct

// TestAllowDups_firstWins: the first occurrence ("alice") sticks; the
// second ("bob") is skipped, NOT overwritten. Confirms the dup is
// scan.SkipValue'd past instead of being parsed and assigned.
func TestAllowDups_firstWins(t *testing.T) {
	in := []byte(`{"name":"alice","name":"bob","n":1}`)
	got, _, err := AllowDupsStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Name != "alice" {
		t.Errorf("Name = %q, want %q (first-wins)", got.Name, "alice")
	}
	if got.N != 1 {
		t.Errorf("N = %d, want 1", got.N)
	}
}

// TestAllowDups_firstWins_numeric: same shape but with a numeric field
// to confirm the skip path works for non-string values too.
func TestAllowDups_firstWins_numeric(t *testing.T) {
	in := []byte(`{"name":"x","n":10,"n":999,"n":-5}`)
	got, _, err := AllowDupsStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.N != 10 {
		t.Errorf("N = %d, want 10 (first-wins, later 999 and -5 must be skipped)", got.N)
	}
}

// TestAllowDups_skipMalformedValueStillErrors: a malformed JSON value
// inside a duplicate key entry must still surface as a parse error —
// scan.SkipValue is responsible for advancing past the value, and a
// truly broken value should fail there.
func TestAllowDups_skipMalformedValueStillErrors(t *testing.T) {
	// First "n":1 parsed; second "n":@@@ is invalid JSON.
	in := []byte(`{"name":"x","n":1,"n":@@@}`)
	if _, _, err := (AllowDupsStruct{}).DecodeFrom(in); err == nil {
		t.Error("expected scan error on malformed duplicate value")
	}
}

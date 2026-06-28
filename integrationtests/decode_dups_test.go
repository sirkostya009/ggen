//go:build goexperiment.jsonv2

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"strings"
	"testing"
)

func TestDuplicateKey_rejected(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	in := []byte(`{"name":"a","name":"b","n":1}`)
	_, _, err := HookedStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected duplicate-key error")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q", err.Error())
	}
}

// AllowDupsStruct opts out of the duplicate-key error: first-wins, later
// occurrences skipped.
//
//ggen:generate allowdups
type AllowDupsStruct HookedStruct

// First occurrence sticks, the second is skipped not overwritten.
func TestAllowDups_firstWins(t *testing.T) {
	t.Parallel()
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

// Same first-wins on a numeric field.
func TestAllowDups_firstWins_numeric(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"x","n":10,"n":999,"n":-5}`)
	got, _, err := AllowDupsStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.N != 10 {
		t.Errorf("N = %d, want 10 (first-wins, later 999 and -5 must be skipped)", got.N)
	}
}

// A malformed value in a skipped duplicate still errors.
func TestAllowDups_skipMalformedValueStillErrors(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"x","n":1,"n":@@@}`)
	if _, _, err := (AllowDupsStruct{}).DecodeFrom(in); err == nil {
		t.Error("expected scan error on malformed duplicate value")
	}
}

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"errors"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
)

// IgnoreUnknownStruct uses ignoreunknown, which silently skips JSON keys that
// match no field.
//
//ggen:generate ignoreunknown
type IgnoreUnknownStruct struct {
	Name string `json:"name"`
}

const validAddress = `{"street": "Main 1", "city": "Lviv", "zipCode": "79000"}`

func TestRead_valid(t *testing.T) {
	t.Parallel()
	got, _, err := Address{}.DecodeFrom([]byte(validAddress))
	if err != nil {
		t.Fatal(err)
	}
	if got.Street != "Main 1" || got.City != "Lviv" || got.ZipCode != "79000" {
		t.Errorf("got %+v", got)
	}
}

func TestRead_missingRequired(t *testing.T) {
	t.Parallel()
	// city is omitted, and Address.city is `required,notempty`.
	_, _, err := Address{}.DecodeFrom([]byte(`{"street":"a","zipCode":"12345"}`))
	if err == nil {
		t.Fatal("expected missing-required error")
	}
	if !strings.Contains(err.Error(), "city") {
		t.Errorf("error = %q, want 'city'", err.Error())
	}
}

func TestRead_notempty(t *testing.T) {
	t.Parallel()
	// city has notempty, so an empty string fails.
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
	t.Parallel()
	// zipCode must have exactly 5 characters (len=5), so 6 fails.
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
	t.Parallel()
	input := `{"street":"s","city":"c","zipCode":"12345","xx":"y"}`
	_, _, err := Address{}.DecodeFrom([]byte(input))
	if err == nil {
		t.Fatal("expected error on unknown key")
	}
	if err.Error() != "xx: unknown key" {
		t.Errorf("error = %q, want unknown-key message", err.Error())
	}
}

func TestRead_unknownFields_ignoreOptIn(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	_, _, err := Address{}.DecodeFrom([]byte(`[1,2,3]`))
	if err == nil {
		t.Fatal("expected not-object error")
	}
}

// Malformed JSON at the top level gives a *ParseError that wraps the scanner's
// sentinel error, so errors.Is works. It has no field path.
func TestRead_parseErrorTopLevel(t *testing.T) {
	t.Parallel()
	_, _, err := (Address{}).DecodeFrom([]byte(`not-an-object`))
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *ggen.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T %v, want *ggen.ParseError", err, err)
	}
	if !errors.Is(err, ggen.ErrBadObject) {
		t.Fatalf("errors.Is(err, ggen.ErrBadObject) = false; got %v", err)
	}
	if len(pe.Keys) != 0 {
		t.Fatalf("Path = %v; want empty for top-level error", pe.Keys)
	}
}

// A wrong field type populates the ParseError path with the failing JSON key.
func TestRead_parseErrorFieldName(t *testing.T) {
	t.Parallel()
	_, _, err := (Address{}).DecodeFrom([]byte(`{"street":123,"city":"C","zipCode":"12345"}`))
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *ggen.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T, want *ggen.ParseError", err)
	}
	if strings.Join(pe.Keys, ".") != "street" {
		t.Fatalf("Path = %v; want [street]", pe.Keys)
	}
	if pe.Pos <= 0 {
		t.Fatalf("Pos = %d; want > 0", pe.Pos)
	}
}

// Validation errors keep their own types and are not wrapped in a ParseError.
func TestRead_validationNotWrapped(t *testing.T) {
	t.Parallel()
	_, _, err := (Address{}).DecodeFrom([]byte(`{"street":"","city":"C","zipCode":"12345"}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := errors.AsType[*ggen.MinLenError](err); !ok {
		t.Fatalf("err = %T %v, want *ggen.MinLenError", err, err)
	}
	if _, ok := errors.AsType[*ggen.ParseError](err); ok {
		t.Fatalf("validation error wrapped in ParseError: %v", err)
	}
}

// The unknown-key branch must copy the key before it calls ConsumeColon or
// SkipValue. The key from KeyView aliases the Stream's buffer (points into
// it, no copy). Both calls can refill the buffer and move its bytes, so a key
// copied later would show up in the error path as garbage.
func TestIgnoreUnknown_streamErrorKeepsKeyName(t *testing.T) {
	t.Parallel()
	payload := `{"unknownkeyname":[` + strings.Repeat("1,", 400) + `@]}`
	var s ggen.Stream
	s.Reset(&chunkReader{data: []byte(payload), max: 1}, make([]byte, 0, 32))
	if _, err := (IgnoreUnknownStruct{}).DecodeFromStream(&s); err == nil {
		t.Fatal("want parse error")
	} else {
		var pe *ggen.ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("err = %T %v", err, err)
		}
		if len(pe.Keys) != 1 || pe.Keys[0] != "unknownkeyname" {
			t.Errorf("Path = %q, want [unknownkeyname]", pe.Keys)
		}
	}
}

// UnknownKeyError.Pos is the offset of the value start on both the bytes and
// the stream path. A key with no colon after it is a grammar error, not an
// unknown key, on both paths. The multierr aggregate carries the same Pos.
func TestRead_unknownKey_streamParity(t *testing.T) {
	t.Parallel()
	for _, p := range []string{`{"zz":1}`, `{"id":1,"zz":2}`, `{ "zz" : 1 }`} {
		_, _, bytesErr := Node{}.DecodeFrom([]byte(p))
		var buk *ggen.UnknownKeyError
		if !errors.As(bytesErr, &buk) {
			t.Fatalf("%q bytes: %v", p, bytesErr)
		}
		for _, chunk := range []int{1, 64} {
			var s ggen.Stream
			s.Reset(&chunkReader{data: []byte(p), max: chunk}, make([]byte, 0, 16))
			_, streamErr := Node{}.DecodeFromStream(&s)
			var suk *ggen.UnknownKeyError
			if !errors.As(streamErr, &suk) {
				t.Fatalf("%q stream chunk=%d: %v", p, chunk, streamErr)
			}
			if suk.Pos != buk.Pos {
				t.Errorf("%q chunk=%d: Pos bytes=%d stream=%d", p, chunk, buk.Pos, suk.Pos)
			}
		}
	}
	for _, p := range []string{`{"zz" 1}`, `{"zz"}`} {
		bytesErr, streamErr := decodeBothPaths[Node](p)
		if !errors.Is(bytesErr, ggen.ErrBadObject) || !errors.Is(streamErr, ggen.ErrBadObject) {
			t.Errorf("%q: bytes=%v stream=%v", p, bytesErr, streamErr)
		}
	}
	p := `{"name":"ab","zz":{"deep":[1]},"age":5,"role":"admin"}`
	bytesErr, streamErr := decodeBothPaths[MultiErrStruct](p)
	var buk, suk *ggen.UnknownKeyError
	if !errors.As(bytesErr, &buk) || !errors.As(streamErr, &suk) {
		t.Fatalf("multierr: bytes=%v stream=%v", bytesErr, streamErr)
	}
	if buk.Pos != suk.Pos {
		t.Errorf("multierr: Pos bytes=%d stream=%d", buk.Pos, suk.Pos)
	}
}

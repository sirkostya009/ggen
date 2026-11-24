package integrationtests

//go:generate ../ggen $GOFILE

import (
	"errors"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
)

// Ping has no decodable field, so every key is unknown.
//
//ggen:generate
type Ping struct{}

// PingHidden has only a field JSON never sees.
//
//ggen:generate
type PingHidden struct {
	A int `json:"-"`
}

func TestEmptyStruct_decode(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`{}`, ` { } `} {
		if _, _, err := (Ping{}).DecodeFrom([]byte(in)); err != nil {
			t.Errorf("bytes %q: %v", in, err)
		}
		s := ggen.NewStream(strings.NewReader(in), nil)
		if _, err := (Ping{}).DecodeFromStream(s); err != nil {
			t.Errorf("stream %q: %v", in, err)
		}
	}

	in := []byte(`{"a":1}`)
	_, n, err := (PingHidden{}).DecodeFrom(in)
	var uk *ggen.UnknownKeyError
	if !errors.As(err, &uk) || uk.Keys[0] != "a" {
		t.Fatalf("bytes: err = %v, want UnknownKeyError for a", err)
	}
	s := ggen.NewStream(strings.NewReader(string(in)), nil)
	_, serr := (PingHidden{}).DecodeFromStream(s)
	var suk *ggen.UnknownKeyError
	if !errors.As(serr, &suk) || suk.Pos != uk.Pos || suk.Keys[0] != "a" {
		t.Fatalf("stream: err = %v, want %v", serr, err)
	}
	if n != uk.Pos {
		t.Errorf("consumed %d, want %d", n, uk.Pos)
	}

	for _, tc := range []struct {
		in   string
		want error
	}{{`{"a"}`, ggen.ErrBadObject}, {`{`, ggen.ErrExpectString}} {
		_, _, berr := (Ping{}).DecodeFrom([]byte(tc.in))
		_, serr := (Ping{}).DecodeFromStream(ggen.NewStream(strings.NewReader(tc.in), nil))
		if !errors.Is(berr, tc.want) || !errors.Is(serr, tc.want) {
			t.Errorf("%q: bytes %v, stream %v, want %v", tc.in, berr, serr, tc.want)
		}
	}
}

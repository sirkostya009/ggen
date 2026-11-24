// Package xpkg holds hosts of types generated in another package. It sorts
// after thirdparty2, so go generate reaches it once thirdparty2's methods
// exist.
package xpkg

//go:generate ../../ggen $GOFILE

import (
	"slices"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty2"
)

// PairHost holds a multierr struct generated in another package.
//
//ggen:generate multierr
type PairHost struct {
	P    thirdparty2.Pair   `json:"p"`
	PS   []thirdparty2.Pair `json:"ps"`
	Name string             `json:"name" pipe:"minlen=3"`
}

func TestMultierr_DrainsForeignMultierrCallee(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{`{"p":{"a":"x","b":"y"},"name":"n"}`, []string{"p.a", "p.b", "name"}},
		{`{"ps":[{"a":"x","b":"y"}],"name":"n"}`, []string{"ps.a", "ps.b", "name"}},
	} {
		_, n, err := PairHost{}.DecodeFrom([]byte(tc.in))
		if n != len(tc.in) {
			t.Errorf("%s: consumed %d of %d", tc.in, n, len(tc.in))
		}
		_, serr := ggen.NewStream(strings.NewReader(tc.in), nil).Value[PairHost]()
		for i, err := range []error{err, serr} {
			errs, ok := err.(ggen.Errors)
			if !ok {
				t.Errorf("%s path %d: want ggen.Errors, got %T %v", tc.in, i, err, err)
				continue
			}
			var got []string
			for _, e := range errs {
				got = append(got, strings.Join(e.Path(), "."))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("%s path %d: paths %v, want %v", tc.in, i, got, tc.want)
			}
		}
	}
}

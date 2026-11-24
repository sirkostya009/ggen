package model

import (
	"go/token"
	"go/types"
	"testing"
)

func TestIsErrorType(t *testing.T) {
	t.Parallel()
	pkg := types.NewPackage("example.com/p", "p")
	errT := types.Universe.Lookup("error").Type()
	alias := types.NewAlias(types.NewTypeName(token.NoPos, pkg, "E", nil), errT)
	ifaceT := errT.Underlying()
	named := types.NewNamed(types.NewTypeName(token.NoPos, pkg, "MyErr", nil), ifaceT, nil)
	cases := []struct {
		name string
		typ  types.Type
		want bool
	}{
		{"error", errT, true},
		{"alias_of_error", alias, true},
		{"alias_of_alias", types.NewAlias(types.NewTypeName(token.NoPos, pkg, "E2", nil), alias), true},
		{"named_error_interface", named, false},
		{"bool", types.Typ[types.Bool], false},
	}
	for _, c := range cases {
		if got := isErrorType(c.typ); got != c.want {
			t.Errorf("%s: isErrorType = %v, want %v", c.name, got, c.want)
		}
	}
}

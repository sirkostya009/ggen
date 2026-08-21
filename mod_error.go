package ggen

import "fmt"

// ModError is the failure of a fallible bool-form mod/converter
// (`func(W) (T, bool)`) whose bool was false. It is a parse error, wrapped by
// [NewParseErr]. Msg is the optional inline tag message (empty renders a
// default).
type ModError struct {
	Pos   int
	Name  string
	Msg   string
	Value any
}

// AddPos rebases the byte offset — the nested bytes-path decode rebase (see
// NewParseErrShift). ModError carries a Pos like the validation errors, so
// it needs the same hook or a nested fallible-mod failure keeps a
// sub-slice-relative position while every sibling error is payload-global.
func (e *ModError) AddPos(d int) { e.Pos += d }

func (e *ModError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("transform %q rejected %v", e.Name, e.Value)
}

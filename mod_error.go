package ggen

import "fmt"

// ModError reports that a fallible mod or converter of the form
// func(W) (T, bool) returned false. It is a parse error, wrapped by
// [NewParseErr]. Msg is the optional message from the tag. When Msg is empty,
// Error prints a default message.
type ModError struct {
	Pos   int
	Name  string
	Msg   string
	Value any
}

// AddPos shifts the byte offset by d. Nested decodes on the bytes path use it
// to turn an offset into a sub-slice into an offset into the whole payload
// (see NewParseErrShift). ModError carries a Pos like the validation errors,
// so it needs the same hook. Without it a nested ModError would keep a
// sub-slice offset while its sibling errors use whole-payload offsets.
func (e *ModError) AddPos(d int) { e.Pos += d }

func (e *ModError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("transform %q rejected %v", e.Name, e.Value)
}

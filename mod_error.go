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

func (e *ModError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("transform %q rejected %v", e.Name, e.Value)
}

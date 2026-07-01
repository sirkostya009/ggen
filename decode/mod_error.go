package decode

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

func (e *ModError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("transform %q rejected %v", e.Name, e.Value)
}

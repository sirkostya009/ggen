package ggen

import (
	"errors"
	"strconv"
	"strings"
	"unsafe"
)

// ParseError wraps a low-level parse failure with where it happened. The
// underlying sentinel, such as ErrBadString, stays reachable through
// errors.Is and errors.Unwrap.
//
// Path lists the keys and indexes from the document root to the failing
// value, one entry per nesting level. Pos is a byte offset. For DecodeFrom it
// is an offset into data. For DecodeFromStream it is the value of s.Pos.
// Validation failures are not wrapped. They carry their own Path and keep
// their type, so errors.As finds them.
type ParseError struct {
	Path []string
	Pos  int
	Err  error
}

// Error renders "parse error[ at <a.b.c>] (pos <n>)[: <cause>]".
func (e *ParseError) Error() string {
	field := strings.Join(e.Path, ".")
	buf := make([]byte, 0, 42+len(field))
	buf = append(buf, "parse error"...)
	if field != "" {
		buf = append(buf, " at "...)
		buf = append(buf, field...)
	}
	buf = append(buf, " (pos "...)
	buf = strconv.AppendInt(buf, int64(e.Pos), 10)
	buf = append(buf, ')')
	if e.Err != nil {
		buf = append(buf, ": "...)
		buf = append(buf, e.Err.Error()...)
	}
	return unsafe.String(unsafe.SliceData(buf), len(buf))
}

func (e *ParseError) Unwrap() error { return e.Err }

// AddPos shifts the error's byte offset by d. Nested decodes on the bytes
// path use it (see NewParseErrShift). If the wrapped cause carries its own
// position, such as a ModError from a nested mod, it is shifted too.
func (e *ParseError) AddPos(d int) {
	e.Pos += d
	if ap, ok := e.Err.(interface{ AddPos(d int) }); ok {
		ap.AddPos(d)
	}
}

// NewParseErrShift is NewParseErr for nested decodes on the bytes path. The
// nested decoder ran on data[pos-n:], where n is the number of bytes it
// consumed before it stopped. So an error it returned that carries a position
// (*ParseError, the typed validation errors, Errors) holds an offset into
// that sub-slice. NewParseErrShift adds the sub-slice start before wrapping,
// so every Pos is an offset into the whole payload. Sentinels and errors from
// other packages carry no position and pass through unchanged. Stream code
// uses NewParseErr, because stream positions already cover the whole input.
// The exception is a stream call to UnmarshalJSON, which runs on a captured
// copy of the value.
func NewParseErrShift(segment string, pos, n int, err error) error {
	if ap, ok := err.(interface{ AddPos(d int) }); ok {
		ap.AddPos(pos - n)
	}
	return NewParseErr(segment, pos, err)
}

// NewParseErr wraps an error returned by a generated decoder:
//
//   - An [Error] gets segment added to the front of its path and is returned
//     as is, so errors.As still finds it.
//   - A *ParseError gets segment added to the front of its Path. Its Pos
//     keeps pointing at the deeper failure.
//   - Any other error is wrapped as &ParseError{Path: {segment}, Pos, Err}.
//
// An empty segment leaves the path unchanged.
func NewParseErr(segment string, pos int, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(Error); ok {
		// Add the segment like the *ParseError case does, so a nested
		// validation error reports its full path.
		if p, ok := err.(interface{ PrependPath(segment string) }); ok && segment != "" {
			p.PrependPath(segment)
		}
		return err
	}
	if pe, ok := errors.AsType[*ParseError](err); ok {
		if segment != "" {
			pe.Path = prependSegment(pe.Path, segment)
		}
		return pe
	}
	var path []string
	if segment != "" {
		path = []string{segment}
	}
	return &ParseError{Path: path, Pos: pos, Err: err}
}

// prependSegment returns segment followed by p, in a new backing array.
// Sibling errors may share p, so it must not be modified in place.
func prependSegment(p []string, segment string) []string {
	out := make([]string, len(p)+1)
	out[0] = segment
	copy(out[1:], p)
	return out
}

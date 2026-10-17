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
// Keys lists the keys and indexes from the document root to the failing
// value, one entry per nesting level. Pos is a byte offset. For DecodeFrom it
// is an offset into data. For DecodeFromStream it is the value of s.Pos.
// Validation failures are not wrapped. They carry their own Keys and keep
// their type, so errors.As finds them.
type ParseError struct {
	Keys []string
	Pos  int
	Err  error
}

// Error renders "parse error[ at <a.b.c>] (pos <n>)[: <cause>]".
func (e *ParseError) Error() string {
	field := strings.Join(e.Keys, ".")
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

// Path returns Keys, like [ValidationError.Path].
func (e *ParseError) Path() []string { return e.Keys }

// Message returns the cause without the path and position.
func (e *ParseError) Message() string {
	if e.Err == nil {
		return "parse error"
	}
	return e.Err.Error()
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
	shiftPos(err, pos-n)
	return NewParseErr(segment, pos, err)
}

// shiftPos adds d to every position err carries. A *ParseError's cause is
// shifted too: a ModError from a nested mod arrives already wrapped in one.
func shiftPos(err error, d int) {
	switch e := err.(type) {
	case ValidationError:
		relocate(e, "", d)
	case Errors:
		for _, v := range e {
			relocate(v, "", d)
		}
	case *ParseError:
		e.Pos += d
		shiftPos(e.Err, d)
	case *ModError:
		e.Pos += d
	}
}

// NewParseErr wraps an error returned by a generated decoder:
//
//   - A [ValidationError] gets segment added to the front of its path and is returned
//     as is, so errors.As still finds it. [Errors] is cut down to its first
//     entry first, see below.
//   - A *ParseError gets segment added to the front of its Keys. Its Pos
//     keeps pointing at the deeper failure.
//   - Any other error is wrapped as &ParseError{Keys: {segment}, Pos, Err}.
//
// An empty segment leaves the path unchanged.
//
// A decoder that calls NewParseErr stops in the middle of its own value, so
// it hands up a single error, never [Errors]. A bare Errors then always means
// a multierr decoder read its whole value, which is what lets [Stream.Array]
// and [Stream.Seq] go on to the next one.
func NewParseErr(segment string, pos int, err error) error {
	if err == nil {
		return nil
	}
	if es, ok := err.(Errors); ok && len(es) > 0 {
		err = es[0]
	}
	if ve, ok := err.(ValidationError); ok {
		// Add the segment like the *ParseError case does, so a nested
		// validation error reports its full path.
		relocate(ve, segment, 0)
		return err
	}
	if pe, ok := errors.AsType[*ParseError](err); ok {
		if segment != "" {
			pe.Keys = prependSegment(pe.Keys, segment)
		}
		return pe
	}
	var path []string
	if segment != "" {
		path = []string{segment}
	}
	return &ParseError{Keys: path, Pos: pos, Err: err}
}

// prependSegment returns segment followed by p, in a new backing array.
// Sibling errors may share p, so it must not be modified in place.
func prependSegment(p []string, segment string) []string {
	out := make([]string, len(p)+1)
	out[0] = segment
	copy(out[1:], p)
	return out
}

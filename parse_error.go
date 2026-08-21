package ggen

import (
	"errors"
	"strconv"
	"strings"
	"unsafe"
)

// ParseError wraps a low-level parse failure with positional and structural
// context. The underlying sentinel (e.g. ErrBadString) stays reachable
// via errors.Is / errors.Unwrap.
//
// Path is the root-relative segment trail through the JSON document, one
// entry per nested level. Pos is the byte offset: into the data slice for
// DecodeFrom, the value of s.Pos for DecodeFromStream. Validation failures
// are NOT wrapped — they carry their own Path and stay typed for errors.As.
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

// AddPos rebases the error's byte offset by d (bytes-path nested-decode
// rebase — see NewParseErrShift).
func (e *ParseError) AddPos(d int) { e.Pos += d }

// NewParseErrShift is NewParseErr for the bytes path's nested-decode sites.
// The callee ran on data[pos-n:] (n = bytes it consumed before stopping), so
// a positional error it returned — *ParseError, validation typed errors,
// Errors — carries sub-slice-relative offsets; rebase by the
// value start before wrapping so every Pos is a full-payload offset, as
// documented. Sentinels and foreign errors carry no Pos and pass through.
// Stream call sites keep NewParseErr: stream positions are already global.
func NewParseErrShift(segment string, pos, n int, err error) error {
	if ap, ok := err.(interface{ AddPos(int) }); ok {
		ap.AddPos(pos - n)
	}
	return NewParseErr(segment, pos, err)
}

// NewParseErr wraps an error returned by a generated decoder:
//
//   - Error  → pass through (stays reachable via errors.As)
//   - *ParseError        → prepend segment onto its Path; Pos left at the
//     deeper site
//   - raw error          → wrap as &ParseError{Path: {segment}, Pos, Err}
//
// Empty segment leaves the Path untouched.
func NewParseErr(segment string, pos int, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(Error); ok {
		// Complete the path like the *ParseError arm does — pass-through
		// left fail-fast nested validation errors without their outer
		// segments. Typed pointers stay reachable via errors.As.
		if p, ok := err.(interface{ PrependPath(string) }); ok && segment != "" {
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

// prependSegment returns p with segment as the new head, on a fresh backing
// array so sibling errors sharing a chain don't trample each other.
func prependSegment(p []string, segment string) []string {
	out := make([]string, len(p)+1)
	out[0] = segment
	copy(out[1:], p)
	return out
}

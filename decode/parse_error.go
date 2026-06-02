package decode

import (
	"errors"
	"strconv"
	"strings"
	"unsafe"

	"github.com/sirkostya009/ggen/decode/validation"
)

// ParseError wraps a low-level parse failure with positional and structural
// context. The underlying sentinel (e.g. scan.ErrBadString) stays reachable
// via errors.Is / errors.Unwrap.
//
// Path is the segment trail through the JSON document — one entry per
// nested object/array level, root-relative (empty at the top level).
// For DecodeFrom, Pos is the byte offset within the data slice that method
// received. For DecodeFromStream, Pos is the value of s.Pos at failure time.
// Validation failures (decode/validation.*Error) are NOT wrapped — they
// already carry their own Path and remain typed for errors.As.
type ParseError struct {
	Path []string
	Pos  int
	Err  error
}

// Error renders "parse error[ at <a.b.c>] (pos <n>)[: <cause>]" into a
// single []byte sized off the fixed prefix only — the cause's Error() is
// called exactly once (when we actually append it), never measured ahead
// of time, so chained ParseError prints don't run Error() twice per
// level. Returned as an unsafe.String alias to skip the strings.Builder
// copy.
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

// NewParseErr is the call-site wrap invoked at every error-return site in
// generated DecodeFrom / DecodeFromStream. The codegen embeds the field
// name as a compile-time literal at each branch ("street", "addr", …) or
// as a runtime expression for dynamic keys (the raw key in the bytes
// path, strings.Clone(key) for the stream path). Behaviour:
//
//   - validation.Error  → pass through (typed pointer stays reachable via errors.As)
//   - *ParseError       → prepend segment onto its Path (chaining "addr"
//     with inner ["zip"] into ["addr","zip"]); Pos left at the deeper
//     site (the most-local diagnostic)
//   - raw error         → wrap as &ParseError{Path: []string{segment}, Pos, Err: err}
//
// Empty segment leaves the Path untouched (top-level boundary wraps).
func NewParseErr(segment string, pos int, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(validation.Error); ok {
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

// prependSegment returns p with segment as the new head, allocating a
// fresh backing array so sibling errors that share a chain don't trample
// each other.
func prependSegment(p []string, segment string) []string {
	out := make([]string, len(p)+1)
	out[0] = segment
	copy(out[1:], p)
	return out
}

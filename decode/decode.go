// Package decode provides a small set of runtime helpers that ggen-generated
// decoders delegate to — the Decoder interface every generated type satisfies,
// plus the array-walking helpers (UnmarshalSlice / ReadSlice /
// UnmarshalSliceStream) callers would otherwise have to reimplement.
//
// Single-value entry points are NOT provided here: call the generated method
// directly with a zero-value receiver, e.g.
//
//	res, _, err := T{}.DecodeFrom(data)
//	res, err := T{}.DecodeFromStream(s)
package decode

import (
	"io"
	"strconv"
	"unsafe"

	"github.com/sirkostya009/ggen/scan"
)

// Decoder is the interface satisfied by every ggen-generated struct.
// DecodeFrom reads one value out of the caller's byte slice and returns
// the decoded value, the number of bytes consumed, and any error.
// Callers chaining multiple values advance their own cursor:
//
//	v1, n, err := zero.DecodeFrom(data)
//	// data[n:] is what remains
//
// DecodeFromStream is the streaming counterpart that pulls bytes from a
// *scan.Stream; the Stream owns the cursor (s.Pos), so the method
// returns only (T, error).
//
// Strings inside the returned value alias the caller's bytes via unsafe.String
// — callers MUST NOT mutate the input buffer while the value is in use.
type Decoder[T any] interface {
	DecodeFrom(data []byte) (T, int, error)
	DecodeFromStream(s *scan.Stream) (T, error)
}

// UnmarshalSlice decodes a JSON array of T by walking the array with scan
// primitives and delegating each element to T.DecodeFrom. Every error
// return is routed through [NewParseErr]: raw scan sentinels get wrapped
// with the current cursor position, element-level errors that already
// arrive as *ParseError pass through unchanged.
func UnmarshalSlice[T Decoder[T]](data []byte) ([]T, error) {
	i := scan.SkipSpace(data, 0)
	if i >= len(data) || data[i] != '[' {
		return nil, NewParseErr("[]", i, scan.ErrBadArray)
	}
	i++
	i = scan.SkipSpace(data, i)
	var result []T
	if i < len(data) && data[i] == ']' {
		return result, nil
	}
	for {
		var zero T
		v, n, err := zero.DecodeFrom(data[i:])
		if err != nil {
			return nil, NewParseErr(arrField(len(result)), i, err)
		}
		result = append(result, v)
		i = scan.SkipSpace(data, i+n)
		if i >= len(data) {
			return nil, NewParseErr(arrField(len(result)-1), i, scan.ErrBadArray)
		}
		if data[i] == ',' {
			i = scan.SkipSpace(data, i+1)
			continue
		}
		if data[i] == ']' {
			return result, nil
		}
		return nil, NewParseErr(arrField(len(result)-1), i, scan.ErrBadArray)
	}
}

// ReadSlice reads an array from r then decodes it via UnmarshalSlice.
// io.ReadAll failures are surfaced as-is (transport errors, not parse
// errors); decode failures keep their UnmarshalSlice wrap.
func ReadSlice[T Decoder[T]](r io.Reader) ([]T, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return UnmarshalSlice[T](data)
}

// UnmarshalSliceStream decodes a JSON array of T lazily from r. buf is a
// reusable working area; pass nil to allocate fresh, or a pre-sized /
// pooled slice. The returned []byte is the (possibly grown) buffer —
// caller can recycle it immediately, the decoded values own their
// string content and have no dependency on the buffer. Every error
// return is routed through [NewParseErr] (see [UnmarshalSlice]).
func UnmarshalSliceStream[T Decoder[T]](r io.Reader, buf []byte) ([]T, []byte, error) {
	var s scan.Stream
	s.Reset(r, buf)
	if err := s.ArrayOpen(); err != nil {
		return nil, s.Bytes(), NewParseErr("[]", s.Pos, err)
	}
	if err := s.SkipSpace(); err != nil {
		return nil, s.Bytes(), NewParseErr("[]", s.Pos, err)
	}
	if s.Pos >= len(s.Bytes()) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, s.Bytes(), NewParseErr("[]", s.Pos, err)
		}
		s.Pos = 0
	}
	var result []T
	if s.Bytes()[s.Pos] == ']' {
		s.Pos++
		return result, s.Bytes(), nil
	}
	for {
		var zero T
		v, err := zero.DecodeFromStream(&s)
		if err != nil {
			return nil, s.Bytes(), NewParseErr(arrField(len(result)), s.Pos, err)
		}
		result = append(result, v)
		if err := s.SkipSpace(); err != nil {
			return nil, s.Bytes(), NewParseErr(arrField(len(result)-1), s.Pos, err)
		}
		if s.Pos >= len(s.Bytes()) {
			if err := s.ReadMore(s.Pos); err != nil {
				return nil, s.Bytes(), NewParseErr(arrField(len(result)-1), s.Pos, err)
			}
			s.Pos = 0
		}
		c := s.Bytes()[s.Pos]
		if c == ',' {
			s.Pos++
			continue
		}
		if c == ']' {
			s.Pos++
			return result, s.Bytes(), nil
		}
		return nil, s.Bytes(), NewParseErr(arrField(len(result)-1), s.Pos, scan.ErrBadArray)
	}
}

// arrField renders "[N]" — used as the Field component on slice-walker
// errors so a wrapped path like "[5].street" pinpoints the failing
// element. Called only on the error path; no fmt dependency. The
// returned string aliases the local buf; safe because buf is freshly
// allocated each call and isn't mutated afterwards.
func arrField(n int) string {
	buf := make([]byte, 0, 12)
	buf = append(buf, '[')
	buf = strconv.AppendInt(buf, int64(n), 10)
	buf = append(buf, ']')
	return unsafe.String(unsafe.SliceData(buf), len(buf))
}

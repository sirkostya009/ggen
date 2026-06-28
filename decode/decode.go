// Package decode provides runtime helpers ggen-generated decoders delegate to:
// the Decoder interface every generated type satisfies, plus array-walking
// helpers (UnmarshalSlice / ReadSlice / UnmarshalSliceStream).
//
// For single values, call the generated method directly on a zero-value
// receiver:
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
// DecodeFrom reads one value and returns it, the bytes consumed, and any
// error; callers chaining values advance their own cursor over data[n:].
// DecodeFromStream is the streaming counterpart — the Stream owns the cursor
// (s.Pos), so it returns only (T, error).
//
// Strings inside the returned value alias the caller's bytes — callers MUST
// NOT mutate the input buffer while the value is in use.
type Decoder[T any] interface {
	DecodeFrom(data []byte) (T, int, error)
	DecodeFromStream(s *scan.Stream) (T, error)
}

// UnmarshalSlice decodes a JSON array of T, delegating each element to
// T.DecodeFrom. Errors route through [NewParseErr].
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
// io.ReadAll failures are surfaced as-is (not wrapped as parse errors).
func ReadSlice[T Decoder[T]](r io.Reader) ([]T, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return UnmarshalSlice[T](data)
}

// UnmarshalSliceStream decodes a JSON array of T lazily from r. buf is a
// reusable working area (nil to allocate fresh). The returned []byte is the
// (possibly grown) buffer — safe to recycle immediately, as decoded values
// own their string content. Errors route through [NewParseErr].
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

// arrField renders "[N]" for the path segment on slice-walker errors
// (e.g. "[5].street").
func arrField(n int) string {
	buf := make([]byte, 0, 12)
	buf = append(buf, '[')
	buf = strconv.AppendInt(buf, int64(n), 10)
	buf = append(buf, ']')
	return unsafe.String(unsafe.SliceData(buf), len(buf))
}

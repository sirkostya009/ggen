// Package decode provides the small set of runtime helpers that ggen-generated
// decoders delegate to — pooled Stream acquisition, and top-level generic
// entry points (Unmarshal, UnmarshalSlice, Read, ReadSlice, and their
// streaming counterparts).
package decode

import (
	"fmt"
	"io"
	"net/http"

	"github.com/sirkostya009/ggen/scan"
)

// Decoder is the interface satisfied by every ggen-generated struct.
// DecodeFrom reads one value out of the caller's byte slice at position i
// and returns the decoded value, the position past the last consumed byte,
// and any error. Unmarshal is the top-level wrapper. DecodeStreamFrom is
// the streaming counterpart that pulls bytes from a *scan.Stream.
//
// Strings inside the returned value alias the caller's bytes via unsafe.String
// — callers MUST NOT mutate the input buffer while the value is in use.
type Decoder[T any] interface {
	DecodeFrom(data []byte, i int) (T, int, error)
	DecodeStreamFrom(s *scan.Stream, i int) (T, int, error)
}

// Unmarshal is the top-level entry: decode one T out of data using the
// hand-rolled scan path. Strings inside the returned value alias data —
// caller MUST NOT mutate the buffer while the value is in use.
func Unmarshal[T Decoder[T]](data []byte) (T, error) {
	var zero T
	v, _, err := zero.DecodeFrom(data, 0)
	return v, err
}

// Read slurps r into memory then decodes. Convenience for callers that don't
// want to manage the intermediate buffer themselves. Use UnmarshalStream when
// you need lazy I/O (e.g. HTTP bodies on slow networks).
func Read[T Decoder[T]](r io.Reader) (T, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		var zero T
		return zero, err
	}
	return Unmarshal[T](data)
}

// UnmarshalSlice decodes a JSON array of T by walking the array with scan
// primitives and delegating each element to T.DecodeFrom.
func UnmarshalSlice[T Decoder[T]](data []byte) ([]T, error) {
	i := scan.SkipSpace(data, 0)
	if i >= len(data) || data[i] != '[' {
		return nil, fmt.Errorf("expected '[': %w", scan.ErrBadArray)
	}
	i++
	i = scan.SkipSpace(data, i)
	var result []T
	if i < len(data) && data[i] == ']' {
		return result, nil
	}
	var zero T
	for {
		v, j, err := zero.DecodeFrom(data, i)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
		i = scan.SkipSpace(data, j)
		if i >= len(data) {
			return nil, scan.ErrBadArray
		}
		if data[i] == ',' {
			i = scan.SkipSpace(data, i+1)
			continue
		}
		if data[i] == ']' {
			return result, nil
		}
		return nil, scan.ErrBadArray
	}
}

// ReadSlice reads an array from r then decodes it via UnmarshalSlice.
func ReadSlice[T Decoder[T]](r io.Reader) ([]T, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return UnmarshalSlice[T](data)
}

// UnmarshalStream decodes a single T from r as bytes arrive, overlapping
// I/O with parsing. buf is a reusable working area; pass nil to allocate
// fresh, or a pre-sized / pooled slice (its capacity is used as initial
// buffer size). The returned []byte is the (possibly grown) buffer —
// caller can recycle it immediately, the decoded T owns its own
// string content and has no dependency on the buffer.
func UnmarshalStream[T Decoder[T]](r io.Reader, buf []byte) (T, []byte, error) {
	var s scan.Stream
	s.Reset(r, buf)
	var zero T
	v, _, err := zero.DecodeStreamFrom(&s, 0)
	return v, s.Bytes(), err
}

// UnmarshalStreamRequest decodes the body of an http.Request using the
// streaming path, pre-sizing the buffer from req.ContentLength when
// available.
func UnmarshalStreamRequest[T Decoder[T]](req *http.Request) (T, []byte, error) {
	hint := max(int(req.ContentLength), 0)
	return UnmarshalStream[T](req.Body, make([]byte, 0, hint))
}

// UnmarshalStreamResponse is the response-body counterpart.
func UnmarshalStreamResponse[T Decoder[T]](resp *http.Response) (T, []byte, error) {
	hint := max(int(resp.ContentLength), 0)
	return UnmarshalStream[T](resp.Body, make([]byte, 0, hint))
}

// UnmarshalSliceStream decodes a JSON array of T lazily from r. Same
// buf / aliasing contract as UnmarshalStream.
func UnmarshalSliceStream[T Decoder[T]](r io.Reader, buf []byte) ([]T, []byte, error) {
	var s scan.Stream
	s.Reset(r, buf)
	i, err := s.ArrayOpen(0)
	if err != nil {
		return nil, s.Bytes(), err
	}
	i, err = s.SkipSpace(i)
	if err != nil {
		return nil, s.Bytes(), err
	}
	if i >= len(s.Bytes()) {
		if err = s.ReadMore(i); err != nil {
			return nil, s.Bytes(), err
		}
		i = 0
	}
	var result []T
	if s.Bytes()[i] == ']' {
		return result, s.Bytes(), nil
	}
	var zero T
	for {
		v, j, err := zero.DecodeStreamFrom(&s, i)
		if err != nil {
			return nil, s.Bytes(), err
		}
		result = append(result, v)
		i, err = s.SkipSpace(j)
		if err != nil {
			return nil, s.Bytes(), err
		}
		if i >= len(s.Bytes()) {
			if err = s.ReadMore(i); err != nil {
				return nil, s.Bytes(), err
			}
			i = 0
		}
		c := s.Bytes()[i]
		if c == ',' {
			i++
			continue
		}
		if c == ']' {
			return result, s.Bytes(), nil
		}
		return nil, s.Bytes(), scan.ErrBadArray
	}
}

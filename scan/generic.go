package scan

import (
	"io"
	"iter"
	"unsafe"
)

// StreamDecoder is the interface a type satisfies to be read off a Stream.
// Every ggen-generated struct implements it; the bytes-path counterpart is
// decode.Decoder, which lives up in decode because it needs no Stream.
//
// It is declared here, not in decode, so [Stream.Value], [Stream.Slice] and
// [Stream.Seq] can constrain on it without scan importing decode.
type StreamDecoder[T any] interface {
	DecodeFromStream(s *Stream) (T, error)
}

// Value reads one T from the Stream, leaving the cursor just past the value so
// the next read continues where this one stopped:
//
//	s := scan.NewStream(r, buf)
//	u, err := s.Value[User]()
//
// An optional rcv is decoded INTO: generated decoders seed their result from
// the receiver and reset containers keeping capacity (`clear(m)`, `sl[:0]`), so
// handing back a previously decoded value reuses its maps and slices instead of
// allocating fresh ones. Only rcv[0] is read.
//
//	u, err = s.Value(u) // reuses u's containers
//
// The rcv's SCALAR fields are overwritten by the payload, but a field the
// payload omits keeps the old value — pass a zero T when that matters.
//
// The error is whatever the generated decoder returned — there is no enclosing
// array to name, so nothing is prepended.
func (s *Stream) Value[T StreamDecoder[T]](rcv ...T) (T, error) {
	var into T
	if len(rcv) > 0 {
		into = rcv[0]
	}
	return into.DecodeFromStream(s)
}

// Slice reads a JSON array of T, leaving the cursor just past the closing
// bracket. Trailing data is NOT rejected — a Stream may legitimately carry
// further values, and probing for them would block a live reader — so the
// caller decides what follows:
//
//	users, err := s.Slice[User]()
//	// s is still positioned; keep reading
//
// An optional rcv is reused as the destination buffer, and NOT merely appended
// into: the slice itself is truncated to keep its capacity, AND element i is
// decoded into rcv[0][i], so each element's own maps and slices are recycled
// too. That makes a steady-state re-decode allocation-free rather than just
// saving the outer backing array.
//
//	users, err = s.Slice(users) // reuses the slice AND every element's containers
//
// Elements past the new length keep their allocations but are not returned.
// A `[]` decodes to a non-nil empty slice, matching the bytes walker and
// jsonv2. Errors carry no element path (that wrapping lives in decode, which
// scan cannot import).
func (s *Stream) Slice[T StreamDecoder[T]](rcv ...[]T) ([]T, error) {
	var prev []T
	if len(rcv) > 0 {
		prev = rcv[0]
	}
	if err := s.ArrayOpen(); err != nil {
		return nil, err
	}
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, NotEOF(err, ErrBadArray)
		}
		s.Pos = 0
	}
	result := prev[:0]
	if cap(result) == 0 {
		result = make([]T, 0, PreallocCap(unsafe.Sizeof(*new(T))))
	}
	if s.buf[s.Pos] == ']' {
		s.Pos++
		return result, nil
	}
	for {
		// Decode into the element the buffer already holds at this index, so
		// its containers are recycled. Read BEFORE the append that overwrites
		// this slot — result and prev share a backing array.
		var into T
		if i := len(result); i < len(prev) {
			into = prev[i]
		}
		v, err := into.DecodeFromStream(s)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(s.Pos); err != nil {
				return nil, NotEOF(err, ErrBadArray)
			}
			s.Pos = 0
		}
		switch s.buf[s.Pos] {
		case ',':
			s.Pos++
			// Separator WS — scalar/alias element decoders don't skip leading
			// space themselves (mirrors the bytes walker's SkipSpace(i+1)).
			if err := s.SkipSpace(); err != nil {
				return nil, err
			}
		case ']':
			s.Pos++
			return result, nil
		default:
			return nil, ErrBadArray
		}
	}
}

// Array iterates a JSON array WITHOUT gathering it, yielding one element at a
// time. It is the lazy sibling of [Stream.Slice] — same grammar and the same
// element decoding, but nothing accumulates, so a million-element array costs
// one element of memory rather than a million:
//
//	for v, err := range scan.NewStream(r, buf).Array[Item]() {
//		if err != nil { break }
//		handle(v)
//	}
//
// The range ends when the closing bracket is consumed, leaving the cursor just
// past it so the Stream can be read from again. A malformed array yields one
// error and stops. Breaking out early leaves the cursor INSIDE the array —
// there is no way to know where the array ends without walking it — so a
// Stream abandoned mid-range is only good for closing.
//
// Like [Stream.Seq] it reuses one value for the whole iteration, seeded from an
// optional rcv, so a long array settles at zero allocations per element; and
// like Seq that means a yielded value is valid only until the next pull. Copy
// anything retained past the loop body (strings are owned, maps/slices alias).
func (s *Stream) Array[T StreamDecoder[T]](rcv ...T) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var into, zero T
		if len(rcv) > 0 {
			into = rcv[0]
		}
		if err := s.ArrayOpen(); err != nil {
			yield(zero, err)
			return
		}
		if err := s.SkipSpace(); err != nil {
			yield(zero, err)
			return
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(s.Pos); err != nil {
				yield(zero, NotEOF(err, ErrBadArray))
				return
			}
			s.Pos = 0
		}
		if s.buf[s.Pos] == ']' {
			s.Pos++
			return
		}
		for {
			// Drop yielded elements once the window is full, so the next refill
			// reuses that space. len == cap is exactly ReadMore's grow
			// condition, which keeps this off the path whenever the buffer has
			// spare tail: generated container decoders refill grow-only, so a
			// full window is the one state that doubles. The gate is what
			// bounds the cost — compacting every element would memmove the
			// unread remainder each time.
			if s.Pos > 0 && len(s.buf) == cap(s.buf) {
				s.consumed += s.Pos
				copy(s.buf, s.buf[s.Pos:])
				s.buf = s.buf[:len(s.buf)-s.Pos]
				s.Pos = 0
			}
			v, err := into.DecodeFromStream(s)
			if err != nil {
				yield(zero, err)
				return
			}
			if !yield(v, nil) {
				return
			}
			into = v
			if err := s.SkipSpace(); err != nil {
				yield(zero, err)
				return
			}
			if s.Pos >= len(s.buf) {
				if err := s.ReadMore(s.Pos); err != nil {
					yield(zero, NotEOF(err, ErrBadArray))
					return
				}
				s.Pos = 0
			}
			switch s.buf[s.Pos] {
			case ',':
				s.Pos++
				// Separator WS — scalar/alias element decoders don't skip
				// leading space themselves.
				if err := s.SkipSpace(); err != nil {
					yield(zero, err)
					return
				}
			case ']':
				s.Pos++
				return
			default:
				yield(zero, ErrBadArray)
				return
			}
		}
	}
}

// Seq yields consecutive top-level values off the Stream — concatenated JSON or
// NDJSON — and keeps going for as long as the reader keeps producing:
//
//	for v, err := range scan.NewStream(conn, buf).Seq[Event]() {
//		if err != nil { break }
//		handle(v)
//	}
//
// The iteration ends cleanly (no error yielded) when the reader drains at a
// value boundary, so a file or a closed socket terminates the range normally.
// Anything else — a malformed value, a real reader error — is yielded once as
// the error half and then ends the sequence. Breaking out of the range stops
// reading; the Stream is left wherever the last value finished, so it can be
// handed to another method afterwards.
//
// It never reads past a completed value before delivering it, so a quiet
// socket cannot stall an element that has already arrived: the refill for the
// NEXT value happens on the next pull, not eagerly after yielding this one.
//
// Seq REUSES ONE VALUE for the whole iteration: each decode goes into the
// previous one, recycling its maps and slices, so a long stream settles at zero
// allocations per element. An optional rcv seeds that value with containers the
// caller already has; without it the sequence starts from a zero T declared
// before the loop and warms up on the first element.
//
// The window is kept bounded: consumed values are dropped (gated — see the
// loop body) so generated container decoders, whose refills are grow-only,
// cannot ratchet the buffer upward across a long stream.
//
// CONSEQUENCE: a yielded value is only valid until the next pull — the
// following iteration decodes over its containers. Copy anything you retain
// past the loop body (strings are owned, so only maps/slices alias). Ranging
// twice over the same Seq is fine; each run starts its own value.
func (s *Stream) Seq[T StreamDecoder[T]](rcv ...T) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var into T
		if len(rcv) > 0 {
			into = rcv[0]
		}
		for {
			if err := s.SkipSpace(); err != nil {
				var zero T
				yield(zero, err)
				return
			}
			if s.Pos >= len(s.buf) {
				// At a value boundary with nothing buffered: pull once. A
				// drained reader is the clean end of the sequence.
				if err := s.ReadMore(s.Pos); err != nil {
					if err == io.ErrUnexpectedEOF {
						return
					}
					var zero T
					yield(zero, err)
					return
				}
				s.Pos = 0
				// The fresh chunk may lead with whitespace (NDJSON newline).
				continue
			}
			// Drop consumed values once the window is full.
			if s.Pos > 0 && len(s.buf) == cap(s.buf) {
				s.consumed += s.Pos
				copy(s.buf, s.buf[s.Pos:])
				s.buf = s.buf[:len(s.buf)-s.Pos]
				s.Pos = 0
			}
			v, err := into.DecodeFromStream(s)
			if !yield(v, err) || err != nil {
				return
			}
			// Carry this element's containers into the next decode.
			into = v
		}
	}
}

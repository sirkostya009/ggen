package ggen

import (
	"io"
	"math"
	"strconv"
	"time"
)

// decimalSeconds scans -?(0|[1-9][0-9]*)(\.[0-9]+)? at data[i:], the
// `format:unix` and `format:sec` number form. It returns the magnitude as
// whole seconds and nanoseconds. Fraction digits past the ninth are checked
// and dropped, and an exponent is ErrBadNumber, as in jsonv2. A whole part
// above posLimit (negLimit for a negative number) is ErrNumberOverflow at the
// digit that crosses it.
func decimalSeconds(data []byte, i int, posLimit, negLimit uint64) (whole uint64, frac int64, neg bool, end int, err error) {
	n := len(data)
	limit := posLimit
	if i < n && data[i] == '-' {
		neg = true
		limit = negLimit
		i++
	}
	if i >= n || data[i] < '0' || data[i] > '9' {
		return 0, 0, neg, i, ErrBadNumber
	}
	if data[i] == '0' {
		i++
	} else {
		for i < n && data[i] >= '0' && data[i] <= '9' {
			d := uint64(data[i] - '0')
			if whole > (limit-d)/10 {
				return 0, 0, neg, i, ErrNumberOverflow
			}
			whole = whole*10 + d
			i++
		}
	}
	if i < n && data[i] == '.' {
		i++
		if i >= n || data[i] < '0' || data[i] > '9' {
			return 0, 0, neg, i, ErrBadNumber
		}
		scale := int64(1e9)
		for i < n && data[i] >= '0' && data[i] <= '9' {
			if scale > 1 {
				scale /= 10
				frac += int64(data[i]-'0') * scale
			}
			i++
		}
	}
	if i < n && (data[i] == 'e' || data[i] == 'E') {
		return 0, 0, neg, i, ErrBadNumber
	}
	return whole, frac, neg, i, nil
}

// unixFromDecimal applies the sign to a decimalSeconds result. The whole
// part is at most 1<<63 when negative, so only -(1<<63) with a fraction falls
// below the int64 seconds range.
func unixFromDecimal(whole uint64, frac int64, neg bool) (time.Time, bool) {
	switch {
	case whole <= math.MaxInt64 && !neg:
		return time.Unix(int64(whole), frac), true
	case whole <= math.MaxInt64:
		return time.Unix(-int64(whole), -frac), true
	case neg && frac == 0:
		return time.Unix(math.MinInt64, 0), true
	}
	return time.Time{}, false
}

// durationFromDecimal applies the sign to a decimalSeconds result and checks
// that it fits a time.Duration. The whole part is at most MaxInt64/1e9.
func durationFromDecimal(whole uint64, frac int64, neg bool) (time.Duration, bool) {
	if whole > math.MaxInt64/uint64(time.Second) {
		return 0, false
	}
	sec := int64(whole)
	if !neg {
		return time.Duration(sec*1e9 + frac), frac <= math.MaxInt64-sec*1e9
	}
	return time.Duration(-sec*1e9 - frac), frac-1 <= math.MaxInt64-sec*1e9
}

// UnixSeconds scans a `format:unix` time: decimal Unix seconds with up to
// nine significant fraction digits. It reads the text exactly, so it returns
// the instant [AppendUnixSeconds] wrote.
func UnixSeconds(data []byte, i int) (time.Time, int, error) {
	whole, frac, neg, i, err := decimalSeconds(data, i, math.MaxInt64, SignedNeg)
	if err != nil {
		return time.Time{}, i, err
	}
	t, ok := unixFromDecimal(whole, frac, neg)
	if !ok {
		return time.Time{}, i, ErrNumberOverflow
	}
	return t, i, nil
}

// DurationSeconds scans a `format:sec` duration: decimal seconds with up to
// nine significant fraction digits. A value outside the time.Duration range
// is ErrNumberOverflow.
func DurationSeconds(data []byte, i int) (time.Duration, int, error) {
	const lim = math.MaxInt64 / uint64(time.Second)
	whole, frac, neg, i, err := decimalSeconds(data, i, lim, lim)
	if err != nil {
		return 0, i, err
	}
	d, ok := durationFromDecimal(whole, frac, neg)
	if !ok {
		return 0, i, ErrNumberOverflow
	}
	return d, i, nil
}

// AppendDurationSeconds appends d as exact decimal seconds, the
// `format:sec` form. Trailing zeros of the fraction are dropped.
func AppendDurationSeconds(dst []byte, d time.Duration) []byte {
	sec, ns := int64(d/time.Second), int64(d%time.Second)
	if d < 0 {
		dst = append(dst, '-')
		sec, ns = -sec, -ns
	}
	dst = strconv.AppendInt(dst, sec, 10)
	if ns == 0 {
		return dst
	}
	return appendFrac9(append(dst, '.'), ns)
}

// appendFrac9 appends ns in [1,1e9) as nine fraction digits without the
// trailing zeros.
func appendFrac9(dst []byte, ns int64) []byte {
	var frac [9]byte
	for i := 8; i >= 0; i-- {
		frac[i] = byte('0' + ns%10)
		ns /= 10
	}
	n := 9
	for n > 1 && frac[n-1] == '0' {
		n--
	}
	return append(dst, frac[:n]...)
}

// decimalSpan reads the number at Pos into the window, the way
// [Stream.Number] does, and returns its bounds. The caller scans it with the
// bytes scanner, so both paths accept the same text and stop at the same
// byte.
func (s *Stream) decimalSpan() (start, end int, err error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			s.Pos = 0
			return 0, 0, NotEOF(err, ErrBadNumber)
		}
		i = 0
	}
	start = i
	buf := s.buf
	for {
		for i < len(buf) {
			c := buf[i]
			if c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
				i++
				continue
			}
			return start, i, nil
		}
		err := s.ReadMore(start)
		i -= start
		start = 0
		if err != nil {
			if err != io.ErrUnexpectedEOF {
				s.Pos = start
				return 0, 0, err
			}
			return start, i, nil
		}
		buf = s.buf
	}
}

// UnixSeconds is the stream version of [UnixSeconds].
func (s *Stream) UnixSeconds() (time.Time, error) {
	start, end, err := s.decimalSpan()
	if err != nil {
		return time.Time{}, err
	}
	whole, frac, neg, j, err := decimalSeconds(s.buf[:end], start, math.MaxInt64, SignedNeg)
	s.Pos = j
	if err != nil {
		return time.Time{}, err
	}
	t, ok := unixFromDecimal(whole, frac, neg)
	if !ok {
		return time.Time{}, ErrNumberOverflow
	}
	return t, nil
}

// DurationSeconds is the stream version of [DurationSeconds].
func (s *Stream) DurationSeconds() (time.Duration, error) {
	start, end, err := s.decimalSpan()
	if err != nil {
		return 0, err
	}
	const lim = math.MaxInt64 / uint64(time.Second)
	whole, frac, neg, j, err := decimalSeconds(s.buf[:end], start, lim, lim)
	s.Pos = j
	if err != nil {
		return 0, err
	}
	d, ok := durationFromDecimal(whole, frac, neg)
	if !ok {
		return 0, ErrNumberOverflow
	}
	return d, nil
}

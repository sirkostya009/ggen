package ggen

import (
	"bytes"
	"errors"
	"math"
	"testing"
	"testing/iotest"
	"time"
)

// streamOneByte returns a Stream over in that reads one byte at a time.
func streamOneByte(in string) *Stream {
	s := &Stream{}
	s.Reset(iotest.OneByteReader(bytes.NewReader([]byte(in))), make([]byte, 0, 1))
	return s
}

// UnixSeconds reads the exact decimal AppendUnixSeconds writes, on both
// paths, and rejects what does not fit int64 seconds.
func TestUnixSeconds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		sec, nsec int64
		end       int
		err       error
	}{
		{in: "1700000000.123", sec: 1700000000, nsec: 123000000, end: 14},
		{in: "1700000000.000000001", sec: 1700000000, nsec: 1, end: 20},
		{in: "1700000000.999999999", sec: 1700000000, nsec: 999999999, end: 20},
		{in: "1700000000.9999999999}", sec: 1700000000, nsec: 999999999, end: 21},
		{in: "-0.5", sec: -1, nsec: 500000000, end: 4},
		{in: "-1.75,", sec: -2, nsec: 250000000, end: 5},
		{in: "0", end: 1},
		{in: "9223372036854775807", sec: math.MaxInt64, end: 19},
		{in: "-9223372036854775808", sec: math.MinInt64, end: 20},
		{in: "9223372036854775808", end: 18, err: ErrNumberOverflow},
		{in: "-9223372036854775808.5", end: 22, err: ErrNumberOverflow},
		{in: "1e30", end: 1, err: ErrBadNumber},
		{in: "1.5E3", end: 3, err: ErrBadNumber},
		{in: "1.", end: 2, err: ErrBadNumber},
		{in: "-", end: 1, err: ErrBadNumber},
		{in: "x", end: 0, err: ErrBadNumber},
	}
	for _, c := range cases {
		got, end, err := UnixSeconds([]byte(c.in), 0)
		s := streamOneByte(c.in)
		sgot, serr := s.UnixSeconds()
		if !errors.Is(err, c.err) || !errors.Is(serr, c.err) {
			t.Errorf("%s: err bytes=%v stream=%v, want %v", c.in, err, serr, c.err)
			continue
		}
		if end != c.end || s.Offset() != c.end {
			t.Errorf("%s: end bytes=%d stream=%d, want %d", c.in, end, s.Offset(), c.end)
		}
		if c.err != nil {
			continue
		}
		want := time.Unix(c.sec, c.nsec)
		if !got.Equal(want) || !sgot.Equal(want) {
			t.Errorf("%s: bytes=%v stream=%v, want %v", c.in, got, sgot, want)
		}
	}
}

// DurationSeconds and AppendDurationSeconds round-trip every duration
// exactly, and a value past the time.Duration range is an error.
func TestDurationSeconds(t *testing.T) {
	t.Parallel()
	for _, d := range []time.Duration{
		0, 1, -1, 1001 * time.Millisecond, 15, 65 * time.Microsecond,
		264199216134, -1500 * time.Millisecond, math.MaxInt64, math.MinInt64,
	} {
		in := string(AppendDurationSeconds(nil, d))
		got, end, err := DurationSeconds([]byte(in), 0)
		sgot, serr := streamOneByte(in).DurationSeconds()
		if err != nil || serr != nil || end != len(in) || got != d || sgot != d {
			t.Errorf("%d wrote %s, read bytes=%d (%v) stream=%d (%v)", d, in, got, err, sgot, serr)
		}
	}
	for in, want := range map[string]string{
		"1.001": "1.001", "1.5e-8": "", "0.000000015": "0.000000015", "-0.5": "-0.5",
	} {
		got, _, err := DurationSeconds([]byte(in), 0)
		if want == "" {
			if !errors.Is(err, ErrBadNumber) {
				t.Errorf("%s: err=%v, want ErrBadNumber", in, err)
			}
			continue
		}
		if err != nil || string(AppendDurationSeconds(nil, got)) != want {
			t.Errorf("%s: got %d (%v)", in, got, err)
		}
	}
	for _, in := range []string{"9223372037", "9223372036.854775808", "-9223372036.854775809", "92233720368"} {
		_, _, err := DurationSeconds([]byte(in), 0)
		_, serr := streamOneByte(in).DurationSeconds()
		if !errors.Is(err, ErrNumberOverflow) || !errors.Is(serr, ErrNumberOverflow) {
			t.Errorf("%s: bytes=%v stream=%v, want ErrNumberOverflow", in, err, serr)
		}
	}
}

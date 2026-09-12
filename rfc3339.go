package ggen

import (
	"errors"
	"time"
)

// AppendRFC3339 appends t in layout (time.RFC3339 or time.RFC3339Nano) and
// refuses the timestamps that grammar cannot spell — a year outside
// [0,9999] or a zone hour of 24 or more — the way time.Time.AppendText and
// jsonv2 do. Writing them produced strings no RFC 3339 parser reads back.
func AppendRFC3339(dst []byte, t time.Time, layout string) ([]byte, error) {
	n0 := len(dst)
	dst = t.AppendFormat(dst, layout)
	b := dst[n0:]
	switch {
	case b[len("9999")] != '-':
		return dst, errors.New("year outside of range [0,9999]")
	case b[len(b)-1] != 'Z':
		c := b[len(b)-len("Z07:00")]
		if c >= '0' && c <= '9' || dec2(b[len(b)-len("07:00"):]) >= 24 {
			return dst, errors.New("timezone hour outside of range [0,23]")
		}
	}
	return dst, nil
}

// ParseRFC3339 parses s as RFC 3339 (fractional seconds optional) with the
// checks time.Parse skips and jsonv2 applies: a two-digit hour, `.` as the
// fraction separator, a zone hour below 24 and a zone minute below 60.
func ParseRFC3339(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return t, err
	}
	// A parsed value is at least "2006-01-02T1:04:05Z" long; the hour check
	// runs first because only a two-digit hour puts index 19 in range.
	switch {
	case s[len("2006-01-02T")+1] == ':':
		return time.Time{}, &time.ParseError{Layout: time.RFC3339, Value: s, LayoutElem: "15", ValueElem: s[len("2006-01-02T"):][:1]}
	case s[len("2006-01-02T15:04:05")] == ',':
		return time.Time{}, &time.ParseError{Layout: time.RFC3339, Value: s, LayoutElem: ".", ValueElem: ","}
	case s[len(s)-1] != 'Z':
		zone := s[len(s)-len("Z07:00"):]
		switch {
		case dec2(s[len(s)-len("07:00"):]) >= 24:
			return time.Time{}, &time.ParseError{Layout: time.RFC3339, Value: s, LayoutElem: "Z07:00", ValueElem: zone, Message: ": timezone hour out of range"}
		case dec2(s[len(s)-len("00"):]) >= 60:
			return time.Time{}, &time.ParseError{Layout: time.RFC3339, Value: s, LayoutElem: "Z07:00", ValueElem: zone, Message: ": timezone minute out of range"}
		}
	}
	return t, nil
}

func dec2[T string | []byte](b T) byte { return 10*(b[0]-'0') + (b[1] - '0') }

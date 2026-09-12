package ggen

import (
	jsonv2 "encoding/json/v2"
	"strconv"
	"testing"
	"time"
)

// RFC 3339 cannot spell these; time.Time.AppendText and jsonv2 refuse them.
var unrepresentableTimes = map[string]time.Time{
	"year_10000": time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
	"year_-1":    time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC),
	"zone_+24h":  time.Date(2020, 1, 1, 0, 0, 0, 0, time.FixedZone("W", 24*3600)),
	"zone_-100h": time.Date(2020, 1, 1, 0, 0, 0, 0, time.FixedZone("W", -100*3600)),
}

func TestAppendRFC3339_RejectsUnrepresentable(t *testing.T) {
	t.Parallel()
	for name, tm := range unrepresentableTimes {
		for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
			if out, err := AppendRFC3339(nil, tm, layout); err == nil {
				t.Errorf("%s/%s: accepted, wrote %q", name, layout, out)
			}
		}
		if _, err := tm.AppendText(nil); err == nil {
			t.Errorf("%s: time.Time.AppendText accepts it; the guard is stricter than the stdlib", name)
		}
	}
}

func TestAppendRFC3339_MatchesAppendFormat(t *testing.T) {
	t.Parallel()
	for _, tm := range []time.Time{
		time.Date(2020, 1, 2, 3, 4, 5, 600000000, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.FixedZone("", 23*3600+59*60)),
		time.Date(0, 1, 1, 0, 0, 0, 0, time.FixedZone("", -23*3600)),
	} {
		for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
			got, err := AppendRFC3339([]byte("x"), tm, layout)
			if err != nil {
				t.Fatalf("%v/%s: %v", tm, layout, err)
			}
			if want := tm.AppendFormat([]byte("x"), layout); string(got) != string(want) {
				t.Errorf("%v/%s: got %q want %q", tm, layout, got, want)
			}
		}
	}
}

// ParseRFC3339 accepts and rejects exactly what jsonv2 does for a time.Time.
func TestParseRFC3339_JSONv2Parity(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"2020-01-01T00:00:00Z",
		"2020-01-01T00:00:00.123456789Z",
		"2020-01-01T00:00:00+01:30",
		"2020-01-01T00:00:00.5-23:59",
		"0000-01-01T00:00:00Z",
		"9999-12-31T23:59:59Z",
		"2020-01-01T00:00:00+24:00",
		"2020-01-01T00:00:00+23:60",
		"2020-01-01T00:00:00,123Z",
		"2020-01-01T1:04:05Z",
		"2020-01-01T1:04:05+01:00",
		"2020-01-01 00:00:00Z",
		"2020-01-01T00:00:00z",
		"10000-01-01T00:00:00Z",
		"2020-01-01T00:00:00",
		"",
	} {
		got, err := ParseRFC3339(s)
		var std time.Time
		sErr := jsonv2.Unmarshal([]byte(strconv.Quote(s)), &std)
		if (err != nil) != (sErr != nil) {
			t.Errorf("%q: ggen err=%v, jsonv2 err=%v", s, err, sErr)
			continue
		}
		if err == nil && !got.Equal(std) {
			t.Errorf("%q: ggen %v, jsonv2 %v", s, got, std)
		}
	}
}

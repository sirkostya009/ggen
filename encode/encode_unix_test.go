package encode

import (
	"testing"
	"time"
)

// AppendUnixSeconds must be exact where float64(UnixNano())/1e9 was not:
// outside the int64-nano range (~1678-2262) and at sub-100ns precision.
func TestAppendUnixSeconds(t *testing.T) {
	cases := []struct {
		t    time.Time
		want string
	}{
		{time.Unix(0, 0), "0"},
		{time.Unix(978307200, 0), "978307200"},
		{time.Unix(978307200, 1), "978307200.000000001"},
		{time.Unix(978307200, 500000000), "978307200.5"},
		{time.Unix(-1, 500000000), "-0.5"},
		{time.Unix(-2, 250000000), "-1.75"},
		{time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), "-30610224000"},
		{time.Date(3000, 1, 1, 0, 0, 0, 123, time.UTC), "32503680000.000000123"},
	}
	for _, c := range cases {
		if got := string(AppendUnixSeconds(nil, c.t)); got != c.want {
			t.Errorf("%v: got %s, want %s", c.t, got, c.want)
		}
	}
}

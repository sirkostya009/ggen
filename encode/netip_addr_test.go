package encode

import (
	"encoding/json"
	"net/netip"
	"testing"
)

// AppendNetipAddr writes body + closing quote (caller opens). Zone bytes are
// arbitrary — ParseAddr accepts `%q"z` — and must escape or the output is
// invalid JSON; zone-free addrs stay on the raw path. Byte-parity against
// stdlib json.Marshal, which escapes the same class.
func TestAppendNetipAddr(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"10.0.0.1",
		"2001:db8::1",
		"fe80::1%eth0",
		`fe80::1%q"z`,
		`fe80::1%a\b`,
		"fe80::1%z\x01n", // ctrl byte in zone → \u0001
	} {
		a, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", raw, err)
		}
		got := string(AppendNetipAddr([]byte{'"'}, a))
		want, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("json.Marshal(%q): %v", raw, err)
		}
		if got != string(want) {
			t.Errorf("AppendNetipAddr(%q) = %s, want %s", raw, got, want)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("invalid JSON: %s", got)
		}
	}
}

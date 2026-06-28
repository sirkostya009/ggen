package scan

import (
	"encoding/json"
	"testing"
)

// numberAccepted reports whether SkipValue consumes tok in full as a number.
func numberAccepted(tok string) bool {
	pos, err := SkipValue([]byte(tok), 0)
	return err == nil && pos == len(tok)
}

func TestSkipNumber_AcceptSetMatchesJSONGrammar(t *testing.T) {
	t.Parallel()
	cases := []string{
		// valid
		"0", "-0", "123", "-123", "1.5", "-1.5", "0.5", "1e5", "1E5",
		"1e+5", "1e-5", "1.5e10", "-1.5E-10", "1e400", "1e999", "1e1000000",
		"12345678901234567890", "0.0000001", "-0.0",
		// invalid — leading + / leading zero / bare or trailing dot
		"+1", "01", "007", "00.5", "1.", "-.5", ".5", "-", "+",
		// invalid — malformed exp / double dot / trailing junk
		"1e", "1e+", "1e-", "1..2", "1.2.3", "--", "e5", "1.e5",
		"1ee5", "0x1", "1.2e", "1.2e+",
		"", "0123", "00",
	}
	for _, tok := range cases {
		got := numberAccepted(tok)
		// Oracle: a leading-number-byte token is a valid JSON number iff
		// json.Valid accepts it. Tokens not starting with -/digit are not
		// routed to skipNumber at all, so restrict the comparison.
		want := json.Valid([]byte(tok))
		if tok == "" || !(tok[0] == '-' || (tok[0] >= '0' && tok[0] <= '9')) {
			// not a number token — skipNumber is never reached; skip oracle
			if got {
				t.Errorf("%q: skipNumber accepted a non-number-leading token", tok)
			}
			continue
		}
		if got != want {
			t.Errorf("%q: skipNumber accepted=%v, json.Valid=%v", tok, got, want)
		}
	}
}

// Stream skip must have the byte-identical accept-set of the bytes path, incl.
// across chunk boundaries (one-byte reader exercises every refill seam).
func TestSkipNumber_StreamMatchesBytes(t *testing.T) {
	t.Parallel()
	cases := []string{
		"0", "-0", "123", "-123", "1.5", "1e5", "1e+5", "1.5e10", "1e400",
		"12345678901234567890", "+1", "01", "1.", ".5", "-", "1e", "1..2",
		"1.2.3", "00.5", "1.2e+",
	}
	for _, tok := range cases {
		bp, bErr := SkipValue([]byte(tok), 0)
		bytesOK := bErr == nil && bp == len(tok)

		// One-byte-per-Read reader forces a refill seam at every offset.
		var s Stream
		s.Reset(&chunkedReader{data: []byte(tok)}, make([]byte, 0, 4))
		sErr := s.SkipValue()
		streamOK := sErr == nil && s.Pos == len(tok)
		if streamOK != bytesOK {
			t.Errorf("%q: stream accepted=%v, bytes accepted=%v", tok, streamOK, bytesOK)
		}
	}
}

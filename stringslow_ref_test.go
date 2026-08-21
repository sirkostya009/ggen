package ggen

import (
	"math/rand"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// stringSlowRef is the pre-gate stringSlow: a byte-at-a-time copy with no bulk
// arm, kept as the differential oracle for the escRunWindow hybrid. Value,
// position, and error identity must match the shipped version exactly.
func stringSlowRef(data []byte, start, j, capHint int, validate bool) (string, int, error) {
	bad, rawHigh := ctrlOrHigh(data[start:j])
	if bad {
		return "", start, ErrBadString
	}
	buf := make([]byte, 0, capHint)
	buf = append(buf, data[start:j]...)
	var rawHi byte
	for j < len(data) {
		c := data[j]
		if c == '"' {
			if validate && (rawHigh || rawHi&0x80 != 0) && !utf8.Valid(buf) {
				return "", j, ErrInvalidUTF8
			}
			return string(buf), j + 1, nil
		}
		if c == '\\' {
			if j+1 >= len(data) {
				return "", len(data), ErrBadString
			}
			esc := data[j+1]
			switch esc {
			case '"', '\\', '/':
				buf = append(buf, esc)
				j += 2
			case 'b':
				buf = append(buf, '\b')
				j += 2
			case 'f':
				buf = append(buf, '\f')
				j += 2
			case 'n':
				buf = append(buf, '\n')
				j += 2
			case 'r':
				buf = append(buf, '\r')
				j += 2
			case 't':
				buf = append(buf, '\t')
				j += 2
			case 'u':
				if j+6 > len(data) {
					return "", len(data), ErrBadString
				}
				r, ok := parseHex4(data[j+2 : j+6])
				if !ok {
					return "", j, ErrBadString
				}
				j += 6
				if utf16.IsSurrogate(r) {
					if j+6 <= len(data) && data[j] == '\\' && data[j+1] == 'u' {
						if r2, ok := parseHex4(data[j+2 : j+6]); ok {
							if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
								r = dec
								j += 6
							}
						}
					}
					if validate && utf16.IsSurrogate(r) {
						return "", j, ErrInvalidUTF8
					}
				}
				buf = utf8.AppendRune(buf, r)
			default:
				return "", j, ErrBadString
			}
			continue
		}
		if c < 0x20 {
			return "", j, ErrBadString
		}
		rawHi |= c
		buf = append(buf, c)
		j++
	}
	return "", len(data), ErrUnterminated
}

// escapeAlphabet: fragments that stress every boundary the escRunWindow gate
// introduces — runs shorter and longer than the window, escapes straddling it,
// ctrl/high bytes on both sides of the switchover, truncation-prone \u forms.
var escapeAlphabet = []string{
	`a`, `bc`, `def`, `ghijklmn`, `opqrstuvwxyz0123`,
	`0123456789abcdef0123456789abcdef`, // 2× window
	`\"`, `\\`, `\/`, `\b`, `\f`, `\n`, `\r`, `\t`,
	`A`, `é`, `😀`, `\ud83d`, `\ude00`, `\u00`, `\uZZZZ`,
	"é", "日本語", "😀", "\xc3", "\xff\xfe", "\x80",
	"\x01", "\x1f", "\x7f", " ", "\t\\t",
}

// TestStringSlow_RefDifferential pins the escRunWindow bulk arm against the
// byte-at-a-time reference: same value, same end position, same error, for
// randomized escape/ctrl/high-byte bodies in both validate modes.
func TestStringSlow_RefDifferential(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(20260809))
	for range 200000 {
		body := make([]byte, 0, 64)
		for range rng.Intn(9) {
			body = append(body, escapeAlphabet[rng.Intn(len(escapeAlphabet))]...)
		}
		// Ensure at least one escape so the input actually reaches stringSlow.
		body = append(body, `\n`...)
		for range rng.Intn(6) {
			body = append(body, escapeAlphabet[rng.Intn(len(escapeAlphabet))]...)
		}
		if rng.Intn(4) != 0 { // 3/4 terminated, 1/4 truncated
			body = append(body, '"')
		}
		// Randomly truncate to exercise every partial-escape/partial-run edge.
		if rng.Intn(5) == 0 && len(body) > 0 {
			body = body[:rng.Intn(len(body))]
		}
		data := append([]byte(`"`), body...)

		for _, validate := range [2]bool{true, false} {
			// start=1 (past the opening quote); j = first backslash, as the
			// callers compute it. No backslash → nothing to compare.
			j := -1
			for k := 1; k < len(data); k++ {
				if data[k] == '\\' {
					j = k
					break
				}
				if data[k] == '"' {
					break
				}
			}
			if j < 0 {
				continue
			}
			capHint := stringSpanEnd(data, 1) - 1

			gotV, gotP, gotE := stringSlow(data, 1, j, capHint, validate)
			wantV, wantP, wantE := stringSlowRef(data, 1, j, capHint, validate)
			if gotE != wantE || gotP != wantP || gotV != wantV {
				t.Fatalf("mismatch validate=%v input=%q\n got: %q %d %v\nwant: %q %d %v",
					validate, data, gotV, gotP, gotE, wantV, wantP, wantE)
			}
		}
	}
}

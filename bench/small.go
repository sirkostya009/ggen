// Small bench family (small_test.go): the ~2.9 KiB Validated payload plus the
// Tiny, ValidationHeavy, RuneGated, and HTMLEscape fixtures that share its
// benchmarks file.

//go:generate ../ggen $GOFILE
package bench

import "strings"

// Validated exercises per-field validation rules for fail-fast streaming
// benchmarks — Email is corrupted to force early rejection.
//
//ggen:generate
type Validated struct {
	Email string   `json:"email" pipe:"required contains=@"`
	Name  string   `json:"name"  pipe:"required minlen=1 maxlen=64"`
	Age   int      `json:"age"   pipe:"gte=0 lte=150"`
	Tags  []string `json:"tags" pipe:"inner:(notempty minlen=1 maxlen=32)"`
	Bio   string   `json:"bio"   pipe:"maxlen=4096"`
}

// CopyValidated is Validated under -copy (`//ggen:generate copy`) — the
// ggen_copy row of BenchmarkSmall_Unmarshal decodes the same ValidPayload
// through the copy-mode bytes path (the 2800 B Bio makes the per-string
// copy tax maximally visible).
//
//ggen:generate copy
type CopyValidated struct {
	Email string   `json:"email" pipe:"required contains=@"`
	Name  string   `json:"name"  pipe:"required minlen=1 maxlen=64"`
	Age   int      `json:"age"   pipe:"gte=0 lte=150"`
	Tags  []string `json:"tags" pipe:"inner:(notempty minlen=1 maxlen=32)"`
	Bio   string   `json:"bio"   pipe:"maxlen=4096"`
}

// --- Tiny (JWT-claim sized ~150 B) payload --------------------------------

// Claim is the JWT-claim-sized struct used by the tiny-payload benchmarks,
// where per-call overhead is visible.
//
//ggen:generate
type Claim struct {
	Sub string `json:"sub" pipe:"required"`
	Iss string `json:"iss" pipe:"required"`
	Exp int64  `json:"exp" pipe:"gte=0"`
	Iat int64  `json:"iat" pipe:"gte=0"`
	Nbf int64  `json:"nbf,omitempty"`
	Aud string `json:"aud,omitempty"`
	Jti string `json:"jti"`
}

// CopyClaim is Claim under -copy (`//ggen:generate copy`) for the ggen_copy
// row of BenchmarkTiny_Unmarshal.
//
//ggen:generate copy
type CopyClaim struct {
	Sub string `json:"sub" pipe:"required"`
	Iss string `json:"iss" pipe:"required"`
	Exp int64  `json:"exp" pipe:"gte=0"`
	Iat int64  `json:"iat" pipe:"gte=0"`
	Nbf int64  `json:"nbf,omitempty"`
	Aud string `json:"aud,omitempty"`
	Jti string `json:"jti"`
}

// EasyClaim mirrors Claim with easyjson methods, kept on a separate type so
// the reflection codecs don't pick them up via json.Marshaler/Unmarshaler.
//
//easyjson:json
type EasyClaim struct {
	Sub string `json:"sub"`
	Iss string `json:"iss"`
	Exp int64  `json:"exp"`
	Iat int64  `json:"iat"`
	Nbf int64  `json:"nbf,omitempty"`
	Aud string `json:"aud,omitempty"`
	Jti string `json:"jti"`
}

// --- Validation-heavy payload ---------------------------------------------

// ValidationHeavy carries enough rules that the per-field check cost shows
// up against codecs that don't validate. Uses minrunes/maxrunes (full UTF-8
// walk) so the per-string scan cost is meaningful.
//
//ggen:generate
type ValidationHeavy struct {
	Email    string  `json:"email" pipe:"required contains=@ maxrunes=128"`
	Username string  `json:"username" pipe:"required minrunes=3 maxrunes=32 alphanum lower"`
	Phone    string  `json:"phone" pipe:"minrunes=7 maxrunes=20 numeric"`
	Age      int     `json:"age" pipe:"gte=0 lte=130"`
	Score    float64 `json:"score" pipe:"gte=0 lte=100"`
	Name     string  `json:"name" pipe:"required minrunes=1 maxrunes=64"`
	URL      string  `json:"url" pipe:"url"`
	Country  string  `json:"country" pipe:"runes=2 upper"`
	Lang     string  `json:"lang" pipe:"oneof=en|es|fr|de|uk"`
	Role     string  `json:"role" pipe:"oneof=admin|user|guest"`
}

// NoValidationHeavy mirrors ValidationHeavy's wire shape but skips validation
// (via novalidate), isolating pure decode cost from the per-rule check cost.
//
//ggen:generate novalidate
type NoValidationHeavy struct {
	Email    string  `json:"email"`
	Username string  `json:"username"`
	Phone    string  `json:"phone"`
	Age      int     `json:"age"`
	Score    float64 `json:"score"`
	Name     string  `json:"name"`
	URL      string  `json:"url"`
	Country  string  `json:"country"`
	Lang     string  `json:"lang"`
	Role     string  `json:"role"`
}

// EasyValidationHeavy mirrors ValidationHeavy with easyjson methods (same
// isolation rationale as EasyClaim).
//
//easyjson:json
type EasyValidationHeavy struct {
	Email    string  `json:"email"`
	Username string  `json:"username"`
	Phone    string  `json:"phone"`
	Age      int     `json:"age"`
	Score    float64 `json:"score"`
	Name     string  `json:"name"`
	URL      string  `json:"url"`
	Country  string  `json:"country"`
	Lang     string  `json:"lang"`
	Role     string  `json:"role"`
}

// RuneGated isolates rune-rule validation on long strings. LongRunes holds
// genuine 4-byte runes (len == 4×runecount); AsciiRunes is ASCII and preceded
// by alphanum.
//
//ggen:generate
type RuneGated struct {
	LongRunes  string `json:"longRunes" pipe:"minrunes=4 maxrunes=1000000"`
	AsciiRunes string `json:"asciiRunes" pipe:"alphanum maxrunes=1000000"`
}

// --- HTML-escape parity payload --------------------------------------------

// HTMLEscape exercises the htmlescape opt-in encoder path; pairs with
// HTMLPlain (default literal) for the parity benchmark.
//
//ggen:generate htmlescape
type HTMLEscape struct {
	Note string `json:"note"`
}

// HTMLPlain mirrors HTMLEscape without htmlescape — marshal output
// matches jsonv2.
//
//ggen:generate
type HTMLPlain struct {
	Note string `json:"note"`
}

// EasyHTMLPlain mirrors HTMLPlain with easyjson-generated methods.
// Same isolation rationale as EasyClaim.
//
//easyjson:json
type EasyHTMLPlain struct {
	Note string `json:"note"`
}

var (
	// ValidPayload + InvalidPayload — same-size bodies for fail-fast
	// streaming benchmarks; the invalid one fails on the first decoded
	// field (email).
	ValidPayload   []byte
	InvalidPayload []byte

	TinyValue     Claim
	EasyTinyValue EasyClaim
	TinyPayload   []byte

	ValidationHeavyPayload []byte

	RuneGatedPayload []byte

	HTMLEscapeValue    HTMLEscape
	HTMLPlainValue     HTMLPlain
	EasyHTMLPlainValue EasyHTMLPlain
)

func init() {
	// ~3 KiB body; Bio padded so a slow reader delivers it in chunks.
	bio := (&gen{n: 1 << 40}).str(2800)
	tags := []string{"alpha", "beta", "gamma", "delta"}
	ValidPayload = mustMarshal(Validated{
		Email: "alice@example.com",
		Name:  "alice",
		Age:   30,
		Tags:  tags,
		Bio:   bio,
	})
	// Same shape, but Email is malformed (no '@') — fails the contains rule
	// on the first decoded field (sorted order: age, bio, then email trips).
	InvalidPayload = mustMarshal(Validated{
		Email: "not-an-email",
		Name:  "alice",
		Age:   30,
		Tags:  tags,
		Bio:   bio,
	})

	// Tiny / Claim. Fixed timestamps — a live clock would change the payload
	// bytes every run.
	const issuedAt = 1718668800 // 2024-06-18T00:00:00Z
	TinyValue = Claim{
		Sub: "user-12345",
		Iss: "https://auth.example.com",
		Exp: issuedAt + 3600,
		Iat: issuedAt,
		Aud: "api",
		Jti: "abc123def456",
	}
	EasyTinyValue = EasyClaim{
		Sub: TinyValue.Sub, Iss: TinyValue.Iss, Exp: TinyValue.Exp,
		Iat: TinyValue.Iat, Aud: TinyValue.Aud, Jti: TinyValue.Jti,
	}
	TinyPayload = mustMarshal(TinyValue)

	// Validation-heavy.
	ValidationHeavyPayload = mustMarshal(ValidationHeavy{
		Email: "user@example.com", Username: "alice42", Phone: "1234567890",
		Age: 30, Score: 99.5, Name: "Alice",
		URL: "https://example.com", Country: "UA", Lang: "en", Role: "user",
	})

	// Rune-gated: long ~8 KB strings. LongRunes = 2048 four-byte runes (8192
	// bytes); AsciiRunes stays ASCII.
	RuneGatedPayload = mustMarshal(RuneGated{
		LongRunes:  strings.Repeat("🦊", 2048),
		AsciiRunes: strings.Repeat("abc123", 1366),
	})

	// HTML-escape parity.
	HTMLEscapeValue = HTMLEscape{Note: strings.Repeat("<a>&", 200)}
	HTMLPlainValue = HTMLPlain{Note: strings.Repeat("<a>&", 200)}
	EasyHTMLPlainValue = EasyHTMLPlain(HTMLPlainValue)
}

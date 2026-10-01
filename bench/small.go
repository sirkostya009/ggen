// Small benchmark family (small_test.go): the Validated payload of about
// 2.9 KiB, plus the Tiny, ValidationHeavy, RuneGated and HTMLEscape fixtures
// that share its benchmark file.

//go:generate ../ggen $GOFILE
package bench

import "strings"

// Validated has per-field validation rules for the streaming benchmarks that
// stop at the first error. In the invalid payload, Email is broken so the
// decode is rejected early.
//
//ggen:generate
type Validated struct {
	Email string   `json:"email" pipe:"required contains=@"`
	Name  string   `json:"name"  pipe:"required minlen=1 maxlen=64"`
	Age   int      `json:"age"   pipe:"gte=0 lte=150"`
	Tags  []string `json:"tags"  pipe:"inner:(notempty minlen=1 maxlen=32)"`
	Bio   string   `json:"bio"   pipe:"maxlen=4096"`
}

// CopyValidated is Validated under -copy (`//ggen:generate copy`). The
// ggen_copy row of BenchmarkSmall_Unmarshal decodes the same ValidPayload
// through the copy-mode bytes path. The 2800-byte Bio makes the cost of
// copying strings as visible as possible.
//
//ggen:generate copy
type CopyValidated struct {
	Email string   `json:"email" pipe:"required contains=@"`
	Name  string   `json:"name"  pipe:"required minlen=1 maxlen=64"`
	Age   int      `json:"age"   pipe:"gte=0 lte=150"`
	Tags  []string `json:"tags"  pipe:"inner:(notempty minlen=1 maxlen=32)"`
	Bio   string   `json:"bio"   pipe:"maxlen=4096"`
}

// --- Tiny payload, the size of a JWT claim set (about 150 bytes) ----------

// Claim is a struct the size of a JWT claim set, used by the tiny-payload
// benchmarks, where the fixed cost of each call shows.
//
//ggen:generate
type Claim struct {
	Sub string `json:"sub"           pipe:"required"`
	Iss string `json:"iss"           pipe:"required"`
	Exp int64  `json:"exp"           pipe:"gte=0"`
	Iat int64  `json:"iat"           pipe:"gte=0"`
	Nbf int64  `json:"nbf,omitempty"`
	Aud string `json:"aud,omitempty"`
	Jti string `json:"jti"`
}

// CopyClaim is Claim under -copy (`//ggen:generate copy`) for the ggen_copy
// row of BenchmarkTiny_Unmarshal.
//
//ggen:generate copy
type CopyClaim struct {
	Sub string `json:"sub"           pipe:"required"`
	Iss string `json:"iss"           pipe:"required"`
	Exp int64  `json:"exp"           pipe:"gte=0"`
	Iat int64  `json:"iat"           pipe:"gte=0"`
	Nbf int64  `json:"nbf,omitempty"`
	Aud string `json:"aud,omitempty"`
	Jti string `json:"jti"`
}

// EasyClaim is Claim with easyjson methods. It is a separate type so the
// reflection-based libraries do not call those methods through
// json.Marshaler and json.Unmarshaler.
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

// ValidationHeavy has enough rules that the cost of the per-field checks
// shows up against libraries that do not validate. It uses minrunes and
// maxrunes, which walk the whole string to count characters (runes), so the
// per-string scan cost is significant.
//
//ggen:generate
type ValidationHeavy struct {
	Email    string  `json:"email"    pipe:"required contains=@ maxrunes=128"`
	Username string  `json:"username" pipe:"required minrunes=3 maxrunes=32 alphanum tolower"`
	Phone    string  `json:"phone"    pipe:"minrunes=7 maxrunes=20 numeric"`
	Age      int     `json:"age"      pipe:"gte=0 lte=130"`
	Score    float64 `json:"score"    pipe:"gte=0 lte=100"`
	Name     string  `json:"name"     pipe:"required minrunes=1 maxrunes=64"`
	URL      string  `json:"url"      pipe:"url"`
	Country  string  `json:"country"  pipe:"runes=2 toupper"`
	Lang     string  `json:"lang"     pipe:"oneof=en|es|fr|de|uk"`
	Role     string  `json:"role"     pipe:"oneof=admin|user|guest"`
}

// NoValidationHeavy has ValidationHeavy's JSON form but skips validation (via
// novalidate). It measures the decode cost without the cost of the rules.
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

// EasyValidationHeavy is ValidationHeavy with easyjson methods. It is a
// separate type for the same reason as EasyClaim.
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

// RuneGated measures only the rune rules on long strings. A rune is one
// Unicode character, which takes 1 to 4 bytes in UTF-8. LongRunes holds
// 4-byte characters only, so its byte length is 4 times its rune count.
// AsciiRunes is ASCII, and its alphanum rule runs before maxrunes.
//
//ggen:generate
type RuneGated struct {
	LongRunes  string `json:"longRunes"  pipe:"minrunes=4 maxrunes=1000000"`
	AsciiRunes string `json:"asciiRunes" pipe:"alphanum maxrunes=1000000"`
}

// --- HTML-escape parity payload --------------------------------------------

// HTMLEscape exercises the encoder with the htmlescape option. It pairs with
// HTMLPlain, which writes <, > and & as is, for the parity benchmark.
//
//ggen:generate htmlescape
type HTMLEscape struct {
	Note string `json:"note"`
}

// HTMLPlain is HTMLEscape without htmlescape. Its marshal output matches
// jsonv2.
//
//ggen:generate
type HTMLPlain struct {
	Note string `json:"note"`
}

// EasyHTMLPlain is HTMLPlain with easyjson-generated methods. It is a
// separate type for the same reason as EasyClaim.
//
//easyjson:json
type EasyHTMLPlain struct {
	Note string `json:"note"`
}

var (
	// ValidPayload and InvalidPayload have bodies of the same size, for the
	// streaming benchmarks that stop at the first error. The invalid one
	// fails at the email field.
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
	// A body of about 3 KiB. Bio is padded so a slow reader delivers it in
	// several chunks.
	bio := (&gen{n: 1 << 40}).str(2800)
	tags := []string{"alpha", "beta", "gamma", "delta"}
	ValidPayload = mustMarshal(Validated{
		Email: "alice@example.com",
		Name:  "alice",
		Age:   30,
		Tags:  tags,
		Bio:   bio,
	})
	// Same shape, but Email is malformed (no '@'), so it fails the contains
	// rule. Fields are written in sorted order, so age and bio decode first
	// and then email fails.
	InvalidPayload = mustMarshal(Validated{
		Email: "not-an-email",
		Name:  "alice",
		Age:   30,
		Tags:  tags,
		Bio:   bio,
	})

	// Tiny payload (Claim). The timestamps are fixed, because a live clock
	// would change the payload bytes on every run.
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

	// RuneGated: long strings of about 8 KB. LongRunes is 2048 four-byte
	// characters (8192 bytes). AsciiRunes stays ASCII.
	RuneGatedPayload = mustMarshal(RuneGated{
		LongRunes:  strings.Repeat("🦊", 2048),
		AsciiRunes: strings.Repeat("abc123", 1366),
	})

	// HTML-escape parity.
	HTMLEscapeValue = HTMLEscape{Note: strings.Repeat("<a>&", 200)}
	HTMLPlainValue = HTMLPlain{Note: strings.Repeat("<a>&", 200)}
	EasyHTMLPlainValue = EasyHTMLPlain(HTMLPlainValue)
}

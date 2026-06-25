// Package bench hosts the macro-benchmark types. Kept in a non-test file so
// easyjson's bootstrap (which compiles the non-test build) can see them.
//
// The mega payload is generated from Node at init with a fixed seed —
// ~1 MiB, 6 levels deep.
package bench

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/sirkostya009/ggen/encode"
)

// Addr is the pointed-to type for Node's Refs []*Addr and Parent *Addr.
//
//ggen:generate
//easyjson:json
type Addr struct {
	Street string `json:"street"`
	City   string `json:"city"`
}

// Node is the deep-tree benchmark target. Exercises the breadth of ggen
// kinds: scalars, slices, maps, tuples, slices of pointers, nested slices,
// pointer fields, time/bytes/raw/any, and validation. All shapes are also
// supported by jsonv2/sonic/easyjson for apples-to-apples comparison.
//
//ggen:generate
//easyjson:json
type Node struct {
	ID        int64             `json:"id" pipe:"required gte=0"`
	Name      string            `json:"name" pipe:"required minlen=1 maxlen=128"`
	Score     float64           `json:"score" pipe:"gte=0 lte=100"`
	Active    bool              `json:"active"`
	Tags      []string          `json:"tags" pipe:"maxlen=64 inner:(minlen=1 maxlen=64)"`
	Props     map[string]string `json:"props" pipe:"maxlen=64"`
	Children  []Node            `json:"children" pipe:"maxlen=16"`
	Coords    [2]float64        `json:"coords"`
	Refs      []*Addr           `json:"refs" pipe:"maxlen=16"`
	Matrix    [][]int           `json:"matrix" pipe:"maxlen=16 inner:maxlen=32"`
	Parent    *Addr             `json:"parent,omitzero"`
	CreatedAt time.Time         `json:"createdAt"`
	Blob      []byte            `json:"blob"`
	Extra     any               `json:"extra"`
	Raw       json.RawMessage   `json:"raw"`
}

// AddrPlain strips easyjson's methods off Addr (`type T U` drops U's methods)
// so NodePlain never falls back to easyjson via the json.Unmarshaler hook.
type AddrPlain Addr

// NodePlain mirrors Node with a self-referential shape free of easyjson's
// generated methods, so the stdjson/jsonv2 rows measure the reflection path
// rather than easyjson via the json.Marshaler/Unmarshaler hooks.
type NodePlain struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Score     float64           `json:"score"`
	Active    bool              `json:"active"`
	Tags      []string          `json:"tags"`
	Props     map[string]string `json:"props"`
	Children  []NodePlain       `json:"children"`
	Coords    [2]float64        `json:"coords"`
	Refs      []*AddrPlain      `json:"refs"`
	Matrix    [][]int           `json:"matrix"`
	Parent    *AddrPlain        `json:"parent,omitzero"`
	CreatedAt time.Time         `json:"createdAt"`
	Blob      []byte            `json:"blob"`
	Extra     any               `json:"extra"`
	Raw       json.RawMessage   `json:"raw"`
}

// nodeToPlain deep-converts a Node tree into NodePlain for the marshal
// benches. One-shot at init.
func nodeToPlain(n Node) NodePlain {
	p := NodePlain{
		ID:        n.ID,
		Name:      n.Name,
		Score:     n.Score,
		Active:    n.Active,
		Tags:      n.Tags,
		Props:     n.Props,
		Coords:    n.Coords,
		Matrix:    n.Matrix,
		CreatedAt: n.CreatedAt,
		Blob:      n.Blob,
		Extra:     n.Extra,
		Raw:       n.Raw,
	}
	if n.Parent != nil {
		ap := AddrPlain(*n.Parent)
		p.Parent = &ap
	}
	if n.Refs != nil {
		p.Refs = make([]*AddrPlain, len(n.Refs))
		for i, r := range n.Refs {
			if r != nil {
				ap := AddrPlain(*r)
				p.Refs[i] = &ap
			}
		}
	}
	if n.Children != nil {
		p.Children = make([]NodePlain, len(n.Children))
		for i, c := range n.Children {
			p.Children[i] = nodeToPlain(c)
		}
	}
	return p
}

// Validated exercises per-field validation rules for fail-fast streaming
// benchmarks — Email (alphabetically first) is corrupted to force early
// rejection.
//
//ggen:generate
type Validated struct {
	Email string   `json:"email" pipe:"required email"`
	Name  string   `json:"name"  pipe:"required minlen=1 maxlen=64"`
	Age   int      `json:"age"   pipe:"gte=0 lte=150"`
	Tags  []string `json:"tags" pipe:"inner:(notempty minlen=1 maxlen=32)"`
	Bio   string   `json:"bio"   pipe:"maxlen=4096"`
}

var (
	MegaValue      Node
	MegaValuePlain NodePlain // converted copy for stdjson/jsonv2/sonic marshal rows
	MegaPayload    []byte

	// ValidPayload + InvalidPayload — same-size bodies for fail-fast
	// streaming benchmarks; the invalid one fails on the first decoded
	// field (email).
	ValidPayload   []byte
	InvalidPayload []byte
)

func init() {
	r := rand.New(rand.NewSource(1))
	MegaValue = buildNode(r, 6, []int{5, 4, 3, 3, 3, 3, 0})
	MegaValuePlain = nodeToPlain(MegaValue)
	var err error
	MegaPayload, err = encode.Marshal(MegaValue)
	if err != nil {
		panic(err)
	}

	// ~3 KiB body; Bio padded so a slow reader delivers it in chunks.
	bio := randString(rand.New(rand.NewSource(3)), 2800)
	tags := []string{"alpha", "beta", "gamma", "delta"}
	ValidPayload, err = encode.Marshal(Validated{
		Email: "alice@example.com",
		Name:  "alice",
		Age:   30,
		Tags:  tags,
		Bio:   bio,
	})
	if err != nil {
		panic(err)
	}
	// Same shape, but Email is malformed — fails the email rule on the
	// first decoded field (sorted order: age, bio, then email trips).
	InvalidPayload, err = encode.Marshal(Validated{
		Email: "not-an-email",
		Name:  "alice",
		Age:   30,
		Tags:  tags,
		Bio:   bio,
	})
	if err != nil {
		panic(err)
	}
}

func buildNode(r *rand.Rand, depth int, fanout []int) Node {
	n := Node{
		ID:        r.Int63(),
		Name:      randString(r, 8+r.Intn(56)),
		Score:     r.Float64() * 100,
		Active:    r.Intn(2) == 0,
		Tags:      randTags(r, 6+r.Intn(20)),
		Props:     randProps(r, 6+r.Intn(20)),
		Coords:    [2]float64{r.Float64()*180 - 90, r.Float64()*360 - 180},
		Refs:      randAddrPtrs(r, 4+r.Intn(8)),
		Matrix:    randMatrix(r, 4+r.Intn(8)),
		Parent:    randAddrPtr(r, r.Intn(3) != 0),
		CreatedAt: time.Unix(r.Int63n(1<<31), r.Int63n(1<<30)).UTC(),
		Blob:      randBytes(r, 32+r.Intn(192)),
		Extra:     randAny(r),
		Raw:       randRaw(r),
	}
	if depth > 0 && depth < len(fanout) {
		kids := fanout[len(fanout)-1-depth]
		if kids > 0 {
			n.Children = make([]Node, kids)
			for i := range n.Children {
				n.Children[i] = buildNode(r, depth-1, fanout)
			}
		}
	}
	return n
}

func randAddr(r *rand.Rand) Addr {
	return Addr{Street: randString(r, 8+r.Intn(16)), City: randString(r, 6+r.Intn(10))}
}

func randAddrPtr(r *rand.Rand, present bool) *Addr {
	if !present {
		return nil
	}
	a := randAddr(r)
	return &a
}

func randAddrPtrs(r *rand.Rand, n int) []*Addr {
	out := make([]*Addr, n)
	for i := range out {
		// every 4th element nil — exercises the slab path's null branch
		out[i] = randAddrPtr(r, i%4 != 0)
	}
	return out
}

func randMatrix(r *rand.Rand, rows int) [][]int {
	out := make([][]int, rows)
	for i := range out {
		cols := 4 + r.Intn(20)
		row := make([]int, cols)
		for j := range row {
			row[j] = r.Intn(1_000_000) - 500_000
		}
		out[i] = row
	}
	return out
}

func randBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Intn(256))
	}
	return b
}

// randAny synthesizes an `any` value — mostly scalars or nil, occasionally
// a small list or object.
func randAny(r *rand.Rand) any {
	switch r.Intn(10) {
	case 0, 1, 2, 3:
		return nil
	case 4:
		return randString(r, 16+r.Intn(64))
	case 5:
		return r.Float64() * 1000
	case 6:
		return r.Int63n(1 << 40)
	case 7:
		return r.Intn(2) == 0
	case 8:
		// Small string list.
		n := 2 + r.Intn(4)
		out := make([]string, n)
		for i := range out {
			out[i] = randString(r, 8+r.Intn(16))
		}
		return out
	default:
		// Small flat object.
		n := 2 + r.Intn(3)
		out := make(map[string]string, n)
		for range n {
			out[randString(r, 4+r.Intn(8))] = randString(r, 8+r.Intn(24))
		}
		return out
	}
}

// randRaw synthesizes JSON snippets aliased as RawMessage — objects,
// arrays of mixed scalars, and standalone scalars.
func randRaw(r *rand.Rand) json.RawMessage {
	switch r.Intn(6) {
	case 0:
		return rawObject(r, 4+r.Intn(8))
	case 1:
		return rawArray(r, 4+r.Intn(12))
	case 2:
		return rawObject(r, 8+r.Intn(16))
	case 3:
		return json.RawMessage(fmt.Sprintf(`%q`, randString(r, 32+r.Intn(96))))
	case 4:
		return json.RawMessage(fmt.Sprintf(`%d`, r.Int63n(1<<60)))
	default:
		return json.RawMessage(`null`)
	}
}

// rawObject builds a JSON object literal with n random string→scalar entries.
func rawObject(r *rand.Rand, n int) json.RawMessage {
	var b []byte
	b = append(b, '{')
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '"')
		b = append(b, randString(r, 4+r.Intn(12))...)
		b = append(b, '"', ':')
		b = appendRawScalar(b, r)
	}
	b = append(b, '}')
	return b
}

func rawArray(r *rand.Rand, n int) json.RawMessage {
	var b []byte
	b = append(b, '[')
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendRawScalar(b, r)
	}
	b = append(b, ']')
	return b
}

func appendRawScalar(b []byte, r *rand.Rand) []byte {
	switch r.Intn(6) {
	case 0:
		return append(b, fmt.Sprintf(`%q`, randString(r, 8+r.Intn(48)))...)
	case 1:
		return append(b, fmt.Sprintf(`%d`, r.Int63n(1<<48))...)
	case 2:
		return append(b, fmt.Sprintf(`%.4f`, r.Float64()*10_000)...)
	case 3:
		if r.Intn(2) == 0 {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case 4:
		return append(b, "null"...)
	default:
		// Nested array of ints.
		k := 2 + r.Intn(6)
		b = append(b, '[')
		for i := range k {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, fmt.Sprintf(`%d`, r.Int63n(1<<32))...)
		}
		return append(b, ']')
	}
}

const asciiLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randString(r *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = asciiLetters[r.Intn(len(asciiLetters))]
	}
	return string(b)
}

func randTags(r *rand.Rand, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = randString(r, 4+r.Intn(10))
	}
	return out
}

func randProps(r *rand.Rand, n int) map[string]string {
	out := make(map[string]string, n)
	for range n {
		out[randString(r, 4+r.Intn(8))] = randString(r, 8+r.Intn(24))
	}
	return out
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

var (
	TinyValue     Claim
	EasyTinyValue EasyClaim
	TinyPayload   []byte
)

// --- Validation-heavy payload ---------------------------------------------

// ValidationHeavy carries enough rules that the per-field check cost shows
// up against codecs that don't validate. Uses minrunes/maxrunes (full UTF-8
// walk) so the per-string scan cost is meaningful.
//
//ggen:generate
type ValidationHeavy struct {
	Email    string  `json:"email" pipe:"required email maxrunes=128"`
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

var ValidationHeavyPayload []byte

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
	HTMLEscapeValue    HTMLEscape
	HTMLPlainValue     HTMLPlain
	EasyHTMLPlainValue EasyHTMLPlain
)

// --- Deep-nested payload (50-level Node chain) ----------------------------

var DeepNestedPayload []byte

// --- Map-heavy payload (1K-entry string→string map) ----------------------

// MapHeavy holds one big string-keyed map (1K+ entries) where map alloc,
// hash fill, and iteration dominate — a different bottleneck from mega.
//
//ggen:generate
type MapHeavy struct {
	Labels map[string]string `json:"labels"`
}

var MapHeavyPayload []byte

func init() {
	// Tiny / Claim.
	now := time.Now().Unix()
	TinyValue = Claim{
		Sub: "user-12345",
		Iss: "https://auth.example.com",
		Exp: now + 3600,
		Iat: now,
		Aud: "api",
		Jti: "abc123def456",
	}
	EasyTinyValue = EasyClaim{
		Sub: TinyValue.Sub, Iss: TinyValue.Iss, Exp: TinyValue.Exp,
		Iat: TinyValue.Iat, Aud: TinyValue.Aud, Jti: TinyValue.Jti,
	}
	var err error
	TinyPayload, err = encode.Marshal(TinyValue)
	if err != nil {
		panic(err)
	}

	// Validation-heavy.
	v := ValidationHeavy{
		Email: "user@example.com", Username: "alice42", Phone: "1234567890",
		Age: 30, Score: 99.5, Name: "Alice",
		URL: "https://example.com", Country: "UA", Lang: "en", Role: "user",
	}
	ValidationHeavyPayload, err = encode.Marshal(v)
	if err != nil {
		panic(err)
	}

	// HTML-escape parity.
	HTMLEscapeValue = HTMLEscape{Note: strings.Repeat("<a>&", 200)}
	HTMLPlainValue = HTMLPlain{Note: strings.Repeat("<a>&", 200)}
	EasyHTMLPlainValue = EasyHTMLPlain(HTMLPlainValue)

	// Deep-nested 50-level chain.
	var deep Node
	deep.ID = 1
	deep.Name = "leaf"
	for i := 0; i < 50; i++ {
		deep = Node{
			ID:       int64(i + 1),
			Name:     "level-" + strconv.Itoa(i),
			Children: []Node{deep},
		}
	}
	DeepNestedPayload, err = encode.Marshal(deep)
	if err != nil {
		panic(err)
	}

	// Map-heavy 1024-entry string map.
	m := make(map[string]string, 1024)
	for i := range 1024 {
		m["key"+strconv.Itoa(i)] = "value" + strconv.Itoa(i)
	}
	MapHeavyPayload, err = encode.Marshal(MapHeavy{Labels: m})
	if err != nil {
		panic(err)
	}
}

// Account is the zero-allocation parse target (BenchmarkNoAlloc_Unmarshal):
// a wide denormalized record — profile, address, employer, settings —
// flattened into one object. Free of every kind that forces a decode alloc
// (no slices/maps/pointers/any/RawMessage), so a full decode makes ZERO
// allocations: strings alias the input, nested structs decode in place.
//
//ggen:generate
type Account struct {
	ID          uint64  `json:"id"`
	Username    string  `json:"username"`
	Email       string  `json:"email"`
	FirstName   string  `json:"firstName"`
	LastName    string  `json:"lastName"`
	MiddleName  string  `json:"middleName"`
	DisplayName string  `json:"displayName"`
	Phone       string  `json:"phone"`
	Age         uint8   `json:"age"`
	Verified    bool    `json:"verified"`
	Active      bool    `json:"active"`
	Premium     bool    `json:"premium"`
	Suspended   bool    `json:"suspended"`
	Deleted     bool    `json:"deleted"`
	Balance     float64 `json:"balance"`
	Reputation  int32   `json:"reputation"`
	TrustScore  float64 `json:"trustScore"`

	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
	LastLogin    int64  `json:"lastLogin"`
	LoginCount   uint32 `json:"loginCount"`
	FailedLogins uint16 `json:"failedLogins"`

	Bio       string `json:"bio"`
	AvatarURL string `json:"avatarUrl"`
	BannerURL string `json:"bannerUrl"`
	Locale    string `json:"locale"`

	FollowerCount  int `json:"followerCount"`
	FollowingCount int `json:"followingCount"`
	PostCount      int `json:"postCount"`

	StorageUsed      int64 `json:"storageUsed"`
	StorageQuota     int64 `json:"storageQuota"`
	TwoFactorEnabled bool  `json:"twoFactorEnabled"`

	Address     PostalAddress `json:"address"`
	Company     Company       `json:"company"`
	Preferences Preferences   `json:"preferences"`
}

//ggen:generate
type PostalAddress struct {
	Line1      string `json:"line1"`
	Line2      string `json:"line2"`
	City       string `json:"city"`
	State      string `json:"state"`
	PostalCode string `json:"postalCode"`
	Country    string `json:"country"`
	Geo        Geo    `json:"geo"`
}

//ggen:generate
type Geo struct {
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
	Altitude float64 `json:"altitude"`
	Accuracy float32 `json:"accuracy"`
}

//ggen:generate
type Company struct {
	Name       string `json:"name"`
	Department string `json:"department"`
	Title      string `json:"title"`
	EmployeeID string `json:"employeeId"`
	Headcount  int    `json:"headcount"`
	Founded    int16  `json:"founded"`
	IsPublic   bool   `json:"isPublic"`
}

//ggen:generate
type Preferences struct {
	Theme              string `json:"theme"`
	Language           string `json:"language"`
	Timezone           string `json:"timezone"`
	Currency           string `json:"currency"`
	EmailNotifications bool   `json:"emailNotifications"`
	PushNotifications  bool   `json:"pushNotifications"`
	SMSNotifications   bool   `json:"smsNotifications"`
	ItemsPerPage       uint8  `json:"itemsPerPage"`
	AutoSave           bool   `json:"autoSave"`
	BetaFeatures       bool   `json:"betaFeatures"`
}

// Easy* mirror the Account family for the easyjson rows, kept on separate
// types so easyjson's methods don't leak into the jsonv2/sonic rows. Same
// wire shape — see "easyjson method leakage" in bench/CLAUDE.md.
//
//easyjson:json
type EasyAccount struct {
	ID          uint64  `json:"id"`
	Username    string  `json:"username"`
	Email       string  `json:"email"`
	FirstName   string  `json:"firstName"`
	LastName    string  `json:"lastName"`
	MiddleName  string  `json:"middleName"`
	DisplayName string  `json:"displayName"`
	Phone       string  `json:"phone"`
	Age         uint8   `json:"age"`
	Verified    bool    `json:"verified"`
	Active      bool    `json:"active"`
	Premium     bool    `json:"premium"`
	Suspended   bool    `json:"suspended"`
	Deleted     bool    `json:"deleted"`
	Balance     float64 `json:"balance"`
	Reputation  int32   `json:"reputation"`
	TrustScore  float64 `json:"trustScore"`

	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
	LastLogin    int64  `json:"lastLogin"`
	LoginCount   uint32 `json:"loginCount"`
	FailedLogins uint16 `json:"failedLogins"`

	Bio       string `json:"bio"`
	AvatarURL string `json:"avatarUrl"`
	BannerURL string `json:"bannerUrl"`
	Locale    string `json:"locale"`

	FollowerCount  int `json:"followerCount"`
	FollowingCount int `json:"followingCount"`
	PostCount      int `json:"postCount"`

	StorageUsed      int64 `json:"storageUsed"`
	StorageQuota     int64 `json:"storageQuota"`
	TwoFactorEnabled bool  `json:"twoFactorEnabled"`

	Address     EasyPostalAddress `json:"address"`
	Company     EasyCompany       `json:"company"`
	Preferences EasyPreferences   `json:"preferences"`
}

//easyjson:json
type EasyPostalAddress struct {
	Line1      string  `json:"line1"`
	Line2      string  `json:"line2"`
	City       string  `json:"city"`
	State      string  `json:"state"`
	PostalCode string  `json:"postalCode"`
	Country    string  `json:"country"`
	Geo        EasyGeo `json:"geo"`
}

//easyjson:json
type EasyGeo struct {
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
	Altitude float64 `json:"altitude"`
	Accuracy float32 `json:"accuracy"`
}

//easyjson:json
type EasyCompany struct {
	Name       string `json:"name"`
	Department string `json:"department"`
	Title      string `json:"title"`
	EmployeeID string `json:"employeeId"`
	Headcount  int    `json:"headcount"`
	Founded    int16  `json:"founded"`
	IsPublic   bool   `json:"isPublic"`
}

//easyjson:json
type EasyPreferences struct {
	Theme              string `json:"theme"`
	Language           string `json:"language"`
	Timezone           string `json:"timezone"`
	Currency           string `json:"currency"`
	EmailNotifications bool   `json:"emailNotifications"`
	PushNotifications  bool   `json:"pushNotifications"`
	SMSNotifications   bool   `json:"smsNotifications"`
	ItemsPerPage       uint8  `json:"itemsPerPage"`
	AutoSave           bool   `json:"autoSave"`
	BetaFeatures       bool   `json:"betaFeatures"`
}

// AccountValue is a representative populated record; AccountPayload is its
// marshalled JSON, built once at init.
var (
	AccountValue   Account
	AccountPayload []byte
)

func init() {
	AccountValue = Account{
		ID:          9876543210,
		Username:    "аліса.андерсон",
		Email:       "аліса.андерсон@приклад-корпорація.укр",
		FirstName:   "Аліса",
		LastName:    "Андерсон",
		MiddleName:  "Маргарита",
		DisplayName: "アリサ・アンダーソン 🦊",
		Phone:       "+1-415-555-0173",
		Age:         34,
		Verified:    true,
		Active:      true,
		Premium:     true,
		Suspended:   false,
		Deleted:     false,
		Balance:     12489.57,
		Reputation:  84213,
		TrustScore:  98.6,

		CreatedAt:    1593561600,
		UpdatedAt:    1718668800,
		LastLogin:    1718712345,
		LoginCount:   4821,
		FailedLogins: 3,

		// Long multilingual non-ASCII bodies — exercise the UTF-8 string
		// scan (multi-byte runes, no escapes) over large fields.
		Bio: "Провідна інженерка розподілених систем. ‹分散システムの主任エンジニア›. " +
			"Кохаю каву ☕, біг по стежках 🏃 та маю давню образу на необмежені черги. " +
			"Mes opinions sont porteuses — μην εμπιστεύεσαι ουρές χωρίς όριο. " +
			"Строю надійні конвеєри даних, пишу про backpressure, спостережуваність і те, " +
			"чому «просто додай ще один воркер» — це не стратегія. 🧵📊🛰️ " +
			"In früheren Leben: компілятори, ядра, та забагато YAML. 日々精進。",
		AvatarURL: "https://кеш.приклад-корпорація.укр/аватари/аліса-андерсон/" +
			"профіль_512x512.webp?версія=42&підпис=a1b2c3d4e5f6&регіон=eu-central&тема=темна",
		BannerURL: "https://кеш.приклад-корпорація.укр/банери/аліса-андерсон/" +
			"обкладинка_1500x500.webp?версія=17&підпис=f6e5d4c3b2a1&регіон=eu-central&палітра=ніч",
		Locale: "uk-UA",

		FollowerCount:  18342,
		FollowingCount: 312,
		PostCount:      2774,

		StorageUsed:      8734092123,
		StorageQuota:     53687091200,
		TwoFactorEnabled: true,

		Address: PostalAddress{
			Line1:      "вулиця Хрещатик, буд. 22",
			Line2:      "офіс 4200, поверх 12",
			City:       "Київ",
			State:      "Київська область",
			PostalCode: "01001",
			Country:    "Україна 🇺🇦",
			Geo: Geo{
				Lat:      50.450100,
				Lng:      30.523400,
				Altitude: 179.5,
				Accuracy: 4.75,
			},
		},
		Company: Company{
			Name:       "Приклад Корпорація Інтернешнл «Хмара»",
			Department: "Платформна інфраструктура",
			Title:      "Головна інженерка з програмного забезпечення",
			EmployeeID: "СПІВ-0000-4821",
			Headcount:  18750,
			Founded:    1998,
			IsPublic:   true,
		},
		Preferences: Preferences{
			Theme:              "темна",
			Language:           "українська",
			Timezone:           "Europe/Kyiv",
			Currency:           "₴ UAH",
			EmailNotifications: true,
			PushNotifications:  false,
			SMSNotifications:   false,
			ItemsPerPage:       50,
			AutoSave:           true,
			BetaFeatures:       true,
		},
	}

	var err error
	if AccountPayload, err = encode.Marshal(AccountValue); err != nil {
		panic(err)
	}
}

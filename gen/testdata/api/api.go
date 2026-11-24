// Package api is the gen test fixture.
package api

import (
	"database/sql"
	"encoding/json"
	"image"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"github.com/sirkostya009/ggen/gen/testdata/other"
)

// EUR extends other.Currency from this package.
const EUR other.Currency = "EUR"

// CreateUser is a request.
//
//ggen:generate
//schema:in
type CreateUser struct {
	// Name is trimmed first.
	Name    string  `json:"name"  pipe:"required trim minlen=1 maxlen=64"`
	Email   string  `json:"email" pipe:"required tolower contains=@"`
	Age     *int    `json:"age"   pipe:"gte=0 @IsAdult:'must be an adult'"`
	Address Address `json:"address"`
}

// User is a response.
//
//ggen:generate
//schema:out
type User struct {
	ID      int64          `json:"id"`
	Role    Role           `json:"role"`
	Status  string         `json:"status" pipe:"oneof=active|banned"`
	Plan    Plan           `json:"plan,omitzero"`
	Tags    []string       `json:"tags,omitempty"`
	Created time.Time      `json:"created"`
	Level   slog.Level     `json:"level"`
	Point   image.Point    `json:"point"`
	Money   other.Money    `json:"money"`
	Address Address        `json:"address"`
	Manager *User          `json:"manager,omitempty"`
	Friends []*User        `json:"friends"`
	State   http.ConnState `json:"state"`
}

// Role is an enum.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// Plan is an integer enum without a zero member.
type Plan int

const (
	PlanFree Plan = iota + 1
	PlanPro
)

// Address is reached from both.
type Address struct {
	City string `json:"city" pipe:"required"`
	Zip  string `json:"zip"  pipe:"numeric len=5"`
}

// Edges covers the remaining wire shapes.
//
//ggen:generate ignoreunknown
type Edges struct {
	List   []string          `json:"list"   pipe:"minlen=1 inner:notempty"`
	Max    []int             `json:"max"    pipe:"maxlen=2"`
	PList  *[]string         `json:"plist"  pipe:"minlen=1"`
	Dict   map[string]int    `json:"dict"   pipe:"keys:minlen=2 inner:gt=0"`
	Quoted int8              `json:"quoted,string"`
	Unix   time.Time         `json:"unix,format:unixmilli"`
	Date   time.Time         `json:"date,format:DateOnly"`
	Dur    time.Duration     `json:"dur"`
	Hex    []byte            `json:"hex,format:hex"`
	Fixed  [4]byte           `json:"fixed"`
	IP     net.IP            `json:"ip"`
	Addr   netip.Addr        `json:"addr"`
	Net    net.IPNet         `json:"net"`
	TCP    *net.TCPAddr      `json:"tcp"`
	UDP    net.UDPAddr       `json:"udp"`
	NS     sql.NullString    `json:"ns"`
	Raw    json.RawMessage   `json:"raw"`
	Lower  string            `json:"lower"  pipe:"tolower oneof=x|y"`
	NZ     int               `json:"nz"     pipe:"nullzero"`
	Conv   int               `json:"conv"   pipe:"nullzero / . / @Atoi"`
	Tags   Tags              `json:"tags"`
	Extra  map[string]string `json:",embed"`
}

// Tags is an annotated container alias.
//
//ggen:generate
type Tags []string

// IsAdult is a custom validator.
func IsAdult(n *int) bool { return n == nil || *n >= 18 }

// Atoi is a converter.
func Atoi(s string) (int, error) { return strconv.Atoi(s) }

// Kind is an annotated enum alias: generated, and a closed set of its own.
//
//ggen:generate
type Kind string

const (
	KindOne Kind = "one"
	KindTwo Kind = "two"
)

// Tree is one half of a cycle between two types.
//
//ggen:generate
type Tree struct {
	Leaf *Leaf `json:"leaf"`
}

// Leaf closes the cycle with Tree.
type Leaf struct {
	Tree *Tree `json:"tree"`
}

// Node is a tree that references only itself.
//
//ggen:generate
type Node struct {
	Name     string  `json:"name"`
	Children []*Node `json:"children"`
}

// Section collects nested sections under every other key.
//
//ggen:generate
type Section struct {
	Title string             `json:"title"`
	Sub   map[string]Section `json:",embed"`
}

// Awkward carries names a target has to escape.
//
//ggen:generate
type Awkward struct {
	Class  string `json:"class"`
	Func   string `json:"func"`
	Val    string `json:"val"`
	In     string `json:"in"`
	Spaced string `json:"a b"`
}

// Collides has two fields that spell one property in a lowerCamel target.
//
//ggen:generate
type Collides struct {
	Id int `json:"id"`
	ID int `json:"ID"`
}

// Omits leaves fields out of the output, each by its own guard.
//
//ggen:generate
type Omits struct {
	Peer   *net.TCPAddr    `json:"peer,omitzero"`
	Count  **int           `json:"count,omitzero"`
	Role   *Role           `json:"role,omitzero"`
	Net    net.IPNet       `json:"net,omitzero"`
	Addr   netip.Addr      `json:"addr,omitzero"`
	NS     sql.NullString  `json:"ns,omitzero"`
	Raw    json.RawMessage `json:"raw,omitzero"`
	Any    any             `json:"any,omitzero"`
	Ints   []int           `json:"ints,omitzero"`
	URL    url.URL         `json:"url,omitempty"`
	Prefix net.IPNet       `json:"prefix,omitempty"`
	Depth  **int           `json:"depth,omitempty"`
	Name   **string        `json:"name,omitempty"`
}

// Nulls decodes null as the zero value of a field with no null of its own.
//
//ggen:generate nullzero
type Nulls struct {
	Name  string      `json:"name"`
	Code  string      `json:"code"  pipe:"minlen=1"`
	Kind  string      `json:"kind"  pipe:"oneof=a|b"`
	Count int         `json:"count" pipe:"gte=0 lte=9"`
	Pos   int         `json:"pos"   pipe:"gt=0"`
	Level int         `json:"level" pipe:"clamp=1|5 gte=1"`
	Eq    int         `json:"eq"    pipe:"eq=0"`
	Neq   int         `json:"neq"   pipe:"neq=0"`
	Alnum string      `json:"alnum" pipe:"alphanum"`
	Pair  [2]int      `json:"pair"  pipe:"maxlen=1"`
	Fixed [4]byte     `json:"fixed"`
	List  []string    `json:"list"  pipe:"notempty"`
	Peer  net.TCPAddr `json:"peer"`
	Ints  []int       `json:"ints"  pipe:"@Ints ~"`
	Conv  []int       `json:"conv"  pipe:"nullzero / @Ints ~ minlen=1"`
}

// Quoted carries rules on `,string` numbers, which Go checks on the parsed
// number.
//
//ggen:generate
type Quoted struct {
	Clamp int     `json:"clamp,string" pipe:"clamp=1|5"`
	Floor int64   `json:"floor,string" pipe:"clamp=1|"`
	Ratio float64 `json:"ratio,string" pipe:"clamp=|0.5"`
	Pick  float64 `json:"pick,string"  pipe:"oneof=1|2"`
	Sign  int     `json:"sign,string"  pipe:"oneof=0|1"`
	Max   int64   `json:"max,string"   pipe:"lte=9223372036854775806"`
	Neq   uint64  `json:"neq,string"   pipe:"neq=9007199254740993"`
	Mul   int64   `json:"mul,string"   pipe:"multiple=3"`
}

// Chained has rules after a oneof that a transform precedes.
//
//ggen:generate
type Chained struct {
	Word  string `json:"word"  pipe:"trim oneof=a|bb toupper minlen=1"`
	Digit int    `json:"digit" pipe:"clamp=0|9 oneof=1|2 gt=0"`
}

// Narrow has rules on float32 fields, which Go checks after rounding.
//
//ggen:generate
type Narrow struct {
	GT   float32 `json:"gt"          pipe:"gt=0.1"`
	LTE  float32 `json:"lte"         pipe:"lte=0.1"`
	Eq   float32 `json:"eq"          pipe:"eq=0.1"`
	Pick float32 `json:"pick"        pipe:"oneof=0.1|0.2"`
	Text float32 `json:"text,string" pipe:"gte=0.1"`
}

// Keyed restricts the keys of its maps. A keys: step applies to the field's
// own map only, not to a map inside it.
//
//ggen:generate
type Keyed struct {
	Two    map[string]int            `json:"two"    pipe:"keys:oneof=ab|cd"`
	One    map[string]int            `json:"one"    pipe:"keys:oneof=ab"`
	Nested map[string]map[string]int `json:"nested" pipe:"keys:(tolower oneof=ab|cd)"`
}

// AddressView delegates to the generated Address.
//
//ggen:generate
type AddressView Address

// Secret encodes itself as text and has no exported fields.
type Secret struct{ v string }

func (s Secret) MarshalText() ([]byte, error) { return []byte(s.v), nil }

func (s *Secret) UnmarshalText(b []byte) error {
	s.v = string(b)
	return nil
}

// Token delegates to the text methods of Secret.
//
//ggen:generate
type Token Secret

// TokenView delegates to Token, so it is the string Token is.
//
//ggen:generate
type TokenView Token

// Ints is a converter into a type that has a null of its own.
func Ints(s string) ([]int, error) {
	n, err := strconv.Atoi(s)
	return []int{n}, err
}

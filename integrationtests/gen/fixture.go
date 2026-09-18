// Package genfixture holds the types the gen differential test renders and
// decodes with ggen.
package genfixture

//go:generate ../../ggen ./...

import (
	"database/sql"
	"encoding/json"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"github.com/sirkostya009/ggen/integrationtests/gen/other"
)

// Status is an unannotated named primitive.
type Status string

// Role is an enum type.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// Count is an annotated primitive alias.
//
//ggen:generate
type Count int32

// Tags is an annotated container alias.
//
//ggen:generate
type Tags []string

// Color has a text codec, so it is external.
type Color struct{ n int }

func (c Color) MarshalText() ([]byte, error) { return []byte(strconv.Itoa(c.n)), nil }

func (c *Color) UnmarshalText(b []byte) error {
	n, err := strconv.Atoi(string(b))
	c.n = n
	return err
}

//ggen:generate
type Scalars struct {
	S    string          `json:"s"`
	B    bool            `json:"b"`
	I8   int8            `json:"i8"`
	U8   uint8           `json:"u8"`
	I32  int32           `json:"i32"`
	U32  uint32          `json:"u32"`
	I64  int64           `json:"i64"`
	U64  uint64          `json:"u64"`
	F32  float32         `json:"f32"`
	F64  float64         `json:"f64"`
	PS   *string         `json:"ps"`
	PI   **int           `json:"pi"`
	QI   int             `json:"qi,string"`
	QU   uint8           `json:"qu,string"`
	QF   float64         `json:"qf,string"`
	St   Status          `json:"st"`
	Role Role            `json:"role"`
	Cnt  Count           `json:"cnt"`
	Col  Color           `json:"col"`
	Req  string          `json:"req" pipe:"required"`
	Any  any             `json:"any"`
	Raw  json.RawMessage `json:"raw"`
	Om   string          `json:"om,omitempty"`
}

//ggen:generate
type Stdlib struct {
	T      time.Time      `json:"t"`
	TUnix  time.Time      `json:"t_unix,format:unix"`
	TMilli time.Time      `json:"t_milli,format:unixmilli"`
	TDate  time.Time      `json:"t_date,format:DateOnly"`
	D      time.Duration  `json:"d"`
	DSec   time.Duration  `json:"d_sec,format:sec"`
	DMilli time.Duration  `json:"d_milli,format:milli"`
	B64    []byte         `json:"b64"`
	B64U   []byte         `json:"b64u,format:base64url"`
	B32    []byte         `json:"b32,format:base32"`
	Hex    []byte         `json:"hex,format:hex"`
	Arr    []byte         `json:"arr,format:array"`
	Fixed  [4]byte        `json:"fixed"`
	MinB   []byte         `json:"min_b" pipe:"minlen=2"`
	IP     net.IP         `json:"ip"`
	Addr   netip.Addr     `json:"addr"`
	Prefix netip.Prefix   `json:"prefix"`
	URL    url.URL        `json:"url"`
	BigI   big.Int        `json:"big_i"`
	BigF   big.Float      `json:"big_f"`
	BigR   big.Rat        `json:"big_r"`
	NS     sql.NullString `json:"ns"`
	NI     sql.NullInt16  `json:"ni"`
	NG     sql.Null[bool] `json:"ng"`
}

//ggen:generate
type Containers struct {
	List   []string          `json:"list"   pipe:"minlen=1 inner:(trim notempty)"`
	Max    []int8            `json:"max"    pipe:"maxlen=2 inner:gte=0"`
	Nested [][]float64       `json:"nested" pipe:"inner:(inner:lte=1)"`
	Tuple  [3]int            `json:"tuple"`
	Empty  [0]int            `json:"empty"`
	Ptrs   []*string         `json:"ptrs"`
	PList  *[]string         `json:"plist"  pipe:"notempty"`
	Dict   map[string]int    `json:"dict"   pipe:"keys:(minlen=2 alphanum) inner:gt=0"`
	NEDict map[string]bool   `json:"nedict" pipe:"notempty"`
	Tags   Tags              `json:"tags"`
	Money  other.Money       `json:"money"`
	Monies []*other.Money    `json:"monies"`
	Extra  map[string]string `json:",embed"`
}

//ggen:generate
type Rules struct {
	Name   string  `json:"name"   pipe:"required trim minlen=2 maxlen=10"`
	Runes  string  `json:"runes"  pipe:"minrunes=2 maxrunes=3"`
	Bytes  string  `json:"bytes"  pipe:"maxlen=3"`
	Kind   string  `json:"kind"   pipe:"oneof=a|b|'c d'"`
	Lower  string  `json:"lower"  pipe:"tolower oneof=x|y"`
	Level  int     `json:"level"  pipe:"oneof=1|2|3"`
	Score  float64 `json:"score"  pipe:"gt=0 lte=100"`
	Even   int     `json:"even"   pipe:"multiple=2 neq=4"`
	Eq     string  `json:"eq"     pipe:"eq=yes"`
	Link   string  `json:"link"   pipe:"url"`
	Code   string  `json:"code"   pipe:"alphanum len=6"`
	Digits string  `json:"digits" pipe:"numeric"`
	HexS   string  `json:"hex_s"  pipe:"hexadecimal"`
	Low    string  `json:"low"    pipe:"islower"`
	Up     string  `json:"up"     pipe:"isupper"`
	Aff    string  `json:"aff"    pipe:"starts=a ends=z contains=m"`
	Tr     string  `json:"tr"     pipe:"trimleft=< trimright=> replace=_|- toupper eq=A-B"`
	Clamp  int     `json:"clamp"  pipe:"clamp=0|10 lte=5"`
	Opt    *int    `json:"opt"    pipe:"gte=1"`
	NZ     int     `json:"nz"     pipe:"nullzero"`
	Conv   int     `json:"conv"   pipe:"nullzero / . / @Atoi"`
	Even2  int     `json:"even2"  pipe:"@IsEven:'must be even'"`
}

// Atoi is a converter from a string input.
func Atoi(s string) (int, error) { return strconv.Atoi(s) }

// IsEven is a custom validator.
func IsEven(n int) bool { return n%2 == 0 }

// Node is recursive through Pair.
//
//ggen:generate ignoreunknown
type Node struct {
	Value    string  `json:"value" pipe:"required"`
	Children []*Node `json:"children"`
	Next     *Pair   `json:"next,omitempty"`
}

// Pair is reached from Node and generated with it.
type Pair struct {
	Left  *Node `json:"left"`
	Right *Node `json:"right"`
}

// Level is an integer enum type.
type Level int

const (
	LevelLow  Level = 1
	LevelHigh Level = 2
)

// Embedded is promoted into More.
type Embedded struct {
	Note string `json:"note"`
}

// More covers the kinds, formats and field shapes the other subjects miss.
//
//ggen:generate
type More struct {
	Embedded
	U       uint                   `json:"u"`
	U16     uint16                 `json:"u16"`
	I16     int16                  `json:"i16"`
	Num     json.Number            `json:"num"`
	QU64    uint64                 `json:"qu64,string"`
	Micro   time.Time              `json:"micro,format:unixmicro"`
	Nano    time.Time              `json:"nano,format:unixnano"`
	DMicro  time.Duration          `json:"d_micro,format:micro"`
	DNano   time.Duration          `json:"d_nano,format:nano"`
	B32H    []byte                 `json:"b32h,format:base32hex"`
	Lvl     Level                  `json:"lvl"`
	NT      sql.NullTime           `json:"nt"`
	Structs map[string]other.Money `json:"structs"`
	PMap    *map[string]int        `json:"pmap"`
	Zero    string                 `json:"zero,omitzero"`
	Skip    string                 `json:"-"`
	Quoted  string                 `json:"'a,b'"`
}

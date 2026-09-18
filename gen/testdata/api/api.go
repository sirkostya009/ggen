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

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sirkostya009/ggen"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty"
)

type headJSONOnly struct{ v string }

func (j headJSONOnly) MarshalJSON() ([]byte, error)  { return []byte(j.v), nil }
func (j *headJSONOnly) UnmarshalJSON(b []byte) error { j.v = string(b); return nil }

type headTextOnly struct{ v string }

func (t headTextOnly) MarshalText() ([]byte, error)  { return []byte(t.v), nil }
func (t *headTextOnly) UnmarshalText(b []byte) error { t.v = string(b); return nil }

// HeadJSONAlias and HeadTextAlias delegate to UnmarshalJSON and
// UnmarshalText.
//
//ggen:generate
type HeadJSONAlias headJSONOnly

//ggen:generate
type HeadTextAlias headTextOnly

//ggen:generate
type HeadTags []string

//ggen:generate
type HeadLookup map[string]int

// HeadPtrTime and its siblings put values behind a pointer. The stream peeks
// one byte for `null` there before it decodes the value, so input that ends
// at the value head is reported by that peek.
//
//ggen:generate
type HeadPtrTime struct {
	TU *time.Time     `json:"tu,format:unix"`
	TM *time.Time     `json:"tm,format:unixnano"`
	TR *time.Time     `json:"tr"`
	DS *time.Duration `json:"ds,format:sec"`
	DN *time.Duration `json:"dn,format:nano"`
	DD *time.Duration `json:"dd"`
}

//ggen:generate
type HeadPtrRungs struct {
	BI *big.Int             `json:"bi"`
	BF *big.Float           `json:"bf"`
	BR *big.Rat             `json:"br"`
	U  *uuid.UUID           `json:"u"`
	X  *thirdparty.External `json:"x"`
	RE *regexp.Regexp       `json:"re"`
	AJ *HeadJSONAlias       `json:"aj"`
	AT *HeadTextAlias       `json:"at"`
}

//ggen:generate
type HeadPtrMisc struct {
	PG *sql.Null[time.Duration] `json:"pg"`
	PN *sql.NullTime            `json:"pn"`
	TG *HeadTags                `json:"tg"`
	LK *HeadLookup              `json:"lk"`
	NA *netip.Addr              `json:"na"`
	UR *url.URL                 `json:"ur"`
	PI *int                     `json:"pi"`
	PS *string                  `json:"ps"`
	JN *json.Number             `json:"jn"`
	BY *[]byte                  `json:"by"`
}

//ggen:generate
type HeadPtrElems struct {
	MU map[string]*uuid.UUID           `json:"mu"`
	SU []*uuid.UUID                    `json:"su"`
	MX map[string]*thirdparty.External `json:"mx"`
	ST []*HeadTextAlias                `json:"st"`
}

//ggen:generate nullzero
type HeadNullZero struct {
	TU time.Time           `json:"tu,format:unixmilli"`
	DS time.Duration       `json:"ds,format:milli"`
	BI big.Int             `json:"bi"`
	U  uuid.UUID           `json:"u"`
	X  thirdparty.External `json:"x"`
	AJ HeadJSONAlias       `json:"aj"`
	AT HeadTextAlias       `json:"at"`
	CI int                 `json:"ci"`
}

//ggen:generate
type HeadSQLNull struct {
	BI sql.Null[big.Int]             `json:"bi"`
	U  sql.Null[uuid.UUID]           `json:"u"`
	X  sql.Null[thirdparty.External] `json:"x"`
	AJ sql.Null[HeadJSONAlias]       `json:"aj"`
	AT sql.Null[HeadTextAlias]       `json:"at"`
	DS sql.Null[time.Duration]       `json:"ds"`
	CI sql.Null[int]                 `json:"ci"`
}

func headKeys[T any]() []string {
	rt := reflect.TypeFor[T]()
	keys := make([]string, 0, rt.NumField())
	for field := range rt.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		keys = append(keys, name)
	}
	return keys
}

// Input that ends where a value should start gets the same sentinel on both
// paths, whatever decodes the value: a format, a text or JSON method, the
// encoding/json fallback, or a generated alias.
func TestTruncationSentinelParity_Rungs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		keys []string
		run  func(string, int) (error, error)
	}{
		{"HeadPtrTime", headKeys[HeadPtrTime](), decodeBothPathsChunked[HeadPtrTime]},
		{"HeadPtrRungs", headKeys[HeadPtrRungs](), decodeBothPathsChunked[HeadPtrRungs]},
		{"HeadPtrMisc", headKeys[HeadPtrMisc](), decodeBothPathsChunked[HeadPtrMisc]},
		{"HeadNullZero", headKeys[HeadNullZero](), decodeBothPathsChunked[HeadNullZero]},
		{"HeadSQLNull", headKeys[HeadSQLNull](), decodeBothPathsChunked[HeadSQLNull]},
	}
	elems := []string{`{"mu":{"a":`, `{"su":[`, `{"su":[null,`, `{"mx":{"a":`, `{"st":[`}
	for _, payload := range elems {
		for _, chunk := range []int{1, 4096} {
			bytesErr, streamErr := decodeBothPathsChunked[HeadPtrElems](payload, chunk)
			var bpe, spe *ggen.ParseError
			if !errors.As(bytesErr, &bpe) || !errors.As(streamErr, &spe) || bpe.Err != spe.Err || bpe.Pos != spe.Pos {
				t.Errorf("HeadPtrElems %s chunk=%d: bytes %v, stream %v", payload, chunk, bytesErr, streamErr)
			}
		}
	}
	for _, c := range cases {
		for _, key := range c.keys {
			payload := `{"` + key + `":`
			for _, chunk := range []int{1, 4096} {
				bytesErr, streamErr := c.run(payload, chunk)
				var bpe, spe *ggen.ParseError
				if !errors.As(bytesErr, &bpe) || !errors.As(streamErr, &spe) {
					t.Errorf("%s %s chunk=%d: bytes=%v stream=%v", c.name, payload, chunk, bytesErr, streamErr)
					continue
				}
				if bpe.Err != spe.Err || bpe.Pos != spe.Pos {
					t.Errorf("%s %s chunk=%d: bytes %v at %d, stream %v at %d", c.name, payload, chunk, bpe.Err, bpe.Pos, spe.Err, spe.Pos)
				}
			}
		}
	}
}

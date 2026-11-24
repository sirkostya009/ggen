// Package other is a second package referenced from genfixture.
package other

import "time"

// Money is an amount in minor units.
//
//ggen:generate
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency" pipe:"required len=3 isupper"`
}

// Instant delegates to time.Time.
//
//ggen:generate
type Instant time.Time

// Color encodes itself as text.
type Color struct{ v string }

func (c Color) MarshalText() ([]byte, error) { return []byte(c.v), nil }

func (c *Color) UnmarshalText(b []byte) error {
	c.v = string(b)
	return nil
}

// Tint delegates to Color, so it is a string.
//
//ggen:generate
type Tint Color

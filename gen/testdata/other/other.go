// Package other is referenced from api.
package other

// Money is an amount in minor units.
//
//ggen:generate
type Money struct {
	Amount   int64    `json:"amount"`
	Currency Currency `json:"currency"`
}

// Currency is an enum whose values are split across packages.
type Currency string

const USD Currency = "USD"

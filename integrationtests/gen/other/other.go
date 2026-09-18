// Package other is a second package referenced from genfixture.
package other

// Money is an amount in minor units.
//
//ggen:generate
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency" pipe:"required len=3 isupper"`
}

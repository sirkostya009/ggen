// Package dep declares types that only package cross references.
package dep

// Tier is an enum no type of this package references.
//
//schema:out
type Tier string

const Gold Tier = "gold"

// Price is generated here and referenced from cross.
//
//ggen:generate
type Price struct {
	Cents int64 `json:"cents"`
}

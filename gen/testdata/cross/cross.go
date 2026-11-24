// Package cross references types of package dep.
package cross

import "github.com/sirkostya009/ggen/gen/testdata/cross/dep"

//ggen:generate
type Order struct {
	Tier  dep.Tier  `json:"tier"`
	Price dep.Price `json:"price"`
}

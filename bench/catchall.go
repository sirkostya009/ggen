// CatchAll benchmark family (catchall_test.go).

//go:generate ../ggen $GOFILE
package bench

import "strconv"

// CatchAllInts is the type for BenchmarkCatchAll_Marshal: a few declared
// fields and a catch-all whose values are numbers, the shape of a metrics or
// counters document.
//
//ggen:generate
type CatchAllInts struct {
	ID   int64            `json:"id"`
	Name string           `json:"name"`
	Rest map[string]int64 `json:",embed"`
}

// CatchAllFloats is the float64 twin.
//
//ggen:generate
type CatchAllFloats struct {
	ID   int64              `json:"id"`
	Name string             `json:"name"`
	Rest map[string]float64 `json:",embed"`
}

var (
	CatchAllIntsValue   CatchAllInts
	CatchAllFloatsValue CatchAllFloats
)

func init() {
	ints := make(map[string]int64, 32)
	floats := make(map[string]float64, 32)
	for i := range 32 {
		k := "metric_" + strconv.Itoa(i)
		ints[k] = int64(i)*1_000_003 + 1_000_000
		floats[k] = float64(i)*1.25 + 0.5
	}
	CatchAllIntsValue = CatchAllInts{ID: 7, Name: "host-1", Rest: ints}
	CatchAllFloatsValue = CatchAllFloats{ID: 7, Name: "host-1", Rest: floats}
}

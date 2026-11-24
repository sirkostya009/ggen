// JSONNumber benchmark family (jsonnumber_test.go).

//go:generate ../ggen $GOFILE
package bench

import (
	"encoding/json"
	"strconv"
)

// Quote is the element type for BenchmarkJSONNumber. Four of its six fields
// are json.Number, the type an API uses to keep a decimal's exact digits.
//
//ggen:generate
type Quote struct {
	ID     int64       `json:"id"`
	Symbol string      `json:"symbol"`
	Bid    json.Number `json:"bid"`
	Ask    json.Number `json:"ask"`
	Volume json.Number `json:"volume"`
	Last   json.Number `json:"last"`
}

// QuoteBook is the type for BenchmarkJSONNumber: a page of quotes.
//
//ggen:generate
type QuoteBook struct {
	Quotes []Quote `json:"quotes"`
}

// JSONNumberValue and JSONNumberPayload are 256 quotes and their encoding.
var (
	JSONNumberValue   QuoteBook
	JSONNumberPayload []byte
)

func init() {
	for i := range 256 {
		JSONNumberValue.Quotes = append(JSONNumberValue.Quotes, Quote{
			ID:     int64(i),
			Symbol: "SYM" + strconv.Itoa(i),
			Bid:    json.Number(strconv.FormatFloat(100+float64(i)*0.37, 'f', 4, 64)),
			Ask:    json.Number(strconv.FormatFloat(100.05+float64(i)*0.37, 'f', 4, 64)),
			Volume: json.Number(strconv.Itoa(1000000 + i*7919)),
			Last:   json.Number(strconv.FormatFloat(100.02+float64(i)*0.37, 'f', 6, 64)),
		})
	}
	var err error
	JSONNumberPayload, err = json.Marshal(JSONNumberValue)
	if err != nil {
		panic(err)
	}
}

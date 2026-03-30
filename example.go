//go:build goexperiment.jsonv2

package schema

import (
	"log/slog"
	"net/http"
)

//go:generate ggen SomePayloadRequestStruct AnotherStruct

type SomePayloadRequestStruct struct {
	Field1 string `json:"field1" jsonvalidate:"optional,minlen=2,maxlen=23"`
	Slice  []int  `json:"array" jsonvalidate:"required,minlen=1,maxlen=10"`
}

type AnotherStruct struct {
	Title   string  `json:"title" jsonvalidate:"required,minlen=1,maxlen=100"`
	Score   float64 `json:"score" jsonvalidate:"min=0,max=10"`
	Active  bool    `json:"active"`
}

func ExampleHandler(w http.ResponseWriter, r *http.Request) {
	req, err := SomePayloadRequestStruct{}.ParseFrom(r.Body)
	defer r.Body.Close()

if err != nil {
		slog.Error(err.Error())
		w.WriteHeader(400)
		return
	}

	// Field1 can be an empty string, but if its not then its at least 2 to 23 characters long
	// Slice is guaranteed to be at least 1 and at most 10 integers long

	_ = req.Slice
}

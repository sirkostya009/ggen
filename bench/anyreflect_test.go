package bench

import (
	jsonv2 "encoding/json/v2"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/sirkostya009/ggen"
)

// The anyReflect types carry no generated methods, so ggen.AppendAny reaches
// them through its reflect walk. That is the path a value of a type ggen does
// not generate takes inside an `any` field.

type anyReflectStatus string

type anyReflectPos struct{ Lat, Lon float64 }

type anyReflectRow struct {
	ID     int64             `json:"id"`
	Name   string            `json:"name"`
	Status anyReflectStatus  `json:"status"`
	Score  float64           `json:"score"`
	Active bool              `json:"active"`
	Pos    anyReflectPos     `json:"pos"`
	Parent *anyReflectRow    `json:"parent,omitempty"`
	Attrs  map[string]string `json:"attrs,omitempty"`
}

type anyReflectEvent struct {
	ID   int64     `json:"id"`
	Tags []string  `json:"tags"`
	At   time.Time `json:"at"`
}

// BenchmarkAnyReflect_Marshal marshals 32 entries of each shape. rows,
// row_ptrs and row_map hold a struct of scalars with a nested struct, a
// pointer and a map. statuses is a slice of a named string. events holds a
// struct with a slice and a time.Time field. jsonv2 is the control row.
// ggen_presized appends into a kept buffer, which leaves the walk itself.
func BenchmarkAnyReflect_Marshal(b *testing.B) {
	const n = 32
	rows := make([]anyReflectRow, n)
	ptrs := make([]*anyReflectRow, n)
	byKey := make(map[string]anyReflectRow, n)
	statuses := make([]anyReflectStatus, n)
	events := make([]anyReflectEvent, n)
	for i := range n {
		rows[i] = anyReflectRow{
			ID: int64(i) << 20, Name: "row-" + strconv.Itoa(i), Status: "active",
			Score: float64(i) / 3, Active: i%2 == 0, Pos: anyReflectPos{Lat: 49.84, Lon: 24.03},
		}
		ptrs[i] = &rows[i]
		byKey["key"+strconv.Itoa(i)] = rows[i]
		statuses[i] = "active"
		events[i] = anyReflectEvent{ID: int64(i), Tags: []string{"a", "b"}, At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	}
	shapes := []struct {
		name string
		v    any
	}{
		{"rows", rows},
		{"row_ptrs", ptrs},
		{"row_map", byKey},
		{"statuses", statuses},
		{"events", events},
	}
	for _, sh := range shapes {
		want, err := jsonv2.Marshal(sh.v)
		if err != nil {
			b.Fatalf("%s: jsonv2: %v", sh.name, err)
		}
		got, err := ggen.AppendAny(nil, sh.v)
		if err != nil {
			b.Fatalf("%s: ggen: %v", sh.name, err)
		}
		// Map order differs between the two, so compare the decoded values.
		var wantV, gotV any
		if err := jsonv2.Unmarshal(want, &wantV); err != nil {
			b.Fatal(err)
		}
		if err := jsonv2.Unmarshal(got, &gotV); err != nil || !reflect.DeepEqual(gotV, wantV) {
			b.Fatalf("%s: ggen wrote %s, jsonv2 %s (err %v)", sh.name, got, want, err)
		}
		buf := make([]byte, 0, 2*len(got))
		codecs := []struct {
			name string
			fn   func() error
		}{
			{"jsonv2", func() error { _, err := jsonv2.Marshal(sh.v); return err }},
			{"ggen", func() error { _, err := ggen.AppendAny(nil, sh.v); return err }},
			{"ggen_presized", func() error { _, err := ggen.AppendAny(buf[:0], sh.v); return err }},
		}
		for _, c := range codecs {
			b.Run(sh.name+"/"+c.name, func(b *testing.B) {
				b.SetBytes(int64(len(want)))
				b.ReportAllocs()
				for b.Loop() {
					if err := c.fn(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

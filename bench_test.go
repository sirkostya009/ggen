package main

import (
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkGenerate parses a representative file (the shared test
// struct module via integrationtests' shared_test.go) and rounds
// through generate() repeatedly. Run with -benchmem to compare
// allocs/op before vs after the buffer pooling change.
func BenchmarkGenerate(b *testing.B) {
	wd, _ := os.Getwd()
	src := filepath.Join(wd, "integrationtests", "shared_test.go")
	if _, err := os.Stat(src); err != nil {
		b.Skipf("no shared_test.go at %s: %v", src, err)
	}
	structs, pkg, err := parseFile(src, nil)
	if err != nil {
		b.Fatal(err)
	}
	if len(structs) == 0 {
		b.Fatal("no annotated structs in shared_test.go")
	}
	b.ReportAllocs()

	for b.Loop() {
		generatedTypes = nil
		generatedAliasKinds = nil
		_, err := generate(pkg, structs)
		if err != nil {
			b.Fatal(err)
		}
	}
}

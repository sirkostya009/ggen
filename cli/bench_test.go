package main

import (
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkGenerate parses integrationtests/shared_test.go and rounds through
// generate() repeatedly.
func BenchmarkGenerate(b *testing.B) {
	wd, _ := os.Getwd()
	src := filepath.Join(wd, "integrationtests", "shared_test.go")
	if _, err := os.Stat(src); err != nil {
		b.Skipf("no shared_test.go at %s: %v", src, err)
	}
	structs, pkg, _, err := parseFile(src, nil)
	if err != nil {
		b.Fatal(err)
	}
	if len(structs) == 0 {
		b.Fatal("no annotated structs in shared_test.go")
	}
	b.ReportAllocs()

	for b.Loop() {
		generatedTypes = nil
		multiErrTypes = nil
		cyclicTypes = nil
		namedKinds = nil
		_, err := generate(pkg, structs)
		if err != nil {
			b.Fatal(err)
		}
	}
}

package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: ggen StructName [StructName...]")
	}
	structNames := os.Args[1:]

	goFile := os.Getenv("GOFILE")
	if goFile == "" {
		log.Fatal("GOFILE environment variable not set (must be run via go generate)")
	}
	goPackage := os.Getenv("GOPACKAGE")
	if goPackage == "" {
		log.Fatal("GOPACKAGE environment variable not set (must be run via go generate)")
	}

	structs, err := parseStructs(goFile, structNames)
	if err != nil {
		log.Fatal(err)
	}

	src, err := generate(goPackage, structs)
	if err != nil {
		log.Fatal(err)
	}

	outFile := strings.TrimSuffix(filepath.Base(goFile), ".go") + "_gen.go"
	if err := os.WriteFile(outFile, src, 0644); err != nil {
		log.Fatal(err)
	}
}

package main

import (
	"flag"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const genSuffix = "_gen.go"

var cliFlags annotationFlags

func main() {
	var (
		outFlag string
		pkgFlag string
	)
	flag.StringVar(&outFlag, "o", "", "output file (single-file or single-dir mode only)")
	flag.StringVar(&pkgFlag, "pkg", "", "override package name")
	flag.BoolVar(&cliFlags.marshal, "marshal", false, "emit MarshalJSON hook (json.Marshaler) on every generated struct")
	flag.BoolVar(&cliFlags.unmarshal, "unmarshal", false, "emit UnmarshalJSON hook (json.Unmarshaler) on every generated struct")
	flag.BoolVar(&cliFlags.multierr, "multierr", false, "collect validation errors instead of returning on the first failure")
	flag.BoolVar(&cliFlags.allowdups, "allowdups", false, "skip the default duplicate-key guard in generated unmarshal code")
	flag.BoolVar(&cliFlags.novalidate, "novalidate", false, "skip validation rules, required-field checks, and mods (trades correctness for speed)")
	flag.BoolVar(&cliFlags.ignoreunknown, "ignoreunknown", false, "silently skip unknown JSON keys on unmarshal (default: error)")
	flag.BoolVar(&cliFlags.nosortkeys, "nosortkeys", false, "emit struct fields in declaration order (default: sorted by JSON name at codegen time)")
	flag.BoolVar(&cliFlags.usenumber, "usenumber", false, "decode JSON numbers into `any` fields as json.Number instead of float64 (mirrors json.Decoder.UseNumber)")
	flag.BoolVar(&cliFlags.htmlescape, "htmlescape", false, "HTML-safe escape <, >, & in emitted strings (default: literal, matches stdlib jsonv2)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage:")
		fmt.Fprintln(os.Stderr, "  ggen ./...                    walk, process every package with //ggen:generate-annotated structs")
		fmt.Fprintln(os.Stderr, "  ggen <dir>                    process one package")
		fmt.Fprintln(os.Stderr, "  ggen <file.go> [Names...]     single-file mode")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Per-struct overrides via doc comment: //ggen:generate marshal unmarshal")
		fmt.Fprintln(os.Stderr)
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		flag.Usage()
		os.Exit(2)
	}
	target := args[0]

	if root, ok := walkTarget(target); ok {
		if err := walkAndGenerate(root); err != nil {
			log.Fatal(err)
		}
		return
	}

	info, err := os.Stat(target)
	if err != nil {
		log.Fatal(err)
	}

	if info.IsDir() {
		if err := generateDir(target, outFlag, pkgFlag); err != nil {
			log.Fatal(err)
		}
		return
	}

	if err := generateSingleFile(target, args[1:], outFlag, pkgFlag); err != nil {
		log.Fatal(err)
	}
}

// applyCLIFlags ORs the CLI hook flags into each struct's flags and propagates
// the MultiErr flag down to every field (template-friendly access).
func applyCLIFlags(structs []StructInfo) {
	for i := range structs {
		if cliFlags.marshal {
			structs[i].Marshal = true
		}
		if cliFlags.unmarshal {
			structs[i].Unmarshal = true
		}
		if cliFlags.multierr {
			structs[i].MultiErr = true
		}
		if cliFlags.allowdups {
			structs[i].AllowDups = true
		}
		if cliFlags.novalidate {
			structs[i].NoValidate = true
		}
		if cliFlags.ignoreunknown {
			structs[i].IgnoreUnknown = true
		}
		if cliFlags.nosortkeys {
			structs[i].NoSort = true
		}
		if cliFlags.usenumber {
			structs[i].UseNumber = true
		}
		if cliFlags.htmlescape {
			structs[i].HTMLEscape = true
		}
		for j := range structs[i].Fields {
			structs[i].Fields[j].MultiErr = structs[i].MultiErr
			structs[i].Fields[j].AllowDups = structs[i].AllowDups
			structs[i].Fields[j].NoValidate = structs[i].NoValidate
			structs[i].Fields[j].UseNumber = structs[i].UseNumber
			structs[i].Fields[j].HTMLEscape = structs[i].HTMLEscape
		}
	}
}

func walkTarget(arg string) (string, bool) {
	switch arg {
	case "...", "./...":
		return ".", true
	}
	if before, ok := strings.CutSuffix(arg, "/..."); ok {
		return before, true
	}
	return "", false
}

func walkAndGenerate(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && shouldSkipDir(d.Name()) {
			return fs.SkipDir
		}
		if err := generateDir(path, "", ""); err != nil {
			return fmt.Errorf("in %s: %w", path, err)
		}
		return nil
	})
}

func shouldSkipDir(name string) bool {
	if name == "" {
		return false
	}
	switch name {
	case "vendor", "testdata", "node_modules":
		return true
	}
	return name[0] == '.' || name[0] == '_'
}

func generateDir(dir, outFlag, pkgFlag string) error {
	structs, pkgName, err := parsePackage(dir)
	if err != nil {
		return err
	}
	if len(structs) == 0 {
		return nil
	}
	applyCLIFlags(structs)

	outPkg := pkgFlag
	if outPkg == "" {
		outPkg = pkgName
	}

	// Bucket structs by (BuildTag, Test). Each bucket emits its own gen
	// file with a matching //go:build header so a struct declared behind
	// `//go:build foo` doesn't pollute the unconstrained file (and break
	// builds without `foo` with "undefined: <Struct>" errors).
	buckets := bucketStructs(structs)
	if outFlag != "" && len(buckets) > 1 {
		return fmt.Errorf("-o cannot be used when %s has structs across multiple build-tag / test groups (%d buckets)", dir, len(buckets))
	}

	// Seed generatedTypes with the full package set BEFORE running any
	// bucket. A struct in the tagged bucket may reference a struct in the
	// untagged bucket (or vice versa); since both end up in the same Go
	// package, the cross-bucket call must route to the direct DecodeFrom
	// (not the encoding/json fallback). Reset after the loop to avoid
	// leaking state into the next generateDir call.
	generatedTypes = make(map[string]struct{}, len(structs))
	generatedAliasKinds = make(map[string]TypeKind)
	for _, s := range structs {
		generatedTypes[s.Name] = struct{}{}
		if s.IsAlias && kindPrimitiveName(s.AliasKind) != "" {
			generatedAliasKinds[s.Name] = s.AliasKind
		}
	}
	defer func() {
		generatedTypes = nil
		generatedAliasKinds = nil
	}()

	for _, bk := range bucketKeys(buckets) {
		group := buckets[bk]
		src, err := generate(outPkg, group)
		if err != nil {
			return err
		}
		out := outFlag
		if out == "" {
			out = filepath.Join(dir, packageFileName(dir, bk.tag, bk.test))
		}
		if err := os.WriteFile(out, src, 0644); err != nil {
			return err
		}
		fmt.Println("wrote", out)
	}
	return nil
}

// bucketKey identifies one output file: structs sharing the same build
// constraint and test/non-test status are emitted together.
type bucketKey struct {
	tag  string
	test bool
}

// bucketStructs groups structs by (BuildTag, Test). Stable iteration order
// over the result is provided by bucketKeys.
func bucketStructs(structs []StructInfo) map[bucketKey][]StructInfo {
	out := make(map[bucketKey][]StructInfo, len(structs))
	for _, s := range structs {
		k := bucketKey{tag: s.BuildTag, test: s.Test}
		out[k] = append(out[k], s)
	}
	return out
}

// bucketKeys returns the keys of m sorted deterministically — empty tag
// first, then by tag string, with non-test before test inside each tag.
// Keeps `wrote` log output stable across runs.
func bucketKeys(m map[bucketKey][]StructInfo) []bucketKey {
	keys := slices.Collect(maps.Keys(m))
	slices.SortFunc(keys, func(a, b bucketKey) int {
		if a.tag != b.tag {
			return strings.Compare(a.tag, b.tag)
		}
		// non-test (false) before test (true)
		if !a.test && b.test {
			return 1
		}
		return 0
	})
	return keys
}

// packageFileName builds the output filename for one bucket. Untagged
// buckets keep the legacy `<dir>_gen.go` / `<dir>_gen_test.go` naming;
// tagged buckets get `<dir>_<slug>_gen.go` so multiple //go:build groups
// in the same package each end up in their own file.
func packageFileName(dir, tag string, testFile bool) string {
	base := filepath.Base(filepath.Clean(dir))
	if base == "." || base == "/" || base == "" {
		abs, err := filepath.Abs(dir)
		if err == nil {
			base = filepath.Base(abs)
		}
	}
	suffix := genSuffix
	if testFile {
		suffix = "_gen_test.go"
	}
	if tag == "" {
		return base + suffix
	}
	return base + "_" + slugifyTag(tag) + suffix
}

// slugifyTag converts a build constraint expression into a filename-safe
// slug. Non-alnum runs collapse into single underscores and leading /
// trailing underscores are trimmed, so `goexperiment.jsonv2` becomes
// `goexperiment_jsonv2` and `foo && bar` becomes `foo_bar`.
func slugifyTag(tag string) string {
	var b strings.Builder
	last := byte(0)
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if isAlnum {
			b.WriteByte(c)
			last = c
			continue
		}
		if last != '_' && b.Len() > 0 {
			b.WriteByte('_')
			last = '_'
		}
	}
	return strings.Trim(b.String(), "_")
}

func generateSingleFile(file string, wanted []string, outFlag, pkgFlag string) error {
	structs, pkgName, err := parseFile(file, wanted)
	if err != nil {
		return err
	}
	applyCLIFlags(structs)

	outPkg := pkgFlag
	if outPkg == "" {
		outPkg = pkgName
	}

	src, err := generate(outPkg, structs)
	if err != nil {
		return err
	}

	out := outFlag
	if out == "" {
		// Source foo_test.go → foo_gen_test.go; otherwise foo.go → foo_gen.go.
		if strings.HasSuffix(file, "_test.go") {
			out = strings.TrimSuffix(file, "_test.go") + "_gen_test.go"
		} else {
			out = strings.TrimSuffix(file, ".go") + genSuffix
		}
	}
	if err := os.WriteFile(out, src, 0644); err != nil {
		return err
	}
	fmt.Println("wrote", out)
	return nil
}

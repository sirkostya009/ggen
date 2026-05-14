package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	genSuffix     = "_ggen.go"
	genTestSuffix = "_ggen_test.go"
)

var (
	cliFlags annotationFlags
	cliLog   Logger
)

func main() {
	var (
		outFlag string
		pkgFlag string
		v       bool
		vv      bool
		vvv     bool
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
	flag.BoolVar(&v, "v", false, "\nverbose: info-level progress (wrote <file>)")
	flag.BoolVar(&vv, "vv", false, "more verbose: per-package / per-struct debug")
	flag.BoolVar(&vvv, "vvv", false, "trace-level diagnostics")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage:")
		fmt.Fprintln(os.Stderr, "  ggen ./...                    walk, process every package with //ggen:generate-annotated structs")
		fmt.Fprintln(os.Stderr, "  ggen <dir>                    process one package")
		fmt.Fprintln(os.Stderr, "  ggen <file.go> [Types...]     single-file mode")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Per-struct overrides via doc comment: //ggen:generate marshal unmarshal")
		fmt.Fprintln(os.Stderr)
		flag.PrintDefaults()
	}
	flag.Parse()

	// Standard `flag` stops at the first non-flag argument, so
	// `ggen in.go -o out.go` would treat `-o out.go` as struct-name
	// filters. Re-parse around each positional to let flags appear in
	// any order — mirrors `go test`'s behaviour on the same surface.
	var positional []string
	for args := flag.Args(); len(args) > 0; args = flag.Args() {
		positional = append(positional, args[0])
		// flag.CommandLine's default ErrorHandling is ExitOnError, so a
		// malformed flag here exits before this returns.
		_ = flag.CommandLine.Parse(args[1:])
	}
	// Logger init must run AFTER the interspersing loop so flags placed
	// after positionals (e.g. `ggen msg.go -vv`) still take effect.
	// Highest-set verbosity wins; -vvv overrides -vv overrides -v.
	// Pretty vs concise impl is decided by env (CI / agent / non-TTY →
	// concise).
	level := LevelQuiet
	switch {
	case vvv:
		level = LevelTrace
	case vv:
		level = LevelDebug
	case v:
		level = LevelInfo
	}
	cliLog = NewLogger(level)

	if len(positional) < 1 {
		flag.Usage()
		os.Exit(2)
	}
	target := positional[0]

	if root, ok := strings.CutSuffix(target, "/..."); ok {
		if outFlag != "" {
			cliLog.Fatal(errors.New("-o cannot be used with ./... (walk visits multiple directories; each writes its own output)"))
		}
		// Walk mode collects errors across packages instead of bailing
		// on the first: a single bad rule in pkg/a shouldn't hide
		// problems in pkg/b. Filesystem-walk errors are fatal — we
		// can't recover from a permissions failure mid-walk.
		if err := walkAndGenerate(root); err != nil {
			cliLog.Fatal(err)
		}
	} else if info, err := os.Stat(target); err != nil {
		cliLog.Fatal(err)
	} else if info.IsDir() {
		if err := generateDir(target, outFlag, pkgFlag); err != nil {
			cliLog.Error(err)
		}
	} else if err := generateSingleFile(target, positional[1:], outFlag, pkgFlag); err != nil {
		cliLog.Error(err)
	}

	// Drain any errors collected during the run, then exit non-zero
	// if any were seen. Errors stay queued until here so they print as
	// a single batch — easier to scan than interleaved with `wrote …`
	// info lines from successful packages.
	cliLog.Flush()
	if cliLog.HasErrors() {
		os.Exit(1)
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
		// generateDir errors are queued via cliLog.Error and the walk
		// continues; main() flushes + exits non-zero at the end. Only
		// genuine WalkDir errors (permissions, IO) bubble up to halt.
		if err := generateDir(path, "", ""); err != nil {
			cliLog.Error(fmt.Errorf("in %s: %w", path, err))
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
	cliLog.Debug("parsing package %s", dir)
	structs, pkgName, err := parsePackage(dir)
	if err != nil {
		return err
	}
	if len(structs) == 0 {
		cliLog.Trace("no annotated structs in %s; skipping", dir)
		return nil
	}
	cliLog.Debug("package %s: %d annotated structs", pkgName, len(structs))
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
		cliLog.Info("wrote %s", out)
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
// buckets get `<dir>_ggen.go` / `<dir>_ggen_test.go`; tagged buckets
// get `<dir>_<slug>_ggen.go` so multiple //go:build groups in the same
// package each end up in their own file.
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
		suffix = genTestSuffix
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
		// Source foo_test.go → foo_ggen_test.go; otherwise foo.go → foo_ggen.go.
		if before, ok := strings.CutSuffix(file, "_test.go"); ok {
			out = before + genTestSuffix
		} else {
			out = strings.TrimSuffix(file, ".go") + genSuffix
		}
	}
	if err := os.WriteFile(out, src, 0644); err != nil {
		return err
	}
	cliLog.Info("wrote %s", out)
	return nil
}

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/sirkostya009/ggen/gen/model"
)

var (
	cliFlags model.Flags
	cliDry   bool
	cliLog   Logger

	// scanStringFn is the bytes-path string scanner emitted into generated
	// code. Under GOEXPERIMENT=simd it defaults to the fused AVX tier;
	// -simd picks a wider tier (or off). The tier is fixed at generate time —
	// generated code carries no runtime probing or branching.
	scanStringFn = "ggen.String"
	// simdSuffix is the tier suffix ("", "AVX", "AVX2", "AVX512") appended to
	// the encode escape helpers (appendStrFn) — set alongside scanStringFn.
	simdSuffix = ""
)

// resolveSIMD maps the -simd flag + GOEXPERIMENT env to the emitted scanner
// name. avx/avx2/avx512 require GOEXPERIMENT=simd (the emitted code can't
// build without it); an empty flag auto-enables the AVX tier when the
// experiment is on.
func resolveSIMD(simdFlag string) error {
	exp := slices.Contains(strings.Split(os.Getenv("GOEXPERIMENT"), ","), "simd")
	switch simdFlag {
	case "off":
	case "":
		if exp {
			scanStringFn = "ggen.StringAVX"
			simdSuffix = "AVX"
		}
	case "avx", "avx2", "avx512":
		if !exp {
			return fmt.Errorf("-simd=%s requires GOEXPERIMENT=simd (generated code imports simd/archsimd, which only exists under the experiment)", simdFlag)
		}
		simdSuffix = strings.ToUpper(simdFlag)
		scanStringFn = "ggen.String" + simdSuffix
	default:
		return fmt.Errorf("-simd=%s: unknown tier (off|avx|avx2|avx512)", simdFlag)
	}
	return nil
}

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
	flag.BoolVar(&cliFlags.Marshal, "marshal", false, "emit MarshalJSON hook (json.Marshaler) on every generated struct")
	flag.BoolVar(&cliFlags.Unmarshal, "unmarshal", false, "emit UnmarshalJSON hook (json.Unmarshaler) on every generated struct")
	flag.BoolVar(&cliFlags.MultiErr, "multierr", false, "collect validation errors instead of returning on the first failure")
	flag.BoolVar(&cliFlags.AllowDups, "allowdups", false, "skip the default duplicate-key guard in generated unmarshal code")
	flag.BoolVar(&cliFlags.NoValidate, "novalidate", false, "skip validation rules, required-field checks, and mods (trades correctness for speed)")
	flag.BoolVar(&cliFlags.IgnoreUnknown, "ignoreunknown", false, "silently skip unknown JSON keys on unmarshal (default: error)")
	flag.BoolVar(
		&cliFlags.NullZero,
		"nullzero",
		false,
		"accept explicit JSON null on non-pointer value fields, decoding it to the Go zero value (default: error)",
	)
	flag.BoolVar(&cliFlags.NoSortKeys, "nosortkeys", false, "emit struct fields in declaration order (default: sorted by JSON name at codegen time)")
	flag.BoolVar(
		&cliFlags.UseNumber,
		"usenumber",
		false,
		"decode JSON numbers into `any` fields as json.Number instead of float64 (mirrors json.Decoder.UseNumber)",
	)
	flag.BoolVar(&cliFlags.HTMLEscape, "htmlescape", false, "HTML-safe escape <, >, & in emitted strings (default: literal, matches stdlib jsonv2)")
	flag.BoolVar(
		&cliFlags.Copy,
		"copy",
		false,
		"bytes-path DecodeFrom copies strings, json.RawMessage, and any-embedded strings out of the input instead of aliasing it (mutating data after decode no longer corrupts decoded values)",
	)
	flag.BoolVar(
		&cliFlags.AllowInvalidUTF8,
		"allowinvalidutf8",
		false,
		"skip decode-side UTF-8 validation (default: reject invalid UTF-8 / unpaired surrogates, jsonv2 parity); permissive structs pass raw bytes through like encoding/json v1 minus the U+FFFD substitution on raw bytes",
	)
	flag.BoolVar(&cliDry, "dry", false, "dry run: parse and validate every annotated struct, surface all errors, emit no file")
	var simdFlag string
	flag.StringVar(
		&simdFlag,
		"simd",
		"",
		"SIMD tier for bytes-path string scans: off|avx|avx2|avx512 (default: avx when GOEXPERIMENT=simd is set, else off; generated code then requires GOEXPERIMENT=simd to build and a matching CPU to run — no runtime probing)",
	)
	flag.BoolVar(&v, "v", false, "\nverbose: info-level progress (wrote <file>)")
	flag.BoolVar(&vv, "vv", false, "more verbose: per-package / per-struct debug")
	flag.BoolVar(&vvv, "vvv", false, "trace-level diagnostics")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage:")
		fmt.Fprintln(os.Stderr, "  ggen ./...                    process every package matched by the pattern (module-scoped, same as `go build`)")
		fmt.Fprintln(os.Stderr, "  ggen <dir>                    process one package")
		fmt.Fprintln(os.Stderr, "  ggen <file.go> [Types...]     single-file mode")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Per-struct overrides via doc comment: //ggen:generate marshal unmarshal")
		fmt.Fprintln(os.Stderr)
		flag.PrintDefaults()
	}
	flag.Parse()

	// Standard `flag` stops at the first non-flag arg; re-parse around each
	// positional so flags can appear in any order (like `go test`).
	var positional []string
	for args := flag.Args(); len(args) > 0; args = flag.Args() {
		positional = append(positional, args[0])
		_ = flag.CommandLine.Parse(args[1:]) // ExitOnError handles malformed flags
	}
	// After the loop so flags placed after positionals still apply. Highest
	// verbosity wins.
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

	if err := resolveSIMD(simdFlag); err != nil {
		cliLog.Fatal(err)
	}

	if len(positional) < 1 {
		flag.Usage()
		os.Exit(2)
	}
	// -dry emits no file, so -o / -pkg are dead — reject rather than drop.
	if cliDry && (outFlag != "" || pkgFlag != "") {
		cliLog.Fatal(errors.New("-o / -pkg cannot be used with -dry (dry run emits no file)"))
	}

	// A leading FILE takes the rest as a struct-name filter; anything else
	// (dir / pattern) is a package target, like `go build`.
	if info, err := os.Stat(positional[0]); err == nil && !info.IsDir() {
		if cliDry {
			err = checkFile(positional[0], positional[1:])
		} else {
			err = generateSingleFile(positional[0], positional[1:], outFlag, pkgFlag)
		}
		if err != nil {
			cliLog.Error(err)
		}
	} else if target := positional[0]; len(positional) == 1 && !model.IsPattern(target) {
		// One plain directory: honours -o / -pkg and picks up a test-only package.
		if err := checkDirTarget(target); err != nil {
			cliLog.Fatal(err)
		}
		var err error
		if cliDry {
			err = checkPackage(target)
		} else {
			err = generateDir(target, outFlag, pkgFlag)
		}
		if err != nil {
			cliLog.Error(err)
		}
	} else {
		if outFlag != "" || pkgFlag != "" {
			name := "-o"
			if outFlag == "" {
				name = "-pkg"
			}
			if slices.ContainsFunc(positional, model.IsPattern) {
				cliLog.Fatal(fmt.Errorf("%s cannot be used with ./... (pattern matches multiple packages; each writes its own output)", name))
			}
			cliLog.Fatal(fmt.Errorf("%s cannot be used with multiple targets (each package writes its own output)", name))
		}
		for _, target := range positional {
			if !model.IsPattern(target) {
				if err := checkDirTarget(target); err != nil {
					cliLog.Fatal(err)
				}
			}
		}
		// One walk over every target: post-order across the union, so an
		// importer never runs before its dependency's _ggen.go exists — no
		// matter the order on the command line. Per-package errors are
		// collected; only packages.Load failures (no go.mod, bad pattern)
		// are fatal.
		act := func(dir string) error { return generateDir(dir, "", "") }
		if cliDry {
			act = checkPackage
		}
		if err := model.WalkPackages(positional, act, cliLog.Error); err != nil {
			cliLog.Fatal(err)
		}
	}

	// Drain collected errors as one batch (not interleaved with `wrote …`
	// lines), then exit non-zero if any were seen.
	cliLog.Flush()
	if cliLog.HasErrors() {
		os.Exit(1)
	}
}

// checkDirTarget rejects a missing target and a file positioned after the
// first argument (only the FIRST positional may be a file; its trailing args
// are struct names, so a file here is a mixed-target mistake).
func checkDirTarget(target string) error {
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: file targets must come first (ggen <file.go> [Names...]); mixing files with packages is not supported", target)
	}
	return nil
}

// genGlobalsMu protects the globally-shared generator state touched by
// generate() (generatedTypes, namedKinds, the oneof registry, the
// pools). Packages parse concurrently but only one goroutine enters the
// generate+write section; the lock is held for the whole post-parse phase.
var genGlobalsMu sync.Mutex

func generateDir(dir, outFlag, pkgFlag string) error {
	cliLog.Debugf("parsing package %s", dir)
	// Unlocked — go/packages.Load does its own concurrency, touches no globals.
	pkg, err := model.ParsePackage(dir)
	if err != nil {
		return err
	}
	structs, pkgName := pkg.Structs, pkg.Name
	if len(structs) == 0 {
		cliLog.Tracef("no annotated structs in %s; skipping", dir)
		return nil
	}
	cliLog.Debugf("package %s: %d annotated structs", pkgName, len(structs))
	cliFlags.Apply(structs)

	outPkg := pkgFlag
	if outPkg == "" {
		outPkg = pkgName
	}

	// Bucket structs by (BuildTag, Test, XTest): each emits its own gen file
	// with a matching //go:build header so a tagged struct doesn't pollute the
	// unconstrained file. The external test package is a package of its own —
	// its buckets generate with their own type seeding and package clause.
	buckets := bucketStructs(structs)
	if outFlag != "" && len(buckets) > 1 {
		return fmt.Errorf("-o cannot be used when %s has structs across multiple build-tag / test groups (%d buckets)", dir, len(buckets))
	}
	var base, xtest []model.StructInfo
	for _, s := range structs {
		if s.XTest {
			xtest = append(xtest, s)
		} else {
			base = append(base, s)
		}
	}
	if err := generateBuckets(dir, outFlag, outPkg, base); err != nil {
		return err
	}
	return generateBuckets(dir, outFlag, outPkg+"_test", xtest)
}

// generateBuckets writes one file per (BuildTag, Test) bucket of structs,
// which all belong to one Go package.
func generateBuckets(dir, outFlag, outPkg string, structs []model.StructInfo) error {
	if len(structs) == 0 {
		return nil
	}
	buckets := bucketStructs(structs)

	// LOCKED through the per-bucket loop. Seed generatedTypes with the full
	// package set first — a tagged-bucket struct may reference an untagged one
	// (same package), so the cross-bucket call must route to direct DecodeFrom.
	// Cleared at the end to avoid leaking into the next call.
	genGlobalsMu.Lock()
	defer genGlobalsMu.Unlock()
	generatedTypes = make(map[string]struct{}, len(structs))
	generatedFields = seedGeneratedFields(structs)
	namedKinds = make(map[string]model.TypeKind)
	for _, s := range structs {
		generatedTypes[s.Name] = struct{}{}
	}
	seedNamedKinds(structs)
	multiErrTypes = seedMultiErrTypes(structs)
	cyclicTypes = model.CyclicTypes(structs)
	defer func() {
		generatedTypes = nil
		generatedFields = nil
		namedKinds = nil
		multiErrTypes = nil
		cyclicTypes = nil
	}()

	for _, bk := range bucketKeys(buckets) {
		out := outFlag
		if out == "" {
			out = filepath.Join(dir, packageFileName(dir, bk.tag, bk.test, bk.xtest))
		}
		if err := writeGenerated(out, outPkg, buckets[bk]); err != nil {
			return err
		}
	}
	return nil
}

// writeGenerated renders structs and replaces out only once the whole file
// has formatted: a failed render must not truncate the previous good output
// and take the package build down with it.
func writeGenerated(out, pkg string, structs []model.StructInfo) error {
	var buf bytes.Buffer
	if err := generateTo(&buf, pkg, out, structs); err != nil {
		return err
	}
	if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
		return err
	}
	cliLog.Infof("wrote %s", out)
	return nil
}

// bucketKey identifies one output file: same build constraint + test status
// + package (base or external test).
type bucketKey struct {
	tag   string
	test  bool
	xtest bool
}

// bucketStructs groups structs by (BuildTag, Test, XTest); bucketKeys gives
// stable iteration order.
func bucketStructs(structs []model.StructInfo) map[bucketKey][]model.StructInfo {
	out := make(map[bucketKey][]model.StructInfo, len(structs))
	for _, s := range structs {
		k := bucketKey{tag: s.BuildTag, test: s.Test, xtest: s.XTest}
		out[k] = append(out[k], s)
	}
	return out
}

// bucketKeys returns m's keys sorted deterministically (empty tag first, then
// by tag, non-test before test, base package before external test) so
// `wrote` output stays stable across runs.
func bucketKeys(m map[bucketKey][]model.StructInfo) []bucketKey {
	keys := slices.Collect(maps.Keys(m))
	boolOrder := func(a, b bool) int {
		switch {
		case a == b:
			return 0
		case a:
			return 1
		}
		return -1
	}
	slices.SortFunc(keys, func(a, b bucketKey) int {
		if a.tag != b.tag {
			return strings.Compare(a.tag, b.tag)
		}
		if a.test != b.test {
			return boolOrder(a.test, b.test)
		}
		return boolOrder(a.xtest, b.xtest)
	})
	return keys
}

// packageFileName builds the output filename for one bucket: untagged buckets
// get `<dir>_ggen.go` / `<dir>_ggen_test.go`, tagged buckets
// `<dir>_<slug>_ggen.go`; the external test package's buckets carry `_xtest`
// before the suffix (`<dir>_xtest_ggen_test.go`).
func packageFileName(dir, tag string, testFile, xtest bool) string {
	base := filepath.Base(filepath.Clean(dir))
	if base == "." || base == "/" || base == "" {
		abs, err := filepath.Abs(dir)
		if err == nil {
			base = filepath.Base(abs)
		}
	}
	name := base
	if tag != "" {
		name += "_" + slugifyTag(tag)
	}
	if xtest {
		name += "_xtest"
	}
	if testFile {
		return name + model.GenTestSuffix
	}
	return name + model.GenSuffix
}

// slugifyTag makes a build-constraint expression filename-safe: non-alnum runs
// collapse to single underscores, trimmed (`goexperiment.simd` →
// `goexperiment_simd`, `foo && bar` → `foobufr`).
func slugifyTag(tag string) string {
	var b strings.Builder
	last := byte(0)
	for i := range len(tag) {
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
	res, err := model.ParseFile(file, wanted)
	if err != nil {
		return err
	}
	structs, pkgName, siblings := res.Structs, res.PkgName, res.Siblings
	cliFlags.Apply(structs)
	// Cycle analysis, the multierr callee set and the field sets are
	// package-wide: a cross-file A↔B cycle is invisible per file and loses the
	// recursion depth cap, a cross-file multierr callee loses its drain
	// branch, and a container emitter asks what a value type declared in
	// ANOTHER file owns.
	var pkgCyclic, pkgMultiErr map[string]struct{}
	var pkgFields map[string][]model.FieldInfo
	if res.Package != nil {
		pkgCyclic = model.CyclicTypes(res.Package)
		pkgMultiErr = seedMultiErrTypes(res.Package)
		pkgFields = seedGeneratedFields(res.Package)
	}

	outPkg := pkgFlag
	if outPkg == "" {
		outPkg = pkgName
	}

	out := outFlag
	if out == "" {
		// foo_test.go → foo_ggen_test.go; otherwise foo.go → foo_ggen.go.
		if before, ok := strings.CutSuffix(file, "_test.go"); ok {
			out = before + model.GenTestSuffix
		} else {
			out = strings.TrimSuffix(file, ".go") + model.GenSuffix
		}
	}
	// Seed generatedTypes with every annotated struct in the package (incl.
	// siblings in other files) so a cross-file reference routes to a direct
	// DecodeFrom before sibling _ggen files exist on disk. AliasKind seeding
	// stays local to the structs we actually emit.
	genGlobalsMu.Lock()
	defer genGlobalsMu.Unlock()
	generatedTypes = make(map[string]struct{}, len(siblings)+len(structs))
	// Package-wide, so a value type declared in a sibling file is judged by
	// what it owns rather than falling back to "unknown".
	generatedFields = make(map[string][]model.FieldInfo, len(pkgFields)+len(structs))
	maps.Copy(generatedFields, pkgFields)
	maps.Copy(generatedFields, seedGeneratedFields(structs))
	namedKinds = make(map[string]model.TypeKind)
	for n := range siblings {
		generatedTypes[n] = struct{}{}
	}
	for _, s := range structs {
		generatedTypes[s.Name] = struct{}{}
	}
	seedNamedKinds(structs)
	// Union with the package-wide multierr set — a cross-file multierr
	// callee otherwise lost its drain branch in single-file mode (same
	// class as the cross-file cycle fix below).
	multiErrTypes = seedMultiErrTypes(structs)
	for n := range pkgMultiErr {
		multiErrTypes[n] = struct{}{}
	}
	// Package-wide cycle set — generateTo's per-file fallback can't see a
	// cross-file A↔B cycle (opt #51 depth cap silently vanished there).
	cyclicTypes = pkgCyclic
	defer func() {
		generatedTypes = nil
		generatedFields = nil
		namedKinds = nil
		multiErrTypes = nil
		cyclicTypes = nil
	}()

	return writeGenerated(out, outPkg, structs)
}

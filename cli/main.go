package main

import (
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
)

const (
	genSuffix     = "_ggen.go"
	genTestSuffix = "_ggen_test.go"
)

var (
	cliFlags annotationFlags
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
	flag.BoolVar(&cliFlags.marshal, "marshal", false, "emit MarshalJSON hook (json.Marshaler) on every generated struct")
	flag.BoolVar(&cliFlags.unmarshal, "unmarshal", false, "emit UnmarshalJSON hook (json.Unmarshaler) on every generated struct")
	flag.BoolVar(&cliFlags.multierr, "multierr", false, "collect validation errors instead of returning on the first failure")
	flag.BoolVar(&cliFlags.allowdups, "allowdups", false, "skip the default duplicate-key guard in generated unmarshal code")
	flag.BoolVar(&cliFlags.novalidate, "novalidate", false, "skip validation rules, required-field checks, and mods (trades correctness for speed)")
	flag.BoolVar(&cliFlags.ignoreunknown, "ignoreunknown", false, "silently skip unknown JSON keys on unmarshal (default: error)")
	flag.BoolVar(&cliFlags.nullzero, "nullzero", false, "accept explicit JSON null on non-pointer value fields, decoding it to the Go zero value (default: error)")
	flag.BoolVar(&cliFlags.nosortkeys, "nosortkeys", false, "emit struct fields in declaration order (default: sorted by JSON name at codegen time)")
	flag.BoolVar(&cliFlags.usenumber, "usenumber", false, "decode JSON numbers into `any` fields as json.Number instead of float64 (mirrors json.Decoder.UseNumber)")
	flag.BoolVar(&cliFlags.htmlescape, "htmlescape", false, "HTML-safe escape <, >, & in emitted strings (default: literal, matches stdlib jsonv2)")
	flag.BoolVar(&cliFlags.copy, "copy", false, "bytes-path DecodeFrom copies strings, json.RawMessage, and any-embedded strings out of the input instead of aliasing it (mutating data after decode no longer corrupts decoded values)")
	flag.BoolVar(&cliFlags.allowinvalidutf8, "allowinvalidutf8", false, "skip decode-side UTF-8 validation (default: reject invalid UTF-8 / unpaired surrogates, jsonv2 parity); permissive structs pass raw bytes through like encoding/json v1 minus the U+FFFD substitution on raw bytes")
	flag.BoolVar(&cliDry, "dry", false, "dry run: parse and validate every annotated struct, surface all errors, emit no file")
	var simdFlag string
	flag.StringVar(&simdFlag, "simd", "", "SIMD tier for bytes-path string scans: off|avx|avx2|avx512 (default: avx when GOEXPERIMENT=simd is set, else off; generated code then requires GOEXPERIMENT=simd to build and a matching CPU to run — no runtime probing)")
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
	// (dir / pattern) makes every positional its own target, like `go build`.
	if info, err := os.Stat(positional[0]); err == nil && !info.IsDir() {
		if cliDry {
			err = checkFile(positional[0], positional[1:])
		} else {
			err = generateSingleFile(positional[0], positional[1:], outFlag, pkgFlag)
		}
		if err != nil {
			cliLog.Error(err)
		}
	} else {
		if outFlag != "" && len(positional) > 1 {
			cliLog.Fatal(errors.New("-o cannot be used with multiple targets (each package writes its own output)"))
		}
		for _, target := range positional {
			if strings.HasSuffix(target, "...") {
				if outFlag != "" {
					cliLog.Fatal(errors.New("-o cannot be used with ./... (pattern matches multiple packages; each writes its own output)"))
				}
				// Pattern mode collects per-package errors rather than bailing;
				// only packages.Load failures (no go.mod, bad pattern) are fatal.
				act := func(dir string) error { return generateDir(dir, "", "") }
				if cliDry {
					act = checkPackage
				}
				if err := walkPackages(target, act); err != nil {
					cliLog.Fatal(err)
				}
				continue
			}
			info, err := os.Stat(target)
			if err != nil {
				cliLog.Fatal(err)
			}
			if !info.IsDir() {
				// Only the FIRST positional may be a file (its trailing args
				// are struct names); a file here is a mixed-target mistake.
				cliLog.Fatal(fmt.Errorf("%s: file targets must come first (ggen <file.go> [Names...]); mixing files with packages is not supported", target))
			}
			if cliDry {
				err = checkPackage(target)
			} else {
				err = generateDir(target, outFlag, pkgFlag)
			}
			if err != nil {
				cliLog.Error(err)
			}
		}
	}

	// Drain collected errors as one batch (not interleaved with `wrote …`
	// lines), then exit non-zero if any were seen.
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
		if cliFlags.nullzero {
			structs[i].NullZero = true
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
		if cliFlags.copy {
			structs[i].Copy = true
		}
		if cliFlags.allowinvalidutf8 {
			structs[i].AllowInvalidUTF8 = true
		}
		for j := range structs[i].Fields {
			structs[i].Fields[j].MultiErr = structs[i].MultiErr
			structs[i].Fields[j].AllowDups = structs[i].AllowDups
			structs[i].Fields[j].NoValidate = structs[i].NoValidate
			structs[i].Fields[j].UseNumber = structs[i].UseNumber
			structs[i].Fields[j].HTMLEscape = structs[i].HTMLEscape
			structs[i].Fields[j].Copy = structs[i].Copy
			structs[i].Fields[j].AllowInvalidUTF8 = structs[i].AllowInvalidUTF8
			// OR, not assign: a per-field json:",nullzero" must survive when the
			// struct flag is off.
			structs[i].Fields[j].NullZero = structs[i].Fields[j].NullZero || structs[i].NullZero
		}
	}
}

// walkPackages resolves `pattern` via go/packages and invokes `act` on every
// matched package's directory — module-scoped, never crossing module bounds
// (like `go build <pattern>`). Processing is post-order over the matched import
// subgraph, so a package's `_ggen.go` lands on disk before any matched importer
// runs and cross-package field types route through direct DecodeFrom/AppendJSON
// rather than encoding/json. Deps outside the matched set are left alone.
func walkPackages(pattern string, act func(dir string) error) error {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports,
	}
	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		return err
	}
	// Surface per-package load errors via the logger rather than returning on
	// the first — a broken-import package shouldn't hide its siblings.
	for _, p := range pkgs {
		for _, e := range p.Errors {
			cliLog.Error(fmt.Errorf("%s: %s", p.PkgPath, e))
		}
	}

	matched := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		matched[p.PkgPath] = true
	}
	// Dedup by directory (packages.Load can return base + test variants for
	// the same dir; generateDir loads both via Tests:true, so visit once).
	visited := make(map[string]struct{}, len(pkgs))
	seenPath := make(map[string]struct{}, len(pkgs))
	var visit func(p *packages.Package)
	visit = func(p *packages.Package) {
		if _, ok := seenPath[p.PkgPath]; ok {
			return
		}
		seenPath[p.PkgPath] = struct{}{}
		for _, imp := range p.Imports {
			if matched[imp.PkgPath] {
				visit(imp)
			}
		}
		if len(p.GoFiles) == 0 {
			return
		}
		dir := filepath.Dir(p.GoFiles[0])
		if _, ok := visited[dir]; ok {
			return
		}
		visited[dir] = struct{}{}
		if err := act(dir); err != nil {
			cliLog.Error(fmt.Errorf("in %s: %w", dir, err))
		}
	}
	for _, p := range pkgs {
		visit(p)
	}
	return nil
}

// genGlobalsMu protects the globally-shared generator state touched by
// generate() (generatedTypes, namedKinds, the oneof registry, the
// pools). Packages parse concurrently but only one goroutine enters the
// generate+write section; the lock is held for the whole post-parse phase.
var genGlobalsMu sync.Mutex

func generateDir(dir, outFlag, pkgFlag string) error {
	cliLog.Debug("parsing package %s", dir)
	// Unlocked — go/packages.Load does its own concurrency, touches no globals.
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

	// Bucket structs by (BuildTag, Test): each emits its own gen file with a
	// matching //go:build header so a tagged struct doesn't pollute the
	// unconstrained file.
	buckets := bucketStructs(structs)
	if outFlag != "" && len(buckets) > 1 {
		return fmt.Errorf("-o cannot be used when %s has structs across multiple build-tag / test groups (%d buckets)", dir, len(buckets))
	}

	// LOCKED through the per-bucket loop. Seed generatedTypes with the full
	// package set first — a tagged-bucket struct may reference an untagged one
	// (same package), so the cross-bucket call must route to direct DecodeFrom.
	// Cleared at the end to avoid leaking into the next call.
	genGlobalsMu.Lock()
	defer genGlobalsMu.Unlock()
	generatedTypes = make(map[string]struct{}, len(structs))
	generatedFields = make(map[string][]FieldInfo, len(structs))
	namedKinds = make(map[string]TypeKind)
	for _, s := range structs {
		generatedTypes[s.Name] = struct{}{}
		generatedFields[s.Name] = s.Fields
	}
	seedNamedKinds(structs)
	multiErrTypes = seedMultiErrTypes(structs)
	cyclicTypes = computeCyclicTypes(structs)
	defer func() {
		generatedTypes = nil
		generatedFields = nil
		namedKinds = nil
		multiErrTypes = nil
		cyclicTypes = nil
	}()

	for _, bk := range bucketKeys(buckets) {
		group := buckets[bk]
		out := outFlag
		if out == "" {
			out = filepath.Join(dir, packageFileName(dir, bk.tag, bk.test))
		}
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		err = generateTo(f, outPkg, out, group)
		cerr := f.Close()
		if err != nil {
			return err
		}
		if cerr != nil {
			return cerr
		}
		cliLog.Info("wrote %s", out)
	}
	return nil
}

// bucketKey identifies one output file: same build constraint + test status.
type bucketKey struct {
	tag  string
	test bool
}

// bucketStructs groups structs by (BuildTag, Test); bucketKeys gives stable
// iteration order.
func bucketStructs(structs []StructInfo) map[bucketKey][]StructInfo {
	out := make(map[bucketKey][]StructInfo, len(structs))
	for _, s := range structs {
		k := bucketKey{tag: s.BuildTag, test: s.Test}
		out[k] = append(out[k], s)
	}
	return out
}

// bucketKeys returns m's keys sorted deterministically (empty tag first, then
// by tag, non-test before test) so `wrote` output stays stable across runs.
func bucketKeys(m map[bucketKey][]StructInfo) []bucketKey {
	keys := slices.Collect(maps.Keys(m))
	slices.SortFunc(keys, func(a, b bucketKey) int {
		if a.tag != b.tag {
			return strings.Compare(a.tag, b.tag)
		}
		// non-test (false) before test (true)
		if a.test != b.test {
			if a.test {
				return 1
			}
			return -1
		}
		return 0
	})
	return keys
}

// packageFileName builds the output filename for one bucket: untagged buckets
// get `<dir>_ggen.go` / `<dir>_ggen_test.go`, tagged buckets `<dir>_<slug>_ggen.go`.
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

// slugifyTag makes a build-constraint expression filename-safe: non-alnum runs
// collapse to single underscores, trimmed (`goexperiment.simd` →
// `goexperiment_simd`, `foo && bar` → `foobufr`).
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
	structs, pkgName, siblings, pkgCyclic, pkgMultiErr, pkgFields, err := parseFile(file, wanted)
	if err != nil {
		return err
	}
	applyCLIFlags(structs)

	outPkg := pkgFlag
	if outPkg == "" {
		outPkg = pkgName
	}

	out := outFlag
	if out == "" {
		// foo_test.go → foo_ggen_test.go; otherwise foo.go → foo_ggen.go.
		if before, ok := strings.CutSuffix(file, "_test.go"); ok {
			out = before + genTestSuffix
		} else {
			out = strings.TrimSuffix(file, ".go") + genSuffix
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
	generatedFields = make(map[string][]FieldInfo, len(pkgFields)+len(structs))
	maps.Copy(generatedFields, pkgFields)
	namedKinds = make(map[string]TypeKind)
	for n := range siblings {
		generatedTypes[n] = struct{}{}
	}
	for _, s := range structs {
		generatedTypes[s.Name] = struct{}{}
		generatedFields[s.Name] = s.Fields
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

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	err = generateTo(f, outPkg, out, structs)
	cerr := f.Close()
	if err != nil {
		return err
	}
	if cerr != nil {
		return cerr
	}
	cliLog.Info("wrote %s", out)
	return nil
}

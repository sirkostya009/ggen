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
	flag.BoolVar(&cliFlags.nullzero, "nullzero", false, "accept explicit JSON null on non-pointer value fields, decoding it to the Go zero value (default: error)")
	flag.BoolVar(&cliFlags.nosortkeys, "nosortkeys", false, "emit struct fields in declaration order (default: sorted by JSON name at codegen time)")
	flag.BoolVar(&cliFlags.usenumber, "usenumber", false, "decode JSON numbers into `any` fields as json.Number instead of float64 (mirrors json.Decoder.UseNumber)")
	flag.BoolVar(&cliFlags.htmlescape, "htmlescape", false, "HTML-safe escape <, >, & in emitted strings (default: literal, matches stdlib jsonv2)")
	flag.BoolVar(&cliDry, "dry", false, "dry run: parse and validate every annotated struct, surface all errors, emit no file")
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

	// -dry routes through the parse-only entry points in check.go. -o
	// (single-file output) and -pkg (package-name override) are dead in
	// that mode — nothing is written — so reject both up front instead
	// of silently dropping them. Parallels the walk's -o rejection.
	if cliDry && (outFlag != "" || pkgFlag != "") {
		cliLog.Fatal(errors.New("-o / -pkg cannot be used with -dry (dry run emits no file)"))
	}

	if strings.HasSuffix(target, "...") {
		if outFlag != "" {
			cliLog.Fatal(errors.New("-o cannot be used with ./... (pattern matches multiple packages; each writes its own output)"))
		}
		// Pattern mode collects errors across packages instead of
		// bailing on the first: a single bad rule in pkg/a shouldn't
		// hide problems in pkg/b. packages.Load failures (no go.mod,
		// invalid pattern, etc.) are fatal — we can't recover.
		act := func(dir string) error { return generateDir(dir, "", "") }
		if cliDry {
			act = checkPackage
		}
		if err := walkPackages(target, act); err != nil {
			cliLog.Fatal(err)
		}
	} else if info, err := os.Stat(target); err != nil {
		cliLog.Fatal(err)
	} else if info.IsDir() {
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
		var err error
		if cliDry {
			err = checkFile(target, positional[1:])
		} else {
			err = generateSingleFile(target, positional[1:], outFlag, pkgFlag)
		}
		if err != nil {
			cliLog.Error(err)
		}
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
		for j := range structs[i].Fields {
			structs[i].Fields[j].MultiErr = structs[i].MultiErr
			structs[i].Fields[j].AllowDups = structs[i].AllowDups
			structs[i].Fields[j].NoValidate = structs[i].NoValidate
			structs[i].Fields[j].UseNumber = structs[i].UseNumber
			structs[i].Fields[j].HTMLEscape = structs[i].HTMLEscape
			// OR, not assign: a struct/CLI opt-in turns every field on, but a
			// per-field json:",nullzero" tag must survive when the struct flag is off.
			structs[i].Fields[j].NullZero = structs[i].Fields[j].NullZero || structs[i].NullZero
		}
	}
}

// walkPackages resolves `pattern` via golang.org/x/tools/go/packages
// and invokes `act` on every matched package's directory. Module-
// scoped, workspace-aware, same dispatch as `go build <pattern>` /
// `go test <pattern>` — never crosses module boundaries. Patterns
// matching no packages produce a load-level diagnostic; the run still
// returns nil so a no-op `ggen ./...` in an empty tree doesn't blow up.
//
// Processing order is post-order over the matched import subgraph:
// a package's `_ggen.go` is on disk before any matched importer
// runs, so the parent's parsePackage reads the child's generated
// methods and routes cross-package field types through direct
// DecodeFrom / AppendJSON calls instead of falling back to
// encoding/json. Dependencies outside the matched set are left
// alone — they're someone else's run.
func walkPackages(pattern string, act func(dir string) error) error {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports,
	}
	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		return err
	}
	// Surface per-package load errors via our logger instead of
	// returning on the first one — a broken-import package shouldn't
	// hide problems in its siblings, and the user wants the full
	// punch list in one run.
	for _, p := range pkgs {
		for _, e := range p.Errors {
			cliLog.Error(fmt.Errorf("%s: %s", p.PkgPath, e))
		}
	}

	matched := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		matched[p.PkgPath] = true
	}
	// Dedup by directory (packages.Load can return base + test
	// variants pointing at the same dir; generateDir already loads
	// both via Tests:true internally, so visit once).
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

// genGlobalsMu protects every globally-shared piece of generator
// state touched by generate() — generatedTypes, generatedAliasKinds,
// the oneof registry, and the smallPool/filePool consumers via the
// renderer functions. walkAndGenerate fans parallel goroutines across
// sibling packages at the same depth; parsing runs concurrently, but
// only one goroutine at a time enters the generate+write section.
// Holding the lock for the whole post-parse phase is fine — generate
// is ~12 ms / package, dwarfed by parsePackage's go/packages.Load.
var genGlobalsMu sync.Mutex

func generateDir(dir, outFlag, pkgFlag string) error {
	cliLog.Debug("parsing package %s", dir)
	// Parsing runs UNLOCKED — go/packages.Load does its own
	// concurrency and doesn't touch our globals.
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

	// LOCKED from here through the per-bucket generate+write loop.
	// Seed generatedTypes with the full package set BEFORE running
	// any bucket — a struct in the tagged bucket may reference one
	// in the untagged bucket (same Go package), so the cross-bucket
	// call must route to the direct DecodeFrom rather than the
	// encoding/json fallback. Clear at the end to avoid leaking
	// state into the next call.
	genGlobalsMu.Lock()
	defer genGlobalsMu.Unlock()
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
		out := outFlag
		if out == "" {
			out = filepath.Join(dir, packageFileName(dir, bk.tag, bk.test))
		}
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		err = generateTo(f, outPkg, group)
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
	structs, pkgName, siblings, err := parseFile(file, wanted)
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
		// Source foo_test.go → foo_ggen_test.go; otherwise foo.go → foo_ggen.go.
		if before, ok := strings.CutSuffix(file, "_test.go"); ok {
			out = before + genTestSuffix
		} else {
			out = strings.TrimSuffix(file, ".go") + genSuffix
		}
	}
	// Seed generatedTypes with every annotated struct in the package
	// (incl. siblings declared in other files) so a cross-file struct
	// reference routes to a direct DecodeFrom call rather than the
	// encoding/json fallback on first run — before sibling _ggen files
	// exist on disk. AliasKind seeding is local to the structs we're
	// actually emitting: primitive-alias casting only matters when the
	// alias's owning file is in the current generation pass.
	genGlobalsMu.Lock()
	defer genGlobalsMu.Unlock()
	generatedTypes = make(map[string]struct{}, len(siblings)+len(structs))
	generatedAliasKinds = make(map[string]TypeKind)
	for n := range siblings {
		generatedTypes[n] = struct{}{}
	}
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

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	err = generateTo(f, outPkg, structs)
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

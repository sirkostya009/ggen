package genfixture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
	"github.com/sirkostya009/ggen/gen"
	"github.com/sirkostya009/ggen/gen/kotlin"
	"github.com/sirkostya009/ggen/gen/swift"
	"github.com/sirkostya009/ggen/gen/ts"
	"github.com/sirkostya009/ggen/gen/valibot"
	"github.com/sirkostya009/ggen/gen/zod"
	"github.com/sirkostya009/ggen/integrationtests/gen/other"
)

type codec[T any] interface {
	ggen.Decoder[T]
	ggen.Marshaler
}

// roundtrip decodes data with the generated decoder and marshals the result.
func roundtrip[T codec[T]](data []byte) ([]byte, error) {
	var zero T
	v, n, err := zero.DecodeFrom(data)
	if err != nil {
		return nil, err
	}
	if n != len(data) {
		return nil, fmt.Errorf("trailing data at %d", n)
	}
	return v.AppendJSON(nil)
}

const (
	fixturePkg = "github.com/sirkostya009/ggen/integrationtests/gen"
	otherPkg   = fixturePkg + "/other"
)

var subjects = []struct {
	pkg, name string
	base      string // a valid input object, or "" for a non-object type
	decode    func([]byte) ([]byte, error)
}{
	{fixturePkg, "Scalars", `{"req":"x"}`, roundtrip[Scalars]},
	{fixturePkg, "Stdlib", `{}`, roundtrip[Stdlib]},
	{fixturePkg, "Containers", `{}`, roundtrip[Containers]},
	{fixturePkg, "Rules", `{"name":"ab"}`, roundtrip[Rules]},
	{fixturePkg, "More", `{}`, roundtrip[More]},
	{fixturePkg, "Node", `{"value":"x"}`, roundtrip[Node]},
	{fixturePkg, "Pair", `{}`, roundtrip[Pair]},
	{fixturePkg, "Count", "", roundtrip[Count]},
	{fixturePkg, "Tags", "", roundtrip[Tags]},
	{otherPkg, "Money", `{"currency":"USD"}`, roundtrip[other.Money]},
}

// probes are the values tried at every field and as every non-object value.
var probes = []string{
	`null`, `true`, `false`,
	`0`, `1`, `-1`, `2`, `4`, `5`, `6`, `10`, `11`, `100`, `101`, `127`, `128`, `255`, `256`, `-129`,
	`2147483648`, `4294967296`, `1.5`, `-0.5`, `1e2`, `1E-2`, `0.1`, `3.5e38`, `9007199254740993`,
	`9223372036854775808`, `18446744073709551615`, `18446744073709551616`, `-9223372036854775809`,
	`""`, `" "`, `"a"`, `"ab"`, `"abc"`, `"ABC"`, `"abcd"`, `"abcdef"`, `"abc123"`, `"AB12CD"`, `"héé"`, `"日本"`,
	`"123"`, `"-1"`, `"1.5"`, `"1e2"`, `" 1"`, `"0x1f"`, `"1f"`, `"yes"`, `"x"`, `"y"`, `"X"`, `"c d"`, `"amz"`, `"a m z"`,
	`"<a_b>"`, `"  ab  "`, `"USD"`, `"Inf"`, `"1_000/3"`, `"0x1.8p1"`, `"010"`, `"1p3"`,
	`"2020-01-01T00:00:00Z"`, `"2020-02-30T00:00:00Z"`, `"2020-02-29T23:59:59.5+01:00"`, `"2021-02-29T00:00:00Z"`,
	`"2020-01-01t00:00:00z"`, `"2020-01-01"`, `"2020-13-01"`, `"2021-02-29"`, `"2020-1-01"`,
	`"1h2m"`, `"-1.5s"`, `"1d"`,
	`"AQID"`, `"AQIDBA=="`, `"AQIDBA"`, `"-_8="`, `"_-8"`, `"MFRGG==="`, `"mfrgg==="`, `"0102"`, `"0x02"`,
	`"1.2.3.4"`, `"01.2.3.4"`, `"::1"`, `"::ffff:1.2.3.4"`, `"fe80::1%eth0"`, `"1.2.3.0/24"`, `"1.2.3.4/33"`,
	`"https://a.b/c"`, `"http://%zz"`, `"a b"`, `"mailto:x"`, `"1/3"`, `"12"`, `"-1.25e3"`,
	`[]`, `[1]`, `[1,2]`, `[1,2,3]`, `[1,2,3,4]`, `[0,-1]`, `[""]`, `[" a "]`, `["a"]`, `["a","b"]`, `[null]`,
	`[[0.5]]`, `[[2]]`, `[[]]`, `[1,"a"]`,
	`{}`, `{"ab":1}`, `{"a":1}`, `{"ab":0}`, `{"a-b":1}`, `{"ab":true}`, `{"ab":"y"}`,
	`{"amount":1,"currency":"USD"}`, `{"amount":1,"currency":"usd"}`, `[{"amount":1,"currency":"EUR"},null]`,
	`{"value":"v"}`, `{"value":""}`, `[{"value":"v"}]`, `[{}]`, `{"left":{"value":"l"},"right":null}`, `{"left":{}}`,
	// the exact width boundaries, and the spellings JavaScript cannot tell
	// apart from an integer
	`1.0`, `-0`, `-0.0`, `1e0`, `1E+2`, `2.0e1`, `32767`, `32768`, `-32768`, `-32769`, `65535`, `65536`,
	`2147483647`, `-2147483648`, `-2147483649`, `4294967295`, `9223372036854775807`, `-9223372036854775808`,
	`1.7976931348623157e308`, `1e400`, `-1e400`, `1e-400`, `1e-46`, `3.4028234663852886e38`, `3.4028235e38`,
	// values the closed sets actually hold
	`"admin"`, `"member"`, `"active"`, `"banned"`,
	// escapes, and text no ASCII-only check would judge the same way
	`"\u0041"`, `"\u00e9"`, `"\ud83d\ude00"`, `"a\/b"`, `"\u0085ab"`, `"\ufeffab"`,
	`"\u039f\u0394\u039f\u03a3"`, `"\u0130"`, `"\u00df"`,
	`"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`,
	// deeper nesting than a field's own level
	`[[[1]]]`, `{"a":{"b":{"c":1}}}`, `[{"value":"v","children":[{"value":"w"}]}]`,
}

// probeCase is one probe at one place, seen from one side: input is judged
// by the input schema, output by the output schema.
type probeCase struct {
	probe  string
	t      *gen.Type // the type at the probed place; nil for an unknown key
	output bool
	goOK   bool // Go's own verdict on the case
}

// divergences are the places where a schema cannot agree with Go. Each must
// explain at least one disagreement.
var divergences = []struct {
	why   string
	match func(c probeCase) bool
}{
	{"JSON.parse reads a number JavaScript cannot tell from an integer, like 1e2 or 1.0", func(c probeCase) bool {
		// The schema sees the parsed double, so any number whose value is a
		// whole one passes an integer check whatever its spelling was.
		return !c.goOK && !c.output && integer(c.t) && jsWholeNumber(c.probe)
	}},
	{"JSON.parse rounds integers beyond 2^53, which Go reads exactly", func(c probeCase) bool {
		n, ok := new(big.Int).SetString(c.probe, 10)
		return ok && integer(c.t) && n.CmpAbs(big.NewInt(1<<53)) >= 0
	}},
	{"an enum type is a closed set in a schema; Go accepts any value of the underlying type", func(c probeCase) bool {
		if !c.goOK || c.t == nil || c.t.Enum == nil || c.t.Enum.Decl == nil {
			return false // only Go accepting, or emitting, what the closed set refuses
		}
		if c.t.Wire == gen.WireString != strings.HasPrefix(c.probe, `"`) {
			return false // a probe of another wire shape is a real disagreement
		}
		return !slices.ContainsFunc(c.t.Enum.Values, func(v gen.EnumValue) bool { return v.Value == c.probe })
	}},
	{"a converter's own checks run only in Go", func(c probeCase) bool {
		return !c.goOK && !c.output && c.t != nil && len(c.t.OneOf) > 0 && strings.HasPrefix(c.probe, `"`)
	}},
	{"JSON.parse reads a number too large for a double as Infinity, which no schema can tell from a finite one", func(c probeCase) bool {
		f, err := strconv.ParseFloat(c.probe, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return false
		}
		return math.IsInf(f, 0) && c.t != nil &&
			(c.t.Wire == gen.WireNumber || c.t.Wire == gen.WireInteger || c.t.Wire == gen.WireAny)
	}},
}

// jsWholeNumber reports whether a JSON number literal parses to a whole
// double: what an integer schema sees after JSON.parse.
func jsWholeNumber(probe string) bool {
	f, err := strconv.ParseFloat(probe, 64)
	return err == nil && !math.IsInf(f, 0) && f == math.Trunc(f)
}

func integer(t *gen.Type) bool {
	if t == nil {
		return false
	}
	if t.Wire == gen.WireInteger {
		return true
	}
	for _, o := range t.OneOf {
		if o.Wire == gen.WireInteger {
			return true
		}
	}
	return t.Wire == gen.WireArray && integer(t.Elem)
}

func render(t *testing.T, set *gen.Set, dir string) {
	t.Helper()
	const color = fixturePkg + ".Color"
	emitters := map[string]func(runtime *gen.File) gen.Emitter{
		"zod": func(runtime *gen.File) gen.Emitter {
			return zod.New(zod.Runtime(runtime), zod.ImportExt(".ts"),
				zod.Func("IsEven", `.refine((x) => x % 2 === 0, "must be even")`),
				zod.External(color, `z.string().regex(/^[+-]?\d+$/)`))
		},
		"valibot": func(runtime *gen.File) gen.Emitter {
			return valibot.New(valibot.Runtime(runtime), valibot.ImportExt(".ts"),
				valibot.Func("IsEven", `v.check((x) => x % 2 === 0, "must be even")`),
				valibot.External(color, `v.pipe(v.string(), v.regex(/^[+-]?\d+$/))`))
		},
	}
	for lib, e := range emitters {
		out := gen.NewOutput(filepath.Join(dir, lib))
		f := out.File("schemas.ts", e(out.File("ggen.ts", ts.Runtime(""))))
		for _, d := range set.Types() {
			if d.Pkg.Path == fixturePkg || d.Pkg.Path == otherPkg {
				f.Place(d.In(), d.Name+"Input")
				f.Place(d.Out(), d.Name)
			}
		}
		if err := out.Write(); err != nil {
			t.Fatal(err)
		}
		for _, i := range out.Report().Issues {
			// A micro or nanosecond Unix time is the one thing JavaScript
			// cannot hold; the lane's divergences cover what that costs.
			if strings.Contains(i.Msg, "integers JavaScript holds exactly") {
				continue
			}
			t.Errorf("%s report: %s", lib, i)
		}
	}
}

// TestDifferential runs every probe through the generated Go decoder and the
// zod and valibot input schemas, requiring the same verdict. What both accept,
// Go must read from the schema's parsed value as it read the probe, and
// whatever Go accepts must marshal to JSON the output schemas accept. It needs
// node on PATH and `npm ci` at the repo root.
func TestDifferential(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	modules := nodeModules()
	if err != nil || modules == "" {
		t.Skip("needs node on PATH and `npm ci` at the repo root")
	}
	set, err := gen.Load(".", "./other")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	render(t, set, dir)
	if err := os.Symlink(modules, filepath.Join(dir, "node_modules")); err != nil {
		t.Fatal(err)
	}
	cfg := `{"compilerOptions":{"strict":true,"noEmit":true,"target":"ES2022","lib":["ES2022","DOM"],"module":"ESNext","moduleResolution":"Bundler","allowImportingTsExtensions":true,"skipLibCheck":true},"include":["*/*.ts"]}`
	write(t, filepath.Join(dir, "tsconfig.json"), []byte(cfg))
	if b, err := exec.CommandContext(t.Context(), filepath.Join(modules, ".bin", "tsc"), "-p", dir).CombinedOutput(); err != nil {
		t.Fatalf("tsc: %v\n%s", err, b)
	}

	cases := probeCases(t, set)
	libs := []string{"zod", "valibot"}
	var js []jsCase
	for _, c := range cases {
		for _, lib := range libs {
			file := filepath.Join(dir, lib, "schemas.ts")
			js = append(js, jsCase{lib, file, c.schema + "Input", c.input})
			if c.err == nil {
				js = append(js, jsCase{lib, file, c.schema, string(c.output)})
			}
		}
	}
	results := runNode(t, node, dir, js)

	explained := make([]int, len(divergences))
	diverges := func(pc probeCase) bool {
		for i, d := range divergences {
			if d.match(pc) {
				explained[i]++
				return true
			}
		}
		return false
	}
	k := 0
	for _, c := range cases {
		for _, lib := range libs {
			in := results[k]
			k++
			switch {
			case in.OK != (c.err == nil):
				if !diverges(probeCase{c.probe, c.in, false, c.err == nil}) {
					t.Errorf("%s input %s: go err=%v, %s ok=%v %s", lib, c.name, c.err, lib, in.OK, oneLine(in.Error))
				}
			case in.OK && c.transforms:
				if msg := sameTransforms(c.output, in.Output); msg != "" {
					t.Errorf("%s input %s: %s", lib, c.name, msg)
				}
			}
			if c.err != nil {
				continue
			}
			if in.OK {
				pc := probeCase{c.probe, c.in, false, c.err == nil}
				switch again, err := decoders[c.schema]([]byte(in.Output)); {
				case err != nil:
					if !diverges(pc) {
						t.Errorf("%s input %s: Go rejects %s's parsed %s: %v", lib, c.name, lib, in.Output, err)
					}
				case !sameJSON(again, c.output):
					if !diverges(pc) {
						t.Errorf("%s input %s: through %s Go reads %s, directly %s", lib, c.name, lib, again, c.output)
					}
				}
			}
			if out := results[k]; !out.OK && !diverges(probeCase{c.probe, c.out, true, c.err == nil}) {
				t.Errorf("%s output %s: %s rejects %s: %s", lib, c.name, lib, c.output, oneLine(out.Error))
			}
			k++
		}
	}
	for i, d := range divergences {
		if explained[i] == 0 {
			t.Errorf("divergence %q explains no disagreement", d.why)
		}
	}
}

type goCase struct {
	name, schema, probe, input string
	in, out                    *gen.Type
	transforms                 bool
	output                     []byte // Go's marshaled value when err is nil
	err                        error
}

// probeCases builds every probe case and runs it through the Go decoder.
func probeCases(t *testing.T, set *gen.Set) []goCase {
	t.Helper()
	var cases []goCase
	for _, s := range subjects {
		d := set.Type(s.pkg, s.name)
		in, out := d.In(), d.Out()
		if s.base == "" {
			for _, p := range probes {
				cases = append(cases, goCase{name: s.name + "=" + p, schema: s.name, probe: p, input: p, in: in.Alias, out: out.Alias})
			}
		} else {
			var base map[string]json.RawMessage
			if err := json.Unmarshal([]byte(s.base), &base); err != nil {
				t.Fatal(err)
			}
			for _, f := range append([]*gen.Field{{JSONName: "zzz"}}, in.Fields...) {
				var outType *gen.Type
				if of := out.Field(f.JSONName); of != nil {
					outType = of.Type
				}
				transforms := hasTransform(f.Type)
				for _, p := range probes {
					cases = append(cases, goCase{
						name: s.name + "." + f.JSONName + "=" + p, schema: s.name, probe: p, input: withField(base, f.JSONName, p),
						in: f.Type, out: outType, transforms: transforms,
					})
				}
			}
		}
	}
	for i := range cases {
		cases[i].output, cases[i].err = decoders[cases[i].schema]([]byte(cases[i].input))
	}
	return cases
}

var decoders = func() map[string]func([]byte) ([]byte, error) {
	m := map[string]func([]byte) ([]byte, error){}
	for _, s := range subjects {
		m[s.name] = s.decode
	}
	return m
}()

func withField(base map[string]json.RawMessage, name, value string) string {
	var b bytes.Buffer
	b.WriteByte('{')
	for _, k := range slices.Sorted(maps.Keys(base)) {
		v := base[k]
		if k != name {
			fmt.Fprintf(&b, "%q:%s,", k, v)
		}
	}
	fmt.Fprintf(&b, "%q:%s}", name, value)
	return b.String()
}

// hasTransform reports whether t, or anything inside it, rewrites its value.
func hasTransform(t *gen.Type) bool {
	if t == nil {
		return false
	}
	if slices.ContainsFunc(t.Rules, func(r gen.Rule) bool { return r.Transform }) {
		return true
	}
	if hasTransform(t.Elem) || hasTransform(t.Key) {
		return true
	}
	return slices.ContainsFunc(t.OneOf, hasTransform)
}

// sameTransforms compares the values a schema's transforms produced with the
// ones Go decoded and marshaled. The comparison is structural: printing them
// would make the string "1" and the number 1 look equal.
func sameTransforms(goOut []byte, jsOut string) string {
	var g, j map[string]any
	if json.Unmarshal(goOut, &g) != nil || json.Unmarshal([]byte(jsOut), &j) != nil {
		return ""
	}
	for k, jv := range j {
		if !reflect.DeepEqual(jv, g[k]) {
			return fmt.Sprintf("%s: go %#v, js %#v", k, g[k], jv)
		}
	}
	return ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

type jsCase struct {
	Lib, File, Schema, Input string
}

type jsResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
	Error  string `json:"error"`
}

func runNode(t *testing.T, node, dir string, cases []jsCase) []jsResult {
	t.Helper()
	payload, _ := json.Marshal(cases)
	in := filepath.Join(dir, "cases.json")
	write(t, in, payload)
	script := `
import * as v from "valibot";
import { readFileSync } from "node:fs";
const cases = JSON.parse(readFileSync(process.argv[2], "utf8"));
const mods = {};
const out = [];
for (const c of cases) {
  const mod = (mods[c.File] ??= await import("file://" + c.File));
  const input = JSON.parse(c.Input);
  if (c.Lib === "zod") {
    const r = mod[c.Schema].safeParse(input);
    out.push(r.success ? { ok: true, output: JSON.stringify(r.data) } : { ok: false, error: r.error.message });
  } else {
    const r = v.safeParse(mod[c.Schema], input);
    out.push(r.success ? { ok: true, output: JSON.stringify(r.output) } : { ok: false, error: v.summarize(r.issues) });
  }
}
process.stdout.write(JSON.stringify(out));
`
	runner := filepath.Join(dir, "run.mjs")
	write(t, runner, []byte(script))
	var stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), node, runner, in)
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.Bytes())
	}
	var res []jsResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("%v: %.200s", err, raw)
	}
	if len(res) != len(cases) {
		t.Fatalf("node returned %d results for %d cases", len(res), len(cases))
	}
	return res
}

// swiftDivergences are the places where Swift types cannot hold what Go
// accepts. Each must explain at least one disagreement.
var swiftDivergences = []laneDivergence{
	{"JSONDecoder has no catch-all map, so collected keys are lost", func(c goCase) bool {
		return c.in == nil && c.schema == "Containers"
	}},
	{"Decimal stops near 3.4e38, where json.Number and big.Int keep the literal", func(c goCase) bool {
		if c.in == nil || c.in.Go != gen.Number && c.in.Go != gen.BigInt {
			return false
		}
		f, err := strconv.ParseFloat(c.probe, 64)
		return err == nil && math.Abs(f) > 3.4028236e38
	}},
	{"swift-foundation rejects a number literal outside the target's float range, underflow included", func(c goCase) bool {
		bits := 64
		if c.in != nil && c.in.Go == gen.Float32 {
			bits = 32
		}
		f, err := strconv.ParseFloat(c.probe, bits)
		return errors.Is(err, strconv.ErrRange) || err == nil && f == 0 && strings.ContainsAny(c.probe, "123456789")
	}},
}

// TestDifferentialSwift decodes every value Go emits with the Swift output
// types, and round-trips every input Go accepts through the Swift input
// types: Go must decode what Swift encodes to the same value. It needs
// GGEN_SWIFTC, the path to swiftc.
func TestDifferentialSwift(t *testing.T) {
	t.Parallel()
	swiftc := os.Getenv("GGEN_SWIFTC")
	if swiftc == "" {
		t.Skip("set GGEN_SWIFTC to a swiftc binary")
	}
	set, err := gen.Load(".", "./other")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := gen.NewOutput(dir)
	e := swift.New("Fixture", swift.Runtime(out.File("JSON.swift", swift.NewRuntime("Fixture"))),
		swift.External(fixturePkg+".Color", "String"))
	f := out.File("Types.swift", e)
	types := map[string]string{} // placed name → the Swift type to decode
	for _, d := range set.Types() {
		if d.Pkg.Path == fixturePkg || d.Pkg.Path == otherPkg {
			for _, p := range []struct {
				name  string
				shape *gen.Shape
			}{{d.Name + "Input", d.In()}, {d.Name, d.Out()}} {
				name, s := p.name, p.shape
				f.Place(s, name)
				types[name] = name
				if s.Alias != nil && s.Alias.Nullable {
					types[name] += "?" // a typealias leaves null to its uses
				}
			}
		}
	}
	if err := out.Write(); err != nil {
		t.Fatal(err)
	}

	var main strings.Builder
	main.WriteString(`import Foundation

func run<T: Codable>(_ type: T.Type, _ json: String) -> [String] {
    do {
        let v = try JSONDecoder().decode(type, from: Data(json.utf8))
        return ["ok", String(decoding: try JSONEncoder().encode(v), as: UTF8.self)]
    } catch {
        return ["err", String(describing: error)]
    }
}

let cases = try JSONDecoder().decode([[String]].self, from: FileManager.default.contents(atPath: CommandLine.arguments[1])!)
var results: [[String]] = []
for c in cases {
    switch c[0] {
`)
	for _, name := range slices.Sorted(maps.Keys(types)) {
		fmt.Fprintf(&main, "    case %q: results.append(run(%s.self, c[1]))\n", name, types[name])
	}
	main.WriteString("    default: results.append([\"err\", \"unknown type\"])\n    }\n}\n")
	main.WriteString("print(String(decoding: try JSONEncoder().encode(results), as: UTF8.self))\n")
	write(t, filepath.Join(dir, "main.swift"), []byte(main.String()))
	build := exec.CommandContext(
		t.Context(),
		swiftc,
		"-module-cache-path",
		filepath.Join(dir, "cache"),
		"-module-name",
		"Fixture",
		"-o",
		filepath.Join(dir, "app"),
		filepath.Join(dir, "main.swift"),
		filepath.Join(dir, "Types.swift"),
		filepath.Join(dir, "JSON.swift"),
	)
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("swiftc: %v\n%s", err, b)
	}

	checkTypedLane(t, "Swift", set, dir, swiftDivergences, func(cases string) ([]byte, error) {
		return exec.CommandContext(t.Context(), filepath.Join(dir, "app"), cases).Output()
	})
}

// kotlinDivergences are the places where Kotlin classes cannot hold what Go
// accepts. Each must explain at least one disagreement.
var kotlinDivergences = []laneDivergence{
	{"kotlinx re-encodes an integer beyond Long and ULong as a double", func(c goCase) bool {
		n, ok := new(big.Int).SetString(c.probe, 10)
		return ok && c.in != nil && c.in.Go == gen.BigInt && (n.Sign() < 0 && !n.IsInt64() || !n.IsUint64())
	}},
	{"kotlinx rejects a literal that overflows Double, as a non-finite value", func(c goCase) bool {
		_, err := strconv.ParseFloat(c.probe, 64)
		return errors.Is(err, strconv.ErrRange)
	}},
	{"kotlinx.serialization has no catch-all map, so collected keys are lost", func(c goCase) bool {
		return c.in == nil && c.schema == "Containers"
	}},
}

// TestDifferentialKotlin is TestDifferentialSwift for kotlinx.serialization
// data classes. It needs GGEN_KOTLIN_HOME, laid out as gen/kotlin's
// TestRuntime expects.
func TestDifferentialKotlin(t *testing.T) {
	t.Parallel()
	home := os.Getenv("GGEN_KOTLIN_HOME")
	jres, _ := filepath.Glob(filepath.Join(home, "jdk*"))
	if home == "" || len(jres) == 0 {
		t.Skip("set GGEN_KOTLIN_HOME to a directory with kotlinc, a JRE and the kotlinx.serialization jars")
	}
	set, err := gen.Load(".", "./other")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := gen.NewOutput(dir)
	f := out.File("Types.kt", kotlin.New("fixture", kotlin.External(fixturePkg+".Color", "String")))
	var names []string
	for _, d := range set.Types() {
		if d.Pkg.Path == fixturePkg || d.Pkg.Path == otherPkg {
			f.Place(d.In(), d.Name+"Input")
			f.Place(d.Out(), d.Name)
			names = append(names, d.Name+"Input", d.Name)
		}
	}
	if err := out.Write(); err != nil {
		t.Fatal(err)
	}

	var main strings.Builder
	main.WriteString(`import fixture.*
import java.io.File
import kotlinx.serialization.KSerializer
import kotlinx.serialization.json.*
import kotlinx.serialization.serializer

fun <T> run(s: KSerializer<T>, json: String): JsonArray = try {
    JsonArray(listOf(JsonPrimitive("ok"), JsonPrimitive(Json.encodeToString(s, Json.decodeFromString(s, json)))))
} catch (e: Exception) {
    JsonArray(listOf(JsonPrimitive("err"), JsonPrimitive(e.message ?: e.toString())))
}

fun main(args: Array<String>) {
    val out = Json.parseToJsonElement(File(args[0]).readText()).jsonArray.map { c ->
        val (type, json) = c.jsonArray.map { it.jsonPrimitive.content }
        when (type) {
`)
	for _, n := range names {
		fmt.Fprintf(&main, "            %q -> run(serializer<%s>(), json)\n", n, n)
	}
	main.WriteString(
		"            else -> JsonArray(listOf(JsonPrimitive(\"err\"), JsonPrimitive(\"unknown type\")))\n        }\n    }\n    println(JsonArray(out))\n}\n",
	)
	write(t, filepath.Join(dir, "Main.kt"), []byte(main.String()))

	env := append(os.Environ(), "JAVA_HOME="+jres[0], "PATH="+filepath.Join(jres[0], "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	cp := filepath.Join(home, "kotlinx-serialization-core-jvm.jar") + ":" + filepath.Join(home, "kotlinx-serialization-json-jvm.jar")
	build := exec.CommandContext(t.Context(), filepath.Join(home, "kotlinc", "bin", "kotlinc"),
		"-Xplugin="+filepath.Join(home, "kotlinc", "lib", "kotlinx-serialization-compiler-plugin.jar"),
		"-cp", cp, "-d", filepath.Join(dir, "classes"), filepath.Join(dir, "Main.kt"), filepath.Join(dir, "Types.kt"))
	build.Env = env
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("kotlinc: %v\n%s", err, b)
	}
	checkTypedLane(t, "Kotlin", set, dir, kotlinDivergences, func(cases string) ([]byte, error) {
		cmd := exec.CommandContext(t.Context(), filepath.Join(jres[0], "bin", "java"), "-Dstdout.encoding=UTF-8", "-cp",
			filepath.Join(dir, "classes")+":"+cp+":"+filepath.Join(home, "kotlinc", "lib", "kotlin-stdlib.jar"), "MainKt", cases)
		cmd.Env = env
		return cmd.Output()
	})
}

// laneDivergence is a place where a typed language cannot hold what Go
// accepts.
type laneDivergence struct {
	why   string
	match func(c goCase) bool
}

// checkTypedLane runs every case Go accepts through run, a program taking a
// JSON file of [type, json] pairs and printing [status, json] pairs: the
// output type must decode Go's output, and Go must read what the input type
// re-encodes as the value it read directly.
func checkTypedLane(t *testing.T, lang string, set *gen.Set, dir string, divergences []laneDivergence, run func(cases string) ([]byte, error)) {
	t.Helper()
	var accepted []goCase
	var input [][]string
	for _, c := range probeCases(t, set) {
		if c.err == nil {
			accepted = append(accepted, c)
			input = append(input, []string{c.schema + "Input", c.input}, []string{c.schema, string(c.output)})
		}
	}
	payload, _ := json.Marshal(input)
	casesFile := filepath.Join(dir, "cases.json")
	write(t, casesFile, payload)
	raw, err := run(casesFile)
	if err != nil {
		t.Fatalf("%s program: %v", lang, err)
	}
	var results [][]string
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("%v: %.200s", err, raw)
	}
	if len(results) != len(input) {
		t.Fatalf("%s returned %d results for %d cases", lang, len(results), len(input))
	}

	explained := make([]int, len(divergences))
	diverges := func(c goCase) bool {
		for i, d := range divergences {
			if d.match(c) {
				explained[i]++
				return true
			}
		}
		return false
	}
	for i, c := range accepted {
		in, out := results[2*i], results[2*i+1]
		switch {
		case out[0] != "ok":
			if !diverges(c) {
				t.Errorf("output %s: %s rejects %s: %.300s", c.name, lang, c.output, out[1])
			}
		case !sameValue([]byte(out[1]), c.output) && !diverges(c):
			// What the output type re-encodes is what a client would send
			// back, so it has to carry the value Go emitted. A key holding
			// null is the one allowed difference: Swift and Kotlin leave a nil
			// property out, and Go reads an absent key as that same zero.
			t.Errorf("output %s: %s re-encodes %s as %s", c.name, lang, c.output, out[1])
		}
		if in[0] != "ok" {
			if !diverges(c) {
				t.Errorf("input %s: %s rejects what Go accepts: %.300s", c.name, lang, in[1])
			}
			continue
		}
		again, err := decoders[c.schema]([]byte(in[1]))
		switch {
		case err != nil:
			if !diverges(c) {
				t.Errorf("input %s: Go rejects %s's %s: %v", c.name, lang, in[1], err)
			}
		case !sameJSON(again, c.output) && !diverges(c):
			t.Errorf("input %s: through %s Go reads %s, directly %s", c.name, lang, again, c.output)
		}
	}
	for i, d := range divergences {
		if explained[i] == 0 {
			t.Errorf("%s divergence %q explains no disagreement", lang, d.why)
		}
	}
}

// write fails the test instead of letting a missing file look like a
// disagreement later.
func write(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// sameValue is sameJSON reading an object key holding null as absent, on both
// sides: a value a language left out is still a disagreement, a null it left
// out is not.
func sameValue(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(dropNulls(x), dropNulls(y))
}

func dropNulls(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			if e == nil {
				delete(v, k)
			} else {
				v[k] = dropNulls(e)
			}
		}
	case []any:
		for i, e := range v {
			v[i] = dropNulls(e)
		}
	}
	return v
}

// tsDivergences are the places where a TypeScript type cannot describe a value
// Go accepts. Each must explain at least one disagreement.
var tsDivergences = []laneDivergence{
	{"an enum type is the script's assertion; the decoder takes any value of the underlying type", func(c goCase) bool {
		return outsideEnum(c.in, c.probe) || outsideEnum(c.out, c.probe)
	}},
}

// outsideEnum reports whether t is an enum type the probe is not a member of.
func outsideEnum(t *gen.Type, probe string) bool {
	if t == nil || t.Enum == nil {
		return false
	}
	if t.Enum.Zero && (probe == `""` || probe == "0") {
		return false
	}
	return !slices.ContainsFunc(t.Enum.Values, func(v gen.EnumValue) bool { return v.Value == probe })
}

// TestDifferentialTS type-checks every value Go accepts against the emitted
// TypeScript types: an accepted input must be assignable to the input type and
// what Go emits to the output type. It needs GGEN_NODE_MODULES, a node_modules
// with typescript.
func TestDifferentialTS(t *testing.T) {
	t.Parallel()
	modules := nodeModules()
	if modules == "" {
		t.Skip("needs `npm ci` at the repo root, or GGEN_NODE_MODULES")
	}
	set, err := gen.Load(".", "./other")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := gen.NewOutput(dir)
	f := out.File("types.ts", ts.New(ts.ImportExt(".ts"), ts.External(fixturePkg+".Color", "string")))
	var names []string
	for _, d := range set.Types() {
		if d.Pkg.Path == fixturePkg || d.Pkg.Path == otherPkg {
			f.Place(d.In(), d.Name+"Input")
			f.Place(d.Out(), d.Name)
			names = append(names, d.Name+"Input", d.Name)
		}
	}
	if err := out.Write(); err != nil {
		t.Fatal(err)
	}
	for _, i := range out.Report().Issues {
		t.Errorf("ts report: %s", i)
	}

	// The value is assigned as a literal, so a tuple, an enum member and a
	// string format narrow to what the type asks for. That also runs
	// TypeScript's excess-property check, which is why a probe holding a key
	// the input type does not declare is left out: an unknown key is the
	// decoder's business, not the type's.
	declared := map[string]map[string]struct{}{}
	for _, d := range set.Types() {
		in := d.In()
		if in.Rest != nil {
			continue // a catch-all takes every key
		}
		keys := map[string]struct{}{}
		for _, f := range in.Fields {
			keys[f.JSONName] = struct{}{}
		}
		declared[d.Name] = keys
	}
	var body strings.Builder
	fmt.Fprintf(&body, "import type { %s } from \"./types.ts\";\n", strings.Join(names, ", "))
	at := map[int]string{} // line number → what it asserts
	line := 2
	add := func(what, typ, value string) {
		fmt.Fprintf(&body, "const c%d: %s = %s;\n", line, typ, value)
		at[line] = what
		line++
	}
	var cases []goCase
	for _, c := range probeCases(t, set) {
		if c.err != nil {
			continue
		}
		cases = append(cases, c)
		if onlyDeclaredKeys(c.input, declared[c.schema]) {
			add("input "+c.name, c.schema+"Input", c.input)
		}
		add("output "+c.name, c.schema, string(c.output))
	}
	write(t, filepath.Join(dir, "cases.ts"), []byte(body.String()))
	write(t, filepath.Join(dir, "tsconfig.json"), []byte(
		`{"compilerOptions":{"strict":true,"noEmit":true,"target":"ES2022","module":"ESNext","moduleResolution":"Bundler","allowImportingTsExtensions":true,"skipLibCheck":true},"include":["*.ts"]}`,
	))
	if err := os.Symlink(modules, filepath.Join(dir, "node_modules")); err != nil {
		t.Fatal(err)
	}
	raw, _ := exec.CommandContext(t.Context(), filepath.Join(modules, ".bin", "tsc"), "-p", dir).CombinedOutput()

	explained := make([]int, len(tsDivergences))
	byName := map[string]goCase{}
	for _, c := range cases {
		byName["input "+c.name], byName["output "+c.name] = c, c
	}
	for l := range strings.SplitSeq(strings.ReplaceAll(string(raw), "\n  ", " "), "\n") {
		m := tscError.FindStringSubmatch(l)
		if m == nil {
			if strings.Contains(l, "error TS") {
				t.Errorf("tsc: %s", l)
			}
			continue
		}
		n, _ := strconv.Atoi(m[1])
		what, ok := at[n]
		if !ok {
			t.Errorf("tsc: %s", l)
			continue
		}
		c := byName[what]
		diverged := false
		for i, d := range tsDivergences {
			if d.match(c) {
				explained[i]++
				diverged = true
				break
			}
		}
		if !diverged {
			t.Errorf("%s: %s", what, m[2])
		}
	}
	for i, d := range tsDivergences {
		if explained[i] == 0 {
			t.Errorf("TypeScript divergence %q explains no disagreement", d.why)
		}
	}
}

var tscError = regexp.MustCompile(`cases\.ts\((\d+),\d+\): (error TS.*)$`)

// onlyDeclaredKeys reports whether every key of a JSON object is one keys
// holds. A nil keys, or a value that is not an object, is every key.
func onlyDeclaredKeys(input string, keys map[string]struct{}) bool {
	var obj map[string]json.RawMessage
	if keys == nil || json.Unmarshal([]byte(input), &obj) != nil {
		return true
	}
	for k := range obj {
		if _, ok := keys[k]; !ok {
			return false
		}
	}
	return true
}

// TestUnloaded pins that a ggen-generated type reached in a package no pattern
// matched is named, so a script can load it, and lowered as an external type.
func TestUnloaded(t *testing.T) {
	t.Parallel()
	set, err := gen.Load(".")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{otherPkg + ".Money"}; !slices.Equal(set.Unloaded(), want) {
		t.Fatalf("Unloaded = %v, want %v", set.Unloaded(), want)
	}
	money := set.Type(fixturePkg, "Containers").Out().Field("money")
	if money == nil || !money.Type.External || money.Type.Wire != gen.WireObject {
		t.Errorf("an unloaded type is external with its wire shape: %#v", money)
	}
}

// nodeModules is the install the JavaScript lanes run against: the one
// GGEN_NODE_MODULES names, else the workspace at the repo root. It returns ""
// when there is neither.
func nodeModules() string {
	path := os.Getenv("GGEN_NODE_MODULES")
	if path == "" {
		path, _ = filepath.Abs("../../node_modules")
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

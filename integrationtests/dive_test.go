package integrationtests

//go:generate ../ggen $GOFILE

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
)

// DiveStruct exercises inner: per-element validation, length checks that
// count runes (Unicode characters) rather than bytes, and a custom @Func
// validator.
//
//ggen:generate
type DiveStruct struct {
	// slice length 1..3, each element 2..10 runes
	Tags []string `json:"tags" pipe:"minlen=1 maxlen=3 inner:(minrunes=2 maxrunes=10)"`
	// rune bounds so "héllo" (5 runes, 6 bytes) passes
	Title string `json:"title" pipe:"minrunes=1 maxrunes=5"`
	// each score 0..100
	Scores []int `json:"scores" pipe:"inner:(gte=0 lte=100)"`
	Count  int   `json:"count"  pipe:"@EvenOnly"`
}

// EvenOnly is the custom validator for DiveStruct.Count.
func EvenOnly(n int) error {
	if n%2 != 0 {
		return fmt.Errorf("%d is not even", n)
	}
	return nil
}

func TestDive_valid(t *testing.T) {
	t.Parallel()
	input := `{"tags":["go","rust"],"title":"héllo","scores":[10,50,100]}`
	got, _, err := (DiveStruct{}).DecodeFrom([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Tags) != 2 {
		t.Errorf("Tags len = %d, want 2", len(got.Tags))
	}
	if got.Title != "héllo" {
		t.Errorf("Title = %q", got.Title)
	}
}

func TestDive_tagTooShort(t *testing.T) {
	t.Parallel()
	// "x" is 1 rune, below minrunes=2, so the element fails.
	input := `{"tags":["ok","x"],"title":"hi","scores":[]}`
	_, _, err := (DiveStruct{}).DecodeFrom([]byte(input))
	if err == nil {
		t.Fatal("expected error for too-short tag element")
	}
	if !strings.Contains(err.Error(), "below minimum") {
		t.Errorf("error = %q, want 'below minimum'", err.Error())
	}
}

func TestDive_scoreOutOfRange(t *testing.T) {
	t.Parallel()
	// 101 violates lte=100 per element
	input := `{"tags":["ok"],"title":"hi","scores":[50,101]}`
	_, _, err := (DiveStruct{}).DecodeFrom([]byte(input))
	if err == nil {
		t.Fatal("expected error for out-of-range score")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("error = %q, want 'exceeds maximum'", err.Error())
	}
}

func TestRuneCount_byteVsRune(t *testing.T) {
	t.Parallel()
	// "héllo" is 5 runes but 6 bytes. With rune-count bounds (1..5), it passes.
	input := `{"tags":["ok"],"title":"héllo","scores":[]}`
	if _, _, err := (DiveStruct{}).DecodeFrom([]byte(input)); err != nil {
		t.Errorf("5-rune multibyte title should pass: %v", err)
	}

	// 6 runes should fail maxrunes=5
	input2 := `{"tags":["ok"],"title":"héllos","scores":[]}`
	if _, _, err := (DiveStruct{}).DecodeFrom([]byte(input2)); err == nil {
		t.Error("6-rune title should fail maxrunes=5")
	}
}

func TestTags_sliceLengthVsElement(t *testing.T) {
	t.Parallel()
	// 4 tags exceed maxlen=3, so the slice itself fails.
	input := `{"tags":["aa","bb","cc","dd"],"title":"hi","scores":[]}`
	_, _, err := (DiveStruct{}).DecodeFrom([]byte(input))
	if err == nil {
		t.Fatal("expected slice-length error")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("error = %q", err.Error())
	}
}

// CustomDiveStruct exercises @Func resolution at container-aware sites: slice
// element (inner:), map key (keys:), and a pointer field whose func takes *T.
//
//ggen:generate
type CustomDiveStruct struct {
	Tags   []string       `json:"tags"   pipe:"inner:@NotBlank"`
	Trim   []string       `json:"trim"   pipe:"inner:@TrimSpace"`
	Lookup map[string]int `json:"lookup" pipe:"keys:@KeyShape"`
	Mixed  map[string]int `json:"mixed"  pipe:"keys:@LowerKey"`
	Ptr    *int           `json:"ptr"    pipe:"@PointerCheck"`
}

// NotBlank is invoked once per slice element via `inner:@NotBlank`.
func NotBlank(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("blank element")
	}
	return nil
}

// TrimSpace is a pure mod called per slice element.
func TrimSpace(s string) string { return strings.TrimSpace(s) }

// KeyShape is invoked once per map key via `keys:@KeyShape`.
func KeyShape(s string) error {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return fmt.Errorf("bad key %q", s)
		}
	}
	return nil
}

// LowerKey is a pure mod called per map key before insertion.
func LowerKey(s string) string { return strings.ToLower(s) }

// PointerCheck verifies a pointer field passes *T, not T.
func PointerCheck(p *int) error {
	if p != nil && *p < 0 {
		return errors.New("negative")
	}
	return nil
}

// inner:@NotBlank rejects a blank slice element.
func TestCustomDive_sliceValidator(t *testing.T) {
	t.Parallel()
	in := []byte(`{"tags":["ok","   "],"trim":[],"lookup":{},"mixed":{},"ptr":null}`)
	_, _, err := (CustomDiveStruct{}).DecodeFrom(in)
	if err == nil {
		t.Fatal("expected blank-element error")
	}
	if !strings.Contains(err.Error(), "blank element") {
		t.Errorf("error = %q, want NotBlank message", err.Error())
	}
}

// inner:@TrimSpace runs on each slice element.
func TestCustomDive_sliceMod(t *testing.T) {
	t.Parallel()
	in := []byte(`{"tags":["ok"],"trim":["  hi  ","  bye"],"lookup":{},"mixed":{},"ptr":null}`)
	got, _, err := (CustomDiveStruct{}).DecodeFrom(in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []string{"hi", "bye"}
	for i, v := range got.Trim {
		if v != want[i] {
			t.Errorf("Trim[%d] = %q, want %q", i, v, want[i])
		}
	}
}

// keys:@KeyShape rejects a key failing the predicate.
func TestCustomDive_keysValidator(t *testing.T) {
	t.Parallel()
	in := []byte(`{"tags":["ok"],"trim":[],"lookup":{"BAD!":1},"mixed":{},"ptr":null}`)
	_, _, err := (CustomDiveStruct{}).DecodeFrom(in)
	if err == nil {
		t.Fatal("expected bad-key error")
	}
	if !strings.Contains(err.Error(), "bad key") {
		t.Errorf("error = %q, want KeyShape message", err.Error())
	}
}

// keys:@LowerKey lowercases each key before insertion.
func TestCustomDive_keysMod(t *testing.T) {
	t.Parallel()
	in := []byte(`{"tags":["ok"],"trim":[],"lookup":{},"mixed":{"FOO":1,"Bar":2},"ptr":null}`)
	got, _, err := (CustomDiveStruct{}).DecodeFrom(in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Mixed["foo"] != 1 || got.Mixed["bar"] != 2 {
		t.Errorf("Mixed = %v, want lowercased keys foo/bar", got.Mixed)
	}
	if _, present := got.Mixed["FOO"]; present {
		t.Errorf("uppercase key still present: %v", got.Mixed)
	}
}

// @PointerCheck is func(*int) error. The resolver must match the exact *T
// type.
func TestCustomDive_pointerField(t *testing.T) {
	t.Parallel()
	// Negative value rejected.
	bad := []byte(`{"tags":["ok"],"trim":[],"lookup":{},"mixed":{},"ptr":-5}`)
	if _, _, err := (CustomDiveStruct{}).DecodeFrom(bad); err == nil {
		t.Error("expected negative-value error from PointerCheck")
	}
	// Positive value accepted.
	good := []byte(`{"tags":["ok"],"trim":[],"lookup":{},"mixed":{},"ptr":5}`)
	got, _, err := (CustomDiveStruct{}).DecodeFrom(good)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Ptr == nil || *got.Ptr != 5 {
		t.Errorf("Ptr = %v, want 5", got.Ptr)
	}
	// nil accepted (PointerCheck handles nil itself).
	null := []byte(`{"tags":["ok"],"trim":[],"lookup":{},"mixed":{},"ptr":null}`)
	if _, _, err := (CustomDiveStruct{}).DecodeFrom(null); err != nil {
		t.Errorf("nil pointer rejected: %v", err)
	}
}

// NoValidateInner puts inner: validators and mods, built-in and @Func, on
// every element and map value position. novalidate drops them like the
// field-level steps. ValidatedInner is the same shape with its steps kept.
//
//ggen:generate novalidate
type NoValidateInner struct {
	Nums  []int             `json:"nums"  pipe:"inner:lte=10"`
	Pair  [2]int            `json:"pair"  pipe:"inner:lte=10"`
	Ptrs  []*int            `json:"ptrs"  pipe:"inner:lte=10"`
	PtrF  []*int            `json:"ptrF"  pipe:"inner:@PositivePtr"`
	PairF [2]*int           `json:"pairF" pipe:"inner:@PositivePtr"`
	PtrM  []*int            `json:"ptrM"  pipe:"inner:@BumpPtr"`
	Grid  [][]int           `json:"grid"  pipe:"inner:(minlen=1 inner:lte=10)"`
	Vals  map[string]int    `json:"vals"  pipe:"inner:lte=10"`
	Tags  []string          `json:"tags"  pipe:"inner:(@NotBlank toupper)"`
	Trim  [1]string         `json:"trim"  pipe:"inner:@TrimSpace"`
	Names map[string]string `json:"names" pipe:"inner:(@NotBlank trim)"`
}

//ggen:generate
type ValidatedInner struct {
	Nums  []int             `json:"nums"  pipe:"inner:lte=10"`
	Pair  [2]int            `json:"pair"  pipe:"inner:lte=10"`
	Ptrs  []*int            `json:"ptrs"  pipe:"inner:lte=10"`
	PtrF  []*int            `json:"ptrF"  pipe:"inner:@PositivePtr"`
	PairF [2]*int           `json:"pairF" pipe:"inner:@PositivePtr"`
	PtrM  []*int            `json:"ptrM"  pipe:"inner:@BumpPtr"`
	Grid  [][]int           `json:"grid"  pipe:"inner:(minlen=1 inner:lte=10)"`
	Vals  map[string]int    `json:"vals"  pipe:"inner:lte=10"`
	Tags  []string          `json:"tags"  pipe:"inner:(@NotBlank toupper)"`
	Trim  [1]string         `json:"trim"  pipe:"inner:@TrimSpace"`
	Names map[string]string `json:"names" pipe:"inner:(@NotBlank trim)"`
}

// PositivePtr and BumpPtr take the `*int` a `[]*int` or `[N]*int` element
// stores, null included.
func PositivePtr(p *int) error {
	if p == nil {
		return errors.New("null")
	}
	if *p <= 0 {
		return errors.New("not positive")
	}
	return nil
}

func BumpPtr(p *int) *int {
	n := 42
	if p != nil {
		n = *p + 100
	}
	return &n
}

func TestNoValidate_innerSteps(t *testing.T) {
	t.Parallel()
	// Each payload fails one element or map value validator.
	for _, in := range []string{
		`{"nums":[11]}`,
		`{"pair":[0,11]}`,
		`{"ptrs":[11]}`,
		`{"ptrF":[-1]}`,
		`{"ptrF":[null]}`,
		`{"pairF":[-1,1]}`,
		`{"pairF":[1,null]}`,
		`{"grid":[[11]]}`,
		`{"grid":[[]]}`,
		`{"grid":[null]}`,
		`{"vals":{"a":11}}`,
		`{"tags":[" "]}`,
		`{"names":{"a":" "}}`,
	} {
		if bErr, sErr := decodeBothPaths[ValidatedInner](in); bErr == nil || sErr == nil {
			t.Errorf("%s: control accepted it (bytes %v, stream %v)", in, bErr, sErr)
		}
		if bErr, sErr := decodeBothPaths[NoValidateInner](in); bErr != nil || sErr != nil {
			t.Errorf("%s: novalidate rejected it (bytes %v, stream %v)", in, bErr, sErr)
		}
	}

	// No validator fails here, so only the mods change the values.
	const in = `{"tags":["a"],"trim":[" b "],"names":{"k":" c "}}`
	bytesV, _, bErr := NoValidateInner{}.DecodeFrom([]byte(in))
	streamV, sErr := ggen.NewStream(strings.NewReader(in), nil).Value[NoValidateInner]()
	if bErr != nil || sErr != nil {
		t.Fatalf("novalidate decode: bytes %v, stream %v", bErr, sErr)
	}
	for path, got := range map[string]NoValidateInner{"bytes": bytesV, "stream": streamV} {
		if got.Tags[0] != "a" || got.Trim[0] != " b " || got.Names["k"] != " c " {
			t.Errorf("%s: novalidate ran a mod: tags %q, trim %q, names %q", path, got.Tags, got.Trim, got.Names)
		}
	}
	ctrlBytes, _, bErr := ValidatedInner{}.DecodeFrom([]byte(in))
	ctrlStream, sErr := ggen.NewStream(strings.NewReader(in), nil).Value[ValidatedInner]()
	if bErr != nil || sErr != nil {
		t.Fatalf("control decode: bytes %v, stream %v", bErr, sErr)
	}
	for path, got := range map[string]ValidatedInner{"bytes": ctrlBytes, "stream": ctrlStream} {
		if got.Tags[0] != "A" || got.Trim[0] != "b" || got.Names["k"] != "c" {
			t.Errorf("%s: control skipped a mod: tags %q, trim %q, names %q", path, got.Tags, got.Trim, got.Names)
		}
	}

	// A `[]*int` element mod runs on the stored pointer, a null one included.
	const ptrIn = `{"ptrM":[1,null]}`
	nvBytes, _, bErr := NoValidateInner{}.DecodeFrom([]byte(ptrIn))
	nvStream, sErr := ggen.NewStream(strings.NewReader(ptrIn), nil).Value[NoValidateInner]()
	if bErr != nil || sErr != nil {
		t.Fatalf("novalidate %s: bytes %v, stream %v", ptrIn, bErr, sErr)
	}
	for path, got := range map[string]NoValidateInner{"bytes": nvBytes, "stream": nvStream} {
		if len(got.PtrM) != 2 || got.PtrM[0] == nil || *got.PtrM[0] != 1 || got.PtrM[1] != nil {
			t.Errorf("%s: novalidate ran a pointer element mod: %v", path, got.PtrM)
		}
	}
	vBytes, _, bErr := ValidatedInner{}.DecodeFrom([]byte(ptrIn))
	vStream, sErr := ggen.NewStream(strings.NewReader(ptrIn), nil).Value[ValidatedInner]()
	if bErr != nil || sErr != nil {
		t.Fatalf("control %s: bytes %v, stream %v", ptrIn, bErr, sErr)
	}
	for path, got := range map[string]ValidatedInner{"bytes": vBytes, "stream": vStream} {
		if len(got.PtrM) != 2 || got.PtrM[0] == nil || *got.PtrM[0] != 101 || got.PtrM[1] == nil || *got.PtrM[1] != 42 {
			t.Errorf("%s: control skipped a pointer element mod: %v", path, got.PtrM)
		}
	}
}

// NestedDive nests `inner:` levels below a map value, and puts a map below a
// slice or array element. Each int leaf runs a built-in mod, an @Func
// validator, an @Func mod and a built-in validator, in that order.
//
//ggen:generate
type NestedDive struct {
	MS  map[string][]int            `json:"ms"  pipe:"inner:(maxlen=2 inner:(clamp=0| @EvenOnly @DoubleInt lte=100))"`
	SM  []map[string]int            `json:"sm"  pipe:"inner:(maxlen=2 inner:(clamp=0| @EvenOnly @DoubleInt lte=100))"`
	MM  map[string]map[string]int   `json:"mm"  pipe:"keys:(minrunes=2 tolower) inner:(maxlen=2 inner:(clamp=0| @EvenOnly @DoubleInt lte=100))"`
	MA  map[string][2]int           `json:"ma"  pipe:"inner:(inner:(clamp=0| @EvenOnly @DoubleInt lte=100))"`
	AM  [2]map[string]int           `json:"am"  pipe:"inner:(maxlen=2 inner:(clamp=0| @EvenOnly @DoubleInt lte=100))"`
	MSM map[string][]map[string]int `json:"msm" pipe:"inner:(maxlen=2 inner:(maxlen=2 inner:(clamp=0| @EvenOnly @DoubleInt lte=100)))"`
	SSM [][]map[string]int          `json:"ssm" pipe:"inner:(maxlen=2 inner:(maxlen=2 inner:(clamp=0| @EvenOnly @DoubleInt lte=100)))"`
	MSS map[string][][]int          `json:"mss" pipe:"inner:(maxlen=2 inner:(maxlen=2 inner:(clamp=0| @EvenOnly @DoubleInt lte=100)))"`
	SMS []map[string][]string       `json:"sms" pipe:"inner:(inner:(inner:(@TrimSpace minrunes=2 tolower)))"`
	// Row steps two levels down, where a row can be null.
	MR map[string][][]int `json:"mr" pipe:"inner:(inner:minlen=1)"`
	SR [][][]int          `json:"sr" pipe:"inner:(inner:minlen=1)"`
	MF map[string][][]int `json:"mf" pipe:"inner:(inner:@FillRow)"`
	SF [][][]int          `json:"sf" pipe:"inner:(inner:@FillRow)"`
}

// FillRow turns a null row into [-1].
func FillRow(r []int) []int {
	if r == nil {
		return []int{-1}
	}
	return r
}

// NestedHint sizes slices below a map with the inner levels of `hint:`.
//
//ggen:generate
type NestedHint struct {
	Hint     map[string][]int   `hint:"inner:37"          json:"hint"`
	HintDeep []map[string][]int `hint:"inner:(4 inner:5)" json:"hintDeep"`
}

// checkNested decodes payload on both paths at several chunk sizes. Both must
// fail with wantErr ("" for success), with identical errors, and an accepted
// value must satisfy ok.
func checkNested[T interface {
	ggen.Decoder[T]
	ggen.StreamDecoder[T]
}](t *testing.T, payload, wantErr string, ok func(T) bool) {
	t.Helper()
	for _, chunk := range []int{1, 7, 64} {
		berr, serr := decodeBothChunked[T](payload, chunk)
		for path, err := range map[string]error{"bytes": berr, "stream": serr} {
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != wantErr {
				t.Errorf("%s chunk=%d %s: err %q, want %q", path, chunk, payload, got, wantErr)
			}
		}
		if !reflect.DeepEqual(berr, serr) {
			t.Errorf("chunk=%d %s: bytes %#v, stream %#v", chunk, payload, berr, serr)
		}
	}
	if wantErr != "" || ok == nil {
		return
	}
	var zero T
	bv, _, _ := zero.DecodeFrom([]byte(payload))
	sv, _ := ggen.NewStream(strings.NewReader(payload), nil).Value[T]()
	if !ok(bv) || !ok(sv) {
		t.Errorf("%s: bytes %+v, stream %+v", payload, bv, sv)
	}
}

// The steps of every level run on both paths. -3 is clamped to 0 before the
// even check, 20 is doubled to 40, 3 fails @EvenOnly, and 60 fails lte=100
// only after @DoubleInt.
func TestNestedDive_levelsBelowMaps(t *testing.T) {
	t.Parallel()
	shapes := []struct {
		payload, path string // the payload takes the leaf at %s
		leaf          func(NestedDive) int
	}{
		{`{"ms":{"a":[%s]}}`, "ms.value[]", func(v NestedDive) int { return v.MS["a"][0] }},
		{`{"sm":[{"a":%s}]}`, "sm[].value", func(v NestedDive) int { return v.SM[0]["a"] }},
		{`{"mm":{"ab":{"x":%s}}}`, "mm.value.value", func(v NestedDive) int { return v.MM["ab"]["x"] }},
		{`{"ma":{"a":[2,%s]}}`, "ma.value[]", func(v NestedDive) int { return v.MA["a"][1] }},
		{`{"am":[{"a":%s},{}]}`, "am[].value", func(v NestedDive) int { return v.AM[0]["a"] }},
		{`{"msm":{"a":[{"b":%s}]}}`, "msm.value[].value", func(v NestedDive) int { return v.MSM["a"][0]["b"] }},
		{`{"ssm":[[{"b":%s}]]}`, "ssm[][].value", func(v NestedDive) int { return v.SSM[0][0]["b"] }},
		{`{"mss":{"a":[[%s]]}}`, "mss.value[][]", func(v NestedDive) int { return v.MSS["a"][0][0] }},
	}
	leaves := []struct {
		in   string
		want int    // the decoded leaf when accepted
		err  string // the message after the path when rejected
	}{
		{"-3", 0, ""},
		{"20", 40, ""},
		{"3", 0, "3 is not even"},
		{"60", 0, "value 120 exceeds maximum 100"},
	}
	for _, sh := range shapes {
		for _, l := range leaves {
			wantErr := ""
			if l.err != "" {
				wantErr = sh.path + ": " + l.err
			}
			checkNested(t, fmt.Sprintf(sh.payload, l.in), wantErr, func(v NestedDive) bool { return sh.leaf(v) == l.want })
		}
	}
}

// Container rules apply at their own level, `keys:` steps touch only the keys
// of the outer map, and a string leaf three levels down is trimmed, checked
// and lowercased.
func TestNestedDive_containerLevelsAndKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		payload, wantErr string
		ok               func(NestedDive) bool
	}{
		{`{"ms":{"a":[2,2,2]}}`, "ms.value: length 3 exceeds maximum 2", nil},
		{`{"sm":[{"a":2,"b":2,"c":2}]}`, "sm[]: length 3 exceeds maximum 2", nil},
		{`{"am":[{},{"a":2,"b":2,"c":2}]}`, "am[]: length 3 exceeds maximum 2", nil},
		{`{"msm":{"a":[{},{},{}]}}`, "msm.value: length 3 exceeds maximum 2", nil},
		{`{"msm":{"a":[{"b":2,"c":2,"d":2}]}}`, "msm.value[]: length 3 exceeds maximum 2", nil},
		{`{"ssm":[[{},{},{}]]}`, "ssm[]: length 3 exceeds maximum 2", nil},
		{`{"ssm":[[{"b":2,"c":2,"d":2}]]}`, "ssm[][]: length 3 exceeds maximum 2", nil},
		{`{"mss":{"a":[[],[],[]]}}`, "mss.value: length 3 exceeds maximum 2", nil},
		{`{"mss":{"a":[[2,2,2]]}}`, "mss.value[]: length 3 exceeds maximum 2", nil},
		{`{"mm":{"a":{"xy":2}}}`, "mm.key: rune count 1 below minimum 2", nil},
		{`{"mm":{"AB":{"X":2}}}`, "", func(v NestedDive) bool { return v.MM["ab"]["X"] == 4 }},
		{`{"sms":[{"a":[" a "]}]}`, "sms[].value[]: rune count 1 below minimum 2", nil},
		{`{"sms":[{"a":[" AB "]}]}`, "", func(v NestedDive) bool { return v.SMS[0]["a"][0] == "ab" }},
	}
	for _, c := range cases {
		checkNested(t, c.payload, c.wantErr, c.ok)
	}
}

// A null row two levels down still runs its level's steps: a row validator
// rejects it and a row mod sees the nil, below a map value and inside a plain
// nested slice.
func TestNestedDive_nullRowsTwoLevelsDown(t *testing.T) {
	t.Parallel()
	checkNested[NestedDive](t, `{"mr":{"a":[null]}}`, "mr.value[]: length 0 below minimum 1", nil)
	checkNested[NestedDive](t, `{"sr":[[null]]}`, "sr[][]: length 0 below minimum 1", nil)
	checkNested[ExtraStruct](t, `{"triple":[[null]]}`, "triple[][]: length 0 below minimum 1", nil)
	filled := [][]int{{-1}}
	checkNested(t, `{"mf":{"a":[null]}}`, "", func(v NestedDive) bool { return reflect.DeepEqual(v.MF["a"], filled) })
	checkNested(t, `{"sf":[[null]]}`, "", func(v NestedDive) bool { return reflect.DeepEqual(v.SF, [][][]int{filled}) })
}

func TestNestedDive_hintBelowMap(t *testing.T) {
	t.Parallel()
	checkNested(t, `{"hint":{"a":[1,2]}}`, "", func(v NestedHint) bool { return cap(v.Hint["a"]) == 37 })
	checkNested(t, `{"hintDeep":[{"a":[1,2]}]}`, "", func(v NestedHint) bool { return cap(v.HintDeep[0]["a"]) == 5 })
}

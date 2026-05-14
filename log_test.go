package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearDetectionEnv unsets every env var shouldUseConcise inspects so
// the test only sees what it explicitly sets. t.Setenv handles the
// restore on test exit. Not parallel-safe (t.Setenv panics under
// t.Parallel), which is intentional — env detection tests touch shared
// process state and must run sequentially.
func clearDetectionEnv(t *testing.T) {
	t.Helper()
	for _, k := range ciEnvVars {
		t.Setenv(k, "")
	}
	for _, k := range agentEnvVars {
		t.Setenv(k, "")
	}
}

func TestShouldUseConcise_EnvDetection(t *testing.T) {
	cases := []struct {
		name        string
		setVar      string // var to set (empty = no env var set)
		setVal      string // value (used when setVar != "")
		stderrIsTTY bool
		want        bool
	}{
		// Universal CI marker.
		{"CI_true", "CI", "true", true, true},
		{"CI_1", "CI", "1", true, true},
		// Vendor-specific CI vars that some runners set without CI=1.
		{"GITHUB_ACTIONS", "GITHUB_ACTIONS", "true", true, true},
		{"GITLAB_CI", "GITLAB_CI", "true", true, true},
		{"CIRCLECI", "CIRCLECI", "true", true, true},
		{"JENKINS_HOME", "JENKINS_HOME", "/var/jenkins", true, true},
		{"BUILDKITE", "BUILDKITE", "true", true, true},
		{"TRAVIS", "TRAVIS", "true", true, true},
		{"APPVEYOR", "APPVEYOR", "true", true, true},
		{"TF_BUILD_AzureDevOps", "TF_BUILD", "True", true, true},
		{"TEAMCITY_VERSION", "TEAMCITY_VERSION", "2024.07", true, true},
		{"CONTINUOUS_INTEGRATION", "CONTINUOUS_INTEGRATION", "true", true, true},

		// Coding agents.
		{"AI_AGENT", "AI_AGENT", "claude-code_2-1-140", true, true},
		{"CLAUDECODE", "CLAUDECODE", "1", true, true},
		{"CURSOR_TRACE_ID", "CURSOR_TRACE_ID", "abc", true, true},
		{"AIDER", "AIDER_AUTO_COMMITS", "true", true, true},

		// Non-TTY stderr alone (no env signal) → concise.
		{"non_tty_stderr_no_env", "", "", false, true},

		// Interactive human: no env signals, TTY stderr → pretty.
		{"human_interactive", "", "", true, false},

		// Empty env value is treated as unset.
		{"CI_empty_string", "CI", "", true, false},
	}
	clearDetectionEnv(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setVar != "" {
				t.Setenv(tc.setVar, tc.setVal)
			}
			got := shouldUseConcise(os.Getenv, tc.stderrIsTTY)
			if got != tc.want {
				t.Errorf("shouldUseConcise(setVar=%q=%q, tty=%v) = %v, want %v",
					tc.setVar, tc.setVal, tc.stderrIsTTY, got, tc.want)
			}
		})
	}
}

// captured drives a logger against a bytes.Buffer so tests can assert on
// exact output bytes. Error() in production queues + waits for Flush;
// most tests want to see the rendered bytes immediately so the returned
// Logger auto-flushes after each Error call. Queue-specific tests
// construct the raw impls directly.
func captured(level Level, pretty, color bool) (Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	var inner Logger
	if pretty {
		inner = &prettyLogger{level: level, w: &buf, color: color}
	} else {
		inner = &conciseLogger{level: level, w: &buf}
	}
	return &autoFlushLogger{Logger: inner}, &buf
}

// autoFlushLogger transparently flushes the underlying logger after
// each Error call so the existing post-Error byte-assert tests keep
// working with the queue-based implementation.
type autoFlushLogger struct{ Logger }

func (a *autoFlushLogger) Error(err error) {
	a.Logger.Error(err)
	a.Flush()
}

func TestLogger_LevelFiltering(t *testing.T) {
	t.Parallel()
	type emit struct {
		method string
		shown  bool
	}
	cases := []struct {
		name  string
		level Level
		emits []emit
	}{
		{"quiet", LevelQuiet, []emit{
			{"info", false},
			{"debug", false},
			{"trace", false},
			{"error", true}, // errors always surface
		}},
		{"info", LevelInfo, []emit{
			{"info", true},
			{"debug", false},
			{"trace", false},
			{"error", true},
		}},
		{"debug", LevelDebug, []emit{
			{"info", true},
			{"debug", true},
			{"trace", false},
			{"error", true},
		}},
		{"trace", LevelTrace, []emit{
			{"info", true},
			{"debug", true},
			{"trace", true},
			{"error", true},
		}},
	}
	for _, mode := range []struct {
		label  string
		pretty bool
	}{
		{"concise", false},
		{"pretty", true},
	} {
		for _, c := range cases {
			name := mode.label + "/" + c.name
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				for _, e := range c.emits {
					log, buf := captured(c.level, mode.pretty, false)
					switch e.method {
					case "info":
						log.Info("hello")
					case "debug":
						log.Debug("hello")
					case "trace":
						log.Trace("hello")
					case "error":
						log.Error(errors.New("boom"))
					}
					out := buf.String()
					if e.shown && !strings.Contains(out, "hello") && !strings.Contains(out, "boom") {
						t.Errorf("%s at level=%s: expected output, got %q", e.method, c.name, out)
					}
					if !e.shown && out != "" {
						t.Errorf("%s at level=%s: expected no output, got %q", e.method, c.name, out)
					}
				}
			})
		}
	}
}

func TestConciseLogger_FormatPrefixes(t *testing.T) {
	t.Parallel()
	log, buf := captured(LevelTrace, false, false)
	log.Info("wrote %s", "x.go")
	log.Debug("parsing %s", "pkg/")
	log.Trace("field %s", "N")
	log.Error(errors.New("bad rule"))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	want := []string{
		"inf: wrote x.go",
		"dbg: parsing pkg/",
		"trc: field N",
		"err: bad rule",
	}
	for i, w := range want {
		if i >= len(lines) || lines[i] != w {
			t.Errorf("line %d: got %q, want %q", i, lines[i], w)
		}
	}
}

func TestPrettyLogger_ColorAndFormat(t *testing.T) {
	t.Parallel()
	log, buf := captured(LevelTrace, true, true)
	log.Info("wrote x.go")
	log.Debug("parsing pkg/")
	log.Trace("field N")
	out := buf.String()
	// Color codes must be present in pretty + color mode.
	if !strings.Contains(out, ansiGreen) {
		t.Errorf("pretty Info missing green color: %q", out)
	}
	if !strings.Contains(out, ansiCyan) {
		t.Errorf("pretty Debug missing cyan color: %q", out)
	}
	if !strings.Contains(out, ansiGray) {
		t.Errorf("pretty Trace missing gray color: %q", out)
	}
	if !strings.Contains(out, ansiReset) {
		t.Errorf("expected ANSI reset codes, got: %q", out)
	}
}

func TestPrettyLogger_NoColorWhenDisabled(t *testing.T) {
	t.Parallel()
	log, buf := captured(LevelInfo, true, false)
	log.Info("wrote x.go")
	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("expected no ANSI codes with color=false, got: %q", out)
	}
	if !strings.Contains(out, "wrote x.go") {
		t.Errorf("expected message body, got: %q", out)
	}
}

func TestPrettyLogger_RichError_FullLayout(t *testing.T) {
	t.Parallel()
	// Pretty mode renders richError as a golangci-lint-style three-line
	// diagnostic with the UserHint folded into the first line as a
	// parenthesized aside:
	//   file:line:col: <Msg> (<UserHint>)
	//   \t<source line with CodeSpan highlighted>
	//   \t<indent>^
	dir := t.TempDir()
	src := "package x\n\ntype Foo struct {\n\tN int `json:\"n\" ggen:\"ascii\"`\n}\n"
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	err := &richError{
		Pos:      token.Position{Filename: path, Line: 4, Column: 2},
		Msg:      `field n: ggen rule "ascii" cannot be applied to int`,
		CodeSpan: "ascii",
		BotHint:  "expected string",
		UserHint: "either change N to string or drop the rule",
	}
	log, buf := captured(LevelQuiet, true, false)
	log.Error(err)
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	if len(lines) != 3 {
		t.Fatalf("expected exactly 3 lines (header + code + caret), got %d:\n%s", len(lines), out)
	}
	// Line 1: <file:line:col>: <msg> (<UserHint>) — no `Error:` prefix.
	// Single-error blocks resolve `col` to the CodeSpan's column in
	// the source, not the field-decl column from token.Position. The
	// source line `\tN int \`json:"n" ggen:"ascii"\`` puts "ascii" at
	// byte index 23 (1-indexed: col 24).
	if strings.Contains(lines[0], "Error:") {
		t.Errorf("line 1 should NOT have an Error: header, got: %q", lines[0])
	}
	if !strings.Contains(lines[0], path+":4:24:") {
		t.Errorf("line 1 should carry position with CodeSpan column (24), got: %q", lines[0])
	}
	// In color=false mode emphasize() strips the quote markers, so
	// `"ascii"` becomes `ascii`. Period-stripping removes the trailing
	// `.` from hints. No parens around the hint either — the gray
	// run is the visual separator in color mode; plain mode just runs
	// the hint on after the msg.
	if !strings.Contains(lines[0], `rule ascii cannot be applied to int`) {
		t.Errorf("line 1 should carry the message, got: %q", lines[0])
	}
	if !strings.Contains(lines[0], "either change N to string or drop the rule") {
		t.Errorf("line 1 should carry the UserHint inline, got: %q", lines[0])
	}
	if strings.Contains(lines[0], "(") {
		t.Errorf("line 1 should NOT wrap the hint in parens any more, got: %q", lines[0])
	}
	// Line 2: tab-indented source line embedding the field decl.
	if !strings.HasPrefix(lines[1], "\t") {
		t.Errorf("line 2 should be tab-indented, got: %q", lines[1])
	}
	if !strings.Contains(lines[1], "N int") {
		t.Errorf("line 2 should embed the source line, got: %q", lines[1])
	}
	// Line 3: tab-indented caret line.
	if !strings.HasPrefix(lines[2], "\t") {
		t.Errorf("line 3 should be tab-indented, got: %q", lines[2])
	}
	if !strings.Contains(lines[2], "^") {
		t.Errorf("line 3 should carry caret, got: %q", lines[2])
	}
}

func TestPrettyLogger_RichError_HighlightedCodeSpan(t *testing.T) {
	t.Parallel()
	// Color on + CodeSpan present + span found in source line → the
	// span must be wrapped in ANSI red+bold within the code line.
	dir := t.TempDir()
	src := "package x\n\ntype Foo struct {\n\tN int `json:\"n\" ggen:\"ascii\"`\n}\n"
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	err := &richError{
		Pos:      token.Position{Filename: path, Line: 4, Column: 2},
		Msg:      "bad rule",
		CodeSpan: "ascii",
		BotHint:  "expected string",
	}
	log, buf := captured(LevelQuiet, true, true)
	log.Error(err)
	out := buf.String()
	// The code-line row must contain ANSI red+bold for the highlighted
	// span. We can't string-equal because of formatting, just probe:
	if !strings.Contains(out, ansiRed+ansiBold+"ascii"+ansiReset) {
		t.Errorf("code line should highlight CodeSpan with red+bold, got: %q", out)
	}
}

func TestPrettyLogger_RichError_NoSourceFile_PositionAlone(t *testing.T) {
	t.Parallel()
	// readSourceLine fails when the file doesn't exist; the renderer
	// should fall back to showing just the position + message without
	// crashing or emitting a stray caret row.
	err := &richError{
		Pos: token.Position{Filename: "/nope/no/such.go", Line: 1, Column: 1},
		Msg: "boom",
	}
	log, buf := captured(LevelQuiet, true, false)
	log.Error(err)
	out := buf.String()
	if !strings.Contains(out, "/nope/no/such.go:1:1") {
		t.Errorf("expected position printed, got: %q", out)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("expected message body, got: %q", out)
	}
	// No `Error:` header in golangci-lint style.
	if strings.Contains(out, "Error:") {
		t.Errorf("should NOT emit Error: header, got: %q", out)
	}
	// No caret row when source is unreadable.
	if strings.Contains(out, "^") {
		t.Errorf("should NOT emit caret when source missing, got: %q", out)
	}
}

func TestConciseLogger_RichError_SingleLineWithBotHint(t *testing.T) {
	t.Parallel()
	// Concise mode: one line, position + msg + parenthesized bot hint.
	// User hint is intentionally NOT included to avoid railroading
	// downstream agent reasoning toward one specific remedy.
	pos := token.Position{Filename: "x.go", Line: 5, Column: 2}
	err := &richError{
		Pos:      pos,
		Msg:      "bad rule",
		BotHint:  "expected string",
		UserHint: "change the field type",
	}
	log, buf := captured(LevelQuiet, false, false)
	log.Error(err)
	out := strings.TrimRight(buf.String(), "\n")
	// Bare filename gets a `./` prefix from relPath — cwd-relative paths
	// stay clickable in editor terminals.
	want := "err: ./x.go:5:2: bad rule (expected string)"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
	// User hint must not leak into concise output.
	if strings.Contains(out, "change the field type") {
		t.Errorf("UserHint leaked into concise output: %q", out)
	}
}

func TestConciseLogger_RichError_NoBotHint(t *testing.T) {
	t.Parallel()
	// Without a BotHint the parens shouldn't appear at all — never
	// emit empty `()` noise.
	pos := token.Position{Filename: "x.go", Line: 5, Column: 2}
	err := &richError{Pos: pos, Msg: "bad thing"}
	log, buf := captured(LevelQuiet, false, false)
	log.Error(err)
	out := strings.TrimRight(buf.String(), "\n")
	want := "err: ./x.go:5:2: bad thing"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestRichError_Unwrap(t *testing.T) {
	t.Parallel()
	inner := errors.New("inner")
	re := &richError{
		Pos: token.Position{Filename: "x.go", Line: 1, Column: 1},
		Msg: "outer",
		Err: inner,
	}
	if !errors.Is(re, inner) {
		t.Errorf("richError must unwrap to its inner error")
	}
	var got *richError
	if !errors.As(fmt.Errorf("wrap: %w", re), &got) {
		t.Errorf("errors.As must thread through fmt.Errorf wrapping")
	}
}

func TestRichError_ErrorString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		re   *richError
		want string
	}{
		{"all_fields",
			&richError{
				Pos:     token.Position{Filename: "x.go", Line: 1, Column: 2},
				Msg:     "bad",
				BotHint: "hint",
			},
			"./x.go:1:2: bad (hint)"},
		{"no_pos",
			&richError{Msg: "bare", BotHint: "h"},
			"bare (h)"},
		{"no_hint",
			&richError{Pos: token.Position{Filename: "x.go", Line: 1, Column: 2}, Msg: "bad"},
			"./x.go:1:2: bad"},
		{"naked",
			&richError{Msg: "alone"},
			"alone"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.re.Error(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestReadSourceLine_Basics(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	body := "package x\n\nfunc f() {}\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := readSourceLine(path, 1); !ok || got != "package x" {
		t.Errorf("line 1: got %q ok=%v", got, ok)
	}
	if got, ok := readSourceLine(path, 3); !ok || got != "func f() {}" {
		t.Errorf("line 3: got %q ok=%v", got, ok)
	}
	if _, ok := readSourceLine(path, 99); ok {
		t.Error("line beyond EOF should return ok=false")
	}
	if _, ok := readSourceLine("/no/such/file.go", 1); ok {
		t.Error("missing file should return ok=false")
	}
	if _, ok := readSourceLine("", 1); ok {
		t.Error("empty filename should return ok=false")
	}
	if _, ok := readSourceLine(path, 0); ok {
		t.Error("line < 1 should return ok=false")
	}
}

// ----- caretIndent: direct unit -----

func TestCaretIndent_AnchorsToCodeSpan(t *testing.T) {
	t.Parallel()
	// The core invariant of the new error layout: the caret must
	// land under CodeSpan (the offending token), NOT under Pos.Column
	// (which points at the field declaration). A naive
	// `strings.Repeat(" ", Pos.Column-1)` regression would pass the
	// "is there a caret somewhere" check from the full-layout test —
	// this test pins the exact column.
	cases := []struct {
		name      string
		line      string
		prefix    string // simulated `./file.go:line:col: ` width
		posCol    int    // 1-indexed Pos.Column
		span      string // CodeSpan
		wantSlash string // visual marker: the indent + ^ should equal this prefix-stripped expected line
	}{
		{
			name:      "tag_with_ascii_rule",
			line:      `	N int ` + "`" + `json:"n" ggen:"ascii"` + "`",
			prefix:    "./f.go:5:2: ",
			posCol:    2, // points at `N`
			span:      "ascii",
			wantSlash: "                      ^", // 12-char prefix + tab byte → spaces; `ascii` starts at byte 22 of the source line (1-tab + "N int `json:\"n\" ggen:\""), caret column = 12+22 = 34. We assert pre-^ length covers tab → space mapping.
		},
		{
			name:   "span_at_start",
			line:   "ascii foo",
			prefix: "p: ",
			posCol: 1,
			span:   "ascii",
		},
		{
			name:   "span_missing_falls_back_to_pos_col",
			line:   "abc def ghi",
			prefix: "p: ",
			posCol: 5, // points at 'd'
			span:   "notthere",
		},
		{
			name:   "empty_span_uses_pos_col",
			line:   "abc def",
			prefix: "p: ",
			posCol: 5, // 'd'
			span:   "",
		},
		{
			name:   "tab_indented_source",
			line:   "\t\tN int",
			prefix: "p: ",
			posCol: 3, // 'N' (after two tabs)
			span:   "N",
		},
		{
			name:   "pos_col_past_eol_clamps",
			line:   "abc",
			prefix: "p: ",
			posCol: 99,
			span:   "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := caretIndent(c.line, c.prefix, c.posCol, c.span)
			// First len(prefix) bytes must be spaces (prefix padding).
			for i := 0; i < len(c.prefix); i++ {
				if got[i] != ' ' {
					t.Fatalf("byte %d of indent should be space, got %q in %q", i, got[i], got)
				}
			}
			// Whitespace bytes from the source must be preserved
			// verbatim (tabs become tabs, not spaces) so the caret
			// aligns under the visual character in terminals.
			body := got[len(c.prefix):]
			for i, b := range []byte(body) {
				if i >= len(c.line) {
					break
				}
				if c.line[i] == '\t' && b != '\t' {
					t.Errorf("source col %d is tab but indent byte is %q (would mis-align)", i, b)
				}
				if c.line[i] != '\t' && b != ' ' {
					t.Errorf("source col %d is non-tab %q but indent byte is %q (expected space)", i, c.line[i], b)
				}
			}
			// Resulting indent length must be ≤ prefix + line length
			// (clamping past EOL).
			if len(got) > len(c.prefix)+len(c.line)+1 {
				t.Errorf("indent overruns line: len=%d, max=%d", len(got), len(c.prefix)+len(c.line))
			}
		})
	}
}

func TestCaretIndent_PicksSpanOverPosCol(t *testing.T) {
	t.Parallel()
	// Verify the span lookup wins over Pos.Column when both are
	// candidates. Source has `bar` at col 4 (1-indexed); Pos.Column
	// points at 'f' (col 1); span "bar" must shift the caret to col 4.
	line := "foo bar baz"
	indent := caretIndent(line, "", 1, "bar")
	// indent should be 4 spaces (cols 1..4 → caret at col 5? no — caret
	// indent itself is the prefix; caret is appended by caller. Span
	// "bar" starts at byte index 4, so indent.len == 4.
	if len(indent) != 4 {
		t.Errorf("caret indent for span at byte 4 should be len 4, got %d (%q)", len(indent), indent)
	}
}

// ----- highlightSpan: direct unit -----

func TestHighlightSpan(t *testing.T) {
	t.Parallel()
	// Wrap the first occurrence of span in ANSI red+bold.
	pl := &prettyLogger{color: true}
	got := pl.highlightSpan(`foo ascii bar`, "ascii")
	want := "foo " + ansiRed + ansiBold + "ascii" + ansiReset + " bar"
	if got != want {
		t.Errorf("highlight: got %q, want %q", got, want)
	}
	// Empty span → no change.
	if got := pl.highlightSpan("line", ""); got != "line" {
		t.Errorf("empty span should pass through, got %q", got)
	}
	// Span absent → no change.
	if got := pl.highlightSpan("line", "missing"); got != "line" {
		t.Errorf("missing span should pass through, got %q", got)
	}
	// Color disabled → no ANSI, even with matching span.
	plNoColor := &prettyLogger{color: false}
	if got := plNoColor.highlightSpan(`foo ascii bar`, "ascii"); got != "foo ascii bar" {
		t.Errorf("color=false should pass through, got %q", got)
	}
}

// ----- relPath / formatPos -----

func TestRelPath_Variants(t *testing.T) {
	t.Parallel()
	if got := relPath(""); got != "" {
		t.Errorf("empty input should return empty, got %q", got)
	}
	// Cwd-relative siblings get a ./ prefix.
	if got := relPath("x.go"); got != "./x.go" {
		t.Errorf("sibling: got %q, want ./x.go", got)
	}
	// Absolute path under cwd: rendered as ./relative.
	cwd, _ := os.Getwd()
	abs := filepath.Join(cwd, "subdir", "file.go")
	if got := relPath(abs); got != "./subdir/file.go" {
		t.Errorf("nested abs: got %q, want ./subdir/file.go", got)
	}
	// Path on a wildly different root: the relative form would be
	// huge ../../../... so relPath falls back to abs.
	out := relPath("/etc/hosts")
	if !strings.HasPrefix(out, "/") {
		t.Errorf("far-away path should fall back to absolute, got %q", out)
	}
}

func TestFormatPos(t *testing.T) {
	t.Parallel()
	if got := formatPos(token.Position{}); got != "" {
		t.Errorf("invalid pos should render empty, got %q", got)
	}
	got := formatPos(token.Position{Filename: "x.go", Line: 5, Column: 2})
	if got != "./x.go:5:2" {
		t.Errorf("got %q, want ./x.go:5:2", got)
	}
}

// ----- richError without Pos: file-level errors -----

// TestPrettyLogger_RichError_NoPos_FoldsHintInline covers the
// file-level error path (no Pos, no source excerpt, no caret). The
// UserHint is folded into the message line as a parenthesized aside,
// so the entire diagnostic fits on a single line.
func TestPrettyLogger_RichError_NoPos_FoldsHintInline(t *testing.T) {
	t.Parallel()
	err := &richError{
		Msg:      "./temp.go: no annotation found",
		BotHint:  "missing //ggen:generate",
		UserHint: "Add `//ggen:generate` above your struct.",
	}
	log, buf := captured(LevelQuiet, true, false)
	log.Error(err)
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 line (msg + paren-hint folded), got %d:\n%s", len(lines), out)
	}
	if strings.Contains(lines[0], "Error:") {
		t.Errorf("line 1 should NOT have Error: header, got: %q", lines[0])
	}
	if !strings.Contains(lines[0], "no annotation found") {
		t.Errorf("line 1 should carry the message, got: %q", lines[0])
	}
	// Color off: backticks stripped, no parens, trailing period gone.
	if !strings.Contains(lines[0], "Add //ggen:generate above your struct") {
		t.Errorf("line 1 should carry UserHint inline (no parens, no quotes), got: %q", lines[0])
	}
	if strings.Contains(lines[0], "(") || strings.HasSuffix(lines[0], ".") {
		t.Errorf("line 1 should drop parens + trailing period, got: %q", lines[0])
	}
}

// TestConciseLogger_RichError_NoPos_NoLeadingPosition: file-level
// errors render the BotHint in parens but skip the position prefix.
func TestConciseLogger_RichError_NoPos_NoLeadingPosition(t *testing.T) {
	t.Parallel()
	err := &richError{
		Msg:     "./temp.go: no annotation found",
		BotHint: "missing //ggen:generate",
	}
	log, buf := captured(LevelQuiet, false, false)
	log.Error(err)
	got := strings.TrimRight(buf.String(), "\n")
	want := "err: ./temp.go: no annotation found (missing //ggen:generate)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ----- bare (non-richError) error rendering -----

func TestPrettyLogger_BareError_RendersMessageOnly(t *testing.T) {
	t.Parallel()
	// Bare (non-richError) errors have no position, no CodeSpan, no
	// UserHint. Pretty mode prints just the raw message — no `Error:`
	// header, no source excerpt, no caret, no Note line. This matches
	// the rich-error single-line layout: just the bytes the error
	// carries.
	log, buf := captured(LevelQuiet, true, false)
	log.Error(errors.New("plain failure"))
	out := strings.TrimRight(buf.String(), "\n")
	if out != "plain failure" {
		t.Errorf("bare error should print message verbatim, got %q", out)
	}
}

func TestConciseLogger_BareError_RendersWithErrPrefix(t *testing.T) {
	t.Parallel()
	log, buf := captured(LevelQuiet, false, false)
	log.Error(errors.New("plain failure"))
	got := strings.TrimRight(buf.String(), "\n")
	want := "err: plain failure"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ----- caret column = CodeSpan column, not Pos.Column -----

// TestPrettyLogger_CaretLandsOnCodeSpan exercises the exact invariant
// behind caretIndent's `strings.Index(line[col:], span)` lookup.
// Pos.Column points at the field declaration's first byte; the caret
// must shift forward to where the offending CodeSpan actually lives
// inside the line (typically inside the struct tag). A regression
// that placed the caret at Pos.Column would still appear "correct"
// to TestPrettyLogger_RichError_FullLayout (which only checks "is a
// caret present"). This test pins the exact column.
func TestPrettyLogger_CaretLandsOnCodeSpan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Pos.Column will be 2 (the `N` after a leading tab), CodeSpan
	// "ascii" lives at byte offset 22 in this line.
	src := "package x\n\ntype Foo struct {\n\tN int `json:\"n\" ggen:\"ascii\"`\n}\n"
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	err := &richError{
		Pos:      token.Position{Filename: path, Line: 4, Column: 2},
		Msg:      "bad rule",
		CodeSpan: "ascii",
	}
	log, buf := captured(LevelQuiet, true, false)
	log.Error(err)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected ≥3 lines, got: %q", buf.String())
	}
	// The caret-line bytes preceding the `^` define the column.
	caretLine := lines[2]
	idx := strings.IndexByte(caretLine, '^')
	if idx < 0 {
		t.Fatalf("no caret in line: %q", caretLine)
	}
	// Now find where `ascii` starts in the previous (source) line —
	// that's our expected caret column. lines[1] has the prefix
	// `./<path>:4:2: ` plus the source line; we measure span position
	// against the WHOLE rendered line because the caret is offset to
	// align with the source bytes as displayed.
	srcLine := lines[1]
	spanCol := strings.Index(srcLine, "ascii")
	if spanCol < 0 {
		t.Fatalf("CodeSpan not in rendered source line: %q", srcLine)
	}
	if idx != spanCol {
		t.Errorf("caret at col %d, want %d (under `ascii`)\nsrc:   %q\ncaret: %q",
			idx, spanCol, srcLine, caretLine)
	}
}

// ----- queue / Flush / HasErrors -----

// TestConciseLogger_Error_QueuesUntilFlush pins the new behaviour:
// Error appends to a queue and prints nothing until Flush. This lets
// a long walk surface ALL package errors in one batch at exit instead
// of bailing on the first failure.
func TestConciseLogger_Error_QueuesUntilFlush(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := &conciseLogger{level: LevelQuiet, w: &buf}

	log.Error(errors.New("first"))
	log.Error(errors.New("second"))
	if buf.Len() != 0 {
		t.Fatalf("Error must not print before Flush; got %q", buf.String())
	}
	if !log.HasErrors() {
		t.Fatalf("HasErrors should be true after Error()")
	}

	log.Flush()
	out := buf.String()
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Errorf("Flush should emit all queued errors, got: %q", out)
	}
	// Flush ordering must match Error call order.
	if strings.Index(out, "first") > strings.Index(out, "second") {
		t.Errorf("Flush must preserve insertion order, got: %q", out)
	}

	// HasErrors stays sticky after Flush so main can still exit non-zero.
	if !log.HasErrors() {
		t.Errorf("HasErrors must remain true after Flush")
	}

	// Flush should be idempotent / safe to re-call: the queue is now
	// drained, no new bytes should appear.
	priorLen := buf.Len()
	log.Flush()
	if buf.Len() != priorLen {
		t.Errorf("second Flush wrote extra bytes: %q", buf.Bytes()[priorLen:])
	}
}

func TestPrettyLogger_Error_QueuesUntilFlush(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := &prettyLogger{level: LevelQuiet, w: &buf, color: false}
	log.Error(errors.New("alpha"))
	log.Error(errors.New("beta"))
	if buf.Len() != 0 {
		t.Fatalf("Error must not print before Flush; got %q", buf.String())
	}
	log.Flush()
	out := buf.String()
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Errorf("queue not drained: %q", out)
	}
}

// TestLogger_HasErrors_FalseUntilError ensures HasErrors only flips
// when an Error is recorded. Info/Debug/Trace must not affect it.
func TestLogger_HasErrors_FalseUntilError(t *testing.T) {
	t.Parallel()
	for _, mode := range []struct {
		name string
		log  Logger
	}{
		{"concise", &conciseLogger{level: LevelTrace, w: io.Discard}},
		{"pretty", &prettyLogger{level: LevelTrace, w: io.Discard}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			mode.log.Info("hi")
			mode.log.Debug("hi")
			mode.log.Trace("hi")
			if mode.log.HasErrors() {
				t.Errorf("Info/Debug/Trace must not set HasErrors")
			}
			mode.log.Error(errors.New("boom"))
			if !mode.log.HasErrors() {
				t.Errorf("Error must set HasErrors")
			}
			mode.log.Flush()
			if !mode.log.HasErrors() {
				t.Errorf("HasErrors must stay sticky after Flush")
			}
		})
	}
}

// TestLogger_InterleavedInfoAndErrors verifies that info messages
// during a long walk print immediately while errors stay queued for
// the final batch. The "wrote X" lines should appear before any
// queued errors.
func TestLogger_InterleavedInfoAndErrors(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := &conciseLogger{level: LevelInfo, w: &buf}
	log.Info("wrote ./a")
	log.Error(errors.New("bad ./b"))
	log.Info("wrote ./c")
	log.Error(errors.New("bad ./d"))

	mid := buf.String()
	// At this point only the info lines should have surfaced.
	if !strings.Contains(mid, "wrote ./a") || !strings.Contains(mid, "wrote ./c") {
		t.Errorf("info lines must print immediately, got: %q", mid)
	}
	if strings.Contains(mid, "bad ./b") || strings.Contains(mid, "bad ./d") {
		t.Errorf("errors leaked before Flush: %q", mid)
	}

	log.Flush()
	full := buf.String()
	// After Flush, errors appear AFTER all info lines (batched at the end).
	infoEnd := strings.LastIndex(full, "wrote ./c")
	errStart := strings.Index(full, "bad ./b")
	if errStart <= infoEnd {
		t.Errorf("errors should batch AFTER info lines, got:\n%s", full)
	}
}

// TestLogger_Fatal_FlushesQueuedFirst pins that a Fatal call drains
// any previously queued errors before emitting the fatal one — so we
// don't lose context on the way out.
func TestLogger_Fatal_FlushesQueuedFirst(t *testing.T) {
	t.Parallel()
	// We can't exercise Fatal directly (it calls os.Exit). Instead
	// drive the same code path manually: Error+Error+Flush+render.
	// This test is a documentation pin for the Fatal contract.
	var buf bytes.Buffer
	log := &conciseLogger{level: LevelQuiet, w: &buf}
	log.Error(errors.New("first"))
	log.Error(errors.New("second"))
	log.Flush()
	// Simulate the Fatal tail: render one more, no queue.
	log.renderError(errors.New("fatal"))
	out := buf.String()
	idxFirst := strings.Index(out, "first")
	idxSecond := strings.Index(out, "second")
	idxFatal := strings.Index(out, "fatal")
	if idxFirst >= idxSecond || idxSecond >= idxFatal {
		t.Errorf("Fatal must surface queued errors first, got:\n%s", out)
	}
}

// ----- pretty mode: line-level error grouping -----

// TestPrettyLogger_GroupsErrorsBySourceLine pins the layout shape
// when multiple errors share a (file, line):
//
//	./<path>:<line>:<firstCol>: <msg1> (<hint1>)
//	                            <msg2> (<hint2>)
//	\t<source line, all spans highlighted>
//	\t^<spaces>^...
//
// Errors at different lines OR different files do NOT group — they
// emit their own block. Bare (non-position) errors render alone.
func TestPrettyLogger_GroupsErrorsBySourceLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := "package x\n\ntype Foo struct {\n\tN int `json:\"n\" ggen:\"ascii\" mod:\"trim\"`\n}\n"
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pos := token.Position{Filename: path, Line: 4, Column: 2}
	// Two errors on the same source line — different CodeSpans.
	err1 := &richError{
		Pos: pos, Msg: `rule "ascii" cannot be applied to int`,
		CodeSpan: "ascii", UserHint: "drop the rule",
	}
	err2 := &richError{
		Pos: pos, Msg: `mod "trim" cannot be applied to int`,
		CodeSpan: "trim", UserHint: "drop the mod",
	}
	// Use the raw prettyLogger — captured() wraps in autoFlushLogger
	// which flushes after every Error call, defeating the grouping
	// (each error would render as its own block).
	var buf bytes.Buffer
	log := &prettyLogger{level: LevelQuiet, w: &buf, color: false}
	log.Error(err1)
	log.Error(err2)
	log.Flush()
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (header + continuation + src + carets), got %d:\n%s", len(lines), out)
	}
	// Line 1: position header + first msg. Multi-error groups OMIT
	// the column — carets disambiguate which span each error points
	// at, so a single header column would be misleading.
	if !strings.Contains(lines[0], path+":4: ") {
		t.Errorf("line 1 should carry `file:4:` (no col, grouped), got: %q", lines[0])
	}
	if strings.Contains(lines[0], path+":4:2:") || strings.Contains(lines[0], path+":4:24:") {
		t.Errorf("line 1 must NOT include a column for multi-error groups, got: %q", lines[0])
	}
	// Color off: quote markers stripped from msg, no parens on hint,
	// trailing period stripped.
	if !strings.Contains(lines[0], "rule ascii cannot be applied to int") {
		t.Errorf("line 1 should carry first msg, got: %q", lines[0])
	}
	if !strings.Contains(lines[0], "drop the rule") {
		t.Errorf("line 1 should carry first hint inline, got: %q", lines[0])
	}
	// Line 2: continuation — second msg, no position prefix.
	if strings.Contains(lines[1], path+":") {
		t.Errorf("line 2 should NOT carry its own position (grouped), got: %q", lines[1])
	}
	if !strings.Contains(lines[1], "mod trim cannot be applied to int") {
		t.Errorf("line 2 should carry second msg, got: %q", lines[1])
	}
	if !strings.Contains(lines[1], "drop the mod") {
		t.Errorf("line 2 should carry second hint inline, got: %q", lines[1])
	}
	// Continuation must be indented to align under the first msg —
	// i.e. its prefix is spaces of width(len(`./foo.go:4: `)).
	expectIndent := len(path) + len(":4: ")
	if len(lines[1])-len(strings.TrimLeft(lines[1], " ")) != expectIndent {
		t.Errorf("line 2 indent width %d, want %d: %q",
			len(lines[1])-len(strings.TrimLeft(lines[1], " ")), expectIndent, lines[1])
	}
	// Line 3: source excerpt, tab-indented.
	if !strings.HasPrefix(lines[2], "\t") || !strings.Contains(lines[2], "N int") {
		t.Errorf("line 3 should be tab + source line, got: %q", lines[2])
	}
	// Line 4: caret row — exactly 2 carets (one per error span).
	if !strings.HasPrefix(lines[3], "\t") {
		t.Errorf("line 4 should be tab-indented, got: %q", lines[3])
	}
	if c := strings.Count(lines[3], "^"); c != 2 {
		t.Errorf("line 4 should carry exactly 2 carets, got %d: %q", c, lines[3])
	}
}

// TestPrettyLogger_DifferentLines_DoNotGroup pins the negative: only
// errors on the EXACT same (file, line) share a block. Different
// lines (or different files) get their own block.
func TestPrettyLogger_DifferentLines_DoNotGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := "package x\n\ntype A struct {\n\tN int `json:\"n\"`\n}\n\ntype B struct {\n\tM int `json:\"m\"`\n}\n"
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &richError{
		Pos: token.Position{Filename: path, Line: 4, Column: 2},
		Msg: "first", CodeSpan: "N",
	}
	b := &richError{
		Pos: token.Position{Filename: path, Line: 8, Column: 2},
		Msg: "second", CodeSpan: "M",
	}
	var buf bytes.Buffer
	log := &prettyLogger{level: LevelQuiet, w: &buf, color: false}
	log.Error(a)
	log.Error(b)
	log.Flush()
	out := buf.String()
	// Each error gets its own 3-line block (header + src + caret),
	// so we expect 6 total lines.
	if got := strings.Count(out, "\n"); got != 6 {
		t.Errorf("expected 6 newlines (two separate 3-line blocks), got %d:\n%s", got, out)
	}
	// Both position prefixes must appear separately.
	if !strings.Contains(out, ":4:2:") || !strings.Contains(out, ":8:2:") {
		t.Errorf("both position prefixes must appear, got:\n%s", out)
	}
}

// TestFlattenErrors_UnwrapsJoinedBatches exercises the flatten path
// that feeds groupByLine: errors.Join'd batches must be unwrapped
// into individual sub-errors so each can be considered for grouping.
func TestFlattenErrors_UnwrapsJoinedBatches(t *testing.T) {
	t.Parallel()
	a := errors.New("a")
	b := errors.New("b")
	c := errors.New("c")
	queue := []error{
		errors.Join(a, b),
		c,
	}
	flat := flattenErrors(queue)
	if len(flat) != 3 {
		t.Fatalf("expected 3 flattened, got %d", len(flat))
	}
	if flat[0] != a || flat[1] != b || flat[2] != c {
		t.Errorf("order broken: %v", flat)
	}
}

// TestGroupByLine_KeysOnFilenameAndLine pins the grouping key —
// neither column nor different files should collapse together.
func TestGroupByLine_KeysOnFilenameAndLine(t *testing.T) {
	t.Parallel()
	mk := func(file string, line, col int) *richError {
		return &richError{Pos: token.Position{Filename: file, Line: line, Column: col}, Msg: "x"}
	}
	errs := []error{
		mk("a.go", 4, 2),
		mk("a.go", 4, 7), // same file+line, different col → SAME group
		mk("a.go", 5, 2), // different line → new group
		mk("b.go", 4, 2), // different file → new group
		errors.New("bare"),
	}
	groups := groupByLine(errs)
	if len(groups) != 4 {
		t.Fatalf("expected 4 groups, got %d", len(groups))
	}
	// First group: 2 rich errors at a.go:4
	if len(groups[0].riches) != 2 {
		t.Errorf("group 0 should have 2 errs (a.go:4 col 2 + col 7), got %d", len(groups[0].riches))
	}
	// Second: 1 rich at a.go:5
	if len(groups[1].riches) != 1 || groups[1].pos.Line != 5 {
		t.Errorf("group 1 should be a.go:5, got %+v", groups[1])
	}
	// Third: 1 rich at b.go:4
	if len(groups[2].riches) != 1 || groups[2].pos.Filename != "b.go" {
		t.Errorf("group 2 should be b.go:4, got %+v", groups[2])
	}
	// Fourth: bare
	if groups[3].pos.IsValid() {
		t.Errorf("group 3 should be bare (pos invalid), got %+v", groups[3])
	}
}

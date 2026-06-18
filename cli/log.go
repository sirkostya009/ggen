package main

import (
	"bufio"
	"errors"
	"fmt"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Level controls the verbosity floor — only messages at or above the
// level are emitted. LevelQuiet is the default; -v / -vv / -vvv lift
// it to Info / Debug / Trace.
type Level int

const (
	LevelQuiet Level = iota
	LevelInfo
	LevelDebug
	LevelTrace
)

// Logger emits diagnostics for the CLI. Two concrete impls share this
// surface:
//
//   - conciseLogger: one line per record, machine-grep-able. Used in
//     CI runs, coding-agent shells, and non-TTY contexts. The agent-
//     facing hint (BotHint) is appended in parens so the caller can
//     reason about the technical cause.
//   - prettyLogger: multi-line, ANSI-coloured. Shows a Go-compiler-
//     style code excerpt with caret + a Note: line carrying the
//     human-actionable remedy.
//
// Two hints are intentionally distinct: the BotHint is a technical
// fact ("expected string"); the UserHint is a suggestion that may not
// always be the right move ("change the field type or remove the
// rule"). Mixing them would poison agents into treating the human
// suggestion as the only valid remedy.
type Logger interface {
	Info(format string, args ...any)
	Debug(format string, args ...any)
	Trace(format string, args ...any)
	// Error queues a non-fatal error for later batch emission. Errors
	// are NOT printed at the call site — the run continues, and all
	// queued errors are rendered together by Flush. This lets one
	// invocation surface every problem at once instead of bailing on
	// the first parsing/applicability failure.
	Error(err error)
	// Fatal renders the error immediately, flushes any previously
	// queued errors, and exits with status 1. Use for pre-condition
	// violations (bad CLI args, missing files) where continuing is
	// pointless.
	Fatal(err error)
	// Flush emits every error queued via Error in insertion order.
	// Subsequent calls are no-ops until more errors are queued.
	Flush()
	// HasErrors reports whether any error has been queued (or
	// printed via Fatal). main() exits non-zero based on this.
	HasErrors() bool
}

// NewLogger returns a Logger appropriate for the current process
// environment. CI runners and AI coding agents get the concise impl;
// interactive humans get the pretty one.
func NewLogger(level Level) Logger {
	if shouldUseConcise(os.Getenv, isTerminal(os.Stderr)) {
		return &conciseLogger{level: level, w: os.Stderr}
	}
	return &prettyLogger{level: level, w: os.Stderr, color: true}
}

// shouldUseConcise is split from NewLogger so tests can inject env
// vars and TTY state. Returns true when the caller is non-interactive
// (CI, agent, or piped stderr).
func shouldUseConcise(getenv func(string) string, stderrIsTTY bool) bool {
	for _, k := range ciEnvVars {
		if getenv(k) != "" {
			return true
		}
	}
	for _, k := range agentEnvVars {
		if getenv(k) != "" {
			return true
		}
	}
	return !stderrIsTTY
}

// ciEnvVars are environment variables whose presence (any non-empty
// value) signals "running under a CI runner". CI itself is set by
// most Linux-CI vendors; the rest cover specific systems that don't.
var ciEnvVars = []string{
	"CI",
	"CONTINUOUS_INTEGRATION",
	"GITHUB_ACTIONS",
	"GITLAB_CI",
	"CIRCLECI",
	"JENKINS_HOME",
	"BUILDKITE",
	"TRAVIS",
	"APPVEYOR",
	"TF_BUILD",
	"TEAMCITY_VERSION",
}

// agentEnvVars are markers set by interactive coding agents that
// drive shell commands programmatically. The set is curated, not
// exhaustive — add new entries as agents adopt env markers.
var agentEnvVars = []string{
	"AI_AGENT",        // generic cross-vendor (Claude Code et al.)
	"CLAUDECODE",      // Anthropic's Claude Code
	"CURSOR_TRACE_ID", // Cursor IDE agent mode
	"AIDER_AUTO_COMMITS",
}

// isTerminal reports whether f refers to a terminal device. Uses the
// stat-mode bit rather than ioctl so it's portable and dep-free.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// relPath returns p relative to the current working directory when
// that yields a shorter, less surprising path. Sibling paths get a
// "./" prefix so they're unambiguously paths (not package paths) and
// stay clickable in editors. Paths outside the cwd subtree fall back
// to the absolute form when the relative one would traverse upward
// more than the abs path's own length.
func relPath(p string) string {
	if p == "" {
		return p
	}
	cwd, err := os.Getwd()
	if err != nil {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil {
		return p
	}
	if !strings.HasPrefix(rel, ".") {
		rel = "./" + rel
	}
	// Heavy "../../" traversal usually means we're outside the
	// project root — the absolute form is easier to read.
	if len(rel) > len(abs) {
		return abs
	}
	return rel
}

// formatPos renders a token.Position with the filename made relative
// to cwd. Both loggers use this in place of the default token.Position
// String() method (which always emits the absolute filename).
func formatPos(pos token.Position) string {
	if !pos.IsValid() {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d", relPath(pos.Filename), pos.Line, pos.Column)
}

// ----- concise impl -----

type conciseLogger struct {
	level Level
	w     io.Writer
	// mu protects every field below — the CLI walks packages in
	// parallel and each goroutine may emit logs / queue errors.
	mu     sync.Mutex
	queue  []error // Error() appends here; Flush() drains
	errSet bool    // sticky: true once any error has been seen (queue or Fatal)
}

func (l *conciseLogger) Info(format string, args ...any) {
	if l.level < LevelInfo {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.w, "inf: %s\n", msg)
}

func (l *conciseLogger) Debug(format string, args ...any) {
	if l.level < LevelDebug {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.w, "dbg: %s\n", msg)
}

func (l *conciseLogger) Trace(format string, args ...any) {
	if l.level < LevelTrace {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.w, "trc: %s\n", msg)
}

func (l *conciseLogger) Error(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queue = append(l.queue, err)
	l.errSet = true
}

// renderError emits one error to the writer in concise format.
// Shared by Flush and Fatal. errors.Join'd batches are unwrapped and
// rendered one-per-line so the user sees every problem in the run.
func (l *conciseLogger) renderError(err error) {
	if subs, ok := unwrapMulti(err); ok {
		for _, sub := range subs {
			l.renderError(sub)
		}
		return
	}
	if re, ok := errors.AsType[*richError](err); ok {
		body := re.Msg
		if re.Pos.IsValid() {
			body = formatPos(re.Pos) + ": " + body
		}
		if re.BotHint != "" {
			body = body + " (" + re.BotHint + ")"
		}
		_, _ = fmt.Fprintf(l.w, "err: %s\n", body)
		return
	}
	_, _ = fmt.Fprintf(l.w, "err: %s\n", err)
}

// unwrapMulti returns the inner errors of an errors.Join batch (or any
// error implementing Unwrap() []error). Single errors return (nil, false)
// so callers fall through to the normal render path.
func unwrapMulti(err error) ([]error, bool) {
	type multi interface{ Unwrap() []error }
	if m, ok := err.(multi); ok {
		return m.Unwrap(), true
	}
	return nil, false
}

func (l *conciseLogger) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.queue {
		l.renderError(e)
	}
	l.queue = l.queue[:0]
}

func (l *conciseLogger) HasErrors() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.errSet
}

func (l *conciseLogger) Fatal(err error) {
	l.Flush()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renderError(err)
	l.errSet = true
	os.Exit(1)
}

// ----- pretty impl -----

type prettyLogger struct {
	level Level
	w     io.Writer
	color bool
	// mu protects every field below — the CLI walks packages in
	// parallel and each goroutine may emit logs / queue errors.
	mu     sync.Mutex
	queue  []error // Error() appends here; Flush() drains
	errSet bool    // sticky: true once any error has been seen
}

const (
	ansiReset    = "\x1b[0m"
	ansiBold     = "\x1b[1m"
	ansiRed      = "\x1b[31m" // dark red — used for caret + highlighted span
	ansiLightRed = "\x1b[91m" // bright/light red — used for the error message body
	ansiYellow   = "\x1b[33m"
	ansiCyan     = "\x1b[36m"
	ansiGreen    = "\x1b[32m"
	ansiGray     = "\x1b[90m"
)

func (l *prettyLogger) paint(c, s string) string {
	if !l.color {
		return s
	}
	return c + s + ansiReset
}

func (l *prettyLogger) Info(format string, args ...any) {
	if l.level < LevelInfo {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.w, "%s %s\n", l.paint(ansiGreen+ansiBold, "✓"), msg)
}

func (l *prettyLogger) Debug(format string, args ...any) {
	if l.level < LevelDebug {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.w, "%s %s\n", l.paint(ansiYellow, "[debug]"), msg)
}

func (l *prettyLogger) Trace(format string, args ...any) {
	if l.level < LevelTrace {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.w, "%s %s\n", l.paint(ansiGreen, "[trace]"), msg)
}

func (l *prettyLogger) Error(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queue = append(l.queue, err)
	l.errSet = true
}

func (l *prettyLogger) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	groups := groupByLine(flattenErrors(l.queue))
	for _, g := range groups {
		l.renderUnit(g)
	}
	l.queue = l.queue[:0]
}

func (l *prettyLogger) HasErrors() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.errSet
}

func (l *prettyLogger) Fatal(err error) {
	l.mu.Lock()
	l.errSet = true
	l.queue = append(l.queue, err)
	l.mu.Unlock()
	l.Flush()
	os.Exit(1)
}

// posKey collapses a token.Position to filename+line — the dimension
// we group on. Column varies per error within the same source line.
type posKey struct {
	filename string
	line     int
}

// renderUnit is one logical block in the pretty output: either a
// group of rich errors at the same (file, line) — rendered as a
// single header + multi-caret block — or a single position-less /
// bare error rendered on its own.
type renderUnit struct {
	pos    token.Position // valid → grouped block; invalid → bare
	riches []*richError   // populated when pos.IsValid()
	bare   error          // populated when pos is invalid
}

// flattenErrors walks every top-level queue entry, recursively
// unwrapping errors.Join batches into a flat slice. Order from the
// queue is preserved (depth-first within each entry).
func flattenErrors(queue []error) []error {
	out := make([]error, 0, len(queue))
	var walk func(error)
	walk = func(err error) {
		if subs, ok := unwrapMulti(err); ok {
			for _, s := range subs {
				walk(s)
			}
			return
		}
		out = append(out, err)
	}
	for _, e := range queue {
		walk(e)
	}
	return out
}

// groupByLine partitions the flat error slice into ordered render
// units. Rich errors with valid positions share a unit when their
// (filename, line) matches a unit emitted earlier in the same Flush;
// position-less and bare errors get their own unit each.
func groupByLine(errs []error) []renderUnit {
	var groups []renderUnit
	index := make(map[posKey]int, len(errs))
	for _, e := range errs {
		var re *richError
		if errors.As(e, &re) && re.Pos.IsValid() {
			k := posKey{re.Pos.Filename, re.Pos.Line}
			if i, ok := index[k]; ok {
				groups[i].riches = append(groups[i].riches, re)
				continue
			}
			index[k] = len(groups)
			groups = append(groups, renderUnit{pos: re.Pos, riches: []*richError{re}})
			continue
		}
		groups = append(groups, renderUnit{bare: e})
	}
	return groups
}

// renderUnit emits one block of pretty output. Grouped (multi-error)
// units share a position header line + source excerpt + one caret
// line bearing N carets, one per error's CodeSpan column. Bare/
// position-less units fall back to the single-error path.
//
// Layout:
//
//	<bold cyan file:line[:col]>: <light-red Msg1> <gray (hint1)>
//	                             <light-red Msg2> <gray (hint2)>
//	\t<source line; spans highlighted>
//	\t<caret indent>^<between-pad>^...
//
// Two header-column subtleties:
//
//  1. Single-error groups get a full `file:line:col:` header where
//     `col` points at the offending CodeSpan inside the source line,
//     NOT at the field declaration that token.Position originally
//     marked. The field-decl column is correct for AST tooling but
//     wrong for a user clicking the path in an IDE.
//  2. Multi-error groups drop the column entirely — `file:line:` —
//     since the errors point at different columns and there's no
//     single "the" column to display in the header. Each cause is
//     pinned by its caret on the line below.
func (l *prettyLogger) renderUnit(u renderUnit) {
	if !u.pos.IsValid() {
		l.renderBare(u.bare)
		return
	}
	first := u.riches[0]
	// Read source first — needed both for the col override below and
	// the excerpt rendered after.
	line, srcOK := readSourceLine(first.Pos.Filename, first.Pos.Line)
	var prefix string
	if len(u.riches) > 1 {
		// Multi-error: no single column makes sense; carets disambiguate.
		prefix = fmt.Sprintf("%s:%d: ", relPath(first.Pos.Filename), first.Pos.Line)
	} else {
		col := first.Pos.Column
		if srcOK {
			col = resolveSpanCol(line, first.Pos.Column, first.CodeSpan, first.Anchor)
		}
		prefix = fmt.Sprintf("%s:%d:%d: ", relPath(first.Pos.Filename), first.Pos.Line, col)
	}
	_, _ = fmt.Fprintf(l.w, "%s%s\n",
		l.paint(ansiBold+ansiCyan, prefix),
		l.formatMsg(first))
	// Continuation messages: indented under the first message column
	// so the eye reads them as siblings of the header msg, not as
	// new diagnostics.
	contIndent := strings.Repeat(" ", len(prefix))
	for _, re := range u.riches[1:] {
		_, _ = fmt.Fprintf(l.w, "%s%s\n", contIndent, l.formatMsg(re))
	}
	// Source + multi-caret. Skipped when source isn't readable.
	if srcOK {
		_, _ = fmt.Fprintf(l.w, "\t%s\n", l.highlightSpans(line, u.riches))
		_, _ = fmt.Fprintf(l.w, "\t%s\n", l.multiCaretLine(line, u.riches))
	}
}

// renderBare renders a position-less rich error or a non-rich error
// on a single line. Used for bucketed-out entries that don't fit the
// position-grouped layout.
func (l *prettyLogger) renderBare(err error) {
	var re *richError
	if !errors.As(err, &re) {
		_, _ = fmt.Fprintf(l.w, "%s\n", err.Error())
		return
	}
	_, _ = fmt.Fprintf(l.w, "%s\n", l.formatMsg(re))
}

// formatMsg builds the `<light-red Msg> <gray (UserHint)>` body used
// by both the header line and continuation lines. Both Msg and
// UserHint are routed through emphasize() so backtick- and
// double-quote-delimited identifiers get promoted to bold (and the
// markers themselves stripped). UserHint is gray and renders without
// surrounding parens — the muted color is enough visual separation.
func (l *prettyLogger) formatMsg(re *richError) string {
	out := l.emphasize(re.Msg, ansiLightRed)
	if re.UserHint != "" {
		// Drop every period in hints — the gray run already provides
		// visual end-of-thought; sentence punctuation is dead weight.
		// Source hints stay readable when written in plain English
		// because abbreviations like "e.g." are pre-stripped here.
		hint := strings.ReplaceAll(re.UserHint, ".", "")
		out = out + " " + l.emphasize(hint, ansiGray)
	}
	return out
}

// emphasize transforms emphasis markers in s and returns a styled
// string. Two markers are recognised:
//
//	`ident`   — Markdown-style code span.
//	"ident"   — Go fmt-style identifier quoting (%q output).
//
// Both render as the inner text in bold, with the markers stripped.
// The non-emphasized text uses baseColor; emphasized segments break
// out to bold-default-fg so they stand out against the surrounding
// hue. When color is off, all markers are stripped and the result
// is plain text (no ANSI). Unbalanced markers are tolerated — the
// trailing reset closes any open run gracefully.
func (l *prettyLogger) emphasize(s, baseColor string) string {
	if !l.color {
		// Plain-text fallback: strip markers so the sentence reads
		// naturally. `foo` becomes foo; "foo" becomes foo.
		var b strings.Builder
		b.Grow(len(s))
		for i := 0; i < len(s); i++ {
			if s[i] == '`' || s[i] == '"' {
				continue
			}
			b.WriteByte(s[i])
		}
		return b.String()
	}
	// `\x1b[1m` enables bold without touching foreground colour, so
	// the base hue (light-red for msg, gray for hint) carries through
	// the emphasized span. `\x1b[22m` disables bold while leaving the
	// colour alone — using ansiReset here would drop both, painting
	// the emphasized text in the terminal's default fg (often near-
	// black, which is exactly what we don't want against gray hints).
	const boldOff = "\x1b[22m"
	var b strings.Builder
	b.WriteString(baseColor)
	var open byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if open == 0 && (c == '`' || c == '"') {
			b.WriteString(ansiBold)
			open = c
			continue
		}
		if open != 0 && c == open {
			b.WriteString(boldOff)
			open = 0
			continue
		}
		b.WriteByte(c)
	}
	b.WriteString(ansiReset)
	return b.String()
}

// highlightSpans wraps every error's CodeSpan in red+bold ANSI when
// color is enabled. Spans are looked up from each error's
// Pos.Column-1 (so identical text earlier on the line isn't mistaken
// for the cause) and applied in column order, with overlapping ranges
// merged to keep the output well-formed.
func (l *prettyLogger) highlightSpans(line string, errs []*richError) string {
	if !l.color {
		return line
	}
	type span struct{ start, end int }
	var spans []span
	for _, re := range errs {
		if re.CodeSpan == "" {
			continue
		}
		// resolveSpanCol returns 1-indexed; subtract to get the byte
		// offset into line. Anchor consumed before searching CodeSpan.
		col := resolveSpanCol(line, re.Pos.Column, re.CodeSpan, re.Anchor) - 1
		if col < 0 || col >= len(line) {
			continue
		}
		end := min(col+len(re.CodeSpan), len(line))
		spans = append(spans, span{col, end})
	}
	if len(spans) == 0 {
		return line
	}
	slices.SortFunc(spans, func(a, b span) int { return a.start - b.start })
	// Merge overlaps so we don't emit half-open ANSI ranges.
	merged := spans[:1]
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if s.start <= last.end {
			if s.end > last.end {
				last.end = s.end
			}
			continue
		}
		merged = append(merged, s)
	}
	var b strings.Builder
	pos := 0
	for _, s := range merged {
		b.WriteString(line[pos:s.start])
		b.WriteString(ansiRed + ansiBold)
		b.WriteString(line[s.start:s.end])
		b.WriteString(ansiReset)
		pos = s.end
	}
	b.WriteString(line[pos:])
	return b.String()
}

// multiCaretLine builds a single caret-row spanning one `^` per
// error column. Whitespace bytes from the source are mirrored
// verbatim so tabs in the source align with tabs in the caret line.
// When errors share a column (rare — typically the user duplicated
// a rule), the caret renders once at that column.
func (l *prettyLogger) multiCaretLine(line string, errs []*richError) string {
	cols := make([]int, len(errs))
	for i, re := range errs {
		col := resolveSpanCol(line, re.Pos.Column, re.CodeSpan, re.Anchor) - 1
		cols[i] = min(max(col, 0), len(line))
	}
	slices.Sort(cols)
	cols = slices.Compact(cols)
	var b strings.Builder
	pos := 0
	for _, c := range cols {
		for i := pos; i < c; i++ {
			if i < len(line) && line[i] == '\t' {
				b.WriteByte('\t')
			} else {
				b.WriteByte(' ')
			}
		}
		b.WriteString(l.paint(ansiRed+ansiBold, "^"))
		pos = c + 1
	}
	return b.String()
}

// resolveSpanCol does the actual column-search work, with optional
// Anchor support. The anchor is a disambiguating prefix the caller
// trusts to appear before the codeSpan target — useful when codeSpan
// alone is a short string that collides with other text earlier on
// the line. Anchor is consumed (search advances past it) but the
// returned column points at codeSpan, not at the anchor.
func resolveSpanCol(line string, posCol int, codeSpan, anchor string) int {
	col := max(posCol-1, 0)
	if anchor != "" && col < len(line) {
		if i := strings.Index(line[col:], anchor); i >= 0 {
			col += i + len(anchor)
		}
	}
	if codeSpan != "" && col < len(line) {
		if i := strings.Index(line[col:], codeSpan); i >= 0 {
			col += i
		}
	}
	return col + 1
}

// caretIndent builds the whitespace prefix for the line under the
// source excerpt so the caret lands directly under the offending
// token. Three quirks that make this non-trivial:
//
//  1. The prefix `./file.go:line:col: ` appears before the source on
//     the previous line; the caret line needs an equal-width run of
//     spaces.
//  2. Source lines often begin with tabs; replacing tabs with spaces
//     in the indent would mis-align the caret in any terminal where a
//     tab is rendered as N columns. Reuse the line's whitespace bytes
//     verbatim, mapping non-whitespace to a single space.
//  3. Pos.Column points at the field declaration, but the offender
//     (CodeSpan = rule name / bad value) typically lives inside the
//     struct tag a few columns later. Search for CodeSpan starting at
//     Pos.Column so the caret lands on the actual cause.
func caretIndent(line, prefix string, posCol int, span, anchor string) string {
	col := min(max(resolveSpanCol(line, posCol, span, anchor)-1, 0), len(line))
	var b strings.Builder
	b.Grow(len(prefix) + col)
	for i := 0; i < len(prefix); i++ {
		b.WriteByte(' ')
	}
	for i := range col {
		if line[i] == '\t' {
			b.WriteByte('\t')
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// highlightSpan returns line with the first occurrence of span
// wrapped in red+bold. Empty span (or span absent from line) leaves
// the line unchanged. Used to draw the eye to the offending substring
// in a code excerpt — the caret below pins the column, this colours
// the cause.
func (l *prettyLogger) highlightSpan(line, span string) string {
	if span == "" || !l.color {
		return line
	}
	before, after, ok := strings.Cut(line, span)
	if !ok {
		return line
	}
	return before + ansiRed + ansiBold + span + ansiReset + after
}

// ----- source line reader -----

// sourceLineCache memoises file reads so multiple errors from the
// same file don't re-open it. Bounded informally by typical run size
// (a single ggen invocation rarely touches > 100 files).
var sourceLineCache struct {
	mu    sync.Mutex
	files map[string][]string
}

// readSourceLine returns the 1-indexed line N from filename, or
// (empty, false) on any read / range error. Read failures are not
// fatal — the renderer falls back to a position-only display.
func readSourceLine(filename string, line int) (string, bool) {
	if filename == "" || line < 1 {
		return "", false
	}
	sourceLineCache.mu.Lock()
	defer sourceLineCache.mu.Unlock()
	if sourceLineCache.files == nil {
		sourceLineCache.files = map[string][]string{}
	}
	lines, ok := sourceLineCache.files[filename]
	if !ok {
		f, err := os.Open(filename)
		if err != nil {
			return "", false
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		// Default scanner buffer (64KiB) is enough for any sane Go
		// source line; grow if needed in the future.
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		if err := sc.Err(); err != nil {
			return "", false
		}
		sourceLineCache.files[filename] = lines
	}
	if line > len(lines) {
		return "", false
	}
	return lines[line-1], true
}

// ----- richError -----

// richError is the structured error type both log impls render. It
// separates the *what* (Msg + Pos), the *bot context* (BotHint, shown
// inline to agents/CI), and the *human remedy* (UserHint, shown as a
// Note: line in pretty mode). Keeping the two hints distinct is
// deliberate: the UserHint may suggest one of several valid fixes,
// and surfacing it to an agent as authoritative would railroad
// downstream tool decisions.
type richError struct {
	Pos      token.Position // file:line:col; zero value when unknown
	Msg      string         // main error message — what failed
	CodeSpan string         // substring within the source line to highlight + point caret at
	Anchor   string         // disambiguating prefix used for caret positioning ONLY; not highlighted. When set, the position search finds Anchor first, then searches for CodeSpan AFTER it. Use when CodeSpan is a short token that may collide with other occurrences earlier on the line (e.g. unknown-rule name `b` collides with `json:"b"`).
	BotHint  string         // technical context for concise/agent output
	UserHint string         // remedy suggestion for human output (Note:)
	Err      error          // optional underlying error for errors.Unwrap
}

func (e *richError) Error() string {
	var b strings.Builder
	if e.Pos.IsValid() {
		b.WriteString(formatPos(e.Pos))
		b.WriteString(": ")
	}
	b.WriteString(e.Msg)
	if e.BotHint != "" {
		b.WriteString(" (")
		b.WriteString(e.BotHint)
		b.WriteString(")")
	}
	return b.String()
}

func (e *richError) Unwrap() error { return e.Err }

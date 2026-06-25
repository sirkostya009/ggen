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

// Level controls the verbosity floor — only messages at or above it are
// emitted. -v / -vv / -vvv lift LevelQuiet to Info / Debug / Trace.
type Level int

const (
	LevelQuiet Level = iota
	LevelInfo
	LevelDebug
	LevelTrace
)

// Logger emits diagnostics for the CLI. Two impls share this surface:
// conciseLogger (one grep-able line per record, for CI / agents / non-TTY,
// with BotHint appended in parens) and prettyLogger (multi-line, ANSI, with a
// code excerpt + caret + a Note: line carrying UserHint). BotHint and UserHint
// stay distinct: a technical fact vs a human remedy that may be one of several
// valid fixes — surfacing the latter to agents as authoritative would railroad
// downstream decisions.
type Logger interface {
	Info(format string, args ...any)
	Debug(format string, args ...any)
	Trace(format string, args ...any)
	// Error queues a non-fatal error for batch emission by Flush — the run
	// continues so one invocation surfaces every problem at once.
	Error(err error)
	// Fatal renders the error, flushes the queue, and exits 1. For
	// pre-condition violations where continuing is pointless.
	Fatal(err error)
	// Flush emits every queued error in insertion order, then clears the queue.
	Flush()
	// HasErrors reports whether any error was queued or printed via Fatal.
	HasErrors() bool
}

// NewLogger returns the concise impl under CI / agents / non-TTY, the pretty
// impl for interactive humans.
func NewLogger(level Level) Logger {
	if shouldUseConcise(os.Getenv, isTerminal(os.Stderr)) {
		return &conciseLogger{level: level, w: os.Stderr}
	}
	return &prettyLogger{level: level, w: os.Stderr, color: true}
}

// shouldUseConcise is split from NewLogger so tests can inject env vars and TTY
// state. True when the caller is non-interactive (CI, agent, or piped stderr).
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

// ciEnvVars: a non-empty value on any of these signals a CI runner.
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

// agentEnvVars are markers set by coding agents that drive shells
// programmatically. Curated, not exhaustive.
var agentEnvVars = []string{
	"AI_AGENT",        // generic cross-vendor (Claude Code et al.)
	"CLAUDECODE",      // Anthropic's Claude Code
	"CURSOR_TRACE_ID", // Cursor IDE agent mode
	"AIDER_AUTO_COMMITS",
}

// isTerminal reports whether f is a terminal device, via the stat-mode bit
// (portable, dep-free) rather than ioctl.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// relPath returns p relative to cwd when that's shorter, with a "./" prefix on
// siblings so they read as paths (not package paths) and stay editor-clickable.
// Falls back to absolute when the relative form is longer (heavy "../" climb).
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
	if len(rel) > len(abs) {
		return abs
	}
	return rel
}

// formatPos renders a token.Position with the filename relative to cwd
// (token.Position.String always emits the absolute filename).
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
	// mu protects every field below — packages are walked in parallel.
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

// renderError emits one error in concise format (shared by Flush and Fatal).
// errors.Join'd batches are unwrapped and rendered one-per-line.
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
// Unwrap() []error). Single errors return (nil, false).
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
	// mu protects every field below — packages are walked in parallel.
	mu     sync.Mutex
	queue  []error // Error() appends here; Flush() drains
	errSet bool    // sticky: true once any error has been seen
}

const (
	ansiReset    = "\x1b[0m"
	ansiBold     = "\x1b[1m"
	ansiRed      = "\x1b[31m" // caret + highlighted span
	ansiLightRed = "\x1b[91m" // error message body
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

// posKey collapses a token.Position to filename+line — the grouping dimension.
type posKey struct {
	filename string
	line     int
}

// renderUnit is one block of pretty output: a group of rich errors at the same
// (file, line), or a single position-less / bare error.
type renderUnit struct {
	pos    token.Position // valid → grouped block; invalid → bare
	riches []*richError   // populated when pos.IsValid()
	bare   error          // populated when pos is invalid
}

// flattenErrors recursively unwraps errors.Join batches into a flat slice,
// preserving queue order (depth-first within each entry).
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

// groupByLine partitions the flat error slice into ordered render units. Rich
// errors share a unit by (filename, line); position-less / bare errors each
// get their own.
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

// renderUnit emits one block of pretty output: a position header, a
// span-highlighted source excerpt, and a caret line (one caret per error's
// CodeSpan column for grouped units). Bare units fall back to renderBare.
// Single-error groups get a full `file:line:col:` header where col points at
// the CodeSpan (not the field decl token.Position marked); multi-error groups
// drop the column — carets disambiguate.
func (l *prettyLogger) renderUnit(u renderUnit) {
	if !u.pos.IsValid() {
		l.renderBare(u.bare)
		return
	}
	first := u.riches[0]
	line, srcOK := readSourceLine(first.Pos.Filename, first.Pos.Line)
	var prefix string
	if len(u.riches) > 1 {
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
	// Continuation messages indented under the header msg as siblings.
	contIndent := strings.Repeat(" ", len(prefix))
	for _, re := range u.riches[1:] {
		_, _ = fmt.Fprintf(l.w, "%s%s\n", contIndent, l.formatMsg(re))
	}
	if srcOK {
		_, _ = fmt.Fprintf(l.w, "\t%s\n", l.highlightSpans(line, u.riches))
		_, _ = fmt.Fprintf(l.w, "\t%s\n", l.multiCaretLine(line, u.riches))
	}
}

// renderBare renders a position-less rich error or a non-rich error on a
// single line.
func (l *prettyLogger) renderBare(err error) {
	var re *richError
	if !errors.As(err, &re) {
		_, _ = fmt.Fprintf(l.w, "%s\n", err.Error())
		return
	}
	_, _ = fmt.Fprintf(l.w, "%s\n", l.formatMsg(re))
}

// formatMsg builds the `<light-red Msg> <gray UserHint>` body for header and
// continuation lines. Msg and UserHint route through emphasize() (backtick /
// double-quote identifiers → bold, markers stripped).
func (l *prettyLogger) formatMsg(re *richError) string {
	out := l.emphasize(re.Msg, ansiLightRed)
	if re.UserHint != "" {
		// Drop periods — the gray run is enough end-of-thought separation.
		hint := strings.ReplaceAll(re.UserHint, ".", "")
		out = out + " " + l.emphasize(hint, ansiGray)
	}
	return out
}

// emphasize renders s with `ident` (Markdown code span) and "ident" (%q) both
// bolded and their markers stripped; non-emphasized text uses baseColor. With
// color off, markers are stripped to plain text. Unbalanced markers tolerated.
func (l *prettyLogger) emphasize(s, baseColor string) string {
	if !l.color {
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
	// boldOff (\x1b[22m) disables bold while keeping the base hue; ansiReset
	// would drop the colour too, painting the span in the terminal default fg.
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

// highlightSpans wraps every error's CodeSpan in red+bold, looked up from each
// Pos.Column-1, applied in column order with overlaps merged.
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
		// resolveSpanCol is 1-indexed; subtract for the byte offset.
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
	// Merge overlaps to avoid half-open ANSI ranges.
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

// multiCaretLine builds a caret row with one `^` per error column. Source
// whitespace is mirrored verbatim so tabs align; shared columns render once.
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

// resolveSpanCol finds codeSpan's 1-indexed column. An optional anchor (a
// disambiguating prefix known to precede codeSpan) is consumed first, so a
// short codeSpan that collides earlier on the line still resolves correctly.
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

// caretIndent builds the whitespace prefix so the caret lands under the
// offending token: the header-prefix width as spaces, then the source's own
// whitespace bytes verbatim (so tabs align) up to the resolved CodeSpan column.
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

// highlightSpan wraps the first occurrence of span in red+bold; an empty or
// absent span leaves the line unchanged.
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

// sourceLineCache memoises file reads so multiple errors from one file don't
// re-open it.
var sourceLineCache struct {
	mu    sync.Mutex
	files map[string][]string
}

// readSourceLine returns the 1-indexed line N from filename, or (empty, false)
// on any read / range error (non-fatal — the renderer drops the excerpt).
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

// richError is the structured error type both log impls render. It separates
// the what (Msg + Pos), the technical context (BotHint, inline for agents/CI),
// and the human remedy (UserHint, a Note: line in pretty mode). See Logger.
type richError struct {
	Pos      token.Position // file:line:col; zero value when unknown
	Msg      string         // main error message — what failed
	CodeSpan string         // substring within the source line to highlight + point caret at
	Anchor   string         // disambiguating prefix searched before CodeSpan (positioning only, not highlighted) when CodeSpan is short enough to collide earlier on the line
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

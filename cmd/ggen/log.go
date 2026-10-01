package main

import (
	"errors"
	"fmt"
	"go/token"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/sirkostya009/ggen/gen/model"
)

// Level is the logger's verbosity. A message is printed only when the
// logger's level is at or above the message's level. -v, -vv and -vvv raise
// the default LevelQuiet to LevelInfo, LevelDebug and LevelTrace.
type Level int

const (
	LevelQuiet Level = iota
	LevelInfo
	LevelDebug
	LevelTrace
)

// Logger prints diagnostics for the CLI. It has two implementations.
// conciseLogger prints one grep-friendly line per record, with BotHint in
// parentheses. It is for CI, coding agents and non-terminal output.
// prettyLogger prints colored, multi-line output with a source excerpt, a
// caret under the problem and the UserHint. BotHint and UserHint are kept
// separate on purpose. BotHint is a technical fact. UserHint is a suggested
// fix for a human, and it may be only one of several valid fixes. Showing it
// to an agent as the one answer would push the agent toward that fix.
type Logger interface {
	Infof(format string, args ...any)
	Debugf(format string, args ...any)
	Tracef(format string, args ...any)
	// Error queues a non-fatal error for Flush to print. The run goes on, so
	// one invocation reports every problem at once.
	Error(err error)
	// Fatal prints the queued errors and err, then exits with status 1. Use
	// it when continuing makes no sense.
	Fatal(err error)
	// Flush prints every queued error in the order it was added, then clears
	// the queue.
	Flush()
	// HasErrors reports whether any error was queued or passed to Fatal.
	HasErrors() bool
}

// NewLogger returns the concise logger under CI, coding agents or when stderr
// is not a terminal. It returns the pretty logger for a human at a terminal.
func NewLogger(level Level) Logger {
	if shouldUseConcise(os.Getenv, isTerminal(os.Stderr)) {
		return &conciseLogger{level: level, w: os.Stderr}
	}
	return &prettyLogger{level: level, w: os.Stderr, color: true}
}

// shouldUseConcise reports whether the caller is non-interactive: CI, a coding
// agent, or stderr that is not a terminal. It is separate from NewLogger so
// tests can pass their own env vars and terminal state.
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

// ciEnvVars are env vars that CI runners set. A non-empty value in any of
// them means the run is in CI.
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

// agentEnvVars are env vars that coding agents set when they run shell
// commands. The list is hand-picked and not complete.
var agentEnvVars = []string{
	"AI_AGENT",        // generic, set by several agents including Claude Code
	"CLAUDECODE",      // Anthropic's Claude Code
	"CURSOR_TRACE_ID", // Cursor IDE agent mode
	"AIDER_AUTO_COMMITS",
}

// isTerminal reports whether f is a terminal device. It checks the file mode
// bit instead of calling ioctl, which keeps it portable and free of
// dependencies.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// ----- concise logger -----

type conciseLogger struct {
	level Level
	w     io.Writer
	// mu guards every field below, so the logger is safe for concurrent use.
	mu     sync.Mutex
	queue  []error // Error appends, Flush drains
	errSet bool    // set once any error is seen, through Error or Fatal
}

func (l *conciseLogger) Infof(format string, args ...any) {
	if l.level < LevelInfo {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.w, "inf: %s\n", msg)
}

func (l *conciseLogger) Debugf(format string, args ...any) {
	if l.level < LevelDebug {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.w, "dbg: %s\n", msg)
}

func (l *conciseLogger) Tracef(format string, args ...any) {
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

// renderError prints one error in concise format, for Flush and Fatal. An
// error made by errors.Join is unwrapped and printed one error per line.
func (l *conciseLogger) renderError(err error) {
	if subs, ok := unwrapMulti(err); ok {
		for _, sub := range subs {
			l.renderError(sub)
		}
		return
	}
	if re, ok := errors.AsType[*model.RichError](err); ok {
		body := re.Msg
		if re.Pos.IsValid() {
			body = model.FormatPos(re.Pos) + ": " + body
		}
		if re.BotHint != "" {
			body = body + " (" + re.BotHint + ")"
		}
		_, _ = fmt.Fprintf(l.w, "err: %s\n", body)
		return
	}
	_, _ = fmt.Fprintf(l.w, "err: %s\n", err)
}

// unwrapMulti returns the inner errors of an error that has an
// Unwrap() []error method, such as one made by errors.Join. For any other
// error it returns (nil, false).
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
	l.renderError(err)
	l.errSet = true
	l.mu.Unlock()
	os.Exit(1)
}

// ----- pretty logger -----

type prettyLogger struct {
	level Level
	w     io.Writer
	color bool
	// mu guards every field below, so the logger is safe for concurrent use.
	mu     sync.Mutex
	queue  []error // Error appends, Flush drains
	errSet bool    // set once any error is seen
}

const (
	ansiReset    = "\x1b[0m"
	ansiBold     = "\x1b[1m"
	ansiRed      = "\x1b[31m" // caret and highlighted span
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

func (l *prettyLogger) Infof(format string, args ...any) {
	if l.level < LevelInfo {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.w, "%s %s\n", l.paint(ansiGreen+ansiBold, "✓"), msg)
}

func (l *prettyLogger) Debugf(format string, args ...any) {
	if l.level < LevelDebug {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.w, "%s %s\n", l.paint(ansiYellow, "[debug]"), msg)
}

func (l *prettyLogger) Tracef(format string, args ...any) {
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

// posKey is a token.Position reduced to file name and line. Errors are
// grouped by it.
type posKey struct {
	filename string
	line     int
}

// renderUnit is one block of pretty output. It is either a group of rich
// errors on the same file and line, or one error with no position.
type renderUnit struct {
	pos    token.Position     // valid for a group, invalid for a bare error
	riches []*model.RichError // populated when pos.IsValid()
	bare   error              // populated when pos is invalid
}

// flattenErrors unwraps joined errors, at any depth, into a flat slice. It
// keeps the queue order and walks each entry depth-first.
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

// groupByLine splits the flat error slice into render units, in order. Rich
// errors on the same file and line share a unit. Every error without a
// position gets its own unit.
func groupByLine(errs []error) []renderUnit {
	var groups []renderUnit
	index := make(map[posKey]int, len(errs))
	for _, e := range errs {
		var re *model.RichError
		if errors.As(e, &re) && re.Pos.IsValid() {
			k := posKey{re.Pos.Filename, re.Pos.Line}
			if i, ok := index[k]; ok {
				groups[i].riches = append(groups[i].riches, re)
				continue
			}
			index[k] = len(groups)
			groups = append(groups, renderUnit{pos: re.Pos, riches: []*model.RichError{re}})
			continue
		}
		groups = append(groups, renderUnit{bare: e})
	}
	return groups
}

// renderUnit prints one block of pretty output: a position header, the
// source line with the error spans highlighted, and a caret line with one
// caret per error's CodeSpan column. Bare units go to renderBare. A group
// with one error gets a full `file:line:col:` header, where col points at the
// CodeSpan, not at the field declaration that token.Position marks. A group
// with several errors leaves out the column, because the carets show where
// each one is.
func (l *prettyLogger) renderUnit(u renderUnit) {
	if !u.pos.IsValid() {
		l.renderBare(u.bare)
		return
	}
	first := u.riches[0]
	line, srcOK := model.ReadSourceLine(first.Pos.Filename, first.Pos.Line)
	var prefix string
	if len(u.riches) > 1 {
		prefix = fmt.Sprintf("%s:%d: ", model.RelPath(first.Pos.Filename), first.Pos.Line)
	} else {
		col := first.Pos.Column
		if srcOK {
			col = model.ResolveSpanCol(line, first.Pos.Column, first.CodeSpan, first.Anchor)
		}
		prefix = fmt.Sprintf("%s:%d:%d: ", model.RelPath(first.Pos.Filename), first.Pos.Line, col)
	}
	_, _ = fmt.Fprintf(l.w, "%s%s\n",
		l.paint(ansiBold+ansiCyan, prefix),
		l.formatMsg(first))
	// Indent the other messages so they line up under the first one.
	contIndent := strings.Repeat(" ", len(prefix))
	for _, re := range u.riches[1:] {
		_, _ = fmt.Fprintf(l.w, "%s%s\n", contIndent, l.formatMsg(re))
	}
	if srcOK {
		_, _ = fmt.Fprintf(l.w, "\t%s\n", l.highlightSpans(line, u.riches))
		_, _ = fmt.Fprintf(l.w, "\t%s\n", l.multiCaretLine(line, u.riches))
	}
}

// renderBare prints a rich error without a position, or a plain error, on
// one line.
func (l *prettyLogger) renderBare(err error) {
	var re *model.RichError
	if !errors.As(err, &re) {
		_, _ = fmt.Fprintf(l.w, "%s\n", err.Error())
		return
	}
	_, _ = fmt.Fprintf(l.w, "%s\n", l.formatMsg(re))
}

// formatMsg builds the message text for header and continuation lines: Msg
// in light red, then UserHint in gray. Both go through emphasize, which
// prints quoted identifiers in bold and removes the quotes.
func (l *prettyLogger) formatMsg(re *model.RichError) string {
	out := l.emphasize(re.Msg, ansiLightRed)
	if re.UserHint != "" {
		// Drop periods. The gray color already separates the hint.
		hint := strings.ReplaceAll(re.UserHint, ".", "")
		out = out + " " + l.emphasize(hint, ansiGray)
	}
	return out
}

// emphasize prints identifiers quoted in s as `ident` or "ident" in bold and
// removes the quotes. Other text uses baseColor. With color off, it only
// removes the quotes. A quote with no closing match is handled without error.
func (l *prettyLogger) emphasize(s, baseColor string) string {
	if !l.color {
		var b strings.Builder
		b.Grow(len(s))
		for i := range len(s) {
			if s[i] == '`' || s[i] == '"' {
				continue
			}
			b.WriteByte(s[i])
		}
		return b.String()
	}
	// boldOff (\x1b[22m) turns bold off but keeps the base color. ansiReset
	// would also drop the color and show the rest in the terminal's default
	// color.
	const boldOff = "\x1b[22m"
	var b strings.Builder
	b.WriteString(baseColor)
	var open byte
	for i := range len(s) {
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

// highlightSpans makes every error's CodeSpan red and bold. Each span is
// found from its Pos.Column. Spans are applied in column order, and
// overlapping spans are merged.
func (l *prettyLogger) highlightSpans(line string, errs []*model.RichError) string {
	if !l.color {
		return line
	}
	type span struct{ start, end int }
	var spans []span
	for _, re := range errs {
		if re.CodeSpan == "" {
			continue
		}
		// ResolveSpanCol counts from 1. Subtract 1 to get the byte offset.
		col := model.ResolveSpanCol(line, re.Pos.Column, re.CodeSpan, re.Anchor) - 1
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
	// Merge overlapping spans so color codes do not nest or break.
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

// multiCaretLine builds a caret line with one `^` per error column. Tabs in
// the source are copied so the carets line up. Errors on the same column share
// one caret.
func (l *prettyLogger) multiCaretLine(line string, errs []*model.RichError) string {
	cols := make([]int, len(errs))
	for i, re := range errs {
		col := model.ResolveSpanCol(line, re.Pos.Column, re.CodeSpan, re.Anchor) - 1
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

// caretIndent builds the whitespace that puts the caret under the bad token.
// It is one space per byte of the header prefix, then one space or tab per
// source byte up to the CodeSpan column. Tabs are copied so they line up.
func caretIndent(line, prefix string, posCol int, span, anchor string) string {
	col := min(max(model.ResolveSpanCol(line, posCol, span, anchor)-1, 0), len(line))
	var b strings.Builder
	b.Grow(len(prefix) + col)
	for range len(prefix) {
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

// highlightSpan makes the first occurrence of span red and bold. If span is
// empty or not found, the line is returned unchanged.
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

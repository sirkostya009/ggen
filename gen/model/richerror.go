package model

import (
	"bufio"
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// RelPath returns p relative to cwd when that's shorter, with a "./" prefix on
// siblings so they read as paths (not package paths) and stay editor-clickable.
// Falls back to absolute when the relative form is longer (heavy "../" climb).
func RelPath(p string) string {
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

// FormatPos renders a token.Position with the filename relative to cwd
// (token.Position.String always emits the absolute filename).
func FormatPos(pos token.Position) string {
	if !pos.IsValid() {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d", RelPath(pos.Filename), pos.Line, pos.Column)
}

// ResolveSpanCol finds codeSpan's 1-indexed column. An optional anchor (a
// disambiguating prefix known to precede codeSpan) is consumed first, so a
// short codeSpan that collides earlier on the line still resolves correctly.
func ResolveSpanCol(line string, posCol int, codeSpan, anchor string) int {
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

// sourceLineCache memoises file reads so multiple errors from one file don't
// re-open it.
var sourceLineCache struct {
	mu    sync.Mutex
	files map[string][]string
}

// ReadSourceLine returns the 1-indexed line N from filename, or (empty, false)
// on any read / range error (non-fatal — the renderer drops the excerpt).
func ReadSourceLine(filename string, line int) (string, bool) {
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

// RichError is the structured error type both log impls render. It separates
// the what (Msg + Pos), the technical context (BotHint, inline for agents/CI),
// and the human remedy (UserHint, a Note: line in pretty mode). See Logger.
type RichError struct {
	Pos      token.Position // file:line:col; zero value when unknown
	Msg      string         // main error message — what failed
	CodeSpan string         // substring within the source line to highlight + point caret at
	Anchor   string         // disambiguating prefix searched before CodeSpan (positioning only, not highlighted) when CodeSpan is short enough to collide earlier on the line
	BotHint  string         // technical context for concise/agent output
	UserHint string         // remedy suggestion for human output (Note:)
	Err      error          // optional underlying error for errors.Unwrap
}

func (e *RichError) Error() string {
	var b strings.Builder
	if e.Pos.IsValid() {
		b.WriteString(FormatPos(e.Pos))
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

func (e *RichError) Unwrap() error { return e.Err }

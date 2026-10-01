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

// RelPath returns p relative to the working directory. A path below the
// working directory gets a "./" prefix, so it reads as a file path and not a
// package path, and editors can still open it on click. When the relative
// form is longer than the absolute one, for example after many "../" steps,
// RelPath returns the absolute path.
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

// FormatPos renders a token.Position with the filename relative to the
// working directory. token.Position.String always prints the absolute name.
func FormatPos(pos token.Position) string {
	if !pos.IsValid() {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d", RelPath(pos.Filename), pos.Line, pos.Column)
}

// ResolveSpanCol returns the 1-based column of codeSpan in line, searching
// from posCol. The optional anchor is a text known to come before codeSpan.
// It is searched for first, so a short codeSpan that also appears earlier on
// the line still resolves to the right place.
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

// sourceLineCache keeps the lines of each file read, so several errors in one
// file read it only once.
var sourceLineCache struct {
	mu    sync.Mutex
	files map[string][]string
}

// ReadSourceLine returns the 1-based line from filename. It returns
// ("", false) when the file cannot be read or the line is out of range. This
// is not fatal: the renderer then prints the error without the source excerpt.
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

// RichError is the structured error that both log implementations render. It
// keeps three things apart. Msg and Pos say what failed and where. BotHint is
// technical context, printed inline for agents and CI. UserHint is a fix for
// a human, printed as a "Note:" line in pretty mode. See Logger.
type RichError struct {
	Pos      token.Position // file:line:col, zero value when unknown
	Msg      string         // what failed
	CodeSpan string         // text in the source line to highlight and point the caret at
	// text searched for before CodeSpan when CodeSpan is short enough to also
	// appear earlier on the line. Used for positioning only, not highlighted.
	Anchor   string
	BotHint  string // technical context for short or agent output
	UserHint string // suggested fix for human output, the "Note:" line
	Err      error  // optional underlying error, returned by Unwrap
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

package textcheck

import (
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"testing"
)

// daemonVisibleMarkdown is the daemon's file of visibleMarkdown and its cases, which
// visible_markdown_test.go repeats because a test package of one module cannot import a test file
// of another.
const daemonVisibleMarkdown = "../../../daemon/internal/textcheck/visible_markdown_test.go"

// The two files of visibleMarkdown hold the same Go, token for token, comments aside: the reader,
// its helpers and its cases. So an edit to the code or to a case of one copy that the other lacks
// fails here, naming where the two part, while their docs, which say which module each serves,
// may differ.
func TestVisibleMarkdownIsTheDaemonsReader(t *testing.T) {
	ours, theirs := readGoTokens(t, "visible_markdown_test.go"), readGoTokens(t, daemonVisibleMarkdown)
	for index := range max(len(ours), len(theirs)) {
		if index < len(ours) && index < len(theirs) && ours[index].text == theirs[index].text {
			continue
		}
		t.Fatalf("visible_markdown_test.go and %s part at %s against %s; make the two the same Go",
			daemonVisibleMarkdown, tokenAt(ours, index), tokenAt(theirs, index))
	}
}

// goToken is one token of a Go file: its text, the literal's own where it has one, and its place.
type goToken struct {
	text  string
	place token.Position
}

// tokenAt names token index of tokens by its place and its text, or the file's end where the file
// holds no token there.
func tokenAt(tokens []goToken, index int) string {
	if index >= len(tokens) {
		return "the file's end"
	}
	return fmt.Sprintf("%s %q", tokens[index].place, tokens[index].text)
}

// readGoTokens reads the Go file at path as its tokens, comments left out, each semicolon the
// scanner inserts at a line's end among them.
func readGoTokens(t *testing.T, path string) []goToken {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	files := token.NewFileSet()
	file := files.AddFile(path, files.Base(), len(source))
	var lex scanner.Scanner
	lex.Init(file, source, func(place token.Position, message string) { t.Fatalf("scan %s: %s", place, message) }, 0)
	var tokens []goToken
	for {
		position, kind, literal := lex.Scan()
		if kind == token.EOF {
			return tokens
		}
		text := kind.String()
		if literal != "" {
			text = literal
		}
		tokens = append(tokens, goToken{text: text, place: files.Position(position)})
	}
}

package harness

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/claude"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
)

// The session text rule (R2, after INFRA-3189): session text reaches Claude models only. The
// guarantee is the compiler's. claude.SessionText has no exported field and no method that
// answers its text, so no code outside package claude can turn it into a string; package llm,
// which holds the Foundry client, cannot name the type, since package claude imports llm; and the
// one method that sends it, claude.Client.CallWithSession, always calls a Claude model over
// Anthropic Messages. A call that would carry session text to a Foundry deployment does not
// compile. The tests below hold the parts of that guarantee a later edit could undo.

var sessionTextType = reflect.TypeOf(claude.SessionText{})

// methodsTaking is the methods of t whose parameters include the session text type.
func methodsTaking(t reflect.Type) []string {
	var out []string
	for i := range t.NumMethod() {
		m := t.Method(i)
		for k := range m.Type.NumIn() {
			if m.Type.In(k) == sessionTextType {
				out = append(out, m.Name)
			}
		}
	}
	return out
}

func TestOnlyTheClaudeClientTakesSessionText(t *testing.T) {
	if got := methodsTaking(reflect.TypeOf(&llm.Foundry{})); len(got) != 0 {
		t.Errorf("the Foundry client has methods that take session text: %v", got)
	}
	if got := methodsTaking(reflect.TypeOf(&claude.Client{})); len(got) != 1 || got[0] != "CallWithSession" {
		t.Errorf("the Claude client's methods that take session text: %v, want CallWithSession alone", got)
	}
	if got := methodsTaking(reflect.TypeOf(&Reader{})); len(got) != 0 {
		t.Errorf("the fresh reader has methods that take session text: %v", got)
	}
	// The interface every client meets takes none, so no code that holds an llm.Client can pass
	// it session text.
	if got := methodsTaking(reflect.TypeOf((*llm.Client)(nil)).Elem()); len(got) != 0 {
		t.Errorf("llm.Client has methods that take session text: %v", got)
	}
}

func TestSessionTextCannotBeReadOutsideItsPackage(t *testing.T) {
	st := sessionTextType
	for i := range st.NumField() {
		if st.Field(i).IsExported() {
			t.Errorf("SessionText has an exported field %s", st.Field(i).Name)
		}
	}
	stringType := reflect.TypeOf("")
	for i := range st.NumMethod() {
		m := st.Method(i)
		for k := range m.Type.NumOut() {
			if m.Type.Out(k) == stringType {
				t.Errorf("SessionText.%s answers a string", m.Name)
			}
		}
	}
	// A SessionText printed by any verb is its placeholder, never its text.
	var s claude.SessionText
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if got := fmt.Sprintf(verb, s); !strings.HasPrefix(got, "[session text:") {
			t.Errorf("%s prints %q", verb, got)
		}
	}
}

// TestNoPackageButClaudeReadsTheSessionExport: the session export is read by package claude
// alone, so no other package of the harness holds a session's text as a string.
func TestNoPackageButClaudeReadsTheSessionExport(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Dir(path) == "claude" {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, lit := range stringLiterals(f) {
			if lit == "session-rows.jsonl.gz" || lit == "session-rows.jsonl" {
				t.Errorf("%s names the session export's rows file; only package claude reads it", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// stringLiterals is every string literal of a file.
func stringLiterals(f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, s)
			}
		}
		return true
	})
	return out
}

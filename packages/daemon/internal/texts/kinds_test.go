package texts

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// kinds.sh reads each quoted lower-case word of the one list's file as a kind
// (grep -oE '"[a-z]+(-[a-z]+)*"'), so that file holds the kind names and nothing else that a
// quoted word could be: no import, no struct tag, no other quoted string and no quoted word in
// a comment. Each kind constant is on the list once, and the list holds no kind twice.
func TestTheKindsFileHoldsKindNamesAlone(t *testing.T) {
	source, err := os.ReadFile("kinds.go")
	if err != nil {
		t.Fatal(err)
	}
	for number, line := range strings.Split(string(source), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "import") {
			t.Errorf("kinds.go:%d is an import line: %s", number+1, line)
		}
	}
	if bytes.ContainsRune(source, '`') {
		t.Error("kinds.go holds a backquote, which opens a struct tag or a raw string")
	}

	files := token.NewFileSet()
	file, err := parser.ParseFile(files, "kinds.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Imports) > 0 {
		t.Errorf("kinds.go imports %d packages", len(file.Imports))
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.Contains(comment.Text, `"`) {
				t.Errorf("%s: a comment holds a double quote: %s", files.Position(comment.Pos()), comment.Text)
			}
		}
	}

	// The constants, by name and by value, and the kinds the list names, in its order.
	values := map[string]string{}
	names := map[string]string{}
	var listed []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			t.Errorf("%s: kinds.go declares a function", files.Position(decl.Pos()))
			continue
		}
		switch gen.Tok {
		case token.CONST:
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				kind, typed := value.Type.(*ast.Ident)
				if !typed || kind.Name != "Kind" || len(value.Names) != 1 || len(value.Values) != 1 {
					t.Errorf("%s: a constant that is not one Kind with one value", files.Position(value.Pos()))
					continue
				}
				literal, ok := value.Values[0].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Errorf("%s: kind %s is not a string literal", files.Position(value.Pos()), value.Names[0].Name)
					continue
				}
				name := value.Names[0].Name
				if other, taken := names[literal.Value]; taken {
					t.Errorf("kinds %s and %s are both %s", other, name, literal.Value)
				}
				values[name], names[literal.Value] = literal.Value, name
			}
		case token.VAR:
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				if len(value.Names) != 1 || value.Names[0].Name != "list" || len(value.Values) != 1 {
					t.Errorf("%s: kinds.go declares a variable other than list", files.Position(value.Pos()))
					continue
				}
				entries, ok := value.Values[0].(*ast.CompositeLit)
				if !ok {
					t.Errorf("%s: list is not a literal of entries", files.Position(value.Pos()))
					continue
				}
				for _, element := range entries.Elts {
					listed = append(listed, listedKind(t, files, element))
				}
			}
		default:
			t.Errorf("%s: kinds.go declares a %s", files.Position(gen.Pos()), gen.Tok)
		}
	}
	if len(values) == 0 {
		t.Fatal("kinds.go declares no kind")
	}

	// Each string literal of the file is the value of one kind constant, and has the form of a
	// kind name.
	form := regexp.MustCompile(`^"[a-z]+(-[a-z]+)*"$`)
	var lex scanner.Scanner
	lex.Init(files.AddFile("kinds.go", files.Base(), len(source)), source, nil, 0)
	literals := map[string]int{}
	for {
		position, tok, literal := lex.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.STRING && tok != token.CHAR {
			continue
		}
		literals[literal]++
		if !form.MatchString(literal) {
			t.Errorf("%s: %s does not have the form of a kind name", files.Position(position), literal)
		} else if _, ok := names[literal]; !ok {
			t.Errorf("%s: %s is not the value of a kind constant", files.Position(position), literal)
		}
	}
	for literal, count := range literals {
		if count > 1 {
			t.Errorf("kinds.go holds %s %d times", literal, count)
		}
	}

	// What kinds.sh reads is exactly the kind names.
	read := regexp.MustCompile(`"[a-z]+(-[a-z]+)*"`).FindAll(source, -1)
	if len(read) != len(values) {
		t.Errorf("kinds.sh would read %d quoted words from kinds.go, and it declares %d kinds", len(read), len(values))
	}
	for _, word := range read {
		if _, ok := names[string(word)]; !ok {
			t.Errorf("kinds.sh would read %s, which is no kind", word)
		}
	}

	// Every constant is on the list once, and the list holds no kind twice.
	counts := map[string]int{}
	for _, name := range listed {
		counts[name]++
		if _, ok := values[name]; !ok {
			t.Errorf("the list names %s, which is no kind constant of kinds.go", name)
		}
	}
	for name := range values {
		if counts[name] != 1 {
			t.Errorf("kind %s is on the list %d times", name, counts[name])
		}
	}

	// The list the package reads is the list of the file.
	entries := Entries()
	if len(entries) != len(listed) {
		t.Fatalf("the package's list holds %d entries, and kinds.go lists %d", len(entries), len(listed))
	}
	seen := map[Kind]bool{}
	for i, entry := range entries {
		want, err := strconv.Unquote(values[listed[i]])
		if err != nil {
			t.Fatal(err)
		}
		if string(entry.Kind) != want {
			t.Errorf("entry %d is %s, and kinds.go lists %s there", i, entry.Kind, want)
		}
		if seen[entry.Kind] {
			t.Errorf("the list holds %s twice", entry.Kind)
		}
		seen[entry.Kind] = true
		if found, ok := Lookup(entry.Kind); !ok || found.Kind != entry.Kind || found.Form != entry.Form {
			t.Errorf("Lookup(%s) gives %+v, %t", entry.Kind, found, ok)
		}
		switch entry.Form {
		case Task, Notice, ControllerNotice, RoleMessage:
		default:
			t.Errorf("%s has the form %q, which is no form", entry.Kind, entry.Form)
		}
	}
}

// listedKind is the name of the kind constant that one element of the list names.
func listedKind(t *testing.T, files *token.FileSet, element ast.Expr) string {
	t.Helper()
	entry, ok := element.(*ast.CompositeLit)
	if !ok {
		t.Errorf("%s: an element of the list is not an entry literal", files.Position(element.Pos()))
		return ""
	}
	for _, field := range entry.Elts {
		pair, ok := field.(*ast.KeyValueExpr)
		if !ok {
			t.Errorf("%s: an entry of the list names its fields by position", files.Position(field.Pos()))
			return ""
		}
		if key, ok := pair.Key.(*ast.Ident); ok && key.Name == "Kind" {
			kind, ok := pair.Value.(*ast.Ident)
			if !ok {
				t.Errorf("%s: an entry's kind is not a kind constant", files.Position(pair.Value.Pos()))
				return ""
			}
			return kind.Name
		}
	}
	t.Errorf("%s: an entry of the list names no kind", files.Position(entry.Pos()))
	return ""
}

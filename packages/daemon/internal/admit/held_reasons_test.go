package admit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// declaredHeldReasons is every word of record.HeldReason the record package's own source
// declares — the const block of record/held.go, and any other file of the package that declares
// one — by the name it is declared under. It reads the source rather than a list a person keeps
// beside the table, because a test over such a list would pass while this measure ignored a word
// nobody added to it, which is the fault dispatch://INFRA-2560 was filed for rather than a new
// place to hide it: a word the daemon's own walk writes and this table has never heard of
// silences nothing, so the tree it holds is named as a stall whatever the daemon is doing about
// it.
//
// It reads the whole package and not one file, so a word declared beside another rule is found
// too. A word whose text is not a plain string literal fails the read rather than being passed
// over: the table is keyed by the word, so a word this could not read is a word the measure
// could ignore. (The claim table's own reader type-checks its package instead, supervise's
// sealed, because what it must see — every type that implements a sealed interface — is not in
// the spelling of the source, while a constant of a named string type is.)
func declaredHeldReasons(t *testing.T) map[string]record.HeldReason {
	t.Helper()
	const dir = "../record"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the record package's own source: %v", err)
	}
	files := token.NewFileSet()
	declared := make(map[string]record.HeldReason)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(files, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range parsed.Decls {
			block, isGeneric := decl.(*ast.GenDecl)
			if !isGeneric || block.Tok != token.CONST {
				continue
			}
			for _, spec := range block.Specs {
				value, isValue := spec.(*ast.ValueSpec)
				if !isValue {
					continue
				}
				named, isNamed := value.Type.(*ast.Ident)
				if !isNamed || named.Name != "HeldReason" {
					continue
				}
				for i, ident := range value.Names {
					if i >= len(value.Values) {
						t.Fatalf("%s declares record.HeldReason %s with no value of its own: this test reads the value and cannot read that one",
							name, ident.Name)
					}
					literal, isLiteral := value.Values[i].(*ast.BasicLit)
					if !isLiteral || literal.Kind != token.STRING {
						t.Fatalf("%s declares record.HeldReason %s as %s rather than a plain string: this test reads the value and cannot read that one",
							name, ident.Name, value.Values[i])
					}
					text, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatalf("read the value of record.HeldReason %s in %s: %v", ident.Name, name, err)
					}
					declared[ident.Name] = record.HeldReason(text)
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("the record package's source declares no record.HeldReason word: this test reads the words from there and has read nothing")
	}
	return declared
}

// Every word the daemon has for why it runs nothing of an issue has an answer in the measure's
// table, and the table answers for no word the vocabulary does not hold. The first half is the
// totality this rule rests on: the measure reads the one word the line's walk gives an issue it
// left behind (record.Waiting, record.HeldReasonOf), and a word with no row here is one it would
// treat as nothing holding the issue — silently, since a lookup that misses answers false — so a
// word added to the vocabulary is a word this measure must decide about. The second half catches
// the other drift: a row left behind by a renamed or deleted word, which says the table covers a
// word nothing writes any more.
func TestTheMeasureAnswersEveryHeldReason(t *testing.T) {
	declared := declaredHeldReasons(t)
	names := make(map[record.HeldReason]string, len(declared))
	for name, word := range declared {
		names[word] = name
		if _, answered := silencedTree[word]; !answered {
			t.Fatalf("record.%s (%q) has no row in silencedTree: every word of the vocabulary must say whether an issue left behind for it is waiting by design, and a word the table does not name silences nothing",
				name, word)
		}
	}
	for word := range silencedTree {
		if _, known := names[word]; !known {
			t.Fatalf("silencedTree answers for %q, which the record package declares no record.HeldReason for: the table has outlived the word it answers for",
				word)
		}
	}
}

// The exported list of the words is exactly the words the source declares. The list is what a
// reader covering every word walks (record.HeldReasons), so a word added to the vocabulary and
// left out of it would be a word such a reader never sees.
func TestTheHeldReasonListIsWhatTheSourceDeclares(t *testing.T) {
	declared := declaredHeldReasons(t)
	listed := make(map[record.HeldReason]struct{}, len(record.HeldReasons))
	for _, word := range record.HeldReasons {
		if _, twice := listed[word]; twice {
			t.Fatalf("record.HeldReasons names %q twice", word)
		}
		listed[word] = struct{}{}
	}
	for name, word := range declared {
		if _, named := listed[word]; !named {
			t.Fatalf("record.HeldReasons leaves out record.%s (%q), which the source declares", name, word)
		}
	}
	if len(listed) != len(declared) {
		t.Fatalf("record.HeldReasons names %d words and the source declares %d: the list names a word of its own",
			len(listed), len(declared))
	}
}

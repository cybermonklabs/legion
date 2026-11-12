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

// declaredStartHolds is every value of record.StartHold the record package's own source
// declares, by the name it is declared under. It reads the source rather than a list a person
// keeps beside the table, because a test over such a list would pass while this measure ignored
// a hold nobody added to it, which is the fault dispatch://INFRA-2560 was filed for rather than
// a new place to hide it: a hold the daemon's own pass writes and this table has never heard of
// silences nothing, so the tree it holds is named as a stall whatever the daemon is doing about
// it.
//
// It reads the whole package and not one file, so a value declared beside another rule is found
// too. A value whose text is not a plain string literal fails the read rather than being passed
// over: the table is keyed by the value, so a value this could not read is a value the measure
// could ignore. (The claim table's own reader type-checks its package instead, supervise's
// sealed, because what it must see — every type that implements a sealed interface — is not in
// the spelling of the source, while a constant of a named string type is.)
func declaredStartHolds(t *testing.T) map[string]record.StartHold {
	t.Helper()
	const dir = "../record"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the record package's own source: %v", err)
	}
	files := token.NewFileSet()
	declared := make(map[string]record.StartHold)
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
				if !isNamed || named.Name != "StartHold" {
					continue
				}
				for i, ident := range value.Names {
					if i >= len(value.Values) {
						t.Fatalf("%s declares record.StartHold %s with no value of its own: this test reads the value and cannot read that one",
							name, ident.Name)
					}
					literal, isLiteral := value.Values[i].(*ast.BasicLit)
					if !isLiteral || literal.Kind != token.STRING {
						t.Fatalf("%s declares record.StartHold %s as %s rather than a plain string: this test reads the value and cannot read that one",
							name, ident.Name, value.Values[i])
					}
					text, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatalf("read the value of record.StartHold %s in %s: %v", ident.Name, name, err)
					}
					declared[ident.Name] = record.StartHold(text)
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("the record package's source declares no record.StartHold value: this test reads the set from there and has read nothing")
	}
	return declared
}

// Every hold the daemon can have on an issue has an answer in the measure's table, and the
// table answers for no hold the set does not hold. The first half is the totality this rule
// rests on: the measure asks the one set why an issue stands still (record.WaitingAndHolds), and a
// hold with no row here is one it would treat as nothing holding the issue — silently, since a
// lookup that misses answers false — so a hold added to the set is a hold this measure must
// decide about. The second half catches the other drift: a row left behind by a renamed or
// deleted value, which says the table covers a hold nothing writes any more.
func TestTheMeasureAnswersEveryStartHold(t *testing.T) {
	declared := declaredStartHolds(t)
	names := make(map[record.StartHold]string, len(declared))
	for name, hold := range declared {
		names[hold] = name
		if _, answered := silencedTree[hold]; !answered {
			t.Fatalf("record.%s (%q) has no row in silencedTree: every hold of the set must say whether an issue held for it is waiting by design, and a hold the table does not name silences nothing",
				name, hold)
		}
	}
	for hold := range silencedTree {
		if _, known := names[hold]; !known {
			t.Fatalf("silencedTree answers for %q, which the record package declares no record.StartHold for: the table has outlived the hold it answers for",
				hold)
		}
	}
}

// The exported list of the set is exactly the values the source declares. The list is what a
// reader covering every hold walks (record.StartHolds), so a value added to the set and left
// out of it would be a hold such a reader never sees.
func TestTheStartHoldListIsWhatTheSourceDeclares(t *testing.T) {
	declared := declaredStartHolds(t)
	listed := make(map[record.StartHold]struct{}, len(record.StartHolds))
	for _, hold := range record.StartHolds {
		if _, twice := listed[hold]; twice {
			t.Fatalf("record.StartHolds names %q twice", hold)
		}
		listed[hold] = struct{}{}
	}
	for name, hold := range declared {
		if _, named := listed[hold]; !named {
			t.Fatalf("record.StartHolds leaves out record.%s (%q), which the source declares", name, hold)
		}
	}
	if len(listed) != len(declared) {
		t.Fatalf("record.StartHolds names %d values and the source declares %d: the list names a value of its own",
			len(listed), len(declared))
	}
}

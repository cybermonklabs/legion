package admit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// recordPackage is the import path of the package whose words these tests read, and recordDir
// is where its source stands beside this package's.
const (
	recordPackage = "github.com/sjawhar/legion/daemon/internal/record"
	recordDir     = "../record"
)

// declaredHeldReasons is every word of record.HeldReason the record package's own source
// declares — the const block of record/held.go, and any other block of the package that declares
// one — by the name it is declared under. It reads the source rather than a list a person keeps
// beside the table, because a test over such a list would pass while this measure ignored a word
// nobody added to it, which is the fault dispatch://INFRA-2560 was filed for rather than a new
// place to hide it: a word the daemon's own walk writes and this table has never heard of
// silences nothing, so the tree it holds is named as a stall whatever the daemon is doing about
// it. It reads the whole package and not one file, so a word declared beside another rule is
// found too. How a word is read is heldReasonsIn's.
func declaredHeldReasons(t *testing.T) map[string]record.HeldReason {
	t.Helper()
	declared, err := heldReasonsIn(recordSource(t))
	if err != nil {
		t.Fatal(err)
	}
	return declared
}

// recordSource is the record package's own non-test source, each file by the path it is read
// from.
func recordSource(t *testing.T) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(recordDir)
	if err != nil {
		t.Fatalf("read the record package's own source: %v", err)
	}
	source := make(map[string][]byte)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(recordDir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		source[path] = body
	}
	return source
}

// heldReasonsIn is every word of record.HeldReason the record package declares when its source
// is source, by the name each word is declared under. It type-checks the package and reads the
// type the compiler gave each constant rather than the spelling that declared it, because a line
// of a const block need not spell the type Go gives its constant: a name standing alone repeats
// the type and the value of the line above it, a conversion (HeldReason("x")) types a constant
// whose line names no type, and a word written with no type at all is an untyped string, which
// the compiler takes wherever a HeldReason is wanted — HeldReasonOf could return it, and
// silencedTree would have no row for it — though no line declares it a HeldReason. So a block
// that declares a HeldReason is a block of words, and each of its constants that is a HeldReason
// or an untyped string is a word; a block that declares none holds none. A constant declared
// inside a function is read the same way. (supervise's sealed and its ProcessEnds guard read the
// compiler for the same reason.)
//
// It fails rather than answer when two constants are one word or one name is two words: the
// block is one constant per word (record/held.go), and the measure's table is keyed by the word,
// so a name left standing alone at the end of the block — which Go gives the value of the word
// above it — is a word someone meant to add that would otherwise read as that other word. It
// fails too when it reads no word at all, since a test over none would pass.
func heldReasonsIn(source map[string][]byte) (map[string]record.HeldReason, error) {
	exports, err := recordDependencies()
	if err != nil {
		return nil, err
	}
	files := token.NewFileSet()
	var parsed []*ast.File
	for _, path := range slices.Sorted(maps.Keys(source)) {
		file, err := parser.ParseFile(files, path, source[path], 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		parsed = append(parsed, file)
	}
	compiled := func(path string) (io.ReadCloser, error) {
		export, found := exports[path]
		if !found {
			return nil, fmt.Errorf("the go command reported no compiled types for %s", path)
		}
		return os.Open(export)
	}
	defs := map[*ast.Ident]types.Object{}
	config := types.Config{Importer: importer.ForCompiler(files, "gc", compiled)}
	pkg, err := config.Check(recordPackage, files, parsed, &types.Info{Defs: defs})
	if err != nil {
		return nil, fmt.Errorf("type-check the record package: %w", err)
	}
	heldReason, isType := pkg.Scope().Lookup("HeldReason").(*types.TypeName)
	if !isType {
		return nil, errors.New("the record package declares no type HeldReason")
	}
	var blocks []*ast.GenDecl
	for _, file := range parsed {
		ast.Inspect(file, func(node ast.Node) bool {
			if block, isBlock := node.(*ast.GenDecl); isBlock && block.Tok == token.CONST {
				blocks = append(blocks, block)
			}
			return true
		})
	}
	declared := make(map[string]record.HeldReason)
	names := make(map[record.HeldReason]string)
	for _, block := range blocks {
		var words []*types.Const
		ofWords := false
		for _, spec := range block.Specs {
			for _, ident := range spec.(*ast.ValueSpec).Names {
				declaredConst, isConst := defs[ident].(*types.Const)
				if !isConst {
					continue
				}
				if types.Identical(declaredConst.Type(), heldReason.Type()) {
					ofWords = true
					words = append(words, declaredConst)
				} else if basic, isBasic := declaredConst.Type().(*types.Basic); isBasic && basic.Kind() == types.UntypedString {
					words = append(words, declaredConst)
				}
			}
		}
		if !ofWords {
			continue
		}
		for _, word := range words {
			text := record.HeldReason(constant.StringVal(word.Val()))
			if other, twice := names[text]; twice {
				return nil, fmt.Errorf("record.%s and record.%s are both %q: the block is one constant per word, and a name standing alone takes the value of the line above it, so one of the two is a word nobody gave a value of its own",
					other, word.Name(), text)
			}
			if _, twice := declared[word.Name()]; twice {
				return nil, fmt.Errorf("the record package declares two words named %s: this reads the words by name and would keep one of the two", word.Name())
			}
			names[text] = word.Name()
			declared[word.Name()] = text
		}
	}
	if len(declared) == 0 {
		return nil, errors.New("the record package's source declares no record.HeldReason word: this test reads the words from there and has read nothing")
	}
	return declared, nil
}

// recordDependencies is where the compiler left the types of everything the record package
// imports, asked of the go command once per test binary: it reports the file of each from its
// build cache. heldReasonsIn reads those rather than type-checking the record package's whole
// dependency tree from its source. That tree holds supervise's, whose read from source is the
// tens of seconds supervise's own compiledDependencies is there to avoid by reading its package's
// the same way.
var recordDependencies = sync.OnceValues(func() (map[string]string, error) {
	listed, err := exec.Command("go", "list", "-deps", "-export", "-json=ImportPath,Export", recordPackage).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("ask the go command where the record package's dependencies were compiled: %w: %s", err, exit.Stderr)
		}
		return nil, fmt.Errorf("ask the go command where the record package's dependencies were compiled: %w", err)
	}
	exports := make(map[string]string)
	decoder := json.NewDecoder(bytes.NewReader(listed))
	for decoder.More() {
		var dependency struct{ ImportPath, Export string }
		if err := decoder.Decode(&dependency); err != nil {
			return nil, fmt.Errorf("read the go command's answer: %w", err)
		}
		if dependency.Export != "" {
			exports[dependency.ImportPath] = dependency.Export
		}
	}
	return exports, nil
})

// withWordInHeldBlock is the record package's source with spec written as the last line of
// record/held.go's block of words. The file is changed in memory only; the one on disk is left as
// it stands. The block is found by its spelling — the parenthesized const block one of whose
// lines names the type HeldReason — which is only where to write, not what is read.
func withWordInHeldBlock(t *testing.T, spec string) map[string][]byte {
	t.Helper()
	source := recordSource(t)
	path := filepath.Join(recordDir, "held.go")
	held, found := source[path]
	if !found {
		t.Fatalf("the record package has no %s, whose block of words this writes into", path)
	}
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, path, held, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range parsed.Decls {
		block, isBlock := decl.(*ast.GenDecl)
		if !isBlock || block.Tok != token.CONST || !block.Rparen.IsValid() {
			continue
		}
		if !slices.ContainsFunc(block.Specs, func(spec ast.Spec) bool {
			named, isNamed := spec.(*ast.ValueSpec).Type.(*ast.Ident)
			return isNamed && named.Name == "HeldReason"
		}) {
			continue
		}
		end := files.Position(block.Rparen).Offset
		source[path] = slices.Concat(held[:end], []byte("\t"+spec+"\n"), held[end:])
		return source
	}
	t.Fatalf("%s has no parenthesized const block naming HeldReason to write a word into", path)
	return nil
}

// everyHeldReasonAnswered is nil when silencedTree has a row for every word of declared and a row
// for no word declared does not hold, and otherwise says which word breaks that.
func everyHeldReasonAnswered(declared map[string]record.HeldReason) error {
	names := make(map[record.HeldReason]string, len(declared))
	for name, word := range declared {
		names[word] = name
		if _, answered := silencedTree[word]; !answered {
			return fmt.Errorf("record.%s (%q) has no row in silencedTree: every word of the vocabulary must say whether an issue left behind for it is waiting by design, and a word the table does not name silences nothing",
				name, word)
		}
	}
	for word := range silencedTree {
		if _, known := names[word]; !known {
			return fmt.Errorf("silencedTree answers for %q, which the record package declares no record.HeldReason for: the table has outlived the word it answers for",
				word)
		}
	}
	return nil
}

// Every word the daemon has for why it runs nothing of an issue has an answer in the measure's
// table, and the table answers for no word the vocabulary does not hold. The first half is the
// totality this rule rests on: the measure reads the one word the line's walk gives an issue it
// left behind (record.Waiting, record.HeldReasonOf), and a word with no row here is one it would
// treat as nothing holding the issue — silently, since a lookup that misses answers false — so a
// word added to the vocabulary is a word this measure must decide about. The second half catches
// the other drift: a row left behind by a renamed or deleted word, which says the table covers a
// word nothing writes any more.
//
// The cases after it hold the totality to a word added inside record/held.go's block in each way
// Go lets it be written there without spelling the type on its line: with no type at all, which
// is an untyped string; typed by a conversion; and as a name standing alone, which repeats the
// line above it. Each is written into a copy of the file in memory and read through the same
// reader and the same check as the package itself, and each must fail naming the word. A reader
// of the spelling passes the first two over, and reads the third as the word above it.
func TestTheMeasureAnswersEveryHeldReason(t *testing.T) {
	if err := everyHeldReasonAnswered(declaredHeldReasons(t)); err != nil {
		t.Fatal(err)
	}
	const added = "HeldAddedInTheBlock"
	for _, spelling := range []struct{ how, spec string }{
		{"with no type of its own", added + ` = "added_in_the_block"`},
		{"typed by a conversion", added + ` = HeldReason("added_in_the_block")`},
		{"standing alone after the last word", added},
	} {
		t.Run("a word added "+spelling.how, func(t *testing.T) {
			declared, err := heldReasonsIn(withWordInHeldBlock(t, spelling.spec))
			if err == nil {
				err = everyHeldReasonAnswered(declared)
			}
			if err == nil || !strings.Contains(err.Error(), added) {
				t.Fatalf("record/held.go with `%s` inside its block of words reads as %v, want a failure naming %s",
					spelling.spec, err, added)
			}
		})
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

package specform

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specform/charge"
	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
)

// The tests of this package make the charge of the readings of a sentence loud (loudReads): a
// reading for the findings that stand in a sentence that would pass ownReads stops the test that
// makes it, as a walk that breaks the bound of a sentence's findings.
func init() { loudReads = true }

// The charge of a judgment's texts (newTexts, charge.Texts) names two sentences of the same words
// by one unit, and a sentence of a title and an address of the same words each by another, whose
// readings are their own: past the charge of the sentence, each still gives the text; it lets
// ownReads readings of one sentence for the findings that stand elsewhere, each giving the text, and
// refuses the next with nothing given; it lets ownReads readings for the findings that stand in it,
// apart from the others, and stops the program at the next where loudReads is set, and refuses it
// where it is not. Control: another sentence's readings are its own.
func TestTheChargeOfASentenceRefusesAReadingPastItAndStopsAnOwnOneLoudly(t *testing.T) {
	const sentence = "Dispatch reads Web.Config at boot."
	texts := newTexts(nil)
	unit, other := texts.Name(sentence, false, false), texts.Name("It logs it.", false, false)
	if again := texts.Name(sentence, false, false); again != unit {
		t.Errorf("two sentences of the same words are the units %v and %v, want one", unit, again)
	}
	title, address := texts.Name(sentence, false, true), texts.Name(sentence, true, false)
	if title == unit || !title.Title() || address == unit || !address.Address() {
		t.Errorf("a sentence of a title and an address of the same words are the units %v and %v, want others than the sentence's %v, a title's and an address's", title, address, unit)
	}
	for count := range ownReads {
		if text, _, ok := texts.Read(unit, false); !ok || text != sentence {
			t.Fatalf("reading %d of %d for the findings that stand elsewhere gives %q, %v, want the sentence", count+1, ownReads, text, ok)
		}
	}
	if text, kept, ok := texts.Read(unit, false); ok || text != "" || kept != nil {
		t.Errorf("reading %d for the findings that stand elsewhere gives %q, %v, %v, want it refused with nothing given", ownReads+1, text, kept, ok)
	}
	for _, each := range []struct {
		kind string
		unit charge.Unit
	}{{"a sentence of a title", title}, {"an address", address}} {
		if text, _, ok := texts.Read(each.unit, false); !ok || text != sentence {
			t.Errorf("past the charge of the sentence, %s of the same words gives %q, %v, want the text from a charge of its own", each.kind, text, ok)
		}
	}
	if _, _, ok := texts.Read(other, false); !ok {
		t.Errorf("the control: the charge refuses the first reading of another sentence")
	}
	for count := range ownReads {
		if _, _, ok := texts.Read(unit, true); !ok {
			t.Fatalf("the charge refuses reading %d of %d for the findings that stand in the sentence", count+1, ownReads)
		}
	}
	stopped := func() (said any) {
		defer func() { said = recover() }()
		_, _, ok := texts.Read(unit, true)
		return ok
	}()
	if message, ok := stopped.(string); !ok || !strings.Contains(message, fmt.Sprintf("past its limit of %d", ownReads)) {
		t.Errorf("reading %d for the findings that stand in the sentence: the charge says %v, want it to stop the program past ownReads", ownReads+1, stopped)
	}
	quiet := charge.New[sentenceAt](ownReads, false)
	quietUnit := quiet.Name(sentence, false, false)
	for range ownReads {
		if _, _, ok := quiet.Read(quietUnit, true); !ok {
			t.Fatalf("with loudReads off, the charge refuses a reading within ownReads")
		}
	}
	if _, _, ok := quiet.Read(quietUnit, true); ok {
		t.Errorf("with loudReads off, the charge lets reading %d for the findings that stand in the sentence, want it refused", ownReads+1)
	}
}

// keptTexts is each place in a value of typ, named from path, that can hold text: a string, a slice,
// an array or a channel of bytes or of runes (as of uint8 or int32, which reflect cannot tell from
// them), at any depth of its struct fields, the elements of its pointers, slices, arrays and
// channels, the keys and values of its maps, and an interface, which can hold any of them. Of
// package charge it walks the values of an Index, as Get gives them with no charge, and not their
// keys, which no walk reads, nor a Texts, which keeps the judgment's texts and gives them only
// through the charge (Read). Of this package it does not walk the fields that allowed names, each a
// "type.field" whose text is no sentence's, nor the types that it names, each a string type of names
// (Rule and Kind); and a function's captured values are no part of its type, so it cannot read
// them.
func keptTexts(typ reflect.Type, path string, allowed map[string]bool, seen map[reflect.Type]bool) []string {
	judge := reflect.TypeFor[wayJudge]().PkgPath()
	switch {
	case typ.PkgPath() == chargePath && strings.HasPrefix(typ.Name(), "Texts["):
		return nil
	case typ.PkgPath() == chargePath && strings.HasPrefix(typ.Name(), "Index["):
		values, ok := typ.FieldByName("values")
		if !ok || values.Type.Kind() != reflect.Map {
			return []string{path + " (" + typ.String() + ": no map of values to walk)"}
		}
		return keptTexts(values.Type.Elem(), path+".values{}", allowed, seen)
	case typ.PkgPath() == judge && allowed[typ.Name()]:
		return nil
	}
	switch typ.Kind() {
	case reflect.String, reflect.Interface:
		return []string{path + " (" + typ.String() + ")"}
	case reflect.Slice, reflect.Array, reflect.Chan:
		if kind := typ.Elem().Kind(); kind == reflect.Uint8 || kind == reflect.Int32 {
			return []string{path + " (" + typ.String() + ")"}
		}
		if typ.Kind() == reflect.Chan {
			return keptTexts(typ.Elem(), path+"<-", allowed, seen)
		}
		return keptTexts(typ.Elem(), path+"[]", allowed, seen)
	case reflect.Pointer:
		return keptTexts(typ.Elem(), path+"[]", allowed, seen)
	case reflect.Map:
		return append(keptTexts(typ.Key(), path+"{key}", allowed, seen), keptTexts(typ.Elem(), path+"{}", allowed, seen)...)
	case reflect.Struct:
		if seen[typ] {
			return nil
		}
		seen[typ] = true
		var kept []string
		for index := range typ.NumField() {
			field := typ.Field(index)
			if typ.PkgPath() != judge || !allowed[typ.Name()+"."+field.Name] {
				kept = append(kept, keptTexts(field.Type, path+"."+field.Name, allowed, seen)...)
			}
		}
		return kept
	}
	return nil
}

// The judge of the ways out keeps no text of a sentence in a field of its own, so that no walk of
// the judge reads a sentence outside its charge (charge.Texts.Read), wherever a copy would sit: from
// the judge (wayJudge) through each field of each type that it holds (keptTexts), text stands only
// in the fields that hold a finding's words, its ways out and their note (findingKey.detail,
// wayKey.from, wayOut.from, wayOut.to, offer.note), in the names of a rule and of a kind (the types
// Rule and Kind), in what the judgment is told of the system and its people (the text fields of
// Context, wayJudge.people), and in the first place of each member of a synonym group
// (wholeText.firsts: a member's words, one for each member of rules.json at most). The allowed text
// is named by its fields, so a field added to one of those types is walked. Controls, each a type of
// this test that holds the judge's two types and one field more, or a type of the judge's with one
// field more: a copy of the records of the sentences, of their words, of their runes, of their
// bytes in an array, of their Spreads (whose Hits hold words), of the records in an Index of package
// charge, the words of the sentences in a channel, and a value of any type are each named, and so
// is a new field of offer; a copy of a count or a flag of each sentence is not, as no type rule can
// tell such a copy from what the judge keeps of the split of its text (wholeText.nth), so the work
// table catches a walk of it where a row's shape reaches the walk.
func TestTheJudgeKeepsNoTextOfASentenceInAField(t *testing.T) {
	allowed := map[string]bool{"findingKey.detail": true, "wayKey.from": true, "wayOut.from": true, "wayOut.to": true, "offer.note": true,
		"Rule": true, "Kind": true, "Context.PersonNames": true, "Context.SystemNames": true, "Context.ServerURL": true, "Context.Pages": true,
		"wayJudge.people": true, "wholeText.firsts": true}
	kept := func(typ reflect.Type) []string { return keptTexts(typ, typ.Name(), allowed, map[reflect.Type]bool{}) }
	if got := kept(reflect.TypeFor[wayJudge]()); len(got) > 0 {
		t.Errorf("the judge of the ways out keeps text outside the charge, which a walk of it would read past the charge: %q", got)
	}
	type recordsCopied struct {
		wayJudge
		wholeText
		records []*sentenceAt
	}
	type wordsCopied struct {
		wayJudge
		wholeText
		words [][]string
	}
	type spreadsCopied struct {
		wayJudge
		wholeText
		spreads []ste.Spread
	}
	type anyCopied struct {
		wayJudge
		wholeText
		line any
	}
	type runesCopied struct {
		wayJudge
		wholeText
		runes [][]rune
	}
	type bytesCopied struct {
		wayJudge
		wholeText
		lines [][64]byte
	}
	type indexCopied struct {
		wayJudge
		wholeText
		byWord charge.Index[*sentenceAt]
	}
	type flagsCopied struct {
		wayJudge
		wholeText
		open []bool
	}
	type chanCopied struct {
		wayJudge
		wholeText
		lines chan string
	}
	// offerGrown is the judge's offer with one field more, under the same name, as a new field of offer
	// would stand.
	offerGrown := func() reflect.Type {
		type offer struct {
			ways [maxWays]wayOut
			note string
			line string
		}
		return reflect.TypeFor[offer]()
	}()
	for _, control := range []struct {
		typ  reflect.Type
		want string
	}{
		{reflect.TypeFor[recordsCopied](), "recordsCopied.records[][].span (string)"},
		{reflect.TypeFor[wordsCopied](), "wordsCopied.words[][] (string)"},
		{reflect.TypeFor[spreadsCopied](), "spreadsCopied.spreads[].Hits[].Match (string)"},
		{reflect.TypeFor[anyCopied](), "anyCopied.line (interface {})"},
		{reflect.TypeFor[runesCopied](), "runesCopied.runes[] ([]int32)"},
		{reflect.TypeFor[bytesCopied](), "bytesCopied.lines[] ([64]uint8)"},
		{reflect.TypeFor[indexCopied](), "indexCopied.byWord.values{}[][].span (string)"},
		{reflect.TypeFor[chanCopied](), "chanCopied.lines<- (string)"},
		{offerGrown, "offer.line (string)"},
		{reflect.TypeFor[flagsCopied](), ""},
	} {
		got := kept(control.typ)
		if control.want == "" && len(got) > 0 || control.want != "" && !slices.Contains(got, control.want) {
			t.Errorf("the control %s keeps text at %q, want %q named", control.typ.Name(), got, control.want)
		}
	}
}

// chargePath is the import path of the package of the judgment's texts.
const chargePath = "github.com/sjawhar/envoy/internal/dispatch/specform/charge"

// droppedAnswers reads the Go files of the package in dir, with the file planted beside them where
// planted is not empty, by their types (go/types: package charge read from its own files, the
// package's other imports as empty packages, whose names are errors that the check passes over),
// and returns each call whose results hold a charge.Answer and that does not keep it: a call that
// is a statement of its own (also after go or defer), one handed to another call, or one whose
// Answer an assignment or a declaration gives the blank name. A call keeps its Answer where a name
// takes it, where the call is the whole of a return statement (its function hands the Answer on as
// its own results), or, for a call of one result, where an expression reads it (as !w.load()). It
// also returns how many calls keep an Answer, how many of them are calls of charge.Texts.Read, and
// where each that is a call of a function, and no conversion to charge.Answer, stands (file:line).
func droppedAnswers(t *testing.T, dir, planted string) (faults []string, keptCalls, reads int, calls []string) {
	t.Helper()
	fset := token.NewFileSet()
	parsed := func(dir string, tests bool) []*ast.File {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var files []*ast.File
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") != tests {
				continue
			}
			file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, file)
		}
		return files
	}
	files := parsed(dir, false)
	if planted != "" {
		file, err := parser.ParseFile(fset, "planted.go", planted, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	empty := map[string]*types.Package{}
	var importer importerFunc
	importer = func(path string) (*types.Package, error) {
		if each, ok := empty[path]; ok {
			return each, nil
		}
		if path == chargePath {
			config := types.Config{Error: func(error) {}, Importer: importer}
			each, _ := config.Check(path, fset, parsed(filepath.Join(dir, "charge"), false), nil)
			empty[path] = each
			return each, nil
		}
		each := types.NewPackage(path, path[strings.LastIndex(path, "/")+1:])
		each.MarkComplete()
		empty[path] = each
		return each, nil
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	config := types.Config{Error: func(error) {}, Importer: importer}
	config.Check("specform", fset, files, info)
	chargePkg, err := importer(chargePath)
	if err != nil || chargePkg.Scope().Lookup("Answer") == nil || chargePkg.Scope().Lookup("Texts") == nil {
		t.Fatalf("the check reads no type Answer or Texts in package charge (%v)", err)
	}
	answer := chargePkg.Scope().Lookup("Answer").Type()
	// answers is where the results of a call of type typ hold an Answer, and how many results it has.
	answers := func(typ types.Type) (at []int, count int) {
		if tuple, ok := typ.(*types.Tuple); ok {
			for index := range tuple.Len() {
				if types.Identical(tuple.At(index).Type(), answer) {
					at = append(at, index)
				}
			}
			return at, tuple.Len()
		}
		if typ != nil && types.Identical(typ, answer) {
			return []int{0}, 1
		}
		return nil, 1
	}
	// named reports whether each of at is a name that is not the blank one.
	named := func(names []ast.Expr, at []int) bool {
		for _, index := range at {
			if name, ok := names[index].(*ast.Ident); !ok || name.Name == "_" {
				return false
			}
		}
		return true
	}
	isRead := func(call *ast.CallExpr) bool {
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		selection, ok := info.Selections[selector]
		if !ok {
			return false
		}
		method, ok := selection.Obj().(*types.Func)
		return ok && method.Origin().Pkg() == chargePkg && method.Name() == "Read"
	}
	for _, file := range files {
		var stack []ast.Node
		ast.Inspect(file, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			defer func() { stack = append(stack, node) }()
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			at, count := answers(info.Types[call].Type)
			if len(at) == 0 {
				return true
			}
			keeps := false
			switch parent := stack[len(stack)-1].(type) {
			case *ast.AssignStmt:
				if len(parent.Rhs) == 1 && len(parent.Lhs) == count {
					keeps = named(parent.Lhs, at)
				} else if index := slices.Index(parent.Rhs, ast.Expr(call)); index >= 0 && index < len(parent.Lhs) {
					keeps = named(parent.Lhs, []int{index})
				}
			case *ast.ValueSpec:
				names := make([]ast.Expr, len(parent.Names))
				for index, name := range parent.Names {
					names[index] = name
				}
				if len(parent.Values) == 1 && len(names) == count {
					keeps = named(names, at)
				} else if index := slices.Index(parent.Values, ast.Expr(call)); index >= 0 && index < len(names) {
					keeps = named(names, []int{index})
				}
			case *ast.ReturnStmt:
				keeps = len(parent.Results) == 1 || count == 1
			case *ast.ExprStmt, *ast.GoStmt, *ast.DeferStmt:
			case *ast.CallExpr:
				keeps = count == 1 && parent.Fun != ast.Expr(call)
			default:
				keeps = count == 1
			}
			where := fset.Position(call.Pos())
			if !keeps {
				faults = append(faults, filepath.Base(where.Filename)+":"+fmt.Sprint(where.Line)+": drops the charge.Answer of a call")
				return true
			}
			keptCalls++
			if isRead(call) {
				reads++
			}
			if function, ok := info.Types[call.Fun]; !ok || !function.IsType() {
				calls = append(calls, filepath.Base(where.Filename)+":"+fmt.Sprint(where.Line))
			}
			return true
		})
	}
	return faults, keptCalls, reads, calls
}

// importerFunc is a types.Importer that a function is.
type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// Each reading of a text of a judgment keeps the answer of its charge: the judge reads a sentence
// only through charge.Texts.Read, which the types hold, and each call whose results hold a
// charge.Answer, Read's and each walk's that hands Read's answer on, binds it to a name or hands it
// on whole, which the check holds by the type alone (droppedAnswers). What the code then does with
// an answer that it bound, the refusal of each reading in turn holds
// (TestEachRefusedReadingRefusesTheWayOutThatAskedIt). Controls, each a file planted beside the
// package's own: the blank answer of Read, Read as a statement, in a loop and after defer, Read's
// results handed to another call, the blank answer of a walk that hands one on (wholeText.other),
// and a walk of one Answer as a statement and given the blank name, are each named; a call that
// keeps its answer is not; and with no plant, no fault, and the check finds the package's calls of
// Read and of the walks.
func TestEachReadingOfATextKeepsTheAnswerOfItsCharge(t *testing.T) {
	faults, keptCalls, reads, _ := droppedAnswers(t, ".", "")
	if len(faults) > 0 || reads < 6 || keptCalls < 20 {
		t.Errorf("the package's own files: faults %q, %d calls that keep an Answer and %d of them of Read, want none, 20 or more and 6 or more", faults, keptCalls, reads)
	}
	t.Logf("the package's own files: %d calls that keep a charge.Answer, %d of them of charge.Texts.Read", keptCalls, reads)
	for _, plant := range []struct{ name, body string }{
		{"the blank answer of Read", "text, _, _ := j.texts.Read(unit, false)\n\treturn len(text)"},
		{"Read as a statement", "j.texts.Read(unit, false)\n\treturn 0"},
		{"Read in a loop as a statement", "for range 3 {\n\t\tj.texts.Read(unit, false)\n\t}\n\treturn 0"},
		{"Read after defer", "defer j.texts.Read(unit, false)\n\treturn 0"},
		{"Read's results handed to another call", "return len(fmt.Sprint(j.texts.Read(unit, false)))"},
		{"the blank answer of a walk", "at, _ := j.whole.other(0, nil)\n\treturn at.index"},
		{"a walk of one Answer as a statement", "j.whole.load()\n\treturn 0"},
		{"a walk of one Answer given the blank name", "_ = j.whole.load()\n\treturn 0"},
	} {
		planted := "package specform\n\nimport (\n\t\"fmt\"\n\n\t\"" + chargePath + "\"\n)\n\nvar _ = fmt.Sprint\n\nfunc (j *wayJudge) planted(unit charge.Unit) int {\n\t" + plant.body + "\n}\n"
		if faults, _, _, _ := droppedAnswers(t, ".", planted); !slices.ContainsFunc(faults, func(fault string) bool { return strings.HasPrefix(fault, "planted.go:") }) {
			t.Errorf("the control, %s: the check gives the faults %q, want it named in planted.go", plant.name, faults)
		}
	}
	kept := "package specform\n\nimport \"" + chargePath + "\"\n\nfunc (j *wayJudge) planted(unit charge.Unit) charge.Answer {\n\t_, _, charged := j.texts.Read(unit, false)\n\treturn charged\n}\n"
	if faults, calls, read, _ := droppedAnswers(t, ".", kept); len(faults) > 0 || calls != keptCalls+1 || read != reads+1 {
		t.Errorf("the control, a call that keeps its answer: faults %q, %d calls and %d of Read, want none, %d and %d", faults, calls, read, keptCalls+1, reads+1)
	}
}

// refusalTexts is the documents whose judgments TestEachRefusedReadingRefusesTheWayOutThatAskedIt
// refuses reading by reading: between them, a way out of their findings is judged through each call
// of the judge that hands on a charge.Answer, and past each read of an answer that the judge keeps
// for later (wholeText.refused). They hold a stop with no space after it in a sentence whose verb
// stands again after it, and beside two verbs of one group; a name that a sentence after it holds in
// code font (holding); one finding in two sentences (two places of one way out); one sentence
// written twice with an open sentence between (changedRuns); a text that ste-lint reads as two lines
// (read whole); a long piece that runs on into the sentence of a finding; a footnote; a sentence of
// two instructions of a procedure, alone and after an instruction that holds a stop with no space
// after it (two such instructions), whose words with a space in them holding does not look for, so
// that after a refused reading for the stop's way out only load's kept answer refuses theirs; and a
// person's address in two sentences, and in a link, where the address is the finding's one place and
// a remote path in a sentence holds its words: with holding's own reading refused, it finds no
// sentence for the whole text to write, so load reads nothing, and only holding's kept answer
// refuses the way out.
var refusalTexts = []string{
	":::check{#c}\nDispatch checks a1.B now. It checks the log.\n:::\n",
	":::check{#c}\nDispatch checks a1.B now. It verifies the log.\n:::\n",
	":::check{#c}\nIt reads Web.Config now. It logs `Web.Config` today.\n:::\n",
	":::check{#c}\nIt reads a1.B now. It logs a1.B today.\n:::\n",
	":::check{#c}\nHe said \"Run a1.B now.\" It waits. He said \"Run a1.B now.\"\n:::\n",
	":::check{#c}\nDispatch reads it.\u2028Dispatch checks a1.B now.\n:::\n",
	":::check{#c}\nIt runs in the U.S. and it said \"wa wb wc wd we wf wg wh wi wj wk wl wm wn wo wp.\" It keeps a1.B now. It ends.\n:::\n",
	":::check{#c}\nDispatch keeps it[^1].\n:::\n\n[^1]: It checks Foo1.Validate at boot.\n",
	":::check{#v run=\"Run the test and read the log.\"}\nDispatch keeps each record.\n:::\n",
	":::check{#v run=\"Run a1.B now. Run the test and read the log.\"}\nDispatch keeps each record.\n:::\n",
	":::check{#v run=\"Open the file a1.B now. Run the test and read the log.\"}\nDispatch keeps each record.\n:::\n",
	":::check{#c}\nIt sends the log to ada@example.com now. Then it mails ada@example.com the report.\n:::\n",
	":::check{#c}\nIt mails [the report](mailto:ada@example.com) each night. It copies the log to ada@example.com:. now.\n:::\n",
}

// Each refused reading of a judgment ends in the refusal of a way out, which says so
// (elsewhereNote), and changes no other answer: so the code acts on each charge.Answer that it binds
// (TestEachReadingOfATextKeepsTheAnswerOfItsCharge holds the binding). For each document of
// refusalTexts, the judgment (Check) runs once with no reading refused, which counts its readings,
// and then once for each of them with that one refused (refuseReading): it holds each finding of the
// first judgment, the same or with the note in its ways out, and more findings with the note than
// the first. A judgment that fails names the refused reading by the file and line of each call of
// the package that led to it, the call that does not act on its answer among them. And each call
// whose results hold a charge.Answer (droppedAnswers) leads to some refused reading, so each answer
// of the package is seen refused: a call that no document reaches so fails the test, which then
// reads too little.
func TestEachRefusedReadingRefusesTheWayOutThatAskedIt(t *testing.T) {
	_, _, _, calls := droppedAnswers(t, ".", "")
	context := planted("ada.lovelace@example.com")
	// judged is the findings of document, or what stopped its judgment.
	judged := func(document string) (found []Finding, stopped any) {
		defer func() { stopped = recover() }()
		read, err := ExtractDocument("spec.md", 1, document)
		if err != nil {
			return nil, err
		}
		return check(context, Input{Texts: read.Texts, Proposed: read.Terms}), nil
	}
	defer func() { refuseReading = nil }()
	passed := map[string]bool{}
	refusals := 0
	for _, document := range refusalTexts {
		readings := 0
		refuseReading = func() bool {
			readings++
			return false
		}
		want, stopped := judged(document)
		if stopped != nil || readings == 0 {
			t.Fatalf("%q: the judgment with no reading refused stops (%v) or reads no sentence (%d readings): %.300v", document, stopped, readings, want)
		}
		for refused := 1; refused <= readings; refused++ {
			count, at := 0, ""
			refuseReading = func() bool {
				if count++; count != refused {
					return false
				}
				at = readingAt(passed)
				return true
			}
			got, stopped := judged(document)
			if at == "" {
				t.Errorf("%q: the judgment that refuses reading %d of %d makes %d readings", document, refused, readings, count)
				continue
			}
			refusals++
			if fault := refusalFault(want, got, stopped); fault != "" {
				t.Errorf("%q: reading %d of %d refused, at %s: %s", document, refused, readings, at, fault)
			}
		}
	}
	var missed []string
	for _, call := range calls {
		if !passed[call] {
			missed = append(missed, call)
		}
	}
	t.Logf("%d documents, %d readings refused; %d calls that hand on a charge.Answer, %d of them on the way to no refused reading", len(refusalTexts), refusals, len(calls), len(missed))
	if len(calls) < 20 || len(missed) > 0 {
		t.Errorf("of the %d calls that hand on a charge.Answer, no refused reading passes through %q: the test reads too little", len(calls), missed)
	}
}

// readingAt is where a reading of a judgment's texts stands: each call of the code of package
// specform that led to it, innermost first, by its file and line and its function, each of which it
// keeps in passed.
func readingAt(passed map[string]bool) string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
	var at []string
	for {
		frame, more := frames.Next()
		if filepath.Base(filepath.Dir(frame.File)) == "specform" && !strings.HasSuffix(frame.File, "_test.go") {
			site := filepath.Base(frame.File) + ":" + fmt.Sprint(frame.Line)
			passed[site] = true
			at = append(at, site+" ("+strings.TrimPrefix(frame.Function[strings.LastIndex(frame.Function, "/")+1:], "specform.")+")")
		}
		if !more {
			break
		}
	}
	return strings.Join(at, " < ")
}

// refusalFault is what got, the findings of a judgment with one reading refused, or what stopped it,
// holds that it should not, against want, the findings with none refused: each finding of want, the
// same or with the note of a refused reading in its ways out (elsewhereNote), and more findings with
// that note than want. It is "" where got holds that.
func refusalFault(want, got []Finding, stopped any) string {
	if stopped != nil {
		return fmt.Sprintf("the judgment stops: %v", stopped)
	}
	if len(got) != len(want) {
		return fmt.Sprintf("%d findings, want the %d of the judgment with no reading refused: %.300v", len(got), len(want), got)
	}
	noted := 0
	for index := range want {
		each, was := got[index], want[index]
		if strings.Contains(each.Ways, elsewhereNote) && !strings.Contains(was.Ways, elsewhereNote) {
			noted++
		}
		if each == was {
			continue
		}
		each.Ways, was.Ways = "", ""
		if each != was || !strings.Contains(got[index].Ways, elsewhereNote) {
			return fmt.Sprintf("finding %d (%s, %q) says %q, want %q or the note of a refused reading", index, want[index].Rule, want[index].Detail, got[index].Ways, want[index].Ways)
		}
	}
	if noted == 0 {
		return "no finding says that a reading was refused: the refusal changed no answer"
	}
	return ""
}

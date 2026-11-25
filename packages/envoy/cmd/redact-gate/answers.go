package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// answersConfig is the answer census's configuration (gate.answers): the reader every client reads
// an answer's body through and the opaque type it answers, with that type's exact exported method
// set; the standard library's own readers of a response's body; the function that holds another
// party's text as the box (makers), the function that reads an answer's request record (record)
// and the box's method that holds a field with it (recorder); and the functions whose unrecorded
// remote text the census allows, each with why.
type answersConfig struct {
	Reader    []string       `json:"reader"`
	Answer    string         `json:"answer"`
	Methods   []string       `json:"methods"`
	Functions []string       `json:"functions"`
	Readers   []string       `json:"readers"`
	Decoders  []string       `json:"decoders"`
	Makers    []string       `json:"makers"`
	Record    string         `json:"record"`
	Recorder  string         `json:"recorder"`
	Exempt    []answerExempt `json:"exempt"`
}

// answerExempt is a function of one file whose remote text the answer census finds held with no
// request record, and why no answer's request can be echoed there.
type answerExempt struct {
	File     string `json:"file"`
	Function string `json:"function"`
	Why      string `json:"why"`
}

// answerCensus is the answer census over one type-checked package. Whatever answers in a server's
// place can echo the request it was sent, credentials and all, in any byte of its answer, so no
// client of this repository reads an answer's bytes but through redact.ReadAnswer, whose
// redact.Answer decodes them into typed values holding the request's record, prints them only as
// the rule reads them with that record, or streams them to a writer. Outside the box's package:
//
//   - every use of the Body field of a response fails, but its Close called (body.Close()) and an
//     assignment to it: a read of the bytes, wherever they go next - a decoder, a string, a helper,
//     a value of any type - is a read outside the reader, and the census does not follow it. A
//     response here is a net/http.Response or a value of a type whose underlying type is its struct:
//     an alias, a type defined over it (type raw http.Response) in any package, a defined pointer
//     type to one, or a struct declared with its fields, which converts to it (responseType);
//   - every use of the Status field of a response fails, but an assignment to it: it is the status
//     line as the answering party wrote it, whose phrase is that party's own words, so a message
//     names an answer's status through redact.StatusLine, its code and the standard phrase;
//   - every call of the standard library's own readers of a response's body (gate.answers.readers:
//     (*net/http.Response).Write, net/http/httputil.DumpResponse) fails, for the same reason;
//   - each call of the reader is listed, and a census that lists none is UNKNOWN, since it cannot
//     have read a client;
//   - text held as the box (gate.answers.makers) from anything but a constant must be held with
//     the record of the answer it came from: the box's recorder given record(resp)..., or a local
//     set only from it, where resp is the response the text names (a header, the status line);
//     text that names no response, or a record of another response, fails unless
//     gate.answers.exempt names its function with why;
//   - the rules of answers_reach.go: a reader or a maker referred to other than by its call, a
//     decode into a value that holds the box outside Answer.Decode, net/http.ReadResponse given no
//     request, and a value a printer reaches a response in made an interface value or a type
//     parameter's value, where a printer can print it whole. Two such values are listed and pass:
//     errors.As's target, which it only writes an error of err's tree through, and a value whose
//     type is one of gate.external_errors, every use of which the whole-value census judges.
//
// In the box's own package the census checks the reader's type: its exported methods are exactly
// gate.answers.methods, and the package's exported functions that take it exactly
// gate.answers.functions, so no method or function that answers the bytes can be added unseen.
// response is net/http.Response as pkg's imports hold it (httpResponse).
func (g *gate) answerCensus(c *census, dir, path string, fset *token.FileSet, files []*ast.File, info *types.Info, pkg *types.Package, response types.Type) {
	cfg := g.inv.Gate.Answers
	boxPkg, boxName := splitTypeName(g.inv.Gate.Box.Type)
	if path == boxPkg {
		g.answerMethods(c, dir, fset, pkg, cfg)
		g.answerFunctions(c, dir, fset, pkg, cfg)
		return
	}
	a := &answerReader{info: info, cfg: cfg, readers: set(cfg.Readers), makers: set(cfg.Makers), decoders: set(cfg.Decoders), boxPkg: boxPkg, boxName: boxName, httpResponse: response, external: set(g.inv.Gate.External)}
	for _, f := range files {
		rel, _ := filepath.Rel(dir, fset.Position(f.Pos()).Filename)
		rel = filepath.ToSlash(rel)
		var stack []ast.Node
		var fn []string
		function := func() string {
			if len(fn) == 0 {
				return path + ".<package>"
			}
			return fn[len(fn)-1]
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				if _, ok := stack[len(stack)-1].(*ast.FuncDecl); ok {
					fn = fn[:len(fn)-1]
				}
				if vs, ok := stack[len(stack)-1].(*ast.ValueSpec); ok && len(vs.Names) > 0 && len(stack) == 3 {
					fn = fn[:len(fn)-1]
				}
				stack = stack[:len(stack)-1]
				return true
			}
			parents := stack
			stack = append(stack, n)
			switch n := n.(type) {
			case *ast.FuncDecl:
				fn = append(fn, goFuncName(path, n))
			case *ast.ValueSpec:
				// A package-level var's value (a func literal, a composite literal of them) is read
				// as code of its own, named for the var.
				if len(parents) == 2 && len(n.Names) > 0 {
					fn = append(fn, path+"."+n.Names[0].Name)
				}
			}
			add := func(kind string, ok bool, what, why string) {
				p := fset.Position(n.Pos())
				c.Answers = append(c.Answers, finding{File: rel, Line: p.Line, Function: function(), What: what, Kind: kind, OK: ok, Why: why})
			}
			for _, cv := range a.conversions(n, parents) {
				if !a.holdsResponse(cv.from) {
					continue
				}
				switch held := types.TypeString(cv.from, nil); {
				case cv.pass != "":
					add("whole-response", true, exprString(cv.expr), cv.pass)
				case a.externalError(cv.from):
					add("whole-response", true, exprString(cv.expr), "a "+held+", an external error type's value, every use of which the whole-value census judges")
				default:
					add("whole-response", false, exprString(cv.expr), "a response handed whole to "+cv.to+", as a "+held+", where fmt, log, slog and encoding/json print the status line the answering party wrote and every header it sent, as they came: name its status through redact.StatusLine and a header through redact.Header")
				}
			}
			if name, ok := a.reference(n, parents); ok {
				add("reference", false, exprString(n.(ast.Expr)), shortName(name)+" referred to other than by its call, as a function or method value, which no call rule follows: call it, so the census reads the call")
			}
			switch n := n.(type) {
			case *ast.SelectorExpr:
				switch {
				case a.responseField(n, "Body"):
					switch {
					case a.closed(n, parents):
						add("close", true, exprString(n), "the body closed")
					case a.assigned(n, parents):
						add("set", true, exprString(n), "the body set, not read")
					default:
						add("body", false, exprString(n), "a read of an answer's body outside "+readerNames(cfg)+": read it through the reader, whose Answer decodes it into typed values with the request's record, prints it only as the rule reads it, or streams it as data")
					}
				case a.responseField(n, "Status"):
					if a.assigned(n, parents) {
						add("status-set", true, exprString(n), "the status line set, not read")
					} else {
						add("status", false, exprString(n), "a read of an answer's status line as the answering party wrote it, whose phrase is its own words: name the status through redact.StatusLine (oauth2call.StatusLine), its code and the standard phrase")
					}
				}
			case *ast.CallExpr:
				if why := a.decodedIntoBox(n, function()); why != "" {
					add("decode", false, exprString(n), why)
				}
				if a.readsNoRequest(n) {
					add("no-request", false, exprString(n), "net/http.ReadResponse given no request: the answer it reads holds no record of what the request carried, so none of its credentials is redacted from the answer's words; hand it the request the answer answers")
				}
				name := a.calleeName(n)
				switch {
				case slices.Contains(cfg.Reader, name):
					add("read", true, exprString(n), "the answer read through "+shortName(name))
				case a.readers[name]:
					add("body", false, exprString(n), name+" reads the answer's body outside "+readerNames(cfg))
				case a.makers[name] && !a.constant(n):
					ok, why := a.recorded(n, parents)
					add("maker", ok, exprString(n), why)
				}
			}
			return true
		})
	}
}

// answerMethods lists the reader's type with its exported method set, OK only where it is exactly
// gate.answers.methods; a type that is not in pkg is listed as not found.
func (g *gate) answerMethods(c *census, dir string, fset *token.FileSet, pkg *types.Package, cfg answersConfig) {
	_, name := splitTypeName(cfg.Answer)
	obj := pkg.Scope().Lookup(name)
	if obj == nil {
		c.Answers = append(c.Answers, finding{Kind: "methods", What: cfg.Answer, Why: "the reader's type is not declared in its package"})
		return
	}
	var got []string
	for _, t := range []types.Type{obj.Type(), types.NewPointer(obj.Type())} {
		ms := types.NewMethodSet(t)
		for i := range ms.Len() {
			if m := ms.At(i).Obj(); m.Exported() && !slices.Contains(got, m.Name()) {
				got = append(got, m.Name())
			}
		}
	}
	p := fset.Position(obj.Pos())
	rel, _ := filepath.Rel(dir, p.Filename)
	c.Answers = append(c.Answers, exactly(finding{File: filepath.ToSlash(rel), Line: p.Line, Function: cfg.Answer, Kind: "methods"}, got, cfg.Methods, "its exported methods are", "a method"))
}

// exactly is f, a listing of the reader's type, with what it lists, got: OK only where got is
// exactly want, in any order. are says what got is ("its exported methods are"), and one what one
// more would be ("a method"), which answers the body's bytes and so lets a client print them.
func exactly(f finding, got, want []string, are, one string) finding {
	sort.Strings(got)
	want = slices.Clone(want)
	sort.Strings(want)
	f.What = strings.Join(got, ", ")
	if slices.Equal(got, want) {
		f.OK, f.Why = true, are+" exactly "+strings.Join(want, ", ")
	} else {
		f.Why = are + " " + strings.Join(got, ", ") + ", not exactly " + strings.Join(want, ", ") + ": " + one + " that answers the body's bytes lets a client print them"
	}
	return f
}

type answerReader struct {
	info                      *types.Info
	cfg                       answersConfig
	readers, makers, decoders map[string]bool
	boxPkg, boxName           string
	httpResponse              types.Type      // net/http.Response as the package's imports hold it, or nil where they hold no net/http
	external                  map[string]bool // gate.external_errors, the types the whole-value census judges every use of
}

// externalError reports whether t, through any number of pointers, is one of gate.external_errors.
func (a *answerReader) externalError(t types.Type) bool {
	for {
		p, ok := types.Unalias(t).(*types.Pointer)
		if !ok {
			break
		}
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && a.external[n.Obj().Pkg().Path()+"."+n.Obj().Name()]
}

// responseField reports whether sel selects the field name of a response (responseType),
// directly, through a pointer or through an embedded one.
func (a *answerReader) responseField(sel *ast.SelectorExpr, name string) bool {
	s := a.info.Selections[sel]
	if s == nil || s.Kind() != types.FieldVal {
		return false
	}
	if v, ok := s.Obj().(*types.Var); !ok || v.Name() != name {
		return false
	}
	t := s.Recv()
	index := s.Index()
	for i, k := range index {
		t = derefType(t)
		st, ok := t.Underlying().(*types.Struct)
		if !ok {
			return false
		}
		if i == len(index)-1 {
			return a.responseType(t)
		}
		t = st.Field(k).Type()
	}
	return false
}

// closed reports whether sel, a response's Body, is the receiver of its Close, called.
func (a *answerReader) closed(sel ast.Expr, parents []ast.Node) bool {
	if len(parents) < 2 {
		return false
	}
	closeSel, ok := parents[len(parents)-1].(*ast.SelectorExpr)
	if !ok || closeSel.X != sel || closeSel.Sel.Name != "Close" {
		return false
	}
	call, ok := parents[len(parents)-2].(*ast.CallExpr)
	return ok && call.Fun == closeSel
}

// assigned reports whether sel, a response's Body, is a target of an assignment.
func (a *answerReader) assigned(sel ast.Expr, parents []ast.Node) bool {
	as, ok := parents[len(parents)-1].(*ast.AssignStmt)
	return ok && slices.Contains(as.Lhs, sel)
}

func (a *answerReader) calleeName(call *ast.CallExpr) string {
	fun := ast.Unparen(call.Fun)
	if ix, ok := fun.(*ast.IndexExpr); ok {
		fun = ix.X
	}
	var fn *types.Func
	switch x := fun.(type) {
	case *ast.Ident:
		fn, _ = a.info.Uses[x].(*types.Func)
	case *ast.SelectorExpr:
		if sel := a.info.Selections[x]; sel != nil {
			fn, _ = sel.Obj().(*types.Func)
		} else {
			fn, _ = a.info.Uses[x.Sel].(*types.Func)
		}
	}
	if fn == nil {
		return ""
	}
	return fn.Origin().FullName()
}

// constant reports whether every argument of call is a constant.
func (a *answerReader) constant(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		if tv, ok := a.info.Types[arg]; !ok || tv.Value == nil {
			return false
		}
	}
	return true
}

// recorded says whether call, text held as the box, is held with the record of the answer it came
// from: the box's recorder called on it, given record(x)... or a local set only from record(x),
// where x is the response the text names. Text that names no response has no record to be held
// with.
func (a *answerReader) recorded(call *ast.CallExpr, parents []ast.Node) (bool, string) {
	from := a.response(call)
	var recorder *ast.CallExpr
	if len(parents) >= 2 {
		if sel, ok := parents[len(parents)-1].(*ast.SelectorExpr); ok && sel.X == call {
			if c, ok := parents[len(parents)-2].(*ast.CallExpr); ok && c.Fun == sel && a.calleeName(c) == a.cfg.Recorder {
				recorder = c
			}
		}
	}
	switch {
	case recorder == nil:
		return false, "text held as the box with no request record: hold it with " + shortName(a.cfg.Recorder) + "(" + shortName(a.cfg.Record) + "(resp)...), resp the answer it came from"
	case from == nil:
		return false, "text that names no answer, held as the box: no request record can hold it"
	}
	record := a.recordOf(recorder, parents)
	switch {
	case record == nil:
		return false, shortName(a.cfg.Recorder) + " is given no request record: hand it " + shortName(a.cfg.Record) + "(resp)..., or a local set only from it"
	case record != from:
		return false, "held with the record of " + record.Name() + ", another answer than " + from.Name() + ", whose text it is"
	}
	return true, "held with the record of " + from.Name() + ", the answer it came from"
}

// response is the response expr names: the first variable it holds whose type is a response or a
// pointer to one (isResponse), or nil.
func (a *answerReader) response(expr ast.Expr) types.Object {
	var found types.Object
	ast.Inspect(expr, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || found != nil {
			return found == nil
		}
		if obj := a.info.Uses[id]; obj != nil && a.isResponse(obj.Type()) {
			found = obj
		}
		return true
	})
	return found
}

// isResponse reports whether t, or what it points to, is a response (responseType).
func (a *answerReader) isResponse(t types.Type) bool {
	return a.responseType(derefType(t))
}

// responseType reports whether t is a response: net/http.Response or an alias of it, or a type
// whose underlying type is the response's struct - one defined over it (type raw http.Response),
// in this package or another, or a struct declared with its fields, which converts to it - so its
// Body and Status are the answer's, whatever the type is named.
func (a *answerReader) responseType(t types.Type) bool {
	if n, ok := types.Unalias(t).(*types.Named); ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "net/http" && n.Obj().Name() == "Response" {
		return true
	}
	st, ok := t.Underlying().(*types.Struct)
	return ok && a.httpResponse != nil && types.Identical(st, a.httpResponse.Underlying())
}

// importedPackage is the package of path among pkg's imports, direct or not, or nil. A package
// loaded from export data lists every package that data loaded as its imports.
func importedPackage(pkg *types.Package, path string) *types.Package {
	if pkg == nil {
		return nil
	}
	for _, direct := range pkg.Imports() {
		if direct.Path() == path {
			return direct
		}
		for _, p := range direct.Imports() {
			if p.Path() == path {
				return p
			}
		}
	}
	return nil
}

// httpResponse is net/http.Response as pkg's imports hold it, or nil where they hold no net/http.
// Export data loads a package only as far as it names it, so net/http can be in the imports
// without its Response, which the answer census compares a type's struct with: imp loads it whole
// there.
func httpResponse(pkg *types.Package, imp types.Importer) (types.Type, error) {
	http := importedPackage(pkg, "net/http")
	if http == nil {
		return nil, nil
	}
	if http.Scope().Lookup("Response") == nil {
		if _, err := imp.Import("net/http"); err != nil {
			return nil, fmt.Errorf("net/http cannot be loaded for the answer census: %v", err)
		}
	}
	response, ok := http.Scope().Lookup("Response").(*types.TypeName)
	if !ok {
		return nil, errors.New("net/http is in its imports without its Response, which the answer census compares a type's struct with")
	}
	return response.Type(), nil
}

// recordOf is the response whose record recorder is given: record(x)... directly, or a local that
// every assignment in the function sets from record(x); nil otherwise.
func (a *answerReader) recordOf(recorder *ast.CallExpr, parents []ast.Node) types.Object {
	if !recorder.Ellipsis.IsValid() || len(recorder.Args) != 1 {
		return nil
	}
	arg := ast.Unparen(recorder.Args[0])
	if c, ok := arg.(*ast.CallExpr); ok && a.calleeName(c) == a.cfg.Record && len(c.Args) == 1 {
		return a.response(c.Args[0])
	}
	id, ok := arg.(*ast.Ident)
	if !ok {
		return nil
	}
	local := a.info.Uses[id]
	var body ast.Node
	for i := len(parents) - 1; i >= 0 && body == nil; i-- {
		switch p := parents[i].(type) {
		case *ast.FuncDecl:
			body = p
		case *ast.FuncLit:
			body = p
		}
	}
	if local == nil || body == nil {
		return nil
	}
	var from types.Object
	sets, fromRecord := 0, true
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			l, isID := lhs.(*ast.Ident)
			if !isID || (a.info.Defs[l] != local && a.info.Uses[l] != local) {
				continue
			}
			sets++
			if len(as.Lhs) != len(as.Rhs) {
				fromRecord = false
				continue
			}
			c, isCall := ast.Unparen(as.Rhs[i]).(*ast.CallExpr)
			if !isCall || a.calleeName(c) != a.cfg.Record || len(c.Args) != 1 {
				fromRecord = false
				continue
			}
			if r := a.response(c.Args[0]); from == nil || from == r {
				from = r
			} else {
				fromRecord = false
			}
		}
		return true
	})
	if sets == 0 || !fromRecord {
		return nil
	}
	return from
}

// derefType is t, or what it points to: through a pointer type, an alias of one, or a type
// defined over one (type p *T), whose field a selector reads as a pointer's.
func derefType(t types.Type) types.Type {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

// shortName is a function's full name without its package path: redact.Carried, (RemoteText).WithoutTokens.
func shortName(full string) string {
	open := strings.HasPrefix(full, "(")
	body := strings.TrimPrefix(full, "(")
	star := strings.HasPrefix(body, "*")
	body = strings.TrimPrefix(body, "*")
	if slash := strings.LastIndex(body, "/"); slash >= 0 {
		body = body[slash+1:]
	}
	switch {
	case open && star:
		return "(*" + body
	case open:
		return "(" + body
	}
	return body
}

// recordedAnswers is how many reads of an answer through the reader the answer census listed.
func recordedAnswers(c census) int {
	n := 0
	for _, a := range c.Answers {
		if a.Kind == "read" {
			n++
		}
	}
	return n
}

// judgeAnswers fails each finding of the answer census that is not OK and that gate.answers.exempt
// does not name, each exemption that names none, a census that listed no read through the reader
// (UNKNOWN), and one that never found the reader's type.
func (g *gate) judgeAnswers(c census) {
	if recordedAnswers(c) == 0 {
		g.fail("stage 2 (answer census) UNKNOWN: it listed %d uses of an answer and no read through %s, so it read no client", len(c.Answers), readerNames(g.inv.Gate.Answers))
	}
	methods := false
	used := map[int]bool{}
	for _, a := range c.Answers {
		if a.Kind == "methods" {
			methods = true
		}
		if a.OK {
			continue
		}
		exempt := false
		for i, e := range g.inv.Gate.Answers.Exempt {
			if a.Kind == "maker" && e.File == a.File && e.Function == a.Function {
				exempt, used[i] = true, true
				fmt.Fprintf(g.out, "EXEMPT answer %s:%d %s in %s: %s\n", a.File, a.Line, a.What, a.Function, e.Why)
			}
		}
		if !exempt {
			g.fail("stage 2 (answer census) %s:%d %s in %s: %s", a.File, a.Line, a.What, a.Function, a.Why)
		}
	}
	if !methods {
		g.fail("stage 2 (answer census) UNKNOWN: the reader's type %s was not found", g.inv.Gate.Answers.Answer)
	}
	for i, e := range g.inv.Gate.Answers.Exempt {
		if !used[i] {
			g.fail("stage 2 (answer census) the exemption of %s in %s matches no text the census failed", e.Function, e.File)
		}
	}
}

// readerNames is the census's readers as a message names them: redact.ReadAnswer or
// redact.StreamAnswer.
func readerNames(cfg answersConfig) string {
	names := make([]string, len(cfg.Reader))
	for i, r := range cfg.Reader {
		names[i] = shortName(r)
	}
	return strings.Join(names, " or ")
}

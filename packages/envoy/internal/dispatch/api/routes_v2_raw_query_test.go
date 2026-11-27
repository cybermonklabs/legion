//go:build !race

// The scan is static, as the copy-lock and keyed-write guards whose graph it reads are: the race
// detector adds nothing to it.

package api

import (
	"go/ast"
	"go/types"
	"maps"
	"slices"
	"strings"
	"testing"
)

// rawQueryReaders are the functions of package api that read the raw query - r.URL.Query(),
// r.URL.RawQuery, r.FormValue, r.Form, r.ParseForm - and may, whatever handler reaches them, each
// with the reason. Every other function that reads it is reached by no handler of a /api/v2 route
// the rules bind: a /api/v1 handler, a test hook's, a row of the INFRA-979 tree's, or what only
// those reach (R4).
var rawQueryReaders = map[string]string{
	"api.v2Decoder.readQuery": "the decoder: the one reader of a /api/v2 route's query, which every handler behind it reads " +
		"through v2Query",
	"api.requestSession": "its fallback: the session a request names, decoded on a route the decoder reads and read off the " +
		"raw query only on a route it does not read (a /api/v1 route, a test hook, a row of the INFRA-979 tree)",
	"api.writeRequestFields": "the idempotency fingerprint, which holds the query whole, so a repeat with another query is " +
		"another request (R4 lets it read the query whole)",
	"api.unstorableParameter": "storable_text.go: every route refuses a parameter holding U+0000 or a byte that is not " +
		"UTF-8 before its handler runs, whatever the route declares",
	"api.legionStateURL": "settings_legion.go: the query of the daemon URL a Legion setting's body names, read to refuse " +
		"a URL that carries one, and no request's query",
}

// v1OnlyWriters are the response writers of package api that only a /api/v1 route's chain wraps an
// answer in, each with the one function that builds it and the reason no /api/v2 answer goes
// through it. The graph resolves a call of an http.ResponseWriter method to the method of every
// writer of the module that implements it (funcTargets), so a /api/v2 handler's w.WriteHeader
// reaches each writer's WriteHeader; a writer only a /api/v1 chain builds is never the w a /api/v2
// handler writes to, and the scan follows no /api/v2 handler into its methods. Each writer is held
// to its builder: one built in another function, or a builder a /api/v2 handler reaches, fails the
// scan.
var v1OnlyWriters = map[string]struct{ builtBy, reason string }{
	"successorWriter": {
		builtBy: "api.linkSuccessor",
		reason: "routeHandler wraps only a /api/v1 route's chain in linkSuccessor, and no /api/v2 answer carries its Link " +
			"(TestAV1AnswerLinksTheV2RouteThatAnswersForIt)",
	},
}

// v1OnlyWriterMethod reports whether fn is a method of a writer v1OnlyWriters names, or a function
// literal inside one.
func v1OnlyWriterMethod(fn *lockFunction) bool {
	name := declaringFunction(fn)
	for writer := range v1OnlyWriters {
		if strings.HasPrefix(name, "api."+writer+".") {
			return true
		}
	}
	return false
}

// rawQueryRead reports whether selector reads the raw query: the URL's Query method or RawQuery
// field, or the http.Request's FormValue, Form and ParseForm, each of which fills or reads r.Form
// from the URL's query as well as the body. info decides what the selector names, so a field or
// method of the same name on another type is none.
func rawQueryRead(info *types.Info, selector *ast.SelectorExpr) bool {
	selection, ok := info.Selections[selector]
	if !ok || selection.Obj().Pkg() == nil {
		return false
	}
	switch selection.Obj().Pkg().Path() + "." + selection.Obj().Name() {
	case "net/url.Query", "net/url.RawQuery", "net/http.FormValue", "net/http.Form", "net/http.ParseForm":
		return true
	}
	return false
}

// declaringFunction is the declared function a graph function stands in: its own name, or for a
// function literal the function whose body holds it.
func declaringFunction(fn *lockFunction) string {
	if at := strings.LastIndex(fn.name, " in "); strings.HasPrefix(fn.name, "the function literal at ") && at >= 0 {
		return fn.name[at+len(" in "):]
	}
	return fn.name
}

// TestNoV2HandlerReadsTheRawQuery holds R4's "no /api/v2 handler reads the raw query" to the
// package's source. It reads every non-test file of package api through the copy-lock guard's
// graph (TestCopyLockComesBeforeEveryPendingRowAndEvent says how a call is resolved), finds every
// read of the raw query (rawQueryRead), and follows each row's handler through everything it
// calls and every function it hands on, as the keyed-write guard does. A handler is a /api/v2
// handler when the row it is declared on is mounted at a /api/v2 route the rules bind
// (apiRoute.v2Bound): the table decides, not the file's name, so a handler a /api/v2 row file
// names and one a /api/v1 route file names are told apart however the files are laid out, and a handler one
// row serves in both versions is a /api/v2 handler. The walk from a /api/v2 handler stops at the
// methods of a writer only a /api/v1 chain builds (v1OnlyWriters). A read is allowed in a function
// rawQueryReaders lists with its reason, and in a function no /api/v2 handler reaches that another
// handler does: a /api/v1 handler, a test hook's, a row of the INFRA-979 tree's. A read in a
// function no handler reaches at all - a layer of the chain, a helper - is named until it is
// listed, so every reader of the raw query outside a handler is one the list states a reason for.
func TestNoV2HandlerReadsTheRawQuery(t *testing.T) {
	graph := copyLockGraph(t)
	bound := map[string]bool{}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		if route.v2Bound() {
			bound[route.Method+" "+route.Pattern] = true
		}
	}

	reachedByV2, reachedByOther := map[*lockFunction]bool{}, map[*lockFunction]bool{}
	var walk func(fn *lockFunction, reached map[*lockFunction]bool, skip func(*lockFunction) bool)
	walk = func(fn *lockFunction, reached map[*lockFunction]bool, skip func(*lockFunction) bool) {
		if fn == nil || reached[fn] || skip(fn) {
			return
		}
		reached[fn] = true
		for _, call := range ownCalls(fn) {
			for _, callee := range graph.callees(call) {
				walk(callee, reached, skip)
			}
			for _, argument := range call.Args {
				for _, callee := range graph.functionValue(argument) {
					walk(callee, reached, skip)
				}
			}
		}
	}
	skipNone := func(*lockFunction) bool { return false }
	v2Handlers := 0
	for _, row := range tableRowsInSource(t, graph) {
		handlers := graph.functionValue(row.handler)
		if slices.ContainsFunc(row.names, func(name string) bool { return bound[name] }) {
			if len(handlers) == 0 {
				t.Errorf("the scan cannot read the handler of %v", row.names)
			}
			v2Handlers++
			for _, handler := range handlers {
				walk(handler, reachedByV2, v1OnlyWriterMethod)
			}
			continue
		}
		for _, handler := range handlers {
			walk(handler, reachedByOther, skipNone)
		}
	}
	if v2Handlers == 0 || !reachedByV2[graph.named["api.server.getRouteIndexV2"]] {
		t.Fatalf("the scan reads %d /api/v2 handlers and does not reach api.server.getRouteIndexV2, GET /api/v2's", v2Handlers)
	}
	for _, writer := range slices.Sorted(maps.Keys(v1OnlyWriters)) {
		held := v1OnlyWriters[writer]
		built := 0
		for _, fn := range graph.functions {
			if fn.pkg != "api" {
				continue
			}
			ast.Inspect(fn.body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.FuncLit:
					return false
				case *ast.CompositeLit:
					// The graph records what each name denotes (Uses), not each expression's type,
					// so the literal is read by the type it names.
					ident, ok := n.Type.(*ast.Ident)
					if !ok {
						return true
					}
					named, ok := graph.info.Uses[ident].(*types.TypeName)
					if !ok || named.Pkg() == nil || named.Pkg().Name() != "api" || named.Name() != writer {
						return true
					}
					built++
					if where := declaringFunction(fn); where != held.builtBy {
						t.Errorf("%s builds a %s at %s, which v1OnlyWriters holds to %s (%s): a /api/v2 answer may go through it, "+
							"so the scan must follow a /api/v2 handler into its methods", where, writer, graph.line(fn.source, n), held.builtBy, held.reason)
					}
				}
				return true
			})
		}
		if built == 0 {
			t.Errorf("v1OnlyWriters names %s, which no function of package api builds: delete its line", writer)
		}
		if reachedByV2[graph.named[held.builtBy]] {
			t.Errorf("a /api/v2 handler reaches %s, which builds %s: a /api/v2 answer may go through it, so v1OnlyWriters "+
				"cannot name it (%s)", held.builtBy, writer, held.reason)
		}
	}

	listed := map[string]bool{}
	var offences []string
	for _, fn := range graph.functions {
		if fn.pkg != "api" {
			continue
		}
		var reads []string
		ast.Inspect(fn.body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.FuncLit:
				// A function literal is a function of its own in the graph, read on its own.
				return false
			case *ast.SelectorExpr:
				if rawQueryRead(graph.info, n) {
					reads = append(reads, graph.line(fn.source, n)+" ("+n.Sel.Name+")")
				}
			}
			return true
		})
		if len(reads) == 0 {
			continue
		}
		name := declaringFunction(fn)
		if _, allowed := rawQueryReaders[name]; allowed {
			listed[name] = true
			continue
		}
		switch {
		case reachedByV2[fn]:
			offences = append(offences, fn.name+" reads the raw query at "+strings.Join(reads, ", ")+
				", and a /api/v2 handler reaches it: read the decoded query (v2Query, requestSession) instead")
		case !reachedByOther[fn]:
			offences = append(offences, fn.name+" reads the raw query at "+strings.Join(reads, ", ")+
				", and no handler reaches it: list it in rawQueryReaders with the reason it may")
		}
	}
	slices.Sort(offences)
	if len(offences) > 0 {
		t.Errorf("the raw query is read where R4 forbids it:\n\t%s", strings.Join(offences, "\n\t"))
	}
	for _, name := range slices.Sorted(maps.Keys(rawQueryReaders)) {
		if !listed[name] {
			t.Errorf("rawQueryReaders lists %s, which reads no raw query: delete its line, or point it at the reader that replaced it", name)
		}
	}
	if !reachedByOther[graph.named["api.server.listIssues"]] {
		t.Error("the scan does not reach api.server.listIssues, a /api/v1 handler that reads the raw query, so it cannot tell a /api/v1 reader from a stray one")
	}
}

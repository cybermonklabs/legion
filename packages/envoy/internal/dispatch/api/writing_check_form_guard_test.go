//go:build !race

// The guard is static, as the writing-check route guard whose call-graph walk it shares: it reads
// the module's source, so the race detector adds nothing to it.

package api

import (
	"go/ast"
	"go/types"
	"maps"
	"slices"
	"strings"
	"testing"
)

// multipartUploadFunction is the function that reads a multipart upload's form, as the call
// graph names it.
const multipartUploadFunction = "api.server.multipartArtifactUpload"

// formFieldReaders names the functions of package api that read one form value by a name their
// caller passes, with the index of that argument: multipartJSONField reads an upload's `actor` and
// `precondition` so. The read is the caller's, under the name the call passes, so the guard reads
// that name at each call - a constant, or a name it cannot read - in place of the reader's own
// body, and walks what the reader calls as it walks any callee. A reader passed as a value, not
// called, is walked as any function is, where its read by its parameter is one the guard cannot
// name. TestEveryFormValueTheUploadReadsIsMarked holds each reader's own body to reading the form
// by that argument alone, so an entry here hides no other read.
var formFieldReaders = map[string]int{"api.multipartJSONField": 3}

// formReads is what the functions a route runs read of its request's form: each form value read
// by name, with the lines that read it, and each read whose name the guard cannot tell.
type formReads struct {
	named   map[string][]string
	unnamed []string
}

// uploadFormReads is every read of the request's form in what handlers run inside package api,
// walked as routeReaches walks it: a call is followed as the copy-lock graph resolves it, plus
// every function handed to it as an argument. A field reader's own body is the one not read where
// it is called (formFieldReaders); what it calls is walked all the same.
func uploadFormReads(graph *lockGraph, handlers []*lockFunction) formReads {
	reads := formReads{named: map[string][]string{}}
	walked := map[*lockFunction]bool{}
	followed := map[*lockFunction]bool{}
	var walk, follow func(fn *lockFunction)
	// walk reads fn's own body, then follows its calls.
	walk = func(fn *lockFunction) {
		if fn == nil || fn.pkg != decisionBlockGuardPkg || walked[fn] {
			return
		}
		walked[fn] = true
		ast.Inspect(fn.body, func(node ast.Node) bool {
			if _, ok := node.(*ast.FuncLit); ok {
				return false
			}
			name, read, known := formReadOf(graph, fn.pkg, node)
			switch {
			case !read:
			case known:
				reads.named[name] = append(reads.named[name], graph.line(fn.source, node))
			default:
				reads.unnamed = append(reads.unnamed, graph.line(fn.source, node))
			}
			return true
		})
		follow(fn)
	}
	// follow walks what fn's calls run, without reading fn's own body: each callee, and every
	// function handed to a call as an argument. A field reader is followed rather than walked: its
	// read was read at the call, by the name the call passes (formReadKeyOf).
	follow = func(fn *lockFunction) {
		if fn == nil || fn.pkg != decisionBlockGuardPkg || followed[fn] {
			return
		}
		followed[fn] = true
		for _, call := range ownCalls(fn) {
			if _, reader := formFieldReaderOf(graph, call); reader {
				follow(graph.named[calledFunctionName(graph, call)])
			} else {
				for _, callee := range graph.callees(call) {
					walk(callee)
				}
			}
			for _, argument := range call.Args {
				for _, passed := range graph.functionValue(argument) {
					walk(passed)
				}
			}
		}
	}
	for _, handler := range handlers {
		walk(handler)
	}
	return reads
}

// formFieldReaderOf reports whether call calls a function formFieldReaders names, and the index of
// the argument that names the form value it reads.
func formFieldReaderOf(graph *lockGraph, call *ast.CallExpr) (int, bool) {
	index, reader := formFieldReaders[calledFunctionName(graph, call)]
	return index, reader && index < len(call.Args)
}

// formReadOf reports whether node reads a value or a file of the request's form, and the name it
// reads when that name is a constant (formReadKeyOf).
func formReadOf(graph *lockGraph, pkg string, node ast.Node) (name string, read, known bool) {
	key, read := formReadKeyOf(graph, node)
	if !read || key == nil {
		return "", read, false
	}
	name, known = literalText(ast.Unparen(key), pkg, graph.constants)
	return name, true, known
}

// formReadKeyOf reports whether node reads a value or a file of the request's form, and the
// expression naming what it reads: a call of a field reader (formFieldReaders), by the argument
// that names its value, a call of http.Request's FormValue, PostFormValue or FormFile, an index or
// a Get or Has of http.Request's Form or PostForm, an index of multipart.Form's Value or File, or
// a read of the raw parts (MultipartReader), which names none and answers a nil key.
func formReadKeyOf(graph *lockGraph, node ast.Node) (key ast.Expr, read bool) {
	switch n := node.(type) {
	case *ast.CallExpr:
		if index, reader := formFieldReaderOf(graph, n); reader {
			return n.Args[index], true
		}
		switch calledFunctionName(graph, n) {
		case "http.Request.FormValue", "http.Request.PostFormValue", "http.Request.FormFile":
			return n.Args[0], true
		case "url.Values.Get", "url.Values.Has":
			selector, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr)
			if !ok || !isFormField(graph, selector.X) {
				return nil, false
			}
			return n.Args[0], true
		case "http.Request.MultipartReader":
			return nil, true
		}
	case *ast.IndexExpr:
		if isFormField(graph, n.X) {
			return n.Index, true
		}
	}
	return nil, false
}

// formFieldReaderReadsOnlyItsArgument reports, for a function formFieldReaders names, each read of
// the request's form in its own body that is not a read by its name argument (the parameter at
// index), so an entry of formFieldReaders cannot hide a read of the reader's own; what the reader
// calls, uploadFormReads walks.
func formFieldReaderReadsOnlyItsArgument(graph *lockGraph, reader *lockFunction, index int) []string {
	var parameter types.Object
	for fn, declared := range graph.declared {
		if declared == reader && index < fn.Signature().Params().Len() {
			parameter = fn.Signature().Params().At(index)
		}
	}
	var other []string
	byArgument := false
	ast.Inspect(reader.body, func(node ast.Node) bool {
		key, read := formReadKeyOf(graph, node)
		if !read {
			return true
		}
		ident, ok := ast.Unparen(key).(*ast.Ident)
		if key == nil || !ok || parameter == nil || graph.info.Uses[ident] != parameter {
			other = append(other, graph.line(reader.source, node))
		} else {
			byArgument = true
		}
		return true
	})
	if !byArgument {
		other = append(other, "no read of the form by that argument at all")
	}
	return other
}

// isFormField reports whether expression selects a field that holds the request's form values:
// http.Request's Form or PostForm, or multipart.Form's Value or File.
func isFormField(graph *lockGraph, expression ast.Expr) bool {
	selector, ok := ast.Unparen(expression).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	selection, ok := graph.info.Selections[selector]
	if !ok || selection.Kind() != types.FieldVal {
		return false
	}
	field := selection.Obj()
	switch field.Pkg().Path() + "." + field.Name() {
	case "net/http.Form", "net/http.PostForm", "mime/multipart.Value", "mime/multipart.File":
		return true
	}
	return false
}

// TestEveryFormValueTheUploadReadsIsMarked holds artifactUploadFormValues to the source of the
// upload routes: every form value or file that a route reaching multipartArtifactUpload reads by
// name, anywhere in what it runs inside package api, is marked prose or not prose with a reason,
// and every mark names a value one of them reads. A form value read and left unmarked is text the
// writing check never reads, so a new one fails here until it is marked.
func TestEveryFormValueTheUploadReadsIsMarked(t *testing.T) {
	graph := copyLockGraph(t)
	if graph.named[multipartUploadFunction] == nil {
		t.Fatalf("%s, the upload this guard reads, is gone: point the guard at its replacement", multipartUploadFunction)
	}
	for _, name := range slices.Sorted(maps.Keys(formFieldReaders)) {
		reader := graph.named[name]
		if reader == nil {
			t.Fatalf("%s, a form field reader formFieldReaders names, is gone: delete its entry", name)
		}
		if other := formFieldReaderReadsOnlyItsArgument(graph, reader, formFieldReaders[name]); len(other) > 0 {
			t.Errorf("%s reads the form by other than its argument %d, which the guard reads at each call:\n\t%s",
				name, formFieldReaders[name], strings.Join(other, "\n\t"))
		}
	}
	routes := writeRouteHandlers(t, graph, writingCheckMethods)
	reads := formReads{named: map[string][]string{}}
	var uploads []string
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		if !routeReaches(graph, routes[name], func(call *ast.CallExpr) bool {
			return calledFunctionName(graph, call) == multipartUploadFunction
		}) {
			continue
		}
		uploads = append(uploads, name)
		route := uploadFormReads(graph, routes[name])
		for value, where := range route.named {
			reads.named[value] = append(reads.named[value], where...)
		}
		reads.unnamed = append(reads.unnamed, route.unnamed...)
	}
	for name := range keyedUploads {
		if !slices.Contains(uploads, name) {
			t.Errorf("%s, an upload route, does not reach %s", name, multipartUploadFunction)
		}
	}
	t.Logf("upload routes: %s", strings.Join(uploads, ", "))

	var unmarked, unexplained []string
	for _, value := range slices.Sorted(maps.Keys(reads.named)) {
		where := strings.Join(slices.Compact(slices.Sorted(slices.Values(reads.named[value]))), ", ")
		mark, marked := artifactUploadFormValues[value]
		switch {
		case !marked:
			unmarked = append(unmarked, value+" ("+where+")")
		case !mark.prose && mark.why == "":
			unexplained = append(unexplained, value)
		default:
			t.Logf("%s: prose %t (%s)", value, mark.prose, where)
		}
	}
	if len(unmarked) > 0 {
		t.Errorf("the upload reads a form value the writing check's registry does not mark:\n\t%s\n"+
			"Mark it in artifactUploadFormValues (writing_check_fields.go): prose, or not prose with the reason.",
			strings.Join(unmarked, "\n\t"))
	}
	if len(unexplained) > 0 {
		t.Errorf("a form value is marked not prose with no reason: %s", strings.Join(unexplained, ", "))
	}
	if len(reads.unnamed) > 0 {
		t.Errorf("the upload reads its form by a name the guard cannot read as a constant, or reads its raw parts:\n\t%s\n"+
			"Read each form value by a constant name, so the guard can hold it to the registry.",
			strings.Join(slices.Compact(slices.Sorted(slices.Values(reads.unnamed))), "\n\t"))
	}
	for _, value := range slices.Sorted(maps.Keys(artifactUploadFormValues)) {
		if _, read := reads.named[value]; !read {
			t.Errorf("artifactUploadFormValues marks %q, which no upload route reads: delete the line, or point the guard at the read it misses", value)
		}
	}
}

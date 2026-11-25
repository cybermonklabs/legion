package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"reflect"
	"strings"
)

// The answer census's rules for the ways an answer's words leave it other than a read of a
// response's fields (answerCensus), each a shape a program can check from the types alone:
//
//   - a reference to one of the standard library's readers of a response's body, or to a maker of
//     the box, other than its call: a function value or a method value, which the call rules
//     cannot follow, since a call through a value resolves to no function;
//   - a decode, outside Answer.Decode, into a value that holds the box (gate.answers.decoders, or
//     a type's own UnmarshalJSON or UnmarshalText called directly): a box filled so holds no record
//     of the request its answer answers. A decode inside a type's own UnmarshalJSON or
//     UnmarshalText is that type's part of the decode that calls it, which the census holds at that
//     call;
//   - net/http.ReadResponse given no request: the answer it reads has no record of what the
//     request carried, so no credential of it is redacted from the answer's words;
//   - a value a printer reaches a response in (holdsResponse) made an interface value, which fmt,
//     log, slog and encoding/json take, or any code that hands it on to them: printed so, it is the
//     status line the answering party wrote and every header it sent, as they came. Go makes one in
//     a call's argument, a conversion, a variable's value, an assignment, a composite literal's
//     element, a return, a send, a map's key and a range's assignment (conversions), and a generic
//     function's argument whose parameter holds a type parameter is one too, since its body can
//     make one of it where the call cannot show it. errors.As's target is listed and passes, since
//     errors.As only writes an error of err's tree through it; so is a value whose type is one of
//     gate.external_errors (x/oauth2's RetrieveError, which holds a response), every use of which
//     the whole-value census lists and judges.
//
// In the box's own package, the exported functions that take the reader's type are exactly
// gate.answers.functions, as its methods are exactly gate.answers.methods (answerMethods): a
// function that answers the body's bytes lets a client print them as a method would.

// reference is the full name of the reader or maker n names, an identifier or a selector, where n
// is not the function a call calls, and whether it is one; n of any other kind is none.
func (a *answerReader) reference(n ast.Node, parents []ast.Node) (string, bool) {
	var obj types.Object
	switch n := n.(type) {
	case *ast.Ident:
		if len(parents) > 0 {
			if sel, ok := parents[len(parents)-1].(*ast.SelectorExpr); ok && sel.Sel == n {
				return "", false
			}
		}
		obj = a.info.Uses[n]
	case *ast.SelectorExpr:
		if s := a.info.Selections[n]; s != nil {
			obj = s.Obj()
		} else {
			obj = a.info.Uses[n.Sel]
		}
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return "", false
	}
	name := fn.Origin().FullName()
	if !a.readers[name] && !a.makers[name] || called(n, parents) {
		return "", false
	}
	return name, true
}

// called reports whether n is the function a call calls, through parentheses and a generic
// instantiation.
func called(n ast.Node, parents []ast.Node) bool {
	child := n
	for i := len(parents) - 1; i >= 0; i-- {
		switch p := parents[i].(type) {
		case *ast.ParenExpr:
			child = p
			continue
		case *ast.IndexExpr:
			if p.X == child {
				child = p
				continue
			}
		case *ast.IndexListExpr:
			if p.X == child {
				child = p
				continue
			}
		case *ast.CallExpr:
			return p.Fun == child
		}
		return false
	}
	return false
}

// decodedIntoBox is why call, a decoder's or a type's own UnmarshalJSON or UnmarshalText called
// directly, fills a value that holds the box outside Answer.Decode, or "". function is the code
// the call is in.
func (a *answerReader) decodedIntoBox(call *ast.CallExpr, function string) string {
	if unmarshalMethod(function) {
		return ""
	}
	name := a.calleeName(call)
	var into []ast.Expr
	switch {
	case a.decoders[name]:
		into = call.Args
	case unmarshalMethod(name):
		if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
			into = []ast.Expr{sel.X}
		}
	}
	for _, arg := range into {
		if t := a.info.TypeOf(arg); t != nil && holdsBox(t, a.boxPkg, a.boxName, map[types.Type]bool{}) {
			return fmt.Sprintf("%s fills a %s, which holds the box, outside %s.Decode: a box filled so holds no record of the request its answer answers; decode the answer with Answer.Decode",
				shortName(name), types.TypeString(t, func(p *types.Package) string { return p.Name() }), shortName(a.cfg.Answer))
		}
	}
	return ""
}

// unmarshalMethod reports whether name, a function's full name, is a type's UnmarshalJSON or
// UnmarshalText.
func unmarshalMethod(name string) bool {
	return strings.HasPrefix(name, "(") && (strings.HasSuffix(name, ").UnmarshalJSON") || strings.HasSuffix(name, ").UnmarshalText"))
}

// readsNoRequest reports whether call is net/http.ReadResponse given no request.
func (a *answerReader) readsNoRequest(call *ast.CallExpr) bool {
	return a.calleeName(call) == "net/http.ReadResponse" && len(call.Args) == 2 && isNil(call.Args[1])
}

// holdsResponse reports whether a printer that walks a value of t by reflection reaches a response
// (responseType) in it, as fmt or encoding/json does, whichever reaches further (fmtEdges,
// jsonEdges). A method a type prints itself by (Error, String, MarshalJSON) is not assumed: fmt's
// %d or %#v prints the fields past Error and String. A field of interface type holds a value made
// an interface where it was set, which the census reads there.
func (a *answerReader) holdsResponse(t types.Type) bool {
	return fmtEdges.reaches(t, a.responseType, map[types.Type]bool{}) || jsonEdges.reaches(t, a.responseType, map[types.Type]bool{})
}

// fmtEdges are what fmt (and log, slog's text handler and an error's %+v) prints of a value: every
// field, exported or not, every element of a slice or an array and every key and value of a map,
// and a pointer only at the top, to a struct, an array, a slice or a map (&{...}): one deeper it
// prints as an address. A channel it prints as an address.
var fmtEdges = edges{topPointer: true}

// jsonEdges are what encoding/json (and slog's JSON handler, text/template and YAML) writes of a
// value: it follows every pointer and writes the exported fields, the fields an embedded struct
// promotes, every element and every map value, but no map key, which it writes as a string; a
// field tagged json:"-" it leaves out, and a channel it refuses.
var jsonEdges = edges{skipMapKeys: true, field: func(s *types.Struct, i int) bool {
	f := s.Field(i)
	switch {
	case reflect.StructTag(s.Tag(i)).Get("json") == "-":
		return false
	case f.Exported():
		return true
	}
	// An embedded struct's exported fields are promoted, whatever its own name.
	_, isStruct := derefType(f.Type()).Underlying().(*types.Struct)
	return f.Embedded() && isStruct
}}

// conversion is a value Go makes an interface value or a type parameter's value of: expr, whose
// type is from, handed to to, which names where; pass is why the census passes it, or "".
type conversion struct {
	expr     ast.Expr
	from     types.Type
	to, pass string
}

// conversions are the values n, a node of the census's walk, makes interface values or type
// parameters' values of: a call's argument whose parameter is an interface, or holds a type
// parameter in the called function's own declaration; a conversion to an interface; a declared
// variable's value; an assignment's value, also one of a range; a composite literal's element,
// key or value; a return's result; a send's value; and a map's key. A value whose own type is an
// interface was made one where it was first, which that site's conversion names; a comparison with
// an interface hands nothing out. errors.As's target is no value handed out: errors.As writes an
// error of err's tree through it, an interface value made where that error was, and keeps and
// prints nothing of the pointer it is given.
func (a *answerReader) conversions(n ast.Node, parents []ast.Node) []conversion {
	var out []conversion
	to := func(e ast.Expr, from, target types.Type, where string) {
		if from != nil && target != nil && types.IsInterface(target) && !types.IsInterface(from) {
			out = append(out, conversion{expr: e, from: from, to: where + " of type " + types.TypeString(target, nil)})
		}
	}
	// assign hands values to targets: one each, or one call's results.
	assign := func(values []ast.Expr, targets []types.Type, where string) {
		switch {
		case len(values) == len(targets):
			for i, v := range values {
				to(v, a.info.TypeOf(v), targets[i], where)
			}
		case len(values) == 1:
			if results, ok := a.info.TypeOf(values[0]).(*types.Tuple); ok {
				for i := range min(results.Len(), len(targets)) {
					to(values[0], results.At(i).Type(), targets[i], where)
				}
			}
		}
	}
	switch n := n.(type) {
	case *ast.CallExpr:
		tv, ok := a.info.Types[n.Fun]
		if !ok || tv.Type == nil {
			return nil
		}
		if tv.IsType() {
			if len(n.Args) == 1 {
				to(n.Args[0], a.info.TypeOf(n.Args[0]), tv.Type, "a conversion")
			}
			return out
		}
		sig, ok := tv.Type.Underlying().(*types.Signature)
		if !ok {
			return nil
		}
		var declared *types.Signature // the called function as declared, where it is generic
		if fn := calleeFunc(a.info, n); fn != nil && fn.Origin() != fn {
			declared, _ = fn.Origin().Type().(*types.Signature)
		} else if fn != nil && fn.Type().(*types.Signature).TypeParams().Len() > 0 {
			declared, _ = fn.Type().(*types.Signature)
		}
		target := -1 // the argument that is errors.As's target
		if a.calleeName(n) == "errors.As" {
			target = 1
		}
		for i, arg := range n.Args {
			if i == target {
				if from := a.info.TypeOf(arg); from != nil {
					out = append(out, conversion{expr: arg, from: from, to: "errors.As's target", pass: "errors.As's target, which it writes an error of err's tree through and keeps and prints nothing of"})
				}
				continue
			}
			param, generic := paramAt(sig, i, n.Ellipsis.IsValid()), types.Type(nil)
			if declared != nil {
				generic = paramAt(declared, i, n.Ellipsis.IsValid())
			}
			from := a.info.TypeOf(arg)
			if generic != nil && holdsTypeParam(generic) && from != nil && !types.IsInterface(from) {
				out = append(out, conversion{expr: arg, from: from, to: "an argument of " + exprString(n.Fun) + " whose parameter holds a type parameter, " + types.TypeString(generic, nil)})
				continue
			}
			to(arg, from, param, "an argument of "+exprString(n.Fun))
		}
	case *ast.AssignStmt:
		if n.Tok != token.ASSIGN && n.Tok != token.DEFINE {
			return nil
		}
		targets := make([]types.Type, len(n.Lhs))
		for i, l := range n.Lhs {
			targets[i] = a.info.TypeOf(l)
		}
		assign(n.Rhs, targets, "an assignment")
	case *ast.ValueSpec:
		if n.Type == nil {
			return nil
		}
		targets := make([]types.Type, len(n.Names))
		for i := range targets {
			targets[i] = a.info.TypeOf(n.Type)
		}
		assign(n.Values, targets, "a variable's value")
	case *ast.ReturnStmt:
		if sig := enclosingSignature(a.info, parents); sig != nil {
			targets := make([]types.Type, sig.Results().Len())
			for i := range targets {
				targets[i] = sig.Results().At(i).Type()
			}
			assign(n.Results, targets, "a return")
		}
	case *ast.SendStmt:
		if ch, ok := typeUnder(a.info.TypeOf(n.Chan)).(*types.Chan); ok {
			to(n.Value, a.info.TypeOf(n.Value), ch.Elem(), "a send")
		}
	case *ast.IndexExpr:
		if m, ok := typeUnder(a.info.TypeOf(n.X)).(*types.Map); ok {
			to(n.Index, a.info.TypeOf(n.Index), m.Key(), "a map key")
		}
	case *ast.CompositeLit:
		lit := typeUnder(a.info.TypeOf(n))
		if p, ok := lit.(*types.Pointer); ok {
			lit = typeUnder(p.Elem())
		}
		const where = "a composite literal's element"
		for i, elt := range n.Elts {
			kv, keyed := elt.(*ast.KeyValueExpr)
			value := elt
			if keyed {
				value = kv.Value
			}
			switch lit := lit.(type) {
			case *types.Struct:
				switch {
				case keyed:
					if id, ok := kv.Key.(*ast.Ident); ok {
						if field, ok := a.info.Uses[id].(*types.Var); ok {
							to(value, a.info.TypeOf(value), field.Type(), where)
						}
					}
				case i < lit.NumFields():
					to(value, a.info.TypeOf(value), lit.Field(i).Type(), where)
				}
			case *types.Slice:
				to(value, a.info.TypeOf(value), lit.Elem(), where)
			case *types.Array:
				to(value, a.info.TypeOf(value), lit.Elem(), where)
			case *types.Map:
				if keyed {
					to(kv.Key, a.info.TypeOf(kv.Key), lit.Key(), where)
				}
				to(value, a.info.TypeOf(value), lit.Elem(), where)
			}
		}
	case *ast.RangeStmt:
		if n.Tok != token.ASSIGN {
			return nil
		}
		key, value := rangeTypes(a.info.TypeOf(n.X))
		for _, v := range []struct {
			target ast.Expr
			from   types.Type
		}{{n.Key, key}, {n.Value, value}} {
			if v.target != nil {
				to(n.X, v.from, a.info.TypeOf(v.target), "a range's assignment")
			}
		}
	}
	return out
}

// paramAt is the type of the parameter of sig that a call's argument i is handed to, the element
// of a variadic one where the call does not spread a slice into it, or nil.
func paramAt(sig *types.Signature, i int, spread bool) types.Type {
	params := sig.Params()
	switch last := params.Len() - 1; {
	case sig.Variadic() && i >= last:
		t := params.At(last).Type()
		if s, ok := t.Underlying().(*types.Slice); ok && !spread {
			return s.Elem()
		}
		return t
	case i < params.Len():
		return params.At(i).Type()
	}
	return nil
}

// holdsTypeParam reports whether t, a parameter's type as its function declares it, holds a type
// parameter: is one, or holds one as a type argument, an element, a field or a function's
// parameter or result. A signature names no type declared over its function's type parameters, so
// a named type in it holds one only as a type argument, and the walk reads those, not its
// underlying type.
func holdsTypeParam(t types.Type) bool {
	return edges{chans: true, signatures: true, typeArgs: true}.reaches(t, func(t types.Type) bool {
		_, ok := t.(*types.TypeParam)
		return ok
	}, map[types.Type]bool{})
}

// enclosingSignature is the signature of the innermost function among parents, a declaration or a
// literal, or nil.
func enclosingSignature(info *types.Info, parents []ast.Node) *types.Signature {
	for i := len(parents) - 1; i >= 0; i-- {
		switch f := parents[i].(type) {
		case *ast.FuncLit:
			sig, _ := info.TypeOf(f).(*types.Signature)
			return sig
		case *ast.FuncDecl:
			if fn, ok := info.Defs[f.Name].(*types.Func); ok {
				sig, _ := fn.Type().(*types.Signature)
				return sig
			}
			return nil
		}
	}
	return nil
}

// rangeTypes are the types a range over a value of t assigns its key and its value, each nil where
// it assigns none.
func rangeTypes(t types.Type) (types.Type, types.Type) {
	u := typeUnder(t)
	if p, ok := u.(*types.Pointer); ok {
		u = typeUnder(p.Elem())
	}
	switch u := u.(type) {
	case *types.Slice:
		return types.Typ[types.Int], u.Elem()
	case *types.Array:
		return types.Typ[types.Int], u.Elem()
	case *types.Map:
		return u.Key(), u.Elem()
	case *types.Chan:
		return u.Elem(), nil
	case *types.Signature:
		if u.Params().Len() == 1 {
			if yield, ok := typeUnder(u.Params().At(0).Type()).(*types.Signature); ok {
				var key, value types.Type
				if yield.Params().Len() > 0 {
					key = yield.Params().At(0).Type()
				}
				if yield.Params().Len() > 1 {
					value = yield.Params().At(1).Type()
				}
				return key, value
			}
		}
	}
	return nil, nil
}

// typeUnder is t's underlying type, or nil where t is nil.
func typeUnder(t types.Type) types.Type {
	if t == nil {
		return nil
	}
	return t.Underlying()
}

// answerFunctions lists the exported package-level functions of the box's package that take the
// reader's type, OK only where they are exactly gate.answers.functions.
func (g *gate) answerFunctions(c *census, dir string, fset *token.FileSet, pkg *types.Package, cfg answersConfig) {
	_, name := splitTypeName(cfg.Answer)
	answer, ok := pkg.Scope().Lookup(name).(*types.TypeName)
	if !ok {
		return
	}
	var got []string
	for _, n := range pkg.Scope().Names() {
		fn, ok := pkg.Scope().Lookup(n).(*types.Func)
		if !ok || !fn.Exported() {
			continue
		}
		params := fn.Type().(*types.Signature).Params()
		for i := range params.Len() {
			if mentions(params.At(i).Type(), answer, map[types.Type]bool{}) {
				got = append(got, fn.Name())
				break
			}
		}
	}
	p := fset.Position(answer.Pos())
	rel, _ := filepath.Rel(dir, p.Filename)
	c.Answers = append(c.Answers, exactly(finding{File: filepath.ToSlash(rel), Line: p.Line, Function: cfg.Answer, Kind: "functions"}, got, cfg.Functions, "the exported functions that take it are", "a function"))
}

// mentions reports whether t is the named type of name, or reaches it through a pointer, a slice,
// an array, a map, a channel, a struct's field or a function's parameter or result.
func mentions(t types.Type, name *types.TypeName, seen map[types.Type]bool) bool {
	return edges{chans: true, signatures: true}.reaches(t, func(t types.Type) bool {
		n, ok := t.(*types.Named)
		return ok && n.Obj() == name
	}, seen)
}

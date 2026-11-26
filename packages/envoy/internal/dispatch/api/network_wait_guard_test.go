//go:build !race

// The guard is static, as the copy-lock and keyed-write guards it shares a graph with: the race
// detector adds nothing to it.

package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// networkWaitHelpers are the functions that run the function they are handed holding no
// admission unit of the request (network_wait.go): awayOnNetwork and awayOnNetworkNeverRefused
// give the unit back for it and take it back after, as awayOnNetworkFor does through
// awayOnNetwork for a wait that answers a value, awayOnNetworkForGood gives it back for good,
// givenBackAlready runs it inside a wait that has given the unit back already, and
// delivery.onGitHub runs it through the wait the request's context carries
// (delivery.WithGitHubWait), or on its own context where the caller holds no hook.
var networkWaitHelpers = []string{
	"api.awayOnNetwork", "api.awayOnNetworkFor", "api.awayOnNetworkNeverRefused", "api.awayOnNetworkForGood",
	"api.givenBackAlready", "delivery.onGitHub",
}

// networkWaitInPlace are the helpers that run what they are handed in place, which is a wait only
// where the request has given its unit back already: givenBackAlready, which a comment's lookups
// run under inside the one wait the request makes for all of them (resolveCommentDeliveries). The
// guard refuses a reference to one in a function a request reaches holding its unit.
var networkWaitInPlace = []string{"api.givenBackAlready"}

// detachedContextMaker makes the context of work that outlives its request: no cancellation and
// no admission ticket (detachedFromRequest).
const detachedContextMaker = "api.detachedFromRequest"

// outboundDeps are the fields of Deps that hold a client of another server, by the server each
// one calls. Every exported method of the field's type is an outbound call (outboundMethods).
var outboundDeps = map[string]string{
	"Envoy":          "the Envoy listener",
	"GitHub":         "GitHub",
	"AgentSecrets":   "the secrets broker",
	"Embedder":       "the embedder",
	"Answerer":       "the model",
	"AgentPublisher": "NATS",
	"AgentStream":    "NATS",
}

// outboundTypes are the clients of another server that no field of Deps names, by package path
// and type name: the NATS connection and its JetStream handle, and the bus client that holds both,
// which AgentPublisher is in production.
var outboundTypes = map[string]string{
	"github.com/nats-io/nats.go.Conn":              "NATS",
	"github.com/nats-io/nats.go.JetStreamContext":  "NATS",
	"github.com/sjawhar/envoy/internal/bus.Client": "NATS",
}

// outboundLocal are the methods of those clients that answer from what the client holds and send
// nothing to its server, each with the reason. They leave the outbound set, and the guard fails on
// an entry that names no method of it.
var outboundLocal = map[string]string{
	// Every embedder answers both from fields it was built with (embed's AzureClient, BedrockClient,
	// DevClient, and RateLimitedEmbedder, which asks its inner one).
	"embed.Embedder.Identity": "names the model and place the embedder was built for, a field it holds",
	"embed.Embedder.Limits":   "reports the batch limits the embedder was built with, a field it holds",
	"cipher.Client.DeviceGrantKnown": "answers the broker's metadata read the client remembers, a field it holds, " +
		"and reads nothing of the broker",
}

// networkWaitGuardAllowed are the functions a route's handler reaches whose outbound calls hold no
// admission unit of a request although no network wait runs them, each with the reason. The
// guard does not follow them, and fails on an entry it no longer reaches or whose function
// reaches no outbound call of its own. None needs one: the related-item check's embedding, which
// runs on a goroutine of its own beside the check's keyword stage, is a goroutine the guard reads
// as waited on away (awaitedAway).
var networkWaitGuardAllowed = map[string]string{}

// outboundSet is every exported method of a client of another server, by the method, labelled
// with that server and the method's name.
type outboundSet map[*types.Func]string

// networkWaitOffence is one outbound call a route's handler reaches while its request holds its
// admission unit: the route, the functions from the handler to the one that makes the call, and
// the call.
type networkWaitOffence struct {
	route string
	path  []string
	at    string
	calls string
}

func (o networkWaitOffence) String() string {
	return o.route + ": " + strings.Join(o.path, " -> ") + " calls " + o.calls + " at " + o.at
}

// networkWaitStep is how the walk first reached a function: from the handler of route, or from
// a call at in the function from.
type networkWaitStep struct {
	route string
	from  *lockFunction
	at    string
}

// waitCarrier is what the guard has read of a variable: whether every value handed it is a
// network wait, and the bodies of those waits that are no helper of their own.
type waitCarrier struct {
	done, waits bool
	bodies      []*lockFunction
}

// networkWaitGuard is what the guard reads off the copy-lock graph (walk).
type networkWaitGuard struct {
	graph *lockGraph
	// keyed lends its callees and detached, so this guard follows a call exactly as the keyed
	// write guard does.
	keyed          *keyedWriteGuard
	outbound       outboundSet
	outboundBodies map[*lockFunction]string
	helpers        map[*lockFunction]bool
	allowed        map[string]string
	// inPlace are the helpers networkWaitInPlace lists.
	inPlace map[*lockFunction]bool
	// honourWaits is false only where the guard's own test reads what it would name with every
	// network wait read as an ordinary call.
	honourWaits bool

	// handed is every value the module hands each function-typed variable, field and parameter:
	// assigned to it, written into it in a struct literal, or passed for it at a call.
	handed map[*types.Var][]ast.Expr
	// carriers is what the guard has read of each such variable that a call runs through.
	carriers map[*types.Var]*waitCarrier
	// returning marks the functions whose returned waits are being read, against a cycle.
	returning map[*lockFunction]bool
	// detachedVars are the variables assigned a context detachedFromRequest made.
	detachedVars map[types.Object]bool

	reached        map[*lockFunction]bool
	via            map[*lockFunction]networkWaitStep
	allowedReached map[string]bool
	offences       []networkWaitOffence
}

// outboundMethods derives the outbound set from the types: every exported method of the type of
// each field outboundDeps names and of each type outboundTypes names, so a method added to one
// of those clients is an outbound call without a change here.
func outboundMethods(t *testing.T, graph *lockGraph) outboundSet {
	t.Helper()
	typeNames := map[string]*types.TypeName{}
	for _, objects := range []map[*ast.Ident]types.Object{graph.info.Defs, graph.info.Uses} {
		for _, object := range objects {
			if name, ok := object.(*types.TypeName); ok && name.Pkg() != nil {
				typeNames[name.Pkg().Path()+"."+name.Name()] = name
			}
		}
	}
	set := outboundSet{}
	add := func(server, what string, typ types.Type) {
		if _, pointer := typ.(*types.Pointer); !pointer && !types.IsInterface(typ) {
			typ = types.NewPointer(typ)
		}
		methods := types.NewMethodSet(typ)
		exported := 0
		for index := range methods.Len() {
			// A method of no package is the universe's error.Error, which an interface that
			// embeds error carries and which sends nothing.
			if fn, ok := methods.At(index).Obj().(*types.Func); ok && fn.Exported() && fn.Pkg() != nil {
				set[fn.Origin()] = server + " " + functionName(fn)
				exported++
			}
		}
		if exported == 0 {
			t.Errorf("%s, a client of %s, has no exported method: point the guard at the client that calls it", what, server)
		}
	}
	deps := typeNames["github.com/sjawhar/envoy/internal/dispatch/api.Deps"]
	if deps == nil {
		t.Fatal("the guard finds no Deps, whose fields hold the clients of other servers")
	}
	fields, _ := deps.Type().Underlying().(*types.Struct)
	for _, name := range slices.Sorted(maps.Keys(outboundDeps)) {
		found := false
		for index := range fields.NumFields() {
			if field := fields.Field(index); field.Name() == name {
				add(outboundDeps[name], "Deps."+name, field.Type())
				found = true
			}
		}
		if !found {
			t.Errorf("Deps has no field %s, which outboundDeps names as a client of %s: point the list at its replacement",
				name, outboundDeps[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(outboundTypes)) {
		typeName := typeNames[name]
		if typeName == nil {
			t.Errorf("the Dispatch server no longer uses %s, which outboundTypes names as a client of %s: point the list at its replacement",
				name, outboundTypes[name])
			continue
		}
		add(outboundTypes[name], name, typeName.Type())
	}
	for _, name := range slices.Sorted(maps.Keys(outboundLocal)) {
		removed := false
		for fn := range set {
			if functionName(fn) == name {
				delete(set, fn)
				removed = true
			}
		}
		if !removed || strings.TrimSpace(outboundLocal[name]) == "" {
			t.Errorf("outboundLocal's %s names no outbound method, or gives no reason it sends nothing", name)
		}
	}
	return set
}

// newNetworkWaitGuard reads graph for the walk: the bodies of the outbound methods and of every
// method of the module's types that implements an outbound interface method (a call through an
// interface or a variable that runs one of them is an outbound call), the helpers, and every
// value the module hands a function-typed variable, field or parameter.
func newNetworkWaitGuard(t *testing.T, graph *lockGraph, outbound outboundSet, allowed map[string]string, helpers, inPlace []string) *networkWaitGuard {
	t.Helper()
	guard := &networkWaitGuard{
		graph: graph, keyed: &keyedWriteGuard{graph: graph}, outbound: outbound,
		outboundBodies: map[*lockFunction]string{}, helpers: map[*lockFunction]bool{},
		inPlace: map[*lockFunction]bool{}, allowed: allowed, honourWaits: true,
		handed: map[*types.Var][]ast.Expr{}, carriers: map[*types.Var]*waitCarrier{},
		returning: map[*lockFunction]bool{}, detachedVars: map[types.Object]bool{},
	}
	guard.restart()
	for _, name := range helpers {
		fn := graph.named[name]
		if fn == nil {
			t.Fatalf("%s, which networkWaitHelpers lists as a network wait, is gone: point the list at its replacement", name)
		}
		guard.helpers[fn] = true
	}
	for _, name := range inPlace {
		fn := graph.named[name]
		if fn == nil || !guard.helpers[fn] {
			t.Fatalf("%s, which networkWaitInPlace lists as a helper that waits in place, is no helper: point the list at its replacement", name)
		}
		guard.inPlace[fn] = true
	}
	implementers := map[*types.Interface][]types.Type{}
	for fn, label := range outbound {
		if body := graph.declared[fn]; body != nil {
			guard.outboundBodies[body] = label
		}
		receiver := fn.Signature().Recv()
		if receiver == nil || !types.IsInterface(receiver.Type()) {
			continue
		}
		iface, _ := receiver.Type().Underlying().(*types.Interface)
		candidates, read := implementers[iface]
		if !read {
			for _, candidate := range graph.concrete {
				if types.Implements(candidate, iface) {
					candidates = append(candidates, candidate)
				}
			}
			implementers[iface] = candidates
		}
		for _, candidate := range candidates {
			object, _, _ := types.LookupFieldOrMethod(candidate, true, fn.Pkg(), fn.Name())
			if method, ok := object.(*types.Func); ok {
				if body := graph.declared[method.Origin()]; body != nil {
					guard.outboundBodies[body] = label
				}
			}
		}
	}
	guard.index()
	return guard
}

// restart clears what a walk found.
func (g *networkWaitGuard) restart() {
	g.reached = map[*lockFunction]bool{}
	g.via = map[*lockFunction]networkWaitStep{}
	g.allowedReached = map[string]bool{}
	g.offences = nil
}

// index records every value the module hands a function-typed variable, field or parameter, and
// every variable assigned a context detachedFromRequest made.
func (g *networkWaitGuard) index() {
	hand := func(target types.Object, value ast.Expr) {
		variable, ok := target.(*types.Var)
		if !ok {
			return
		}
		if _, function := variable.Type().Underlying().(*types.Signature); function {
			variable = variable.Origin()
			g.handed[variable] = append(g.handed[variable], value)
		}
	}
	target := g.graph.objectOf
	// detached marks a variable, not a field, assigned a context detachedFromRequest made.
	detached := func(object types.Object) {
		if variable, ok := object.(*types.Var); ok && !variable.IsField() {
			g.detachedVars[variable] = true
		}
	}
	read := func(node ast.Node) {
		ast.Inspect(node, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.FuncLit:
				// A literal is a function of its own, read where the graph registered it.
				return false
			case *ast.AssignStmt:
				for index, value := range n.Rhs {
					if len(n.Lhs) == len(n.Rhs) {
						hand(target(n.Lhs[index]), value)
					}
					if g.makesDetached(value) {
						if len(n.Lhs) == len(n.Rhs) {
							detached(target(n.Lhs[index]))
						} else {
							detached(target(n.Lhs[0]))
						}
					}
				}
			case *ast.ValueSpec:
				for index, value := range n.Values {
					if index < len(n.Names) {
						hand(g.graph.info.Defs[n.Names[index]], value)
						if g.makesDetached(value) {
							detached(g.graph.info.Defs[n.Names[index]])
						}
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok {
					hand(g.graph.info.Uses[key], n.Value)
				}
			case *ast.CompositeLit:
				g.handPositional(n, hand)
			case *ast.CallExpr:
				callee, _ := g.callee(n).(*types.Func)
				if callee == nil {
					break
				}
				parameters := callee.Signature().Params()
				for index, argument := range n.Args {
					if index < parameters.Len() && !(callee.Signature().Variadic() && index >= parameters.Len()-1) {
						hand(parameters.At(index), argument)
					}
				}
			}
			return true
		})
	}
	files := map[*ast.File]bool{}
	for _, fn := range g.graph.functions {
		read(fn.body)
		files[fn.source.file] = true
	}
	for file := range files {
		for _, declared := range file.Decls {
			if general, ok := declared.(*ast.GenDecl); ok {
				read(general)
			}
		}
	}
}

// handPositional hands each value of a struct literal written without field names to its field.
func (g *networkWaitGuard) handPositional(literal *ast.CompositeLit, hand func(types.Object, ast.Expr)) {
	if literal.Type == nil || len(literal.Elts) == 0 {
		return
	}
	if _, keyed := literal.Elts[0].(*ast.KeyValueExpr); keyed {
		return
	}
	var object types.Object
	switch typ := ast.Unparen(literal.Type).(type) {
	case *ast.Ident:
		object = g.graph.info.Uses[typ]
	case *ast.SelectorExpr:
		object = g.graph.info.Uses[typ.Sel]
	}
	name, ok := object.(*types.TypeName)
	if !ok {
		return
	}
	fields, ok := name.Type().Underlying().(*types.Struct)
	if !ok {
		return
	}
	for index, value := range literal.Elts {
		if index < fields.NumFields() {
			hand(fields.Field(index), value)
		}
	}
}

// callee is the function or the variable call names, at its origin: nil for a call of anything
// else (a literal called where it stands, a function a call returns).
func (g *networkWaitGuard) callee(call *ast.CallExpr) types.Object {
	fun := ast.Unparen(call.Fun)
	switch generic := fun.(type) {
	case *ast.IndexExpr:
		fun = generic.X
	case *ast.IndexListExpr:
		fun = generic.X
	}
	return g.valueObject(fun)
}

// valueObject is the function or variable expression names, at its origin.
func (g *networkWaitGuard) valueObject(expression ast.Expr) types.Object {
	switch o := g.graph.objectOf(expression).(type) {
	case *types.Func:
		return o.Origin()
	case *types.Var:
		return o.Origin()
	}
	return nil
}

// makesDetached reports whether expression holds a call of detachedFromRequest.
func (g *networkWaitGuard) makesDetached(expression ast.Node) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if fn, ok := g.callee(call).(*types.Func); ok && fn.Pkg() != nil && functionName(fn) == detachedContextMaker {
				found = true
			}
		}
		return !found
	})
	return found
}

// onDetachedContext reports whether a go statement's call hands its goroutine a context
// detachedFromRequest made: the call, or the function literal it runs, names one.
func (g *networkWaitGuard) onDetachedContext(call *ast.CallExpr) bool {
	if g.makesDetached(call) {
		return true
	}
	found := false
	ast.Inspect(call, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && g.graph.info.Uses[ident] != nil && g.detachedVars[g.graph.info.Uses[ident]] {
			found = true
		}
		return !found
	})
	return found
}

// waitOf reports whether call is a network wait: a call of one of networkWaitHelpers, or a call
// through a variable, field or parameter every value of which is a network wait (waiterValue).
// What it is handed runs holding no unit of the request. waiters are the bodies of the waits it
// may run that are no helper - a function that hands its callback on to a helper - which run
// themselves holding the unit, and are walked.
func (g *networkWaitGuard) waitOf(call *ast.CallExpr) (bool, []*lockFunction) {
	if !g.honourWaits {
		return false, nil
	}
	switch callee := g.callee(call).(type) {
	case *types.Func:
		return g.helpers[g.graph.declared[callee]], nil
	case *types.Var:
		return g.carrier(callee)
	}
	return false, nil
}

// carrier reads variable, which a call runs through: it is a network wait when it has a wait's
// shape (waitShaped), the module hands it at least one value, and every value it hands it is one.
func (g *networkWaitGuard) carrier(variable *types.Var) (bool, []*lockFunction) {
	if read, ok := g.carriers[variable]; ok {
		// One still being read is a cycle through itself, which adds no value of its own.
		return !read.done || read.waits, read.bodies
	}
	read := &waitCarrier{}
	g.carriers[variable] = read
	signature, _ := variable.Type().Underlying().(*types.Signature)
	values := g.handed[variable]
	read.waits = signature != nil && waitShaped(signature) && len(values) > 0
	for _, value := range values {
		if !read.waits {
			break
		}
		waits, bodies := g.waiterValue(value)
		read.waits = waits
		read.bodies = append(read.bodies, bodies...)
	}
	read.done = true
	return read.waits, read.bodies
}

// waitShaped reports whether signature has a network wait's shape: the function it runs is the
// second thing it is handed, after the context.
func waitShaped(signature *types.Signature) bool {
	if signature.Params().Len() < 2 {
		return false
	}
	_, callback := signature.Params().At(1).Type().Underlying().(*types.Signature)
	return callback
}

// waiterValue reports whether expression is a network wait: nil, a helper, a variable, field or
// parameter every value of which is one, a call of a function every return of which is one, or a
// function that hands its callback (its second parameter) on to a network wait and runs it
// nowhere else (handsOnly).
func (g *networkWaitGuard) waiterValue(expression ast.Expr) (bool, []*lockFunction) {
	switch e := ast.Unparen(expression).(type) {
	case *ast.Ident:
		if _, isNil := g.graph.info.Uses[e].(*types.Nil); isNil {
			return true, nil
		}
	case *ast.FuncLit:
		literal := g.graph.literals[e]
		if literal == nil || e.Type.Params == nil {
			return false, nil
		}
		var callback types.Object
		index := 0
		for _, field := range e.Type.Params.List {
			for _, name := range field.Names {
				if index == 1 {
					callback = g.graph.info.Defs[name]
				}
				index++
			}
		}
		return g.handsOnly(literal, callback), []*lockFunction{literal}
	case *ast.CallExpr:
		called, _ := g.callee(e).(*types.Func)
		body := g.graph.declared[called]
		if body == nil || g.returning[body] {
			return false, nil
		}
		g.returning[body] = true
		defer delete(g.returning, body)
		waits, returns := true, 0
		var bodies []*lockFunction
		ast.Inspect(body.body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.FuncLit:
				return false
			case *ast.ReturnStmt:
				returns++
				if len(n.Results) != 1 {
					waits = false
					return false
				}
				returned, more := g.waiterValue(n.Results[0])
				waits = waits && returned
				bodies = append(bodies, more...)
			}
			return waits
		})
		return waits && returns > 0, bodies
	}
	switch object := g.valueObject(expression).(type) {
	case *types.Func:
		body := g.graph.declared[object]
		if g.helpers[body] {
			return true, nil
		}
		if body == nil || !waitShaped(object.Signature()) {
			return false, nil
		}
		return g.handsOnly(body, object.Signature().Params().At(1)), []*lockFunction{body}
	case *types.Var:
		return g.carrier(object)
	}
	return false, nil
}

// handsOnly reports whether fn uses callback, a function it is handed, only as what it hands a
// network wait: every use of it inside fn's body is inside the arguments of a call waitOf reads
// as a wait.
func (g *networkWaitGuard) handsOnly(fn *lockFunction, callback types.Object) bool {
	if callback == nil {
		return false
	}
	if _, function := callback.Type().Underlying().(*types.Signature); !function {
		return false
	}
	only := true
	ast.Inspect(fn.body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			if waits, _ := g.waitOf(n); waits {
				return false
			}
		case *ast.Ident:
			if g.graph.info.Uses[n] == callback {
				only = false
			}
		}
		return only
	})
	return only
}

// outboundCall is the outbound method call runs, labelled, or "": a method of the outbound set
// it names, the module's body of one that a call through an interface or a variable runs, or an
// outbound method it hands on as a value, which whatever it is handed to runs.
func (g *networkWaitGuard) outboundCall(call *ast.CallExpr) string {
	if fn, ok := g.callee(call).(*types.Func); ok {
		if label, outbound := g.outbound[fn]; outbound {
			return label
		}
	}
	bare := *call
	bare.Args = nil
	for _, body := range g.graph.callees(&bare) {
		if label, outbound := g.outboundBodies[body]; outbound {
			return label
		}
	}
	for _, argument := range call.Args {
		if fn, ok := g.valueObject(argument).(*types.Func); ok {
			if label, outbound := g.outbound[fn]; outbound {
				return label + ", handed on as a value"
			}
		}
	}
	return ""
}

// walkTable walks every route of the table from its handler, in the table's order, and answers
// the routes it read, each as `METHOD pattern`.
func (g *networkWaitGuard) walkTable(t *testing.T) []string {
	t.Helper()
	var routes []string
	for _, route := range tableRoutes(t, g.graph) {
		name := route.method + " " + route.pattern
		routes = append(routes, name)
		for _, handler := range route.handlers {
			g.walk(handler, networkWaitStep{route: name})
		}
	}
	return routes
}

// walk marks fn, which runs while the request holds its admission unit, reached, and walks
// everything it runs that also does: each call of its own body, a go statement's included, and
// whatever the call runs or is handed (callees). It does not follow work handed to the server's
// background tracker (keyedWriteGuard.detached) or a go statement on a context
// detachedFromRequest made, which run after the request or with no ticket, nor a go statement
// whose goroutine fn waits on alone only inside a network wait (awaitedAway), nor what a network
// wait is handed (waitOf), which runs with the unit given back. An outbound call it meets is an
// offence. A function networkWaitGuardAllowed names is not followed.
func (g *networkWaitGuard) walk(fn *lockFunction, step networkWaitStep) {
	if fn == nil || g.reached[fn] || g.helpers[fn] {
		return
	}
	if _, allowed := g.allowed[fn.name]; allowed {
		g.allowedReached[fn.name] = true
		return
	}
	g.reached[fn] = true
	g.via[fn] = step
	fired := map[*ast.CallExpr]bool{}
	ast.Inspect(fn.body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.GoStmt:
			fired[n.Call] = true
		case *ast.Ident:
			if called, ok := g.graph.info.Uses[n].(*types.Func); ok && g.honourWaits && g.inPlace[g.graph.declared[called.Origin()]] {
				g.offend(fn, g.graph.line(fn.source, n),
					functionName(called)+", which runs its wait in place, so only where the unit is given back already")
			}
		}
		return true
	})
	for _, call := range ownCalls(fn) {
		if g.keyed.detached(call) || (fired[call] && (g.onDetachedContext(call) || g.awaitedAway(fn, call))) {
			continue
		}
		at := g.graph.line(fn.source, call)
		if label := g.outboundCall(call); label != "" {
			g.offend(fn, at, label)
			continue
		}
		if waits, waiters := g.waitOf(call); waits {
			for _, waiter := range waiters {
				g.walk(waiter, networkWaitStep{from: fn, at: at})
			}
			continue
		}
		for _, callee := range g.callees(call) {
			if label, outbound := g.outboundBodies[callee]; outbound {
				g.offend(fn, at, label+", handed on as a value")
				continue
			}
			g.walk(callee, networkWaitStep{from: fn, at: at})
		}
	}
}

// channelReceive is one receive from a channel in the function that starts the goroutine handing
// it over, as awaitedAway reads it: away, inside a function handed to a network wait; polled, a
// case of a select with a default case; or beside the channels the other cases of its select
// receive from, nil where it is no select's case. A receive that is none of these waits on the
// channel alone, holding the unit.
type channelReceive struct {
	away, polled bool
	beside       []types.Object
}

// awaitedAway reports whether call, a go statement's in fn, starts a goroutine whose waiter gives
// its unit back around the wait on it alone: a function literal that hands its answer over on
// channels of fn's (a send or a close, handsOver), at least one receive of which is inside a
// network wait, and every other receive of which, in fn, is polled or is a case of a select that
// also receives what one of fn's own goroutines hands over - a goroutine of fn's no network wait
// waits for, the request's own work, which the guard follows. The request then waits on the
// goroutine holding its unit only while it waits on its own work too, and holds none once the
// goroutine is all it has left to wait on: the related-item check's embedding, beside its keyword
// stage (runRelatedCheck). Such a goroutine's outbound calls hold no unit of the request, and the
// walk does not follow it. Any other goroutine - one fn waits on alone holding its unit, beside a
// clock or a context alone, or nowhere - is followed as the request's own.
func (g *networkWaitGuard) awaitedAway(fn *lockFunction, call *ast.CallExpr) bool {
	aside, ok := ast.Unparen(call.Fun).(*ast.FuncLit)
	if !ok {
		return false
	}
	answers := g.handsOver(aside)
	if len(answers) == 0 {
		return false
	}
	receives := g.receives(fn, aside)
	workers := map[types.Object]bool{}
	ast.Inspect(fn.body, func(node ast.Node) bool {
		statement, ok := node.(*ast.GoStmt)
		if !ok {
			return true
		}
		literal, ok := ast.Unparen(statement.Call.Fun).(*ast.FuncLit)
		if !ok || literal == aside {
			return true
		}
		channels := g.handsOver(literal)
		for channel := range channels {
			if slices.ContainsFunc(receives[channel], func(receive channelReceive) bool { return receive.away }) {
				return true
			}
		}
		maps.Copy(workers, channels)
		return true
	})
	away := false
	for channel := range answers {
		for _, receive := range receives[channel] {
			switch {
			case receive.away:
				away = true
			case receive.polled:
			case slices.ContainsFunc(receive.beside, func(other types.Object) bool { return workers[other] }):
			default:
				return false
			}
		}
	}
	return away
}

// handsOver is every channel literal sends on or closes, by its variable or field.
func (g *networkWaitGuard) handsOver(literal *ast.FuncLit) map[types.Object]bool {
	channels := map[types.Object]bool{}
	ast.Inspect(literal.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SendStmt:
			if channel := g.channelOf(n.Chan); channel != nil {
				channels[channel] = true
			}
		case *ast.CallExpr:
			if name, ok := ast.Unparen(n.Fun).(*ast.Ident); ok && len(n.Args) == 1 {
				if _, builtin := g.graph.info.Uses[name].(*types.Builtin); builtin && name.Name == "close" {
					if channel := g.channelOf(n.Args[0]); channel != nil {
						channels[channel] = true
					}
				}
			}
		}
		return true
	})
	return channels
}

// receives is every receive in fn's body from a channel, outside skip, by the channel's variable
// or field: each read as away, polled or beside its select's other cases (channelReceive). A range
// over a channel waits on it alone.
func (g *networkWaitGuard) receives(fn *lockFunction, skip *ast.FuncLit) map[types.Object][]channelReceive {
	received := map[types.Object][]channelReceive{}
	var stack []ast.Node
	ast.Inspect(fn.body, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if node == skip {
			return false
		}
		switch n := node.(type) {
		case *ast.UnaryExpr:
			if channel := g.channelOf(n.X); channel != nil && n.Op == token.ARROW {
				received[channel] = append(received[channel], g.receiveAt(stack))
			}
		case *ast.RangeStmt:
			if channel := g.channelOf(n.X); channel != nil {
				received[channel] = append(received[channel], channelReceive{})
			}
		}
		stack = append(stack, node)
		return true
	})
	return received
}

// receiveAt reads a receive whose enclosing nodes are stack, outermost first.
func (g *networkWaitGuard) receiveAt(stack []ast.Node) channelReceive {
	var receive channelReceive
	for index := len(stack) - 1; index > 0; index-- {
		literal, ok := stack[index].(*ast.FuncLit)
		if !ok {
			continue
		}
		if wait, ok := stack[index-1].(*ast.CallExpr); ok && slices.Contains(wait.Args, ast.Expr(literal)) {
			if waits, _ := g.waitOf(wait); waits {
				receive.away = true
				return receive
			}
		}
	}
	// A select's case is its comm statement, an expression or an assignment receiving, inside the
	// case clause, inside the select's body, inside the select.
	depth := len(stack)
	if depth < 4 {
		return receive
	}
	clause, ok := stack[depth-2].(*ast.CommClause)
	if !ok || clause.Comm != stack[depth-1] {
		return receive
	}
	selection, ok := stack[depth-4].(*ast.SelectStmt)
	if !ok {
		return receive
	}
	for _, other := range selection.Body.List {
		other := other.(*ast.CommClause)
		switch {
		case other.Comm == nil:
			receive.polled = true
		case other != clause:
			if channel := g.commChannel(other.Comm); channel != nil {
				receive.beside = append(receive.beside, channel)
			}
		}
	}
	return receive
}

// commChannel is the channel a select case's comm statement receives from, nil for a send or for
// a receive from what a call returns (a context's Done).
func (g *networkWaitGuard) commChannel(comm ast.Stmt) types.Object {
	var received ast.Expr
	switch c := comm.(type) {
	case *ast.ExprStmt:
		received = c.X
	case *ast.AssignStmt:
		if len(c.Rhs) == 1 {
			received = c.Rhs[0]
		}
	}
	if unary, ok := ast.Unparen(received).(*ast.UnaryExpr); ok && unary.Op == token.ARROW {
		return g.channelOf(unary.X)
	}
	return nil
}

// channelOf is the variable or field expression names where it is a channel, nil otherwise.
func (g *networkWaitGuard) channelOf(expression ast.Expr) types.Object {
	variable, ok := g.graph.objectOf(expression).(*types.Var)
	if !ok {
		return nil
	}
	if _, channel := variable.Type().Underlying().(*types.Chan); !channel {
		return nil
	}
	return variable.Origin()
}

// callees is what call runs while the request holds its unit: what keyedWriteGuard.callees says,
// less each function handed for a parameter that the called function runs only inside a network
// wait (handsOnly) - relayBrokerRead's read, which it runs inside awayOnNetwork - since that
// function runs with the unit given back.
func (g *networkWaitGuard) callees(call *ast.CallExpr) []*lockFunction {
	called, _ := g.callee(call).(*types.Func)
	body := g.graph.declared[called]
	if !g.honourWaits || body == nil {
		return g.keyed.callees(call)
	}
	signature := called.Signature()
	kept := *call
	kept.Args = nil
	for index, argument := range call.Args {
		parameter := index < signature.Params().Len() && !(signature.Variadic() && index >= signature.Params().Len()-1)
		if parameter && len(g.graph.functionValue(argument)) > 0 && g.handsOnly(body, signature.Params().At(index)) {
			continue
		}
		kept.Args = append(kept.Args, argument)
	}
	return g.keyed.callees(&kept)
}

// alone is a guard that reads the graph as g does and walks afresh, with allowed and honour. One
// that reads no wait reads no carrier, and keeps none of g's.
func (g *networkWaitGuard) alone(allowed map[string]string, honour bool) *networkWaitGuard {
	fork := *g
	fork.allowed, fork.honourWaits = allowed, honour
	if !honour {
		fork.carriers = map[*types.Var]*waitCarrier{}
	}
	fork.restart()
	return &fork
}

// offend records the outbound call at, in fn, with the path the walk took to fn.
func (g *networkWaitGuard) offend(fn *lockFunction, at, label string) {
	var path []string
	route := ""
	for f := fn; f != nil; {
		step := g.via[f]
		if step.from == nil {
			path = append(path, f.name)
			route = step.route
			break
		}
		path = append(path, f.name+" (called at "+step.at+")")
		f = step.from
	}
	slices.Reverse(path)
	g.offences = append(g.offences, networkWaitOffence{route: route, path: path, at: at, calls: label})
}

// report is every offence, one line each, sorted.
func (g *networkWaitGuard) report() []string {
	var lines []string
	for _, offence := range g.offences {
		lines = append(lines, offence.String())
	}
	slices.Sort(lines)
	return slices.Compact(lines)
}

// staleAllowed is every entry of the allowlist the guard must refuse: one without a reason, one
// whose function is gone or no route reaches, and one whose function reaches no outbound call,
// so it excuses nothing. Run it after walkTable.
func (g *networkWaitGuard) staleAllowed() []string {
	var stale []string
	for _, name := range slices.Sorted(maps.Keys(g.allowed)) {
		fn := g.graph.named[name]
		switch {
		case strings.TrimSpace(g.allowed[name]) == "":
			stale = append(stale, name+" has no reason")
		case fn == nil:
			stale = append(stale, name+" names no function")
		case !g.allowedReached[name]:
			stale = append(stale, name+" is reached from no route's handler")
		default:
			excused := maps.Clone(g.allowed)
			delete(excused, name)
			alone := g.alone(excused, g.honourWaits)
			alone.walk(fn, networkWaitStep{route: "the allowlisted " + name})
			if len(alone.offences) == 0 {
				stale = append(stale, name+" reaches no outbound call, so excuses nothing")
			}
		}
	}
	return stale
}

// TestNoRequestHoldsItsAdmissionUnitAcrossAnOutboundCall enforces network_wait.go's rule: no
// request holds its admission unit across a call of another server - the Envoy listener, GitHub,
// the secrets broker, the embedder, the model, NATS - since a server slow to answer would fill
// its class with requests doing no work, and refuse every request that waits on nothing.
//
// The outbound calls are every exported method of the clients of those servers, derived from
// their types (outboundMethods): the type of each Deps field outboundDeps names, and the NATS
// connection, JetStream handle and bus client outboundTypes names, less the methods outboundLocal
// names, which answer from what the client holds. A call is one when it names such a method, when
// a call through an interface or a variable runs the module's body of one, or when it hands one
// on as a value.
//
// The guard reads every route's handler from the routes table and follows it through the
// copy-lock guard's graph as the keyed-write guard does (keyedWriteGuard.callees): its own calls,
// a go statement's, and every function a call is handed, since a request that waits for an
// errgroup or a goroutine holds its unit while they wait. A request holds no unit inside a network
// wait: what a call of a networkWaitHelpers function is handed is not followed, nor what a call
// through a variable, field or parameter is handed when every value the module hands that
// carrier is a network wait - a helper, or a function that hands its callback on to one and runs
// it nowhere else (waitOf) - nor a function handed for a parameter that the called function runs
// only inside such a wait (callees). Nor is a goroutine the request waits on alone only inside
// such a wait, and otherwise only beside a goroutine of its own work or by a poll - an outbound
// call run beside the request's own reads, whose waiter gives its unit back once the call is all
// it has left to wait on, as the related-item check's embedding runs (awaitedAway).
// givenBackAlready runs what it is handed in place, so a reference to it in a function a request
// reaches holding its unit is refused (networkWaitInPlace). Work outside the request is not
// followed either: what goes to the server's background tracker (goDetached), and a go statement
// whose goroutine runs on a context detachedFromRequest made. A call on such a context on the
// request's own goroutine is followed, since that goroutine still holds the unit. Every outbound
// call left is refused, named with its path from the handler. networkWaitGuardAllowed names, with
// their reasons, the functions it does not follow; an entry that is no longer reached, or that
// reaches no outbound call, fails it.
func TestNoRequestHoldsItsAdmissionUnitAcrossAnOutboundCall(t *testing.T) {
	t.Parallel()
	graph := copyLockGraph(t)
	guard := newNetworkWaitGuard(t, graph, outboundMethods(t, graph), networkWaitGuardAllowed, networkWaitHelpers, networkWaitInPlace)
	guard.walkTable(t)
	if offences := guard.report(); len(offences) > 0 {
		t.Errorf("a route's request holds its admission unit across a call of another server:\n\t%s\n"+
			"Run the call inside a network wait (awayOnNetwork, or awayOnNetworkNeverRefused after a side "+
			"effect, network_wait.go), which gives the unit back for it and takes it back after.",
			strings.Join(offences, "\n\t"))
	}
	if stale := guard.staleAllowed(); len(stale) > 0 {
		t.Errorf("networkWaitGuardAllowed holds entries the guard must refuse:\n\t%s", strings.Join(stale, "\n\t"))
	}
}

// networkWaitFixedSites are functions whose outbound calls run inside a network wait: walked alone
// they reach none while their request holds its unit, and walked alone with every wait read as an
// ordinary call they reach one. One for each server a route's network wait waits on, and each kind
// of wait: awayOnNetwork on a read, on a write before its transaction, around an errgroup's
// lookups and around a function a caller hands in; delivery.onGitHub through the hook a request's
// results read and a result's read-through set; awayOnNetworkNeverRefused after a side effect, and
// as a take-over loop's carrier; a related-item check's embedding, a goroutine beside its keyword
// stage that the check waits on alone only through embedAway (awaitedAway); and
// awayOnNetworkForGood.
var networkWaitFixedSites = []string{
	"api.server.writeSubscribers",
	"api.server.issueLegionAgents",
	"api.server.getAppInstallation",
	"api.server.deriveCIResults",
	"api.server.runSearch",
	"api.server.listCredentialPending",
	"api.server.judgeHolder",
	"api.server.createBroadcast",
	"api.server.resolveCommentDeliveries",
	"api.server.syncArchitectureSource",
	"api.server.listCheckResults",
	"api.server.readThroughPullRequests",
	"api.server.sendResolvedDelivery",
	"api.server.queueForUnknownSession",
	"api.server.deliverMessageResuming",
	"api.server.runRelatedCheck",
	"api.server.approveCredentialRecord",
}

// TestTheNetworkWaitGuardSeesWhatItGuards holds the guard to what it must see, so it cannot pass
// by seeing nothing: every route of the table, an outbound method of each client, the outbound
// calls of networkWaitFixedSites, each inside a network wait (which it names when it reads a wait
// as an ordinary call), and a planted package of handlers whose offences it must name with their
// paths and whose waits it must pass, with an allowlist it must refuse.
func TestTheNetworkWaitGuardSeesWhatItGuards(t *testing.T) {
	t.Parallel()
	graph := copyLockGraph(t)
	outbound := outboundMethods(t, graph)
	labels := map[string]bool{}
	for fn := range outbound {
		labels[functionName(fn)] = true
	}
	for _, method := range []string{
		"envoy.Client.Sessions", "envoy.Client.Send", "githubapp.Client.Installed",
		"githubapp.Client.CommitCheckRuns", "cipher.Client.Pending", "cipher.Client.Approve",
		"embed.Embedder.Embed", "api.Answerer.Run", "api.AgentPublisher.PublishReportingDuplicateWithin",
		"agentstream.Source.Replay", "nats.Conn.Request", "bus.Client.PublishReportingDuplicateWithin",
	} {
		if !labels[method] {
			t.Errorf("the guard does not count %s as an outbound call", method)
		}
	}

	guard := newNetworkWaitGuard(t, graph, outbound, networkWaitGuardAllowed, networkWaitHelpers, networkWaitInPlace)
	read := guard.walkTable(t)
	want := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	if len(read) != len(want) {
		t.Errorf("the guard reads %d routes, want the table's %d", len(read), len(want))
	}
	for _, function := range []string{"api.server.writeSubscribers", "api.server.createBroadcast", "api.server.listCheckResults"} {
		if fn := graph.named[function]; fn == nil || !guard.reached[fn] {
			t.Errorf("the guard does not follow a handler to %s", function)
		}
	}

	for _, site := range networkWaitFixedSites {
		fn := graph.named[site]
		if fn == nil {
			t.Errorf("networkWaitFixedSites names %s, which is gone: point the list at its replacement", site)
			continue
		}
		waited := guard.alone(networkWaitGuardAllowed, true)
		waited.walk(fn, networkWaitStep{route: site})
		for _, offence := range waited.offences {
			t.Errorf("walked alone, %s holds its unit across an outbound call: %s", site, offence)
		}
		unwaited := guard.alone(map[string]string{}, false)
		unwaited.walk(fn, networkWaitStep{route: site})
		if len(unwaited.offences) == 0 {
			t.Errorf("with every network wait read as an ordinary call, the guard names no outbound call %s reaches", site)
		}
	}

	planted := plantedNetworkWaitGraph(t, graph)
	plantedGuard := newNetworkWaitGuard(t, planted, outbound, map[string]string{
		"api.server.allowedLookup": "planted: its outbound call is excused",
		"api.server.reasonless":    "",
		"api.server.quiet":         "planted: it reaches no outbound call",
		"api.server.unreached":     "planted: no route reaches it",
		"api.server.gone":          "planted: it names no function",
	}, []string{"api.awayOnNetwork", "api.givenBackAlready"}, []string{"api.givenBackAlready"})
	plantedGuard.walkTable(t)
	got := map[string]networkWaitOffence{}
	for _, offence := range plantedGuard.offences {
		got[offence.route] = offence
	}
	for route, calls := range map[string]string{
		"GET /held":       "the Envoy listener envoy.Client.Sessions",
		"GET /deep":       "GitHub githubapp.Client.Installed",
		"POST /group":     "the Envoy listener envoy.Client.Send",
		"GET /value":      "the embedder embed.Embedder.Embed, handed on as a value",
		"GET /carried":    "the secrets broker cipher.Client.Pending",
		"POST /fired":     "the Envoy listener envoy.Client.Sessions",
		"POST /through":   "the Envoy listener envoy.Client.Sessions",
		"POST /inplace":   "api.givenBackAlready, which runs its wait in place, so only where the unit is given back already",
		"GET /asideheld":  "the embedder embed.Embedder.Embed",
		"GET /asideclock": "the embedder embed.Embedder.Embed",
	} {
		offence, named := got[route]
		switch {
		case !named:
			t.Errorf("the guard does not name the planted %s, which calls %s holding its unit", route, calls)
		case offence.calls != calls:
			t.Errorf("the guard names the planted %s's call as %q, want %q", route, offence.calls, calls)
		}
		delete(got, route)
	}
	for route, offence := range got {
		t.Errorf("the guard names the planted %s, whose outbound calls hold no unit: %s", route, offence)
	}
	for route, function := range map[string]string{
		"GET /deep":       "api.server.installed",
		"GET /carried":    "api.server.through",
		"POST /group":     "api.server.group",
		"POST /through":   "api.server.throughMixed",
		"GET /asideheld":  "api.server.asideHeld",
		"GET /asideclock": "api.server.asideClock",
	} {
		for _, offence := range plantedGuard.offences {
			if offence.route == route && !strings.Contains(strings.Join(offence.path, " -> "), function) {
				t.Errorf("the guard's path for the planted %s does not pass %s: %s", route, function, offence)
			}
		}
	}
	stale := plantedGuard.staleAllowed()
	for _, entry := range []string{
		"api.server.reasonless has no reason",
		"api.server.quiet reaches no outbound call, so excuses nothing",
		"api.server.unreached is reached from no route's handler",
		"api.server.gone names no function",
	} {
		if !slices.Contains(stale, entry) {
			t.Errorf("the guard does not refuse the planted allowlist entry: want %q among %q", entry, stale)
		}
	}
	if slices.ContainsFunc(stale, func(entry string) bool { return strings.HasPrefix(entry, "api.server.allowedLookup ") }) {
		t.Errorf("the guard refuses the planted allowlist entry that excuses a reached outbound call: %q", stale)
	}
}

// plantedNetworkWaitSource is a package of handlers the guard must judge, type-checked against
// the Dispatch server's own packages: its outbound calls are the real clients' methods, and its
// helper, background tracker and detached context stand where network_wait.go's and server.go's do.
const plantedNetworkWaitSource = `package api

import (
	"context"
	"net/http"

	"golang.org/x/sync/errgroup"

	dispatch "github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
)

type routeRow struct {
	Method, Pattern, Auth, Description string
	Handler                            http.HandlerFunc
}

type backgroundWork struct{}

func (backgroundWork) goDetached(work func()) { go work() }

type server struct {
	deps     dispatch.Deps
	detached backgroundWork
}

func awayOnNetwork(ctx context.Context, wait func(context.Context) error) error { return wait(ctx) }

func givenBackAlready(ctx context.Context, wait func(context.Context) error) error { return wait(ctx) }

func detachedFromRequest(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

func (s *server) routes() []routeRow {
	return []routeRow{
		{http.MethodGet, "/held", "any", "the listener, on the request's goroutine", s.held},
		{http.MethodGet, "/deep", "any", "GitHub, two calls down", s.deep},
		{http.MethodPost, "/group", "any", "a send an errgroup the request waits for makes", s.group},
		{http.MethodGet, "/value", "any", "the embedder's method handed on as a value", s.value},
		{http.MethodGet, "/carried", "any", "the broker through a carrier handed no network wait", s.carried},
		{http.MethodPost, "/fired", "any", "a go statement on the request's own context", s.fired},
		{http.MethodPost, "/through", "any", "a carrier handed a wait and a function that is none", s.through2},
		{http.MethodGet, "/away", "any", "the listener inside awayOnNetwork", s.away},
		{http.MethodGet, "/waits", "any", "carriers handed only network waits", s.waits},
		{http.MethodPost, "/detached", "any", "work that outlives the request", s.detachedWork},
		{http.MethodGet, "/allowed", "any", "an allowlisted call beside one whose entry has no reason", s.allowed},
		{http.MethodPost, "/inplace", "any", "the listener inside givenBackAlready, holding its unit", s.inPlace},
		{http.MethodGet, "/aside", "any", "the embedder beside the request's own work, waited on alone inside awayOnNetwork", s.aside},
		{http.MethodGet, "/asideheld", "any", "the embedder on a goroutine waited on alone holding the unit", s.asideHeld},
		{http.MethodGet, "/asideclock", "any", "the embedder on a goroutine waited on beside the request's context alone", s.asideClock},
	}
}

func (s *server) held(w http.ResponseWriter, r *http.Request) { s.deps.Envoy.Sessions(r.Context()) }

func (s *server) deep(w http.ResponseWriter, r *http.Request) { s.installed(r.Context()) }

func (s *server) installed(ctx context.Context) bool {
	ok, _ := s.deps.GitHub.Installed(ctx, "owner", "repo")
	return ok
}

func (s *server) group(w http.ResponseWriter, r *http.Request) {
	var sends errgroup.Group
	sends.Go(func() error {
		_, err := s.deps.Envoy.Send(r.Context(), envoy.SendInput{})
		return err
	})
	_ = sends.Wait()
}

func (s *server) value(w http.ResponseWriter, r *http.Request) { keep(s.deps.Embedder.Embed) }

func keep(any) {}

func (s *server) carried(w http.ResponseWriter, r *http.Request) {
	_ = s.through(r.Context(), func(ctx context.Context, wait func(context.Context) error) error { return wait(ctx) })
}

func (s *server) through(ctx context.Context, away func(context.Context, func(context.Context) error) error) error {
	return away(ctx, func(ctx context.Context) error {
		_, err := s.deps.AgentSecrets.Pending(ctx, "approver")
		return err
	})
}

func (s *server) fired(w http.ResponseWriter, r *http.Request) { go s.deps.Envoy.Sessions(r.Context()) }

func (s *server) through2(w http.ResponseWriter, r *http.Request) {
	_ = s.throughMixed(r.Context(), awayOnNetwork)
	_ = s.throughMixed(r.Context(), runInPlace)
}

func runInPlace(ctx context.Context, wait func(context.Context) error) error { return wait(ctx) }

func (s *server) throughMixed(ctx context.Context, away func(context.Context, func(context.Context) error) error) error {
	return away(ctx, func(ctx context.Context) error {
		_, err := s.deps.Envoy.Sessions(ctx)
		return err
	})
}

func (s *server) away(w http.ResponseWriter, r *http.Request) {
	_ = awayOnNetwork(r.Context(), func(ctx context.Context) error {
		_, err := s.deps.Envoy.Sessions(ctx)
		return err
	})
}

type check struct {
	wait func(context.Context, func(context.Context) error) error
}

func (s *server) waits(w http.ResponseWriter, r *http.Request) {
	_ = s.throughAway(r.Context(), awayOnNetwork)
	c := check{wait: afterWrite(r.Context())}
	_ = s.throughAway(r.Context(), c.wait)
}

func afterWrite(request context.Context) func(context.Context, func(context.Context) error) error {
	return func(_ context.Context, wait func(context.Context) error) error { return awayOnNetwork(request, wait) }
}

func (s *server) throughAway(ctx context.Context, away func(context.Context, func(context.Context) error) error) error {
	return away(ctx, func(ctx context.Context) error {
		_, err := s.deps.Envoy.Sessions(ctx)
		return err
	})
}

func (s *server) detachedWork(w http.ResponseWriter, r *http.Request) {
	sending := detachedFromRequest(r.Context())
	go s.deps.Envoy.Sessions(sending)
	s.detached.goDetached(func() { s.deps.Envoy.Sessions(r.Context()) })
}

func (s *server) allowed(w http.ResponseWriter, r *http.Request) {
	s.allowedLookup(r.Context())
	s.reasonless(r.Context())
	s.quiet()
}

func (s *server) allowedLookup(ctx context.Context) { s.deps.Envoy.Sessions(ctx) }

func (s *server) reasonless(ctx context.Context) { s.deps.Envoy.Sessions(ctx) }

func (s *server) quiet() {}

func (s *server) unreached(ctx context.Context) { s.deps.Envoy.Sessions(ctx) }

func (s *server) inPlace(w http.ResponseWriter, r *http.Request) {
	_ = givenBackAlready(r.Context(), func(ctx context.Context) error {
		_, err := s.deps.Envoy.Sessions(ctx)
		return err
	})
}

func (s *server) ownWork(ctx context.Context) error { return ctx.Err() }

func (s *server) aside(w http.ResponseWriter, r *http.Request) {
	embedded := make(chan error, 1)
	go func() {
		_, err := s.deps.Embedder.Embed(r.Context(), []string{"text"}, embed.InputQuery)
		embedded <- err
	}()
	worked := make(chan error, 1)
	go func() { worked <- s.ownWork(r.Context()) }()
	select {
	case <-embedded:
		<-worked
		return
	case <-worked:
	}
	select {
	case <-embedded:
		return
	default:
	}
	_ = awayOnNetwork(r.Context(), func(ctx context.Context) error {
		<-embedded
		return nil
	})
}

func (s *server) asideHeld(w http.ResponseWriter, r *http.Request) {
	embedded := make(chan error, 1)
	go func() {
		_, err := s.deps.Embedder.Embed(r.Context(), []string{"text"}, embed.InputQuery)
		embedded <- err
	}()
	if r.URL.Query().Has("held") {
		<-embedded
		return
	}
	_ = awayOnNetwork(r.Context(), func(ctx context.Context) error {
		<-embedded
		return nil
	})
}

func (s *server) asideClock(w http.ResponseWriter, r *http.Request) {
	embedded := make(chan error, 1)
	go func() {
		_, err := s.deps.Embedder.Embed(r.Context(), []string{"text"}, embed.InputQuery)
		embedded <- err
	}()
	select {
	case <-embedded:
		return
	case <-r.Context().Done():
	}
	_ = awayOnNetwork(r.Context(), func(ctx context.Context) error {
		<-embedded
		return nil
	})
}
`

// plantedNetworkWaitGraph type-checks plantedNetworkWaitSource as a package of its own, against
// the very packages the Dispatch server's graph holds, and reads it into a graph of its own, so
// the shared graph the other guards read is left as it was.
func plantedNetworkWaitGraph(t *testing.T, real *lockGraph) *lockGraph {
	t.Helper()
	packages := plantedImporter{}
	for _, objects := range []map[*ast.Ident]types.Object{real.info.Defs, real.info.Uses} {
		for _, object := range objects {
			if object != nil && object.Pkg() != nil {
				packages[object.Pkg().Path()] = object.Pkg()
			}
		}
	}
	fset := token.NewFileSet()
	path := filepath.Join(real.root, "internal", "dispatch", "api", "planted_network_waits.go")
	file, err := parser.ParseFile(fset, path, plantedNetworkWaitSource, 0)
	if err != nil {
		t.Fatalf("parse the planted handlers: %v", err)
	}
	info := &types.Info{
		Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	config := types.Config{Importer: packages}
	if _, err := config.Check("github.com/sjawhar/envoy/internal/dispatch/api/planted", fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("type-check the planted handlers: %v", err)
	}
	graph := &lockGraph{
		root: real.root, info: info, constants: map[string]string{}, named: map[string]*lockFunction{},
		declared: map[*types.Func]*lockFunction{}, literals: map[*ast.FuncLit]*lockFunction{},
		stored: map[*types.Var][]*lockFunction{}, implementations: map[*types.Func][]*lockFunction{},
	}
	source := moduleSource{path: path, file: file, positions: fset}
	graph.addFile(source)
	graph.addStored(source)
	return graph
}

// plantedImporter imports a package the planted handlers name from the Dispatch server's graph.
type plantedImporter map[string]*types.Package

func (p plantedImporter) Import(path string) (*types.Package, error) {
	if pkg, ok := p[path]; ok {
		return pkg, nil
	}
	return nil, fmt.Errorf("the Dispatch server's graph holds no package %s", path)
}

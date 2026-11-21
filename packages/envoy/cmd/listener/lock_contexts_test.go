package main

import (
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// listenerPackage is the listener's import path.
const listenerPackage = "github.com/sjawhar/envoy/cmd/listener"

// freshContexts are the standard library's ways to make a context that carries none of its
// caller's: its deadline, its cancellation, and its mark of a lock held across the call
// (bus.WithLockHeld). A call of one of them in envoy's own code is what this check reads.
var freshContexts = []string{"context.Background", "context.TODO"}

// lockTakers are the calls that take a session's lock or a role's keep guard. A function that makes
// one is where a walk starts: it and everything it reaches runs with the lock held, as far as a
// call graph can tell.
var lockTakers = []string{
	"(*" + listenerPackage + ".sessionLocks).lock",
	"(*" + listenerPackage + ".sessionLocks).lockWithin",
	"(*" + listenerPackage + ".roleKeepGuards).tryHold",
	"(*" + listenerPackage + ".roleKeepGuards).hold",
}

// handedLocks are the functions that run with a lock a caller took and handed them, which no call
// of a lockTakers function marks: each is a root of the walk too.
var handedLocks = map[string]string{
	"(*" + listenerPackage + ".listenerDeps).registerPass": "holds the session's lock sessionDelivery.consumerFor takes and hands it (the unlock it returns)",
}

// releasedWaits are the waits a caller makes for a lost connection only once it has let its lock
// go (bus.Reconnected's rule: release what is held first): relockAfterReconnect releases the
// session's lock and then waits, and the role's handovers and the sweep's head read give the guard
// back first (handOverSweptLane, drainKeptRole, sweptLaneHead). What such a wait reaches - the dial
// that replaces a connection, the reconnect's hooks - runs with nothing held, so the walk stops
// there.
var releasedWaits = []string{"github.com/sjawhar/envoy/internal/bus.Reconnected"}

// walkStops are functions the walk does not enter, each with why what it reaches is no context a
// lock is held across.
var walkStops = map[string]string{
	// The wait for a connection that a call not marked as made under a lock makes, the dial that
	// replaces a connection and the install of its JetStream context. The mark is a value, which
	// a call graph cannot see, so whether a marked call reaches this wait is the rows' to show
	// (bus.TestAMarkedCallDuringAHeldHandshakeAnswersAtOnce, one row per path); what the wait
	// reaches - the socks dialer, the credential's signing, the identity token, the wrapper's
	// construction - is the dial's own, bounded by its connect timeout.
	"(*github.com/sjawhar/envoy/internal/bus.Client).ensureConnWithContext": "the wait of a call not marked as made under a lock",
}

// heldFresh is one fresh context a function under a lock makes: how many call sites make it, and
// why that is no wait its caller's lock is held across.
type heldFresh struct {
	sites int
	why   string
}

// heldFreshContexts are those calls, and those reads of a context kept in a package variable or a
// field, each keyed by the function that makes it and the call or the read (freshContext.call).
// Any other the walk reaches is a context under a lock that carries no mark of the lock, which a
// request made under it would wait on for a reconnect with the lock held. A count that changes
// fails too, in either direction, so a function here that makes one more, or one fewer, is read
// again rather than let through on the old reason.
var heldFreshContexts = map[string]heldFresh{
	"(*github.com/sjawhar/envoy/internal/bus.Client).requestLife -> context.Background": {1,
		"the life of a connection, which no request is made under: requests are bound to it and end at its end, the drop"},
	"github.com/sjawhar/envoy/internal/bus.callContext[github.com/nats-io/nats.go.PubOpt] -> the field github.com/nats-io/nats.go.ContextOpt.Context": {2,
		"the caller's own context, which its options carry (nats.Context(ctx)) and callContext returns: made by the caller, not here"},
	"(*github.com/sjawhar/envoy/internal/bus.Client).requestLife -> the field github.com/sjawhar/envoy/internal/bus.Client.life": {1,
		"the connection's life, compared with the life read before the request's own read of the connection; no request is made under a life: bindRequest makes the request's context from its caller's and ends it at the life's end (context.AfterFunc)"},
	"(*github.com/sjawhar/envoy/internal/bus.Client).requestLife -> the package variable github.com/sjawhar/envoy/internal/bus.endedLife": {3,
		"the life that has already ended, which a request on a connection that is down is bound to, so the request is not sent (Request)"},
	"(*github.com/sjawhar/envoy/internal/bus.Client).runningLifeLocked -> the field github.com/sjawhar/envoy/internal/bus.Client.life": {3,
		"the connection's life, read to tell whether it still runs and to bind a request to it; no request is made under a life (bindRequest)"},
	"(*github.com/sjawhar/envoy/internal/bus.Client).takeLifeLocked -> the field github.com/sjawhar/envoy/internal/bus.Client.life": {2,
		"the connection's life, read to tell whether there is one to end; no request is made under a life (bindRequest)"},
	"(*github.com/sjawhar/envoy/internal/bus.SessionPull).lostConnection -> the field github.com/sjawhar/envoy/internal/bus.SessionPull.life": {1,
		"the life of the pull's connection, read to tell whether it ended; no request is made under it"},
	"(*github.com/sjawhar/envoy/internal/bus.PendingForward).Wait -> the field github.com/sjawhar/envoy/internal/bus.PendingForward.ctx": {2,
		"the forward's own context, which StartRequestStoredTo builds from its caller's (context.WithoutCancel, which keeps the caller's mark) and its own timeout; the forward row reads that at run time"},
	"(*github.com/sjawhar/envoy/cmd/listener.keptRoleOwed).settle -> the field github.com/sjawhar/envoy/cmd/listener.keptRoleOwed.ctx": {1,
		"the handover caller's own context with its cancellation taken off (newKeptRoleOwed), which no guard marks: settle reads its writes back with the guard given back (drainKeptRole, handOverSweptLane), and the settled-confirm row reads that at run time"},
	"github.com/sjawhar/envoy/internal/bus.callContext[github.com/nats-io/nats.go.JSOpt] -> the field github.com/nats-io/nats.go.ContextOpt.Context": {2,
		"the caller's own context, which its options carry (nats.Context(ctx)) and callContext returns: made by the caller, not here"},
	"(github.com/sjawhar/envoy/internal/bus.contextRequests).with -> the field github.com/sjawhar/envoy/internal/bus.contextRequests.ctx": {1,
		"the start's own context (requestsUnder, ensureStreamReported), reached here only through the call graph's join of the JetStream interface: the takeover records' read uses the client's bound wrapper"},
}

// A context a function makes from context.Background() or context.TODO() while its caller holds a
// session's lock or a role's keep guard carries no mark of that lock (bus.WithLockHeld), nor its
// caller's deadline: a request made under it waits for nats.go's own answer, and for a reconnect,
// with the lock held. Review 5985435210 found two such paths (StartRequestStoredTo's context and
// RoleClaim's unbound read); this check walks the call graph from every function that takes a lock
// (lockTakers) or holds one handed to it (handedLocks), and fails on every fresh context it reaches
// that heldFreshContexts does not hold, naming the call site and a path to it. A context read from
// a package variable or a struct field is one too: it was made outside the walk, where no call the
// walk reads made it, so each such read is named as a call of context.Background is, by what it
// reads. A goroutine a walked function starts, a closure made only to be started among them, runs
// outside the lock and is not walked. One shape it does not see: a context a closure captures as a
// free variable from a function the walk does not reach, which is made outside the walk as a
// stored one is; envoy's code holds no such closure (the deep review of round 11 searched for one,
// and for contexts kept in maps, slices and channels, and found none).
//
// The walk is variable type analysis over the program (golang.org/x/tools/go/callgraph/vta), with
// the launcher walk's rules for callbacks (childstarts_test.go): a call the standard library makes
// through a function value or an interface is not followed; every function value, closure and
// method set that walked code outside the standard library hands out is walked as though it were
// called, and so is every function stored into a package variable walked code reads.
//
// It cannot pass by reading nothing: each of lockTakers must be called by a root, and its control
// plants a fresh context in each shape a path under a lock can take, and must see each named: the
// registration repair's record read (a plain call under the delete's lock), the filter step's row
// read (a closure the fence calls), the role claim read RoleClaim makes (a call into the store),
// the forward's own context (StartRequestStoredTo, reached through the configuration's forwardRole
// field), the sweep's lane-head read (under a guard tryHold took), and the walk's next-copy read
// made under a context kept in a package variable. It plants one more, in a goroutine the sweep's
// handover starts, which runs outside the guard, and must not see that one named.
func TestNoContextMadeUnderALockIsFresh(t *testing.T) {
	t.Run("this tree", func(t *testing.T) {
		sites := map[string][]freshContext{}
		for _, fresh := range freshContextsUnderLocks(t, nil) {
			sites[fresh.call()] = append(sites[fresh.call()], fresh)
		}
		for call, found := range sites {
			held, ok := heldFreshContexts[call]
			switch {
			case !ok:
				for _, fresh := range found {
					t.Errorf("%s makes a fresh context (%s at %s) while its caller holds a lock, so a request under it waits for a reconnect with the lock held; derive it from the caller's context, or add it to heldFreshContexts with why it is no such wait. Reached by: %s",
						fresh.caller, fresh.callee, fresh.site, fresh.path)
				}
			case len(found) != held.sites:
				t.Errorf("heldFreshContexts allows %s at %d call sites, and the walk reaches %d: %v; read them again and set the count", call, held.sites, len(found), found)
			}
		}
		for call := range heldFreshContexts {
			if len(sites[call]) == 0 {
				t.Errorf("heldFreshContexts allows %s, which the walk no longer reaches. Remove it only if the code that made it is gone: if that code is still there, the walk has gone blind to it, and the control subtest names the rule that broke", call)
			}
		}
	})
	t.Run("planted fresh contexts are named", func(t *testing.T) {
		overlay := map[string][]byte{}
		plantContext(t, overlay, "session_delivery.go", "entry, err = d.sessions.Refresh(ctx, sessionID)",
			"entry, err = d.sessions.Refresh(context.Background(), sessionID)")
		plantContext(t, overlay, "session_filter_fence.go", "topics, revision, err = storedTopics(ctx, registry, sessionID)",
			"topics, revision, err = storedTopics(context.TODO(), registry, sessionID)")
		plantContext(t, overlay, "../../internal/store/roles.go", "return roles.Get(ctx, role) })",
			"return roles.Get(context.Background(), role) })")
		plantContext(t, overlay, "../../internal/bus/nats.go", "ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)",
			"ctx, cancel := context.WithTimeout(context.Background(), timeout)")
		// S2: a JetStream call under the role's guard whose options carry no nats.Context, which the
		// wrapper's fallback would bind to a fresh context under no mark (bus.callContext).
		plantContext(t, overlay, "../../internal/bus/takeover.go", "&nats.StreamInfoRequest{SubjectsFilter: filter}, nats.Context(ctx))",
			"&nats.StreamInfoRequest{SubjectsFilter: filter})")
		// S6 and S7: a guarded JetStream call made through a plain function, and through a function
		// value, that takes the options and passes them on, called with none.
		plantContext(t, overlay, "../../internal/bus/takeover.go", "func (c *Client) SessionReadFloor(",
			"func plantedForward(js nats.JetStreamContext, opts ...nats.JSOpt) { _, _ = js.StreamInfo(Stream, opts...) }\n\nvar plantedForwardValue = plantedForward\n\nfunc (c *Client) SessionReadFloor(")
		plantContext(t, overlay, "../../internal/bus/takeover.go", "\tname, err := sessionConsumerNameOf(sessionID)\n",
			"\tplantedForward(c.JS())\n\tname, err := sessionConsumerNameOf(sessionID)\n")
		plantContext(t, overlay, "../../internal/bus/stored.go", "func (c *Client) LastStoredMessage(ctx context.Context, subject string) (StoredMessage, error) {\n",
			"func (c *Client) LastStoredMessage(ctx context.Context, subject string) (StoredMessage, error) {\n\tplantedForwardValue(c.JS())\n")
		plantContext(t, overlay, "role_keep_sweep.go", "head, err = readKeptLaneHead(locked, cfg.client, role)",
			"head, err = readKeptLaneHead(context.Background(), cfg.client, role)\n\t\t\t_ = locked")
		plantContext(t, overlay, "role_keep.go", "stored, err := cfg.client.NextStoredMessage(ctx, keptSubject, kept+1)",
			"stored, err := cfg.client.NextStoredMessage(plantedContext, keptSubject, kept+1)")
		plantContext(t, overlay, "role_keep.go", "func walkKeptRole(",
			"var plantedContext = context.Background()\n\nfunc walkKeptRole(")
		// A goroutine the sweep's handover starts runs outside the guard, so a fresh context in it
		// is no context made under the lock, and the check must not name it.
		plantContext(t, overlay, "role_keep_sweep.go", "\towed := newKeptRoleOwed(handoverCtx, cfg, role)\n",
			"\tgo func() { _ = context.TODO() }()\n\towed := newKeptRoleOwed(handoverCtx, cfg, role)\n")
		found := freshContextsUnderLocks(t, overlay)
		for _, want := range []struct{ what, caller string }{
			{"the registration repair's record read, a plain call under the delete's lock", "(*" + listenerPackage + ".sessionDelivery).registeredTarget"},
			{"the filter step's row read, in a closure the fence calls", "(*" + listenerPackage + ".sessionDelivery).storedFilterStep"},
			{"the role claim read, a call into the store", "(*github.com/sjawhar/envoy/internal/store.Registry).RoleClaim$1"},
			{"the forward's own context, reached through the forwardRole field", "(*github.com/sjawhar/envoy/internal/bus.Client).StartRequestStoredTo"},
			{"the sweep's lane-head read, under a guard tryHold took", listenerPackage + ".sweptLaneHead"},
			{"the walk's next-copy read, under a context kept in a package variable", listenerPackage + ".walkKeptRole"},
			{"the takeover records' stream read with no nats.Context among its options (S2)", "(*github.com/sjawhar/envoy/internal/bus.Client).TakeoverRecords"},
			{"a guarded JetStream call through a plain function that passes its options on, called with none (S6)", "(*github.com/sjawhar/envoy/internal/bus.Client).SessionReadFloor"},
			{"a guarded JetStream call through a function value that passes its options on, called with none (S7)", "(*github.com/sjawhar/envoy/internal/bus.Client).LastStoredMessage"},
		} {
			named := slices.ContainsFunc(found, func(fresh freshContext) bool { return fresh.caller == want.caller })
			if !named {
				t.Errorf("the check did not name %s (%s), so it could pass over a real one; it named: %v", want.what, want.caller, found)
			}
		}
		if started := slices.IndexFunc(found, func(fresh freshContext) bool {
			return strings.HasPrefix(fresh.caller, listenerPackage+".handOverSweptLane$")
		}); started >= 0 {
			t.Errorf("the check named %s, a fresh context in a goroutine the sweep's handover starts, which runs outside the guard", found[started])
		}
	})
}

// plantContext replaces, in overlay's copy of file (a path from this package's directory; the file
// on disk when the overlay holds none yet), the first anchor with replacement, and imports context
// where the file does not.
func plantContext(t *testing.T, overlay map[string][]byte, file, anchor, replacement string) {
	t.Helper()
	path, err := filepath.Abs(file)
	if err != nil {
		t.Fatal(err)
	}
	source, planted := overlay[path]
	if !planted {
		if source, err = os.ReadFile(file); err != nil {
			t.Fatal(err)
		}
	}
	text := string(source)
	if !strings.Contains(text, anchor) {
		t.Fatalf("%s no longer holds %q, where the control plants its context", file, anchor)
	}
	text = strings.Replace(text, anchor, replacement, 1)
	if !strings.Contains(text, strconv.Quote("context")) {
		text = strings.Replace(text, "import (\n", "import (\n\t\"context\"\n", 1)
	}
	overlay[path] = []byte(text)
}

// freshContext is one call of a fresh context that code under a lock reaches: the function that
// makes it, what it calls, where, and a path from a root to it.
type freshContext struct {
	caller, callee, site, path string
}

func (f freshContext) String() string { return f.call() + " at " + f.site }

// call is the context's key in heldFreshContexts: the function that makes it and what it calls.
func (f freshContext) call() string { return f.caller + " -> " + f.callee }

// freshContextsUnderLocks walks the listener from every function that takes a lock or holds one
// handed to it, with overlay (file contents in place of the files on disk), and answers every call
// of a freshContexts function that envoy's own code makes there, one per call site.
func freshContextsUnderLocks(t *testing.T, overlay map[string][]byte) []freshContext {
	t.Helper()
	cfg := &packages.Config{Mode: packages.LoadAllSyntax, Dir: ".", Overlay: overlay}
	pkgs, err := packages.Load(cfg, listenerPackage)
	if err != nil {
		t.Fatal(err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("the listener's packages did not load")
	}
	prog, _ := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	functions := ssautil.AllFunctions(prog)
	graph := vta.CallGraph(functions, cha.CallGraph(prog))
	contextPackage := prog.ImportedPackage("context")
	if contextPackage == nil {
		t.Fatal("the listener's program holds no package context")
	}
	contextType := contextPackage.Pkg.Scope().Lookup("Context").Type()

	var roots []*ssa.Function
	for fn := range functions {
		if fn.Pkg == nil || fn.Pkg.Pkg.Path() != listenerPackage {
			continue
		}
		if _, handed := handedLocks[fn.String()]; handed || takesALock(fn) {
			roots = append(roots, fn)
		}
	}
	slices.SortFunc(roots, func(a, b *ssa.Function) int { return strings.Compare(a.String(), b.String()) })
	for name := range handedLocks {
		if !slices.ContainsFunc(roots, func(fn *ssa.Function) bool { return fn.String() == name }) {
			t.Fatalf("handedLocks names %s, which the listener no longer holds", name)
		}
	}
	for _, taker := range lockTakers {
		if !slices.ContainsFunc(roots, func(fn *ssa.Function) bool { return slices.Contains(staticCallees(fn), taker) }) {
			t.Fatalf("lockTakers names %s, which no function of the listener calls, so the walk starts from no scope that takes it: %v", taker, roots)
		}
	}

	w := lockWalk{prog: prog, graph: graph, stored: storedFunctionValues(functions),
		parent: map[*ssa.Function]*ssa.Function{}, seen: map[*ssa.Function]bool{}}
	for _, root := range roots {
		w.visit(root, nil)
	}
	var fresh []freshContext
	for len(w.queue) > 0 {
		fn := w.queue[0]
		w.queue = w.queue[1:]
		std := standardLibraryFunction(fn)
		if envoyFunction(fn) {
			for _, read := range storedContextReads(fn, contextType) {
				fresh = append(fresh, freshContext{
					caller: fn.String(), callee: read.what,
					site: siteIn(prog.Fset, read.pos), path: w.pathTo(fn),
				})
			}
			for _, call := range jetStreamCallsWithoutContext(fn) {
				fresh = append(fresh, freshContext{
					caller: fn.String(), callee: "a JetStream call whose options carry no nats.Context (" + call.callee + ")",
					site: siteIn(prog.Fset, call.pos), path: w.pathTo(fn),
				})
			}
		}
		if node := graph.Nodes[fn]; node != nil {
			for _, edge := range node.Out {
				callee := edge.Callee.Func
				if _, started := edge.Site.(*ssa.Go); started {
					continue
				}
				if slices.Contains(releasedWaits, callee.String()) || slices.Contains(lockTakers, callee.String()) {
					continue
				}
				if _, stop := walkStops[callee.String()]; stop {
					continue
				}
				// bus.callContext answers the context a JetStream call's options carry, and makes a
				// fresh one only for options that carry none: that is a fact of the call site that
				// built the options, which jetStreamCallsWithoutContext reads at every site the walk
				// reaches, so the fallback itself is not named here.
				if envoyFunction(fn) && slices.Contains(freshContexts, callee.String()) && !isCallContext(fn) {
					fresh = append(fresh, freshContext{
						caller: fn.String(), callee: callee.String(),
						site: siteIn(prog.Fset, edge.Pos()), path: w.pathTo(fn),
					})
				}
				if edge.Site != nil && edge.Site.Common().StaticCallee() == nil {
					if std {
						continue
					}
					// A call through a function its caller was handed (a parameter, or a free
					// variable of a closure, or either spilled to the heap because a closure
					// captures it) is a call of a function value made where it was made, which the
					// walk reaches there (handedOut); following every function such a value might
					// be, as the call graph does, would join unrelated callers.
					if !edge.Site.Common().IsInvoke() && handedIn(edge.Site.Common().Value) {
						continue
					}
				}
				w.visit(callee, fn)
			}
		}
		if !std {
			w.handedOut(fn)
		}
	}
	slices.SortFunc(fresh, func(a, b freshContext) int { return strings.Compare(a.String(), b.String()) })
	return slices.CompactFunc(fresh, func(a, b freshContext) bool { return a.String() == b.String() })
}

// handedIn is whether v is a function value its function was handed: a parameter, a closure's free
// variable, or a load of a cell that holds only such values, which is where a parameter a closure
// captures is spilled.
func handedIn(v ssa.Value) bool {
	switch value := v.(type) {
	case *ssa.Parameter, *ssa.FreeVar:
		return true
	case *ssa.ChangeType:
		return handedIn(value.X)
	case *ssa.UnOp:
		if value.Op != token.MUL {
			return false
		}
		switch cell := value.X.(type) {
		case *ssa.FreeVar:
			return true
		case *ssa.Alloc:
			stored := false
			for _, referrer := range *cell.Referrers() {
				if store, ok := referrer.(*ssa.Store); ok && store.Addr == cell {
					if !handedIn(store.Val) {
						return false
					}
					stored = true
				}
			}
			return stored
		}
	}
	return false
}

// takesALock is whether fn calls one of lockTakers, and is not one itself: what a lock taker does
// before it holds the lock (its wait for it) is not done under it.
func takesALock(fn *ssa.Function) bool {
	if slices.Contains(lockTakers, fn.String()) {
		return false
	}
	return slices.ContainsFunc(staticCallees(fn), func(callee string) bool { return slices.Contains(lockTakers, callee) })
}

// staticCallees is every function fn calls by name.
func staticCallees(fn *ssa.Function) []string {
	var callees []string
	for _, block := range fn.Blocks {
		for _, instruction := range block.Instrs {
			if call, ok := instruction.(ssa.CallInstruction); ok {
				if callee := call.Common().StaticCallee(); callee != nil {
					callees = append(callees, callee.String())
				}
			}
		}
	}
	return callees
}

// contextRead is one read of a context kept in a package variable or a struct field: what it
// reads, and where.
type contextRead struct {
	what string
	pos  token.Pos
}

// storedContextReads is every read fn makes of a context kept in a package variable or a struct
// field, the field read through its struct's value or through a pointer to it.
func storedContextReads(fn *ssa.Function, contextType types.Type) []contextRead {
	var reads []contextRead
	for _, block := range fn.Blocks {
		for _, instruction := range block.Instrs {
			switch value := instruction.(type) {
			case *ssa.UnOp:
				if value.Op != token.MUL || !types.Identical(value.Type(), contextType) {
					continue
				}
				switch address := value.X.(type) {
				case *ssa.Global:
					reads = append(reads, contextRead{"the package variable " + address.String(), readSite(fn, value.Pos())})
				case *ssa.FieldAddr:
					reads = append(reads, contextRead{"the field " + fieldName(address.X.Type().Underlying().(*types.Pointer).Elem(), address.Field), readSite(fn, address.Pos())})
				}
			case *ssa.Field:
				if types.Identical(value.Type(), contextType) {
					reads = append(reads, contextRead{"the field " + fieldName(value.X.Type(), value.Field), readSite(fn, value.Pos())})
				}
			}
		}
	}
	return reads
}

// fieldName is field index of structType, named with its struct.
func fieldName(structType types.Type, index int) string {
	return types.TypeString(structType, nil) + "." + structType.Underlying().(*types.Struct).Field(index).Name()
}

// readSite is pos, or fn's own position where the read has none (an implicit load).
func readSite(fn *ssa.Function, pos token.Pos) token.Pos {
	if pos.IsValid() {
		return pos
	}
	return fn.Pos()
}

// storedFunctionValues is every function value and closure that a Store anywhere in the program
// writes directly into a package variable, or into a field or element of one, by that variable
// (childstarts_test.go's storedFunctions).
func storedFunctionValues(functions map[*ssa.Function]bool) map[*ssa.Global][]*ssa.Function {
	stored := map[*ssa.Global][]*ssa.Function{}
	for fn := range functions {
		for _, block := range fn.Blocks {
			for _, instruction := range block.Instrs {
				store, ok := instruction.(*ssa.Store)
				if !ok {
					continue
				}
				global := packageVariableUnder(store.Addr)
				if global == nil {
					continue
				}
				switch value := store.Val.(type) {
				case *ssa.Function:
					stored[global] = append(stored[global], value)
				case *ssa.MakeClosure:
					stored[global] = append(stored[global], value.Fn.(*ssa.Function))
				}
			}
		}
	}
	return stored
}

// packageVariableUnder is the package variable address names, directly or through fields and
// elements of it, and nil for any other address.
func packageVariableUnder(address ssa.Value) *ssa.Global {
	for {
		switch value := address.(type) {
		case *ssa.Global:
			return value
		case *ssa.FieldAddr:
			address = value.X
		case *ssa.IndexAddr:
			address = value.X
		default:
			return nil
		}
	}
}

// lockWalk is the state of one walk: the functions it has reached, in the order it reached them,
// and for each the function it was reached from.
type lockWalk struct {
	prog   *ssa.Program
	graph  *callgraph.Graph
	stored map[*ssa.Global][]*ssa.Function
	parent map[*ssa.Function]*ssa.Function
	seen   map[*ssa.Function]bool
	queue  []*ssa.Function
}

func (w *lockWalk) visit(fn, from *ssa.Function) {
	if fn == nil || w.seen[fn] {
		return
	}
	w.seen[fn] = true
	w.parent[fn] = from
	w.queue = append(w.queue, fn)
}

// handedOut walks what fn hands out, but not to a goroutine it starts: every function it uses as a
// value, every closure it makes, and every function stored into a package variable it reads. A
// call through an interface is a call edge the walk reads, so the methods of a type put into an
// interface are not walked whole.
func (w *lockWalk) handedOut(fn *ssa.Function) {
	for _, block := range fn.Blocks {
		for _, instruction := range block.Instrs {
			if _, started := instruction.(*ssa.Go); started {
				continue
			}
			if closure, ok := instruction.(*ssa.MakeClosure); ok && startedOnly(closure) {
				continue
			}
			var operands []*ssa.Value
			if call, ok := instruction.(ssa.CallInstruction); ok && call.Common().StaticCallee() != nil {
				for i := range call.Common().Args {
					operands = append(operands, &call.Common().Args[i])
				}
			} else {
				operands = instruction.Operands(nil)
			}
			for _, operand := range operands {
				if operand == nil || *operand == nil {
					continue
				}
				switch value := (*operand).(type) {
				case *ssa.Function:
					w.visit(value, fn)
				case *ssa.MakeClosure:
					w.visit(value.Fn.(*ssa.Function), fn)
				case *ssa.Global:
					for _, stored := range w.stored[value] {
						w.visit(stored, fn)
					}
				}
			}
		}
	}
}

// startedOnly is whether every use of closure is a go statement that runs it: a closure made only
// to be started runs on a goroutine of its own, outside its maker's lock.
func startedOnly(closure *ssa.MakeClosure) bool {
	referrers := closure.Referrers()
	if referrers == nil || len(*referrers) == 0 {
		return false
	}
	for _, referrer := range *referrers {
		if started, ok := referrer.(*ssa.Go); !ok || started.Call.Value != closure {
			return false
		}
	}
	return true
}

// pathTo is the chain of functions from a root to fn, first to last.
func (w *lockWalk) pathTo(fn *ssa.Function) string {
	var chain []string
	for current := fn; current != nil; current = w.parent[current] {
		chain = append(chain, current.String())
	}
	slices.Reverse(chain)
	return strings.Join(chain, " -> ")
}

// envoyFunction is whether fn is envoy's own code.
func envoyFunction(fn *ssa.Function) bool {
	pkg := functionPackage(fn)
	return pkg != nil && strings.HasPrefix(pkg.Path(), "github.com/sjawhar/envoy/")
}

// isCallContext is whether fn is an instance of bus.callContext, the JetStream wrapper's read of the
// context a call's options carry.
func isCallContext(fn *ssa.Function) bool {
	origin := fn
	if fn.Origin() != nil {
		origin = fn.Origin()
	}
	return origin.String() == "github.com/sjawhar/envoy/internal/bus.callContext"
}

// contextOptionTypes are the option types of the JetStream calls whose options can carry the call's
// context (nats.Context), which the bound wrapper reads (bus.callContext).
var contextOptionTypes = []string{"github.com/nats-io/nats.go.JSOpt", "github.com/nats-io/nats.go.PubOpt"}

// jetStreamCall is one call fn makes of a JetStream method with options that carry no context: what
// it calls, and where.
type jetStreamCall struct {
	callee string
	pos    token.Pos
}

// jetStreamConstructors open a JetStream context, which sends no request: their options say how
// the context's own calls wait, and carry no call's context.
var jetStreamConstructors = []string{
	"(*github.com/nats-io/nats.go.Conn).JetStream",
	"github.com/sjawhar/envoy/internal/bus.JetStream",
}

// jetStreamCallsWithoutContext is every call fn makes whose last parameter is a variadic JSOpt or
// PubOpt list - a method, a plain function or a function value, all but jetStreamConstructors -
// where the options fn passes carry no nats.Context: none at all, or a list fn builds with no
// nats.Context in it, or a list the check cannot read. A list fn was handed (its own parameter,
// passed on) is the list its own caller built, and that caller's call is read here where the walk
// reaches it, whatever it calls.
func jetStreamCallsWithoutContext(fn *ssa.Function) []jetStreamCall {
	var calls []jetStreamCall
	for _, block := range fn.Blocks {
		for _, instruction := range block.Instrs {
			call, ok := instruction.(ssa.CallInstruction)
			if !ok {
				continue
			}
			if _, started := instruction.(*ssa.Go); started {
				continue
			}
			common := call.Common()
			sig := common.Signature()
			if sig == nil || !sig.Variadic() || len(common.Args) == 0 {
				continue
			}
			options := common.Args[len(common.Args)-1]
			slice, ok := options.Type().Underlying().(*types.Slice)
			if !ok || !slices.Contains(contextOptionTypes, types.TypeString(slice.Elem(), nil)) {
				continue
			}
			if _, builtin := common.Value.(*ssa.Builtin); builtin {
				// append builds a list; the list is read where a call passes it.
				continue
			}
			var callee string
			switch {
			case common.IsInvoke():
				callee = types.TypeString(common.Value.Type(), nil) + "." + common.Method.Name()
			case common.StaticCallee() != nil:
				callee = common.StaticCallee().String()
				if slices.Contains(jetStreamConstructors, callee) {
					continue
				}
			default:
				callee = "a function value of type " + types.TypeString(common.Value.Type(), nil)
			}
			if !optionsCarryContext(options) {
				calls = append(calls, jetStreamCall{callee: callee, pos: instruction.Pos()})
			}
		}
	}
	return calls
}

// optionsCarryContext is whether options, the variadic list a call passes, carries a nats.Context.
// A list its function was handed counts as carrying one, since its caller's call is read where the
// walk reaches it; so does a closure's captured list, read where the closure is made. A list built
// carries one when a nats.ContextOpt is stored into it, an append when either of its lists does,
// a join (a phi) when every list joined does, and a helper's answer when every list it returns does.
// Any other list is read as carrying none.
func optionsCarryContext(options ssa.Value) bool {
	return carriesContext(options, 0)
}

func carriesContext(v ssa.Value, depth int) bool {
	if depth > 6 {
		return false
	}
	switch list := v.(type) {
	case *ssa.Parameter:
		return true
	case *ssa.UnOp:
		// A load of a cell: a list a closure captures is spilled to one, which holds what the
		// function that made the closure stored into it.
		if list.Op != token.MUL {
			return false
		}
		return cellCarriesContext(list.X, depth+1)
	case *ssa.FreeVar:
		fn := list.Parent()
		index := slices.Index(fn.FreeVars, list)
		if fn.Parent() == nil || index < 0 {
			return false
		}
		for _, block := range fn.Parent().Blocks {
			for _, instruction := range block.Instrs {
				if closure, ok := instruction.(*ssa.MakeClosure); ok && closure.Fn == fn {
					return carriesContext(closure.Bindings[index], depth+1)
				}
			}
		}
		return false
	case *ssa.Phi:
		for _, edge := range list.Edges {
			if !carriesContext(edge, depth+1) {
				return false
			}
		}
		return len(list.Edges) > 0
	case *ssa.Call:
		common := list.Common()
		if builtin, ok := common.Value.(*ssa.Builtin); ok {
			return builtin.Name() == "append" && slices.ContainsFunc(common.Args, func(arg ssa.Value) bool { return carriesContext(arg, depth+1) })
		}
		callee := common.StaticCallee()
		if callee == nil || len(callee.Blocks) == 0 {
			return false
		}
		returns := 0
		for _, block := range callee.Blocks {
			if len(block.Instrs) == 0 {
				continue
			}
			if ret, ok := block.Instrs[len(block.Instrs)-1].(*ssa.Return); ok && len(ret.Results) == 1 {
				returns++
				if !carriesContext(ret.Results[0], depth+1) {
					return false
				}
			}
		}
		return returns > 0
	case *ssa.Slice:
		array, ok := list.X.(*ssa.Alloc)
		if !ok || array.Referrers() == nil {
			return false
		}
		for _, use := range *array.Referrers() {
			index, ok := use.(*ssa.IndexAddr)
			if !ok || index.Referrers() == nil {
				continue
			}
			for _, store := range *index.Referrers() {
				stored, ok := store.(*ssa.Store)
				if !ok {
					continue
				}
				if made, ok := stored.Val.(*ssa.MakeInterface); ok && types.TypeString(made.X.Type(), nil) == "github.com/nats-io/nats.go.ContextOpt" {
					return true
				}
			}
		}
	}
	return false
}

// cellCarriesContext is whether every list stored into cell carries a nats.Context: a cell a
// function allocates, or a closure's captured cell, read where the closure is made.
func cellCarriesContext(cell ssa.Value, depth int) bool {
	if depth > 6 {
		return false
	}
	if free, ok := cell.(*ssa.FreeVar); ok {
		fn := free.Parent()
		index := slices.Index(fn.FreeVars, free)
		if fn.Parent() == nil || index < 0 {
			return false
		}
		for _, block := range fn.Parent().Blocks {
			for _, instruction := range block.Instrs {
				if closure, ok := instruction.(*ssa.MakeClosure); ok && closure.Fn == fn {
					return cellCarriesContext(closure.Bindings[index], depth+1)
				}
			}
		}
		return false
	}
	alloc, ok := cell.(*ssa.Alloc)
	if !ok || alloc.Referrers() == nil {
		return false
	}
	stores := 0
	for _, use := range *alloc.Referrers() {
		if store, ok := use.(*ssa.Store); ok && store.Addr == alloc {
			stores++
			if !carriesContext(store.Val, depth+1) {
				return false
			}
		}
	}
	return stores > 0
}

// standardLibraryFunction is whether fn is the standard library's: its package path's first element
// has no dot, as every module path's does.
func standardLibraryFunction(fn *ssa.Function) bool {
	pkg := functionPackage(fn)
	if pkg == nil {
		return false
	}
	first, _, _ := strings.Cut(pkg.Path(), "/")
	return !strings.Contains(first, ".")
}

// functionPackage is the package fn is declared in, a synthetic wrapper's being its method's.
func functionPackage(fn *ssa.Function) *types.Package {
	for fn.Pkg == nil && fn.Origin() != nil && fn.Origin() != fn {
		fn = fn.Origin()
	}
	switch {
	case fn.Pkg != nil:
		return fn.Pkg.Pkg
	case fn.Object() != nil:
		return fn.Object().Pkg()
	}
	return nil
}

// siteIn is where pos is, as a path relative to envoy's module, a line and a column.
func siteIn(fset *token.FileSet, pos token.Pos) string {
	position := fset.Position(pos)
	file := position.Filename
	if i := strings.Index(file, "/packages/envoy/"); i >= 0 {
		file = file[i+len("/packages/envoy/"):]
	}
	return file + ":" + strconv.Itoa(position.Line) + ":" + strconv.Itoa(position.Column)
}

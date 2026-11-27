// census lists every call in the non-test code of the packages it loads to a function or method of
// nats.go (github.com/nats-io/nats.go and its jetstream package), with file:line and the callee, and
// every method of those packages passed as a value. A method call is a REQ line when the callee
// sends a request and waits for its reply, and a LOCAL line otherwise (see local). A call to a
// package-level function, an option, a constructor or Connect, is a FUNC line, which the census
// does not classify. A method passed as a value is a VALUE line.
//
// It leaves out of that list every package of the module whose non-test imports reach the standard
// library's testing package through packages of the module: one that imports testing in a non-test
// file, and every package that imports such a package in a non-test file, transitively. It computes
// that set from the import graph it loads, with no list, and prints each package in it as a SKIP
// line. A product package that imports testing is left out with every package above it, up to the
// binaries that build it; the check's run line fails when a binary under cmd/ is left out. When it
// loads no package, or no non-test file, it prints UNKNOWN and exits 2.
package main

import (
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

const (
	natsPkg = "github.com/nats-io/nats.go"
	jsPkg   = "github.com/nats-io/nats.go/jetstream"
)

// local lists the methods that send nothing to the server, send without waiting for an answer, or
// wait only for the server's PONG to one PING, which carries no subject and no payload, under a
// bound nats.go keeps: Flush, FlushTimeout, FlushWithContext and RTT. At nats.go v1.55.0, Flush and
// RTT call FlushTimeout with 10 s (nats.go:6146-6164); FlushTimeout refuses a timeout <= 0 and ends
// at its timer (nats.go:6105-6143); FlushWithContext refuses a context with no deadline and ends at
// it (context.go:177-218); the PING is "PING\r\n" (nats.go:2108, 6071-6076). A listed ack that
// passes an AckOpt is a request all the same (passesAckOpt). Every method not listed is a request.
//
// One listed method waits on a condition the census cannot see: Subscription.Unsubscribe of a
// JetStream subscription whose consumer nats.go created sends DeleteConsumer and waits for the
// answer (nats.go:5477-5500; js.go:1472-1488, 2033-2035). It is listed because the unsubscribe of a
// core subscription, or of one bound to an existing consumer with nats.Bind, sends no request.
// KeyWatcher.Stop and KeyLister.Stop are not listed: a KV watch's subscription is an ordered
// consumer that nats.go creates (jetstream/kv.go:1335-1360), so a stop deletes it and waits
// (jetstream/kv.go:1236-1241, 1495-1497).
var local = map[string]bool{
	// *nats.Conn
	"Conn.NewInbox": true, "Conn.IsConnected": true, "Conn.IsClosed": true, "Conn.IsReconnecting": true,
	"Conn.IsDraining": true, "Conn.Status": true, "Conn.Stats": true, "Conn.LastError": true,
	"Conn.ConnectedServerVersion": true, "Conn.ConnectedUrl": true, "Conn.ConnectedServerId": true,
	"Conn.ConnectedServerName": true, "Conn.ConnectedAddr": true, "Conn.ConnectedClusterName": true,
	"Conn.Servers": true, "Conn.DiscoveredServers": true, "Conn.MaxPayload": true, "Conn.Close": true,
	"Conn.Publish": true, "Conn.PublishMsg": true, "Conn.PublishRequest": true, "Conn.Subscribe": true,
	"Conn.SubscribeSync": true, "Conn.QueueSubscribe": true, "Conn.QueueSubscribeSync": true,
	"Conn.ChanSubscribe": true, "Conn.ChanQueueSubscribe": true, "Conn.Drain": true, "Conn.Flush": true,
	"Conn.FlushTimeout": true, "Conn.FlushWithContext": true, "Conn.Buffered": true, "Conn.JetStream": true,
	"Conn.StatusChanged": true, "Conn.SetDisconnectErrHandler": true, "Conn.SetReconnectHandler": true,
	"Conn.SetClosedHandler": true, "Conn.SetErrorHandler": true, "Conn.RTT": true, "Conn.ForceReconnect": true,
	"Conn.HeadersSupported": true, "Conn.AuthRequired": true, "Conn.TLSRequired": true, "Conn.GetClientID": true,
	"Conn.GetClientIP": true, "Conn.Opts": true, "Conn.NumSubscriptions": true, "Conn.RemoveStatusListener": true,
	// *nats.Subscription
	"Subscription.Unsubscribe": true, "Subscription.Drain": true, "Subscription.IsValid": true,
	"Subscription.AutoUnsubscribe": true, "Subscription.SetPendingLimits": true, "Subscription.Pending": true,
	"Subscription.Delivered": true, "Subscription.Dropped": true, "Subscription.Type": true,
	"Subscription.QueuedMsgs": true, "Subscription.PendingLimits": true, "Subscription.StatusChanged": true,
	"Subscription.SetClosedHandler": true, "Subscription.MaxPending": true, "Subscription.ClearMaxPending": true,
	// *nats.Msg
	"Msg.Respond": true, "Msg.RespondMsg": true, "Msg.Ack": true, "Msg.Nak": true, "Msg.NakWithDelay": true,
	"Msg.Term": true, "Msg.InProgress": true, "Msg.Metadata": true, "Msg.Equal": true, "Msg.Size": true,
	// jetstream.Msg. DoubleAck is not listed: it sends the ack as a request and waits for the reply
	// (jetstream/message.go:372-374, 441-442 at v1.55.0).
	"Msg.Data": true, "Msg.Headers": true, "Msg.Subject": true, "Msg.Reply": true,
	"Msg.TermWithReason": true,
	// KV values and handles
	"KeyValueEntry.Value": true, "KeyValueEntry.Revision": true, "KeyValueEntry.Key": true,
	"KeyValueEntry.Bucket": true, "KeyValueEntry.Created": true, "KeyValueEntry.Delta": true,
	"KeyValueEntry.Operation": true, "KeyValue.Bucket": true, "KeyWatcher.Updates": true,
	"KeyWatcher.Context": true, "KeyWatcher.Error": true,
	"KeyValueStatus.Bucket": true, "KeyValueStatus.Values": true, "KeyValueStatus.History": true,
	"KeyValueStatus.TTL": true, "KeyValueStatus.BackingStore": true, "KeyValueStatus.Bytes": true,
	"KeyValueStatus.IsCompressed": true, "KeyValueStatus.LimitMarkerTTL": true, "KeyValueStatus.Config": true,
	"Stream.CachedInfo": true, "Consumer.CachedInfo": true, "ConsumeContext.Stop": true,
	"ConsumeContext.Drain": true, "ConsumeContext.Closed": true, "MessagesContext.Stop": true,
	"MessagesContext.Drain": true, "MessageBatch.Messages": true, "MessageBatch.Error": true,
	"JetStream.Conn": true, "JetStream.Options": true, "JetStream.CleanupPublisher": true,
	"JetStream.PublishAsyncPending": true, "JetStream.PublishAsyncComplete": true,
	"JetStreamContext.PublishAsyncPending": true, "JetStreamContext.PublishAsyncComplete": true,
	"StreamInfoLister.Info": true, "StreamInfoLister.Err": true, "StreamNameLister.Name": true,
	"StreamNameLister.Err": true, "ConsumerInfoLister.Info": true, "ConsumerInfoLister.Err": true,
	"KeyLister.Keys": true, "PubAckFuture.Ok": true, "PubAckFuture.Err": true,
	"PubAckFuture.Msg": true, "APIError.Error": true, "APIError.APIError": true, "APIError.Is": true,
	"Header.Get": true, "Header.Set": true, "Header.Add": true, "Header.Values": true, "Header.Del": true,
	"ErrHandler": true, "Status.String": true,
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: census <absolute path of the module directory>")
		os.Exit(2)
	}
	dir := os.Args[1]
	cfg := &packages.Config{Dir: dir, Tests: false, Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if packages.PrintErrors(pkgs) > 0 {
		os.Exit(2)
	}
	if len(pkgs) == 0 {
		fmt.Println("UNKNOWN: loaded no packages")
		os.Exit(2)
	}
	skipped := testOnly(pkgs)
	type row struct{ pos, callee string }
	var requests, others, funcs []row
	files := 0
	for _, pkg := range pkgs {
		if skipped[pkg.PkgPath] {
			continue
		}
		for _, file := range pkg.Syntax {
			name := pkg.Fset.File(file.Pos()).Name()
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			files++
			called := map[*ast.Ident]bool{}
			ast.Inspect(file, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					switch fn := ast.Unparen(call.Fun).(type) {
					case *ast.SelectorExpr:
						called[fn.Sel] = true
					case *ast.Ident:
						called[fn] = true
					}
				}
				return true
			})
			ast.Inspect(file, func(n ast.Node) bool {
				// A method value, a method named but not called here (passed as a function), is
				// listed where it is named.
				if sel, ok := n.(*ast.SelectorExpr); ok && !called[sel.Sel] {
					if f, ok := pkg.TypesInfo.Uses[sel.Sel].(*types.Func); ok && f.Pkg() != nil && (f.Pkg().Path() == natsPkg || f.Pkg().Path() == jsPkg) {
						if sig, ok := f.Type().(*types.Signature); ok && sig.Recv() != nil {
							rel, _ := filepath.Rel(dir, name)
							fmt.Printf("VALUE\t%s:%d\t%s\n", rel, pkg.Fset.Position(sel.Pos()).Line, f.FullName())
						}
					}
				}
				return true
			})
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var id *ast.Ident
				switch fn := ast.Unparen(call.Fun).(type) {
				case *ast.SelectorExpr:
					id = fn.Sel
				case *ast.Ident:
					id = fn
				case *ast.IndexExpr:
					if sel, ok := fn.X.(*ast.SelectorExpr); ok {
						id = sel.Sel
					}
				}
				if id == nil {
					return true
				}
				f, ok := pkg.TypesInfo.Uses[id].(*types.Func)
				if !ok || f.Pkg() == nil || (f.Pkg().Path() != natsPkg && f.Pkg().Path() != jsPkg) {
					return true
				}
				sig, _ := f.Type().(*types.Signature)
				recv := ""
				if sig != nil && sig.Recv() != nil {
					t := sig.Recv().Type()
					if p, ok := t.(*types.Pointer); ok {
						t = p.Elem()
					}
					if named, ok := t.(*types.Named); ok {
						recv = named.Obj().Name()
					}
				}
				rel, _ := filepath.Rel(dir, name)
				pos := fmt.Sprintf("%s:%d", rel, pkg.Fset.Position(call.Pos()).Line)
				pkgName := "nats"
				if f.Pkg().Path() == jsPkg {
					pkgName = "jetstream"
				}
				if recv == "" {
					// A package-level function: an option, a constructor or Connect.
					funcs = append(funcs, row{pos: pos, callee: pkgName + "." + f.Name()})
					return true
				}
				key := recv + "." + f.Name()
				r := row{pos: pos, callee: pkgName + "." + key}
				if local[key] && !passesAckOpt(sig, call) {
					others = append(others, r)
				} else {
					requests = append(requests, r)
				}
				return true
			})
		}
	}
	if files == 0 {
		fmt.Println("UNKNOWN: scanned no non-test file")
		os.Exit(2)
	}
	// Rows sort by position, then by callee, so two calls on one line print in one order whatever
	// else the census found: sort.Slice is not stable.
	sortRows := func(rows []row) {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].pos != rows[j].pos {
				return rows[i].pos < rows[j].pos
			}
			return rows[i].callee < rows[j].callee
		})
	}
	sortRows(requests)
	for _, r := range requests {
		fmt.Printf("REQ\t%s\t%s\n", r.pos, r.callee)
	}
	sortRows(others)
	for _, r := range others {
		fmt.Printf("LOCAL\t%s\t%s\n", r.pos, r.callee)
	}
	sortRows(funcs)
	for _, r := range funcs {
		fmt.Printf("FUNC\t%s\t%s\n", r.pos, r.callee)
	}
	var skips []string
	for path := range skipped {
		skips = append(skips, path)
	}
	sort.Strings(skips)
	for _, path := range skips {
		fmt.Printf("SKIP\t%s\n", path)
	}
	fmt.Fprintf(os.Stderr, "packages %d, left out %d, non-test files %d, request calls %d, local calls %d, function calls %d\n", len(pkgs), len(skips), files, len(requests), len(others), len(funcs))
}

// passesAckOpt reports whether call passes an argument to sig's variadic nats.AckOpt parameter. A
// nats.go ack (Ack, Nak, NakWithDelay, Term, InProgress) sends the ack as a request and waits for the
// reply when it is given a ContextOpt or a positive AckWait, and publishes it otherwise (js.go:3777-3811
// at v1.55.0). Those two are the AckOpt types a caller can pass (js.go:1288, 1318; the third, nakDelay,
// is unexported), so the census counts an ack given any AckOpt as a request.
func passesAckOpt(sig *types.Signature, call *ast.CallExpr) bool {
	if sig == nil || !sig.Variadic() {
		return false
	}
	params := sig.Params()
	slice, ok := params.At(params.Len() - 1).Type().(*types.Slice)
	if !ok {
		return false
	}
	named, ok := slice.Elem().(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != natsPkg || named.Obj().Name() != "AckOpt" {
		return false
	}
	return len(call.Args) >= params.Len()
}

// testOnly is the set of the loaded packages left out of the scan, by package path: every package
// whose non-test files import "testing", or import a package of the set, found by adding importers
// until none is added.
func testOnly(pkgs []*packages.Package) map[string]bool {
	set := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, pkg := range pkgs {
			if set[pkg.PkgPath] {
				continue
			}
			for path := range pkg.Imports {
				if path == "testing" || set[path] {
					set[pkg.PkgPath] = true
					changed = true
					break
				}
			}
		}
	}
	return set
}

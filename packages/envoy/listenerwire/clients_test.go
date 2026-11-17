package listenerwire_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/wirecheck"
)

// The listener's Go clients encode every request body and decode every answer with a type this
// package declares, and copy none, whole or in part: the broker's wake, Dispatch's client, the
// load-test rigs' listener client and the refill rig, and the daemon's publisher and controller
// probe, which is another module and so is read by its own directory. A type with the same
// fields, a map and an anonymous struct are each a finding.
func TestListenerClientsUseListenerwire(t *testing.T) {
	const wire = "github.com/sjawhar/envoy/listenerwire"
	for _, clients := range []struct {
		dir  string
		pkgs []string
	}{
		{dir: "..", pkgs: []string{"./internal/broker/wake", "./internal/dispatch/envoy", "./loadtest/listenerclient", "./internal/testnats/cmd/refillrig"}},
		{dir: "../../daemon", pkgs: []string{"./internal/notify", "./internal/controller"}},
	} {
		for _, finding := range wirecheck.Check(t, clients.dir, clients.pkgs, wire, nil) {
			t.Errorf("%s: %s", clients.dir, finding)
		}
	}
}

// listenerRoutePrefixes begin every route of the listener's HTTP API.
var listenerRoutePrefixes = []string{"/v1/messages", "/v1/interests", "/v1/sessions", "/v1/registry", "/v1/roles", "/v1/legion"}

// The load-test harness's main package also calls Dispatch, NATS and Cloud SQL with bodies of its
// own, so wirecheck cannot read it whole; its calls to the listener go through its listener client,
// which TestListenerClientsUseListenerwire reads. This holds the rest of the package to that: no
// string literal of its source names a listener route, so a call of its own to the listener has no
// route to send to. A package whose source this read finds no file of has read nothing and fails
// as UNKNOWN.
func TestTheLoadTestCallsTheListenerOnlyThroughItsClient(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", "{{.Dir}}{{range .GoFiles}} {{.}}{{end}}", "../loadtest").Output()
	if err != nil {
		t.Fatalf("go list ../loadtest: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		t.Fatalf("UNKNOWN: go list ../loadtest named %q, no directory and Go file to read", out)
	}
	fset := token.NewFileSet()
	for _, name := range fields[1:] {
		file, err := parser.ParseFile(fset, filepath.Join(fields[0], name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			for _, prefix := range listenerRoutePrefixes {
				if strings.HasPrefix(value, prefix) {
					position := fset.Position(literal.Pos())
					t.Errorf("loadtest/%s:%d names the listener route %q: call the listener through loadtest/listenerclient", name, position.Line, value)
				}
			}
			return true
		})
	}
}

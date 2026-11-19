package listenerwire_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
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

// namesListenerRoute reports whether s, a string literal's value, names a route of the listener's
// API, which the listener mounts whole at /v1 of its base URL (cmd/listener/main.go, v1Handler): a
// /v1 path segment at the start of s, after a format verb that stands for the base ("%s/v1/..."),
// or after a URL's scheme and host with no path between ("http://host:port/v1/..."), and the
// "v1" segment a path join takes on its own. A /v1 after a path segment is another API's version
// (Dispatch's /api/v1, the metadata server's /computeMetadata/v1), which the harness also calls.
func namesListenerRoute(s string) bool {
	if s == "v1" || strings.HasPrefix(s, "v1/") {
		return true
	}
	for i := strings.Index(s, "/v1"); i >= 0; {
		end := i + len("/v1")
		if end == len(s) || strings.ContainsRune("/?#", rune(s[end])) {
			if before := s[:i]; before == "" || formatVerbAtEnd.MatchString(before) || baseURLOnly.MatchString(before) {
				return true
			}
		}
		next := strings.Index(s[end:], "/v1")
		if next < 0 {
			break
		}
		i = end + next
	}
	return false
}

var (
	formatVerbAtEnd = regexp.MustCompile(`%[a-z]$`)
	baseURLOnly     = regexp.MustCompile(`^[a-z][a-z0-9+.-]*://[^/]*$`)
)

func TestTheRouteGuardNamesEveryFormOfAListenerRoute(t *testing.T) {
	for value, want := range map[string]bool{
		"/v1":                                true,
		"/v1/":                               true,
		"/v1/sessions":                       true,
		"/v1/a-route-added-later":            true,
		"%s/v1/interests/subscribe":          true,
		"GET %s/v1/sessions answered %d":     true,
		"http://127.0.0.1:8080/v1/sessions":  true,
		"https://listener.example/v1?x=1":    true,
		"v1":                                 true,
		"v1/sessions":                        true,
		"/api/v1/issues":                     false,
		"GET %s/api/v1/projects answered %d": false,
		"http://metadata.example/computeMetadata/v1/instance": false,
		"/v10/sessions": false,
		"v12":           false,
	} {
		if got := namesListenerRoute(value); got != want {
			t.Errorf("namesListenerRoute(%q) = %v, want %v", value, got, want)
		}
	}
}

// The load-test harness's main package also calls Dispatch, NATS and Cloud SQL with bodies of its
// own, so wirecheck cannot read it whole; its calls to the listener go through its listener client,
// which TestListenerClientsUseListenerwire reads. This holds the rest of the package to that: no
// string literal of its source names a listener route in any form namesListenerRoute reads, so a
// call of its own to the listener has no route to send to. It reads literals, so a route built
// from no literal at all, which nothing in the package writes, is outside it. A package whose
// source this read finds no file of has read nothing and fails as UNKNOWN.
func TestTheLoadTestCallsTheListenerOnlyThroughItsClient(t *testing.T) {
	out := goList(t, "./loadtest", "-f", "{{.Dir}}{{range .GoFiles}} {{.}}{{end}}", "../loadtest")
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
			if err == nil && namesListenerRoute(value) {
				position := fset.Position(literal.Pos())
				t.Errorf("loadtest/%s:%d names the listener route %q: call the listener through loadtest/listenerclient", name, position.Line, value)
			}
			return true
		})
	}
}

package textcheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// envoyModule is the module path of packages/envoy, and the prefix of every package path beneath
// it: the infra module's (packages/envoy/infra) among them.
const envoyModule = "github.com/sjawhar/envoy"

// serverFixtures are the packages through which a test starts a NATS server or reaches a Postgres.
var serverFixtures = []string{
	envoyModule + "/internal/testnats",
	envoyModule + "/internal/testpg",
	envoyModule + "/internal/testpg/testdb",
	envoyModule + "/internal/dispatch/store/storetest",
	envoyModule + "/internal/broker/store/storetest",
}

// fixedAddressListenersAllowed are the listeners on a fixed address the check below lets stand,
// each by its file and the function that opens it, with what it serves. None of them is in front
// of a NATS server or a database or in place of one, so no client of NATS or Postgres in another
// test binary dials it; a listener that is goes through testport.Listen instead.
var fixedAddressListenersAllowed = map[string]string{
	"cmd/agent-secrets/chain_test.go closedAddress":                                                                 "a port closed at once: a metadata server that is not there",
	"cmd/agent-secrets/secret_gcp_proxy_test.go TestGCPSecretListReachesSecretManagerThroughTheAirlockOverGRPC":     "a stand-in Secret Manager over gRPC and the airlock in front of it",
	"cmd/agent-secrets/secret_gcp_test.go startFakeSecretManager":                                                   "a stand-in Secret Manager over gRPC",
	"cmd/broker/ordering_test.go TestBootLoadsThePolicyAgainAfterTheFirstListen":                                    "the broker's API and metrics ports, HTTP",
	"cmd/broker/ordering_test.go TestBootBindsOnlyAfterTheCatchUp":                                                  "the broker's API and metrics ports, HTTP",
	"cmd/broker/ordering_test.go TestBootBindsTheAirlockPortAfterTheAPIAndInsideTheBoot":                            "the broker's API, airlock and metrics ports",
	"cmd/broker/ordering_test.go TestBootRefusesWhenTheAirlockPortCannotBeBound":                                    "the broker's API and metrics ports, HTTP",
	"cmd/broker/ordering_test.go TestBootBindsTheMetricsPortAfterTheOtherTwoAndInsideTheBoot":                       "the broker's API, airlock and metrics ports",
	"cmd/broker/ordering_test.go TestBootRefusesWhenTheMetricsPortCannotBeBound":                                    "the broker's API and metrics ports, HTTP",
	"cmd/broker/ordering_test.go TestBootRefusesWithNoMetricsPort":                                                  "the broker's API port, HTTP",
	"cmd/broker/outage_test.go startForwarder":                                                                      "a forwarder to the broker's HTTP API, as a Kubernetes Service is",
	"cmd/broker/requester_rule_e2e_test.go startConnectProxy":                                                       "an HTTP CONNECT proxy to the IAM host",
	"cmd/dispatch/main_test.go TestResolveBootConfigDevSignInChecksTheListenSocket":                                 "sockets Dispatch inspects as its HTTP socket, one on every address by design",
	"cmd/dispatch/pprof_test.go TestPprofUnsetStartsNoListener":                                                     "startPprof with no address, which listens on nothing",
	"cmd/dispatch/pprof_test.go TestPprofServesTheIndexOnLoopbackAndStopsWithTheContext":                            "Dispatch's pprof port, HTTP",
	"cmd/dispatch/pprof_test.go TestPprofPortInUseIsAnError":                                                        "a port held so that Dispatch's pprof bind of 127.0.0.1 fails",
	"cmd/dispatch/settings_test.go TestEverySettingReachesItsReader":                                                "the socket DISPATCH_LISTEN_FD hands Dispatch, HTTP",
	"cmd/dispatch/shutdown_test.go TestShutdownWaitsForAnIntakeHandlerBeforeClosingThePool":                         "Dispatch's HTTP server",
	"cmd/dispatch/shutdown_test.go TestDispatchRefusesAListenFDThatIsNotASocketAndBindsNothing":                     "a port held so that a bind of Dispatch's HTTP port would fail",
	"cmd/listener/shutdown_test.go TestListenerRefusesAnInheritedSocketOnEveryInterfaceWithoutCredentials":          "an inherited HTTP socket on every address, which the listener must refuse",
	"cmd/listener/shutdown_test.go TestListenerRefusesAListenFDThatIsNotASocketAndBindsNothing":                     "a port held so that a bind of the listener's HTTP port would fail",
	"cmd/listener/slowbody_test.go serveWebhook":                                                                    "the listener's webhook server, HTTP",
	"infra/gcp/clusteredhub_live_test.go TestReservedPortTakesAServerAndNoOtherBind":                                "a bind of a reserved port on every address, as the hub's exporter binds it, closed at once",
	"infra/internal/pulumitest/heldport.go HoldPort":                                                                "the HTTP port a listener container serves on",
	"infra/seed/gcpfake_test.go serveGCP":                                                                           "a stand-in Secret Manager over TLS, whose certificate names 127.0.0.1",
	"internal/broker/airlock/airlocktest/airlocktest.go NewService":                                                 "an HTTP service behind the airlock",
	"internal/broker/api/mint_echo_test.go TestAMintFailureEchoingTheRefreshTokenReachesNeitherTheSessionNorTheLog": "a token endpoint, HTTP",
	"internal/cmdtest/cmdtest.go HoldPort":                                                                          "the HTTP port a test hands the Dispatch or listener process it starts",
	"internal/dispatch/docs/schema_admission_test.go sendBufferGrant":                                               "one connection of the test's own, to measure the kernel's send buffer",
}

// fixedAddressListener is one TCP listener a test opens on a fixed address: path and line of the
// literal that names the address (the listen call, or the call that hands the address to a
// function that listens on it), the top-level function that holds it, and the host it names.
type fixedAddressListener struct {
	path     string
	line     int
	function string
	host     string
}

func (l fixedAddressListener) key() string { return l.path + " " + l.function }

func (l fixedAddressListener) String() string {
	return fmt.Sprintf("%s:%d %s: listens on %q", l.path, l.line, l.function, l.host)
}

// goFile is one parsed Go file of a package.
type goFile struct {
	path    string // relative to the walk's root, with slashes
	file    *ast.File
	imports map[string]string // the name this file gives each package it imports, to its path
}

// goPackage is one directory of Go files: the imports of its test files and of its other files,
// and, once parsed in full, its files.
type goPackage struct {
	paths        []string
	tests, code  map[string]bool
	parsed       []*goFile
	inspectTests bool // its test files are inspected
	inspectCode  bool // its other files are inspected
}

// fixedAddressListeners walks the Go packages under root (packages/envoy, by its module path) and
// returns every TCP listener on a fixed address that the tests of a package whose tests start a NATS
// server or a Postgres open: in the package's test files, or in a test helper those test files
// import (a package whose name begins or ends with "test", as testsocks5 and cmdtest do). A fixed
// address is a literal one of 127.0.0.0/8, localhost or ::1, which every test binary built from
// this source shares, or one that names every address (":0", "0.0.0.0"), which a dial of
// 127.0.0.1 reaches too. It sees a literal in the listen call's address, in a local variable or a
// constant of the package it names, and in an argument handed, through any number of the package's
// own functions, to a parameter that ends in a listen call's address: net.Listen, net.ListenTCP,
// tls.Listen and a net.ListenConfig's Listen.
func fixedAddressListeners(root string) ([]fixedAddressListener, error) {
	packages := map[string]*goPackage{}
	fileSet := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == "testdata" || entry.Name() == "node_modules" || entry.Name() == "vendor" || strings.HasPrefix(entry.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		dir, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		importPath := envoyModule
		if dir != "." {
			importPath += "/" + filepath.ToSlash(dir)
		}
		pkg := packages[importPath]
		if pkg == nil {
			pkg = &goPackage{tests: map[string]bool{}, code: map[string]bool{}}
			packages[importPath] = pkg
		}
		pkg.paths = append(pkg.paths, path)
		imports := pkg.code
		if strings.HasSuffix(path, "_test.go") {
			imports = pkg.tests
		}
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			imports[imported] = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}

	// startsServers reports whether a package starts a NATS server or a Postgres, itself or through
	// the packages its non-test files import.
	memo := map[string]bool{}
	var startsServers func(string) bool
	startsServers = func(importPath string) bool {
		if starts, done := memo[importPath]; done {
			return starts
		}
		memo[importPath] = false
		starts := slices.Contains(serverFixtures, importPath)
		if pkg := packages[importPath]; pkg != nil && !starts {
			for imported := range pkg.code {
				if startsServers(imported) {
					starts = true
					break
				}
			}
		}
		memo[importPath] = starts
		return starts
	}
	helper := func(importPath string) bool {
		name := importPath[strings.LastIndex(importPath, "/")+1:]
		return packages[importPath] != nil && (strings.HasPrefix(name, "test") || strings.HasSuffix(name, "test"))
	}
	var helpers []string
	for _, pkg := range packages {
		for imported := range pkg.tests {
			if startsServers(imported) {
				pkg.inspectTests = true
				break
			}
		}
		if !pkg.inspectTests {
			continue
		}
		for imported := range pkg.tests {
			if helper(imported) {
				helpers = append(helpers, imported)
			}
		}
	}
	for len(helpers) > 0 {
		importPath := helpers[len(helpers)-1]
		helpers = helpers[:len(helpers)-1]
		pkg := packages[importPath]
		if pkg.inspectCode {
			continue
		}
		pkg.inspectCode = true
		for imported := range pkg.code {
			if helper(imported) {
				helpers = append(helpers, imported)
			}
		}
	}

	var found []fixedAddressListener
	for _, pkg := range packages {
		if !pkg.inspectTests && !pkg.inspectCode {
			continue
		}
		for _, path := range pkg.paths {
			file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return nil, err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil, err
			}
			parsed := &goFile{path: filepath.ToSlash(rel), file: file, imports: map[string]string{}}
			for _, spec := range file.Imports {
				imported, _ := strconv.Unquote(spec.Path.Value)
				name := imported[strings.LastIndex(imported, "/")+1:]
				if spec.Name != nil {
					name = spec.Name.Name
				}
				parsed.imports[name] = imported
			}
			pkg.parsed = append(pkg.parsed, parsed)
		}
		found = append(found, pkg.listeners(fileSet)...)
	}
	slices.SortFunc(found, func(a, b fixedAddressListener) int {
		return strings.Compare(fmt.Sprintf("%s:%09d", a.path, a.line), fmt.Sprintf("%s:%09d", b.path, b.line))
	})
	return slices.CompactFunc(found, func(a, b fixedAddressListener) bool { return a == b }), nil
}

// inspected reports whether the check reads the listeners file opens.
func (p *goPackage) inspected(file *goFile) bool {
	if strings.HasSuffix(file.path, "_test.go") {
		return p.inspectTests
	}
	return p.inspectCode
}

// listenParameter is a parameter of a function of the package that ends in a listen call's
// address: the function's package name and name, and the parameter's index.
type listenParameter struct {
	pkg, function string
	index         int
}

// listeners returns the fixed-address listeners the package's inspected files open.
func (p *goPackage) listeners(fileSet *token.FileSet) []fixedAddressListener {
	var found []fixedAddressListener
	var pending []listenParameter
	seen := map[listenParameter]bool{}
	note := func(file *goFile, fn *ast.FuncDecl, at ast.Node, expr ast.Expr) {
		host, params := p.fixedHost(file, fn, expr, map[ast.Node]bool{})
		if host != nil && p.inspected(file) {
			found = append(found, fixedAddressListener{path: file.path, line: fileSet.Position(at.Pos()).Line, function: fn.Name.Name, host: *host})
		}
		for _, index := range params {
			parameter := listenParameter{file.file.Name.Name, fn.Name.Name, index}
			if !seen[parameter] {
				seen[parameter] = true
				pending = append(pending, parameter)
			}
		}
	}
	for _, file := range p.parsed {
		for _, decl := range file.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if address := listenAddress(file, call); address != nil {
					note(file, fn, call, address)
				}
				return true
			})
		}
	}
	for len(pending) > 0 {
		parameter := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, file := range p.parsed {
			if file.file.Name.Name != parameter.pkg {
				continue
			}
			for _, decl := range file.file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok || calledName(file, call) != parameter.function || parameter.index >= len(call.Args) {
						return true
					}
					note(file, fn, call.Args[parameter.index], call.Args[parameter.index])
					return true
				})
			}
		}
	}
	return found
}

// calledName is the name of the package's own function or method call calls, or "".
func calledName(file *goFile, call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if x, ok := fun.X.(*ast.Ident); ok {
			if _, imported := file.imports[x.Name]; imported {
				return ""
			}
		}
		return fun.Sel.Name
	}
	return ""
}

// listenAddress is the address argument of call when call opens a TCP listener (net.Listen,
// net.ListenTCP, tls.Listen, or a Listen of three arguments on a value, as net.ListenConfig's),
// or nil.
func listenAddress(file *goFile, call *ast.CallExpr) ast.Expr {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	network, address := -1, -1
	if x, ok := selector.X.(*ast.Ident); ok && file.imports[x.Name] != "" {
		switch file.imports[x.Name] + "." + selector.Sel.Name {
		case "net.Listen", "net.ListenTCP", "crypto/tls.Listen":
			network, address = 0, 1
		}
	} else if selector.Sel.Name == "Listen" && len(call.Args) == 3 {
		network, address = 1, 2
	}
	if address < 0 || address >= len(call.Args) {
		return nil
	}
	if literal, ok := call.Args[network].(*ast.BasicLit); ok {
		if value, err := strconv.Unquote(literal.Value); err == nil && !strings.HasPrefix(value, "tcp") {
			return nil
		}
	}
	return call.Args[address]
}

// fixedHost returns the fixed host expr names, read in fn of file, or nil, and the indexes of fn's
// parameters expr is made from.
func (p *goPackage) fixedHost(file *goFile, fn *ast.FuncDecl, expr ast.Expr, seen map[ast.Node]bool) (*string, []int) {
	var host *string
	var params []int
	take := func(h string) {
		if host == nil {
			host = &h
		}
	}
	ast.Inspect(expr, func(node ast.Node) bool {
		if node == nil || seen[node] {
			return false
		}
		seen[node] = true
		switch n := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.BasicLit:
			if n.Kind == token.STRING {
				if value, err := strconv.Unquote(n.Value); err == nil {
					if h, fixed := fixedListenHost(value); fixed {
						take(h)
					}
				}
			}
		case *ast.CallExpr:
			if selector, ok := n.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "IPv4" && len(n.Args) == 4 && file.importedAs(selector.X, "net") {
				if first, ok := n.Args[0].(*ast.BasicLit); ok && first.Value == "127" {
					take("127.x.x.x (net.IPv4)")
				}
			}
		case *ast.SelectorExpr:
			if file.importedAs(n.X, "net") {
				switch n.Sel.Name {
				case "IPv4zero", "IPv6zero", "IPv6unspecified":
					take("every address (net." + n.Sel.Name + ")")
				case "IPv6loopback":
					take("::1 (net.IPv6loopback)")
				}
			}
			return false
		case *ast.CompositeLit:
			if selector, ok := n.Type.(*ast.SelectorExpr); ok && selector.Sel.Name == "TCPAddr" && file.importedAs(selector.X, "net") {
				hasIP := slices.ContainsFunc(n.Elts, func(element ast.Expr) bool {
					kv, ok := element.(*ast.KeyValueExpr)
					if !ok {
						return true // a positional literal names its IP first
					}
					key, isIdent := kv.Key.(*ast.Ident)
					return isIdent && key.Name == "IP"
				})
				if !hasIP {
					take("every address (a net.TCPAddr with no IP)")
				}
			}
		case *ast.Ident:
			if index := parameterIndex(fn, n.Name); index >= 0 {
				params = append(params, index)
				return false
			}
			for _, value := range p.valuesOf(file, fn, n.Name) {
				h, more := p.fixedHost(file, fn, value, seen)
				if h != nil {
					take(*h)
				}
				params = append(params, more...)
			}
		}
		return true
	})
	return host, params
}

// importedAs reports whether x names the import of path in file.
func (f *goFile) importedAs(x ast.Expr, path string) bool {
	ident, ok := x.(*ast.Ident)
	return ok && f.imports[ident.Name] == path
}

// parameterIndex is the index of fn's parameter name, or -1.
func parameterIndex(fn *ast.FuncDecl, name string) int {
	index := 0
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			index++
			continue
		}
		for _, param := range field.Names {
			if param.Name == name {
				return index
			}
			index++
		}
	}
	return -1
}

// valuesOf is every expression name is given in fn (an assignment or a var declaration) or, when
// fn gives it none, at the top level of a file of the package (a const or a var).
func (p *goPackage) valuesOf(file *goFile, fn *ast.FuncDecl, name string) []ast.Expr {
	var values []ast.Expr
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) == len(n.Rhs) {
				for i, lhs := range n.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok && ident.Name == name {
						values = append(values, n.Rhs[i])
					}
				}
			}
		case *ast.ValueSpec:
			values = append(values, specValues(n, name)...)
		}
		return true
	})
	if len(values) > 0 {
		return values
	}
	for _, other := range p.parsed {
		if other.file.Name.Name != file.file.Name.Name {
			continue
		}
		for _, decl := range other.file.Decls {
			general, ok := decl.(*ast.GenDecl)
			if !ok || (general.Tok != token.CONST && general.Tok != token.VAR) {
				continue
			}
			for _, spec := range general.Specs {
				values = append(values, specValues(spec.(*ast.ValueSpec), name)...)
			}
		}
	}
	return values
}

// specValues is the value spec gives name, if it names it with one.
func specValues(spec *ast.ValueSpec, name string) []ast.Expr {
	for i, ident := range spec.Names {
		if ident.Name == name && i < len(spec.Values) {
			return []ast.Expr{spec.Values[i]}
		}
	}
	return nil
}

// fixedListenHost reports the host value names when it is a fixed address of the loopback (any of
// 127.0.0.0/8, ::1 or localhost) or every address ("", 0.0.0.0, ::), as a whole or as the host of
// host:port.
func fixedListenHost(value string) (string, bool) {
	host := value
	if h, _, err := net.SplitHostPort(value); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return "every address (" + strconv.Quote(value) + ")", true
	}
	if strings.EqualFold(host, "localhost") {
		return host, true
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return host, true
	}
	return "", false
}

// A test in a package whose tests start a NATS server or a Postgres listens on a fixed address only
// where fixedAddressListenersAllowed says why. A client of NATS or Postgres in another test binary,
// left reconnecting to a 127.0.0.1 port its own test let go, reaches whatever listens there next,
// and a test whose relay, proxy or fake server got that port failed for a cause outside the change
// it tests: such a listener opens through testport.Listen, on the test binary's own loopback
// address, which testport's TestAListenerOnThisBinarysLoopbackIsNotReachedByAStrangersDialOf127001
// shows no stranger reaches.
func TestNoTestListensOnAFixedAddressWhereItsTestsStartNATSOrPostgres(t *testing.T) {
	found, err := fixedAddressListeners(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	var refused []string
	for _, listener := range found {
		if _, allowed := fixedAddressListenersAllowed[listener.key()]; allowed {
			used[listener.key()] = true
			continue
		}
		refused = append(refused, listener.String())
	}
	if len(refused) > 0 {
		t.Errorf("%d listeners of tests that start a NATS server or a Postgres open on a fixed address another test binary's client can reach; open each through testport.Listen, or, for one that serves neither, name it in fixedAddressListenersAllowed with what it serves:\n%s",
			len(refused), strings.Join(refused, "\n"))
	}
	for key := range fixedAddressListenersAllowed {
		if !used[key] {
			t.Errorf("fixedAddressListenersAllowed names %s, which opens no listener on a fixed address now: remove it", key)
		}
	}
}

// plantedListeners is a tree of packages under the envoy module's path, for the control below:
// demo's tests start a NATS server and open a listener on a fixed address in each shape the check
// names, and on this binary's own loopback in each it must let pass; testfake is a helper demo's
// tests import, production a package they import that is no helper, quiet a package whose tests
// start no server, and transitive one whose tests start NATS through a helper of their own.
var plantedListeners = map[string]string{
	"internal/testnats/nats.go": "package testnats\n\nfunc URL() string { return \"\" }\n",
	"internal/demo/demo_test.go": `package demo

import (
	"context"
	"crypto/tls"
	"net"
	stdnet "net"

	"github.com/sjawhar/envoy/internal/production"
	"github.com/sjawhar/envoy/internal/testfake"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
)

const fixedAddress = "127.0.0.1:0"

func direct()       { net.Listen("tcp", "127.0.0.1:0") }
func named()        { net.Listen("tcp4", "localhost:0") }
func everyAddress() { net.Listen("tcp", ":0") }

func relayFrom(host string) { net.Listen("tcp", net.JoinHostPort(host, "0")) }
func relay()                { relayFrom("127.0.0.1") }
func outer(host string)     { relayFrom(host) }
func twoDeep()              { outer("::1") }

func local() {
	address := "127.0.0.1:0"
	net.Listen("tcp", address)
}
func constant() { net.Listen("tcp", fixedAddress) }
func config() {
	var lc net.ListenConfig
	lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
}
func secure()  { tls.Listen("tcp", "127.0.0.1:0", nil) }
func aliased() { stdnet.Listen("tcp", "127.0.0.1:0") }
func ip()      { net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}) }
func noIP()    { net.ListenTCP("tcp", &net.TCPAddr{}) }

func own() {
	testport.Listen()
	net.Listen("tcp", net.JoinHostPort(testport.Loopback, "0"))
	relayFrom(testport.Loopback)
	_ = testnats.URL()
}
func unix()        { net.Listen("unix", "/run/demo.sock") }
func dial()        { net.Dial("tcp", "127.0.0.1:1") }
func serves()      { production.Serve() }
func fake()        { testfake.Start() }
`,
	"internal/testfake/fake.go":           "package testfake\n\nimport \"net\"\n\nfunc Start() { net.Listen(\"tcp\", \"127.0.0.1:0\") }\n",
	"internal/production/serve.go":        "package production\n\nimport \"net\"\n\nfunc Serve() { net.Listen(\"tcp\", \"127.0.0.1:0\") }\n",
	"internal/quiet/quiet_test.go":        "package quiet\n\nimport \"net\"\n\nfunc listens() { net.Listen(\"tcp\", \"127.0.0.1:0\") }\n",
	"internal/stacktest/stack.go":         "package stacktest\n\nimport \"github.com/sjawhar/envoy/internal/testnats\"\n\nfunc Start() string { return testnats.URL() }\n",
	"internal/transitive/through_test.go": "package transitive\n\nimport (\n\t\"net\"\n\n\t\"github.com/sjawhar/envoy/internal/stacktest\"\n)\n\nfunc listens() { _ = stacktest.Start(); net.Listen(\"tcp\", \"127.0.0.1:0\") }\n",
}

// The control of the check above: in a planted tree it finds a listener on a fixed address in every
// shape it names, in a test of a package whose tests start a server and in a helper those tests
// import, and nothing on this binary's own loopback, nothing that is no TCP listener, nothing in a
// package that is no helper, and nothing in a package whose tests start no server.
func TestTheListenerCheckFindsEveryShapeItNamesAndNothingElse(t *testing.T) {
	root := t.TempDir()
	for name, contents := range plantedListeners {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found, err := fixedAddressListeners(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, listener := range found {
		got = append(got, listener.key())
	}
	slices.Sort(got)
	want := []string{
		"internal/demo/demo_test.go aliased",
		"internal/demo/demo_test.go config",
		"internal/demo/demo_test.go constant",
		"internal/demo/demo_test.go direct",
		"internal/demo/demo_test.go everyAddress",
		"internal/demo/demo_test.go ip",
		"internal/demo/demo_test.go local",
		"internal/demo/demo_test.go named",
		"internal/demo/demo_test.go noIP",
		"internal/demo/demo_test.go relay",
		"internal/demo/demo_test.go secure",
		"internal/demo/demo_test.go twoDeep",
		"internal/testfake/fake.go Start",
		"internal/transitive/through_test.go listens",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the check found %q in the planted tree, want %q", got, want)
	}
}

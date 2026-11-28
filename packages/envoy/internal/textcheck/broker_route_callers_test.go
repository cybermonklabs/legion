package textcheck

//ci:reads packages/envoy/internal/cipher/api/routes_table.go keySetRoutePaths parses it with go/parser (TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut)
//ci:reads packages/**/*.go !**/*_test.go !packages/**/testdata/** !packages/**/node_modules/** scanPackages walks ../../../../packages, the directories it passes over aside, and parses each .go file but the test files (TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut)
//ci:reads packages/**/*.ts !**/*.test.ts !packages/**/__tests__/** !packages/**/node_modules/** scanPackages reads each .ts file of that walk but the test files (TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut)
//ci:reads packages/**/*.tsx !**/*.test.tsx !packages/**/__tests__/** !packages/**/node_modules/** scanPackages reads each .tsx file of that walk but the test files (TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut)
//ci:reads packages/envoy/internal/cipherclient/*_test.go goTestNames parses the test files of each package the tables name a Go test in (TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut)
//ci:reads packages/daemon/internal/cipher/*_test.go goTestNames parses the test files of each package the tables name a Go test in (TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut)
//ci:reads packages/envoy/cmd/cipher/*_test.go goTestNames parses the test files of each package the tables name a Go test in (TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut)
//ci:reads packages/envoy/internal/ciphertoken/*_test.go goTestNames parses the test files of each package the tables name a Go test in (TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut)
//ci:reads packages/envoy/internal/dispatch/cipher/*_test.go goTestNames parses the test files of each package the tables name a Go test in (TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut)
//ci:reads packages/envoy/internal/dispatch/api/*_test.go goTestNames parses the test files of each package the tables name a Go test in (TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut)
//ci:reads packages/envoy/cmd/listener/*_test.go goTestNames parses the test files of each package the tables name a Go test in (TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut)
//ci:reads packages/envoy/internal/holdrecord/*_test.go goTestNames parses the test files of each package the tables name a Go test in (TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut)
//ci:reads packages/cipher-client/src/__tests__/*.test.ts titledIn reads each TypeScript test file the tables name a test in (TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut)

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The broker answers 503 with Retry-After, never a refusal, when it cannot read the key set a
// token is checked against, and every production client of those routes must wait that
// Retry-After out. The routes are a class, so their clients are one too:
// this test finds every file that names one of the routes' paths and every server that reaches
// one through a client package, and holds each to the tables below, a client with the tests that
// hold it to the wait or a file with why it is no client. A new caller, a row that no longer
// matches anything and a named test that does not exist each fail it.

// repository is the repository's root, from this directory: every path the tables name is from
// it, and the scan reads its packages/.
const repository = "../../../.."

// brokerRoutesTable is the broker's route table, from this directory.
const brokerRoutesTable = "../cipher/api/routes_table.go"

// keySetRouteWrappers are the adapters of the route table whose caller presents an identity token
// the broker checks against an issuer's key set: a row whose handler is a call of one of them is
// a route of the class.
var keySetRouteWrappers = []string{"accountAuth", "serviceAuth", "edgeListenerAuth", "operatorAuth"}

// bodyTokenRoutes are the routes that check a token in the request body, which no adapter marks:
// the token login, the pod enrolment and the renewal with a pod token, the token exchange's
// subject and actor tokens, and the sign-in callback's ID token. The pod-session registration's
// body token is checked on a serviceAuth row, which the table gives.
var bodyTokenRoutes = []string{"/v1/launcher-credentials/token", "/v1/enrollments", "/v1/enrollments/", "/v1/oauth/token", "/signin/callback"}

// passedOverDirs are the directories the scan passes over wherever they are, by name: tests, their
// fixtures and support, dependencies, the end-to-end tests and the check and development scripts.
// A directory named dev-* under a cmd directory is a development command and passed over too, and
// so is any hidden directory.
var passedOverDirs = []string{"node_modules", "__tests__", "test", "testdata", "e2e", "scripts", "ciphertest", "ciphertokentest", "holdrecordtest"}

// brokerServer is the broker's own server, from the repository's root: its packages answer the
// routes, and a path they name is a route they serve or a link they write, not a call.
var brokerServer = []string{
	"packages/envoy/internal/cipher/api",
	"packages/envoy/internal/cipher/oauth",
	"packages/envoy/internal/cipher/oauthstore",
	"packages/envoy/internal/cipher/config",
	"packages/envoy/internal/cipher/machine",
	"packages/envoy/internal/cipher/enroll",
	"packages/envoy/internal/cipher/natshold",
	"packages/envoy/internal/cipher/natscallout",
	"packages/envoy/internal/cipher/projectrepos",
	"packages/envoy/internal/cipher/requests",
	"packages/envoy/internal/cipher/store",
	"packages/envoy/internal/cipher/approval",
	"packages/envoy/internal/cipher/judge",
	"packages/envoy/internal/cipher/wire",
	"packages/envoy/cmd/cipherd",
}

// clientConstructors are the client packages a server reaches a route through without writing its
// path, by import path, each with the name its constructor is reported by.
var clientConstructors = map[string]string{
	"github.com/sjawhar/envoy/internal/ciphertoken":     "ciphertoken.New",
	"github.com/sjawhar/envoy/internal/dispatch/cipher": "internal/dispatch/cipher's New",
}

// namedTest is a test a table row names: a Go test by its package's directory and its function's
// name, or a TypeScript test by its file and its title, each from the repository's root.
type namedTest struct {
	where, name string
}

// goTest is the Go test name in the package at dir.
func goTest(dir, name string) namedTest { return namedTest{where: dir, name: name} }

// tsTest is the TypeScript test titled title in file.
func tsTest(file, title string) namedTest { return namedTest{where: file, name: title} }

// callerRow is a row of the callers' table: files the scan finds naming a path of a route of the
// class, the tests that hold them to the wait, and what they are. A row naming no test is a file
// that is no client of a route of the class, and its why says why.
type callerRow struct {
	files []string
	tests []namedTest
	why   string
}

// routeCallers is the callers' table: every file the scan finds, a client with its tests or a file
// with why it is none.
var routeCallers = []callerRow{
	{
		files: []string{"packages/envoy/internal/cipherclient/brokerclient.go"},
		tests: []namedTest{
			goTest("packages/envoy/internal/cipherclient", "TestARunWaitsOutA503sRetryAfter"),
			goTest("packages/envoy/internal/cipherclient", "TestATokenCallWaitsOutA503sRetryAfterInsideItsContext"),
			goTest("packages/envoy/internal/cipherclient", "TestAnAirlockPlaceholderWaitsOutA503sRetryAfter"),
		},
		why: "client: the Go library every Go trade goes through (TokenPath)",
	},
	{
		files: []string{"packages/daemon/internal/cipher/client.go"},
		tests: []namedTest{
			goTest("packages/daemon/internal/cipher", "TestALoginTheBrokerRateLimitedWaitsItsRetryAfter"),
			goTest("packages/daemon/internal/cipher", "TestAPodSessionRegistrationWaitsOutA503sRetryAfter"),
			goTest("packages/daemon/internal/cipher", "TestAPodEnrolmentWaitsOutA503sRetryAfter"),
			goTest("packages/daemon/internal/cipher", "TestAPodSessionReleaseWaitsOutA503sRetryAfter"),
		},
		why: "client: the daemon's token login, pod enrolment and its renewal, pod-session registration and release",
	},
	{
		files: []string{"packages/envoy/cmd/cipher/pod.go", "packages/envoy/cmd/cipher/client.go"},
		tests: []namedTest{
			goTest("packages/envoy/cmd/cipher", "TestEnrollPodWaitsOutA503sRetryAfter"),
			goTest("packages/envoy/cmd/cipher", "TestRenewWaitsOutA503sRetryAfter"),
			goTest("packages/envoy/cmd/cipher", "TestEnrollPodAgainstTheBrokerWaitsOutAnUnreadableKeySetsRetryAfter"),
		},
		why: "client: agent-secrets enroll --pod (its token login, the pod enrolment and the renewal of a live one) and agent-secrets renew; client.go's proof routes are the same file's",
	},
	{
		files: []string{"packages/envoy/internal/ciphertoken/introspect.go"},
		tests: []namedTest{goTest("packages/envoy/internal/ciphertoken", "TestAnIntrospectionTheBrokerAnswered503CarriesItsRetryAfter")},
		why:   "client: the introspection Dispatch and the listener read a broker token back through (IntrospectPath)",
	},
	{
		files: []string{"packages/envoy/internal/dispatch/cipher/client.go"},
		tests: []namedTest{goTest("packages/envoy/internal/dispatch/cipher", "TestABrokerAnswerKeepsItsRetryAfter")},
		why:   "client: Dispatch's broker client, which relays a person's pages and decisions",
	},
	{
		files: []string{"packages/envoy/cmd/listener/edge_repositories.go"},
		tests: []namedTest{
			goTest("packages/envoy/cmd/listener", "TestAnEdgeRegistrationRelaysTheBrokersRetryAfter"),
			goTest("packages/envoy/cmd/listener", "TestAnEdgeRefreshWaitsOutA503sRetryAfter"),
		},
		why: "client: the edge listener's read of its projects' repositories",
	},
	{
		files: []string{"packages/cipher-client/src/wire.ts"},
		tests: []namedTest{
			tsTest("packages/cipher-client/src/__tests__/retry-loop.test.ts", "Retry-After: a trade answered 503 asking 45 s trades again no sooner than 45 s"),
			tsTest("packages/cipher-client/src/__tests__/retry-loop.test.ts", "Retry-After: a trade answered 503 asking 5 s trades at its own 15 s step"),
			tsTest("packages/cipher-client/src/__tests__/retry-loop.test.ts", "Retry-After: a trade answered 503 asking 90 s waits 60 s"),
			tsTest("packages/cipher-client/src/__tests__/proxy-placeholder.test.ts", "Retry-After: a placeholder answered 503 asking 5 s is traded again after 5 s"),
			tsTest("packages/cipher-client/src/__tests__/proxy-placeholder.test.ts", "Retry-After: with no signal, a broker that answers 503 every time fails within 2 minutes naming the outage"),
			tsTest("packages/cipher-client/src/__tests__/proxy-placeholder.test.ts", "Retry-After: with a signal, the wait ends when the signal does"),
			tsTest("packages/cipher-client/src/__tests__/renewal.test.ts", "Retry-After: a live token in hand is answered at once and the broker is not asked before the wait ends"),
		},
		why: "client: the token endpoint's path BrokerTokenClient trades at (TOKEN_PATH)",
	},
	{
		files: []string{"packages/envoy/internal/holdrecord/client.go"},
		tests: []namedTest{goTest("packages/envoy/internal/holdrecord", "TestTheHoldJournal")},
		why:   "reader of the NATS writer hold's answers; nothing in production imports it",
	},
	{
		files: []string{"packages/envoy/cmd/cipher/login.go"},
		why:   "not a client: it enrols a person's session (kind session) under a launcher proof, with no token in the body",
	},
	{
		files: []string{"packages/envoy/cmd/cipher/chain.go"},
		why:   "not a client: GET /v1/enrollments/self, a proof route",
	},
	{
		files: []string{"packages/envoy/internal/cipher/helper/broker.go"},
		why:   "not a client of these routes: the device-code and refresh grants (oauthConfig), and enrolments and renewals of machine and session kinds under a launcher proof; its token exchange goes through cipherclient",
	},
	{
		files: []string{"packages/envoy/internal/cipher/secrets/anthropicfederation.go"},
		why:   "not a client: Anthropic's token endpoint",
	},
	{
		files: []string{"packages/envoy/cmd/cipher-devrelay/main.go"},
		why:   "not a client of production: the local development stack's stand-in for Dispatch's relay, which decides a machine login by hand without a Dispatch deployment (its package comment)",
	},
}

// constructionRow is a row of the constructions' table: a file that calls a client package's
// constructor, a server relaying a route of the class, and the tests that hold it to the wait.
type constructionRow struct {
	file, constructor string
	tests             []namedTest
	why               string
}

// clientConstructions is the constructions' table: every server the scan finds reaching a route of
// the class through a client package.
var clientConstructions = []constructionRow{
	{
		file: "packages/envoy/cmd/dispatch/main.go", constructor: "ciphertoken.New",
		tests: []namedTest{goTest("packages/envoy/internal/dispatch/api", "TestABrokerTokenTheBrokerCouldNotCheckIsAnswered503WithItsRetryAfter")},
		why:   "Dispatch's introspection of a broker token",
	},
	{
		file: "packages/envoy/cmd/listener/brokerauth.go", constructor: "ciphertoken.New",
		tests: []namedTest{goTest("packages/envoy/cmd/listener", "TestAPIAuthAnswersTheBrokersRetryAfterWhenItCouldNotCheckAToken")},
		why:   "the listener's introspection of a broker token",
	},
	{
		file: "packages/envoy/internal/dispatch/api/server.go", constructor: "internal/dispatch/cipher's New",
		tests: []namedTest{goTest("packages/envoy/internal/dispatch/api", "TestACredentialRouteRelaysTheBrokersRetryAfter")},
		why:   "Dispatch's broker client, which relays the broker's answer to a person's page",
	},
}

// TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut is the inventory of the clients
// of the routes that check a token against a key set. It reads those routes' paths from the
// route table (keySetRoutePaths), adds the routes that check a token in the body, and walks
// packages/: every Go literal and every TypeScript literal naming one of the paths, and every call
// of a client package's constructor, outside the tests, their support and the broker's own server
// (scanPackages). Each file and each call it finds must be on its table, each row of the tables
// must still match what it finds, and each test a row names must exist: a Go test function that
// go/parser reads in the row's package, or a TypeScript test's title in the row's file.
//
// What it cannot see is a client that names no path: one that reaches a route only through
// internal/cipherclient or @legion/cipher-client, whose tests hold the library, or one that takes
// another package's exported path constant.
func TestEveryCallerOfAnUnreadableKeySetRouteIsAClientThatWaitsItOut(t *testing.T) {
	t.Parallel()
	paths := keySetRoutePaths(t)
	scan := scanPackages(t, paths)
	t.Logf("the paths of the routes: %s", strings.Join(paths, " "))
	t.Logf("read %d Go and %d TypeScript files under packages/", scan.goFiles, scan.typeScriptFiles)
	for _, file := range slices.Sorted(maps.Keys(scan.callers)) {
		t.Logf("found %s, lines %v", file, scan.callers[file])
	}
	for _, call := range slices.SortedFunc(maps.Keys(scan.constructions), compareConstructions) {
		t.Logf("found a call of %s in %s, lines %v", call.constructor, call.file, scan.constructions[call])
	}

	tabled := map[string]bool{}
	for _, row := range routeCallers {
		if row.why == "" {
			t.Errorf("the callers' row of %s says nothing of what it is", strings.Join(row.files, ", "))
		}
		for _, file := range row.files {
			tabled[file] = true
			if _, found := scan.callers[file]; !found {
				t.Errorf("the callers' table names %s (%s), which names no path of the routes any more: take it off the table", file, row.why)
			}
		}
	}
	for _, file := range slices.Sorted(maps.Keys(scan.callers)) {
		if !tabled[file] {
			t.Errorf("%s names a path of a route that checks a token against a key set (lines %v), and the callers' table does not name it: add it, a client with the tests that hold it to the 503's Retry-After, or a file with why it is no client", file, scan.callers[file])
		}
	}

	tabledCalls := map[construction]bool{}
	for _, row := range clientConstructions {
		call := construction{file: row.file, constructor: row.constructor}
		tabledCalls[call] = true
		if _, found := scan.constructions[call]; !found {
			t.Errorf("the constructions' table names a call of %s in %s (%s), which the file no longer makes: take it off the table", row.constructor, row.file, row.why)
		}
	}
	for _, call := range slices.SortedFunc(maps.Keys(scan.constructions), compareConstructions) {
		if !tabledCalls[call] {
			t.Errorf("%s calls %s (lines %v), a server reaching a route that checks a token against a key set, and the constructions' table does not name it: add it with the test that holds it to the broker's Retry-After", call.file, call.constructor, scan.constructions[call])
		}
	}

	tests := newTestIndex(t)
	for _, row := range routeCallers {
		for _, test := range row.tests {
			if !tests.has(test) {
				t.Errorf("the callers' row of %s names the test %q in %s, which does not exist (%s)", strings.Join(row.files, " and "), test.name, test.where, row.why)
			}
		}
	}
	for _, row := range clientConstructions {
		for _, test := range row.tests {
			if !tests.has(test) {
				t.Errorf("the call of %s in %s names the test %q in %s, which does not exist (%s)", row.constructor, row.file, test.name, test.where, row.why)
			}
		}
	}
}

// keySetRoutePaths is the path of every route that checks a token against a key set: each row of
// the route table whose handler is a call of one of keySetRouteWrappers, its pattern up to its
// first wildcard segment, so /v1/grants/{id}/revoke-by-approver is /v1/grants/, beside
// bodyTokenRoutes. It fails t when a wrapper serves no row, which would be a table this reading
// no longer understands.
func keySetRoutePaths(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), brokerRoutesTable, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the route table: %v", err)
	}
	paths := slices.Clone(bodyTokenRoutes)
	served := map[string]int{}
	ast.Inspect(file, func(n ast.Node) bool {
		row, ok := n.(*ast.CompositeLit)
		if !ok || len(row.Elts) != 3 {
			return true
		}
		call, ok := row.Elts[2].(*ast.CallExpr)
		if !ok {
			return true
		}
		wrapper, ok := call.Fun.(*ast.Ident)
		if !ok || !slices.Contains(keySetRouteWrappers, wrapper.Name) {
			return true
		}
		literal, ok := row.Elts[1].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			t.Errorf("a row of %s whose handler is %s has no pattern literal", brokerRoutesTable, wrapper.Name)
			return true
		}
		pattern, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Errorf("the pattern %s of %s: %v", literal.Value, brokerRoutesTable, err)
			return true
		}
		if wildcard := strings.Index(pattern, "{"); wildcard >= 0 {
			pattern = pattern[:wildcard]
		}
		paths = append(paths, pattern)
		served[wrapper.Name]++
		return true
	})
	for _, wrapper := range keySetRouteWrappers {
		if served[wrapper] == 0 {
			t.Fatalf("no row of %s is served by %s: the reading of the table's rows no longer matches it", brokerRoutesTable, wrapper)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// construction is one call of a client package's constructor: the file it is in and the
// constructor, as clientConstructors names it.
type construction struct {
	file, constructor string
}

// compareConstructions orders constructions by file, then constructor.
func compareConstructions(a, b construction) int {
	return strings.Compare(a.file+" "+a.constructor, b.file+" "+b.constructor)
}

// routeScan is what scanPackages found: each file naming a path of the routes with the lines that
// name one, each call of a client package's constructor with its lines, and how many files of
// each language it read.
type routeScan struct {
	callers                  map[string][]int
	constructions            map[construction][]int
	goFiles, typeScriptFiles int
}

// scanPackages walks packages/ and reads every file that is no test or test support: each Go file
// through go/parser, where a string literal names a route when namesRoute says so and a call of a
// clientConstructors constructor is a construction, whatever name its import takes; and each .ts
// and .tsx file but a declaration file, where a literal opening with a quote or a backtick names a
// route by the same rule, on a line that is not a comment. It passes over the test files (_test.go,
// .test.ts, .test.tsx), passedOverDirs, a cmd's dev-* directories, hidden directories and
// brokerServer.
func scanPackages(t *testing.T, paths []string) routeScan {
	t.Helper()
	scan := routeScan{callers: map[string][]int{}, constructions: map[construction][]int{}}
	files := token.NewFileSet()
	root := filepath.Join(repository, "packages")
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repository, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		name := entry.Name()
		if entry.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || slices.Contains(passedOverDirs, name) ||
				(path.Base(path.Dir(rel)) == "cmd" && strings.HasPrefix(name, "dev-")) || slices.Contains(brokerServer, rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(name, "_test.go"), strings.HasSuffix(name, ".d.ts"),
			strings.HasSuffix(name, ".test.ts"), strings.HasSuffix(name, ".test.tsx"):
			return nil
		case strings.HasSuffix(name, ".go"):
			scan.goFiles++
			return scan.readGo(files, p, rel, paths)
		case strings.HasSuffix(name, ".ts"), strings.HasSuffix(name, ".tsx"):
			scan.typeScriptFiles++
			return scan.readTypeScript(p, rel, paths)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan packages/: %v", err)
	}
	if scan.goFiles == 0 || scan.typeScriptFiles == 0 {
		t.Fatalf("read %d Go and %d TypeScript files under %s: the walk found none of one language", scan.goFiles, scan.typeScriptFiles, root)
	}
	for file, lines := range scan.callers {
		slices.Sort(lines)
		scan.callers[file] = slices.Compact(lines)
	}
	return scan
}

// readGo records the string literals of the Go file at p that name a route, and its calls of a
// client package's constructor, under rel.
func (scan *routeScan) readGo(files *token.FileSet, p, rel string, paths []string) error {
	file, err := parser.ParseFile(files, p, nil, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	constructors := map[string]string{}
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return fmt.Errorf("%s: import %s: %w", rel, spec.Path.Value, err)
		}
		constructor, ok := clientConstructors[imported]
		if !ok {
			continue
		}
		local := path.Base(imported)
		if spec.Name != nil {
			local = spec.Name.Name
		}
		constructors[local] = constructor
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			if value, err := strconv.Unquote(n.Value); err == nil && namesRoute(value, paths) {
				scan.callers[rel] = append(scan.callers[rel], files.Position(n.Pos()).Line)
			}
		case *ast.CallExpr:
			selector, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "New" {
				return true
			}
			if pkg, ok := selector.X.(*ast.Ident); ok {
				if constructor, ok := constructors[pkg.Name]; ok {
					call := construction{file: rel, constructor: constructor}
					scan.constructions[call] = append(scan.constructions[call], files.Position(n.Pos()).Line)
				}
			}
		}
		return true
	})
	return nil
}

// readTypeScript records the lines of the TypeScript file at p, under rel, on which a literal
// opening with a quote or a backtick names a route. A line whose text starts a comment (//, /*, or
// a block comment's *) is no code.
func (scan *routeScan) readTypeScript(p, rel string, paths []string) error {
	text, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	for number, line := range strings.Split(string(text), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		for i := range len(line) {
			if strings.IndexByte("\"'`", line[i]) >= 0 && namesRoute(line[i+1:], paths) {
				scan.callers[rel] = append(scan.callers[rel], number+1)
				break
			}
		}
	}
	return nil
}

// namesRoute reports whether text, a literal's content from its start, names a route of paths: it
// is one of them, starts with one, or is an http(s)://host address whose path starts with one.
// The path must follow the literal's opening or the host directly, so Dispatch's own /api/v1/...
// routes and the daemon's /legion/v1/... never count.
func namesRoute(text string, paths []string) bool {
	for _, scheme := range []string{"http://", "https://"} {
		if rest, ok := strings.CutPrefix(text, scheme); ok {
			host, after, found := strings.Cut(rest, "/")
			if !found || host == "" || strings.ContainsAny(host, "\"'` \t") {
				return false
			}
			text = "/" + after
			break
		}
	}
	return slices.ContainsFunc(paths, func(p string) bool { return strings.HasPrefix(text, p) })
}

// testIndex answers whether a named test exists, reading each package's test files and each
// TypeScript test file once.
type testIndex struct {
	t       *testing.T
	goTests map[string]map[string]bool
}

func newTestIndex(t *testing.T) *testIndex {
	return &testIndex{t: t, goTests: map[string]map[string]bool{}}
}

// has reports whether test exists: a TypeScript test's title in its file (titledIn), or a Go test
// function of its package (goTestNames).
func (index *testIndex) has(test namedTest) bool {
	if strings.HasSuffix(test.where, ".ts") {
		return titledIn(index.t, test.where, test.name)
	}
	names, read := index.goTests[test.where]
	if !read {
		names = goTestNames(index.t, test.where)
		index.goTests[test.where] = names
	}
	return names[test.name]
}

// goTestNames is the name of each top-level Test function the _test.go files of the package at dir,
// from the repository's root, declare, as go/parser reads them; none for a directory that is not
// there.
func goTestNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(repository, filepath.FromSlash(dir)))
	if err != nil {
		t.Errorf("read the package %s, whose tests a table names: %v", dir, err)
		return names
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, filepath.Join(repository, filepath.FromSlash(dir), entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Errorf("parse %s/%s: %v", dir, entry.Name(), err)
			continue
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				names[fn.Name.Name] = true
			}
		}
	}
	return names
}

// titledIn reports whether file, from the repository's root, holds title as a whole string
// literal: a test's title, written in its call or in its table's row.
func titledIn(t *testing.T, file, title string) bool {
	t.Helper()
	text, err := os.ReadFile(filepath.Join(repository, filepath.FromSlash(file)))
	if err != nil {
		t.Errorf("read %s, whose tests a table names: %v", file, err)
		return false
	}
	return slices.ContainsFunc([]string{`"`, `'`, "`"}, func(quote string) bool {
		return strings.Contains(string(text), quote+title+quote)
	})
}

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// lockSite is whose goroutine calls a function of the mention sender that takes its lock
// (mentionSender.mu), as the sender claim model reads it (cmCaller).
type lockSite uint8

const (
	// lockSiteNone is a function the list does not name: the claim model panics if it takes the
	// lock, since a locked section the model does not know of would run outside every step.
	lockSiteNone lockSite = iota
	// lockSiteLane is a function only a lane goroutine calls (serve), each of whose locked
	// sections the model makes a step of its own.
	lockSiteLane
	// lockSiteRefresh is a function only the refresh calls (keepClaims), the same.
	lockSiteRefresh
	// lockSiteModel is a function only the model's own calls make (enqueue, unpark, wait), which
	// take the lock as they come.
	lockSiteModel
	// lockSiteOutsideTheModel is a function the model never calls: the held gauges' read, which no
	// scrape makes in the model. It panics there, as an unnamed one does.
	lockSiteOutsideTheModel
)

// mentionSenderLockSites is every function of the mention sender that takes its lock, by whose
// goroutine calls it. The claim model reads a lock's caller off the stack by its function's name
// (cmCaller) and parkGate reads park's the same way (lockCaller, parkLockSite), so this list must
// name the sender's functions as its source names them: TestTheLockSiteListNamesTheSendersLocks
// holds it to that source in every run of this package, where the model itself runs only in its
// own job (envoy-go-claim-model).
var mentionSenderLockSites = map[string]lockSite{
	"next": lockSiteLane, "claimKnown": lockSiteLane, "markTrying": lockSiteLane, "claimedDuring": lockSiteLane,
	"park": lockSiteLane, "reportLate": lockSiteLane,
	"refreshHeldClaims": lockSiteRefresh, "rereadUnsure": lockSiteRefresh,
	"enqueue": lockSiteModel, "unpark": lockSiteModel, "wait": lockSiteModel,
	"heldSamples": lockSiteOutsideTheModel,
}

// parkLockSite is the function whose Lock parkGate holds at its gate: park, a lane's.
const parkLockSite = "park"

// The claim model's list of the sender's lock sites (mentionSenderLockSites) names exactly the
// methods of mentionSender that take its lock: no name in it is gone from the package's source,
// and no method that calls its receiver's mu.Lock() is missing from it. A rename of a sender
// method would otherwise make the model panic, and only in the claim model's own job, which the
// full-ci label gates. The control in the same run reads the same source with claimKnown renamed,
// and must find both faults.
func TestTheLockSiteListNamesTheSendersLocks(t *testing.T) {
	sources := map[string][]byte{}
	for _, name := range goFilesOfThisPackage(t) {
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sources[name] = source
	}
	problems := lockSiteProblems(t, sources)
	t.Logf("the list names %d lock sites; problems: %q", len(mentionSenderLockSites), problems)
	if len(problems) != 0 {
		t.Fatalf("the claim model's lock-site list (mentionSenderLockSites) does not match the sender's source:\n%s",
			strings.Join(problems, "\n"))
	}
	if mentionSenderLockSites[parkLockSite] != lockSiteLane {
		t.Fatalf("parkGate holds the Lock of %q, which the list does not name as a lane goroutine's", parkLockSite)
	}

	// Control: the same source with claimKnown renamed claimKnownRenamed, as a rename leaves it.
	const declared = "func (m *mentionSender) claimKnown("
	renamed := map[string][]byte{}
	found := 0
	for name, source := range sources {
		found += strings.Count(string(source), declared)
		renamed[name] = []byte(strings.ReplaceAll(string(source), declared, "func (m *mentionSender) claimKnownRenamed("))
	}
	if found != 1 {
		t.Fatalf("the control finds %q %d times, want once", declared, found)
	}
	planted := lockSiteProblems(t, renamed)
	t.Logf("control, claimKnown renamed: %q", planted)
	if len(planted) != 2 || !strings.Contains(planted[0], `"claimKnown"`) || !strings.Contains(planted[1], `"claimKnownRenamed"`) {
		t.Fatalf("control: with claimKnown renamed the guard reports %q, want the listed name gone and the renamed method unlisted", planted)
	}
}

// lockSiteProblems is what is wrong with mentionSenderLockSites against sources, this package's
// non-test files by name: each name it lists that is no method of mentionSender, then each method
// that calls its receiver's mu.Lock() that it does not list, each set sorted.
func lockSiteProblems(t *testing.T, sources map[string][]byte) []string {
	t.Helper()
	methods, locking := map[string]bool{}, map[string]bool{}
	files := parseSources(t, sources)
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Body == nil {
				continue
			}
			field := fn.Recv.List[0]
			typ := field.Type
			if star, ok := typ.(*ast.StarExpr); ok {
				typ = star.X
			}
			if ident, ok := typ.(*ast.Ident); !ok || ident.Name != "mentionSender" {
				continue
			}
			methods[fn.Name.Name] = true
			if len(field.Names) != 1 {
				continue
			}
			receiver := field.Names[0].Name
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				lock, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || lock.Sel.Name != "Lock" {
					return true
				}
				mu, ok := lock.X.(*ast.SelectorExpr)
				if !ok || mu.Sel.Name != "mu" {
					return true
				}
				if base, ok := mu.X.(*ast.Ident); ok && base.Name == receiver {
					locking[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	if len(methods) == 0 {
		t.Fatal("the source holds no method of mentionSender: the guard read nothing")
	}
	var gone, unlisted []string
	for name := range mentionSenderLockSites {
		if !methods[name] {
			gone = append(gone, `the list names "`+name+`", which is no method of mentionSender`)
		}
	}
	for name := range locking {
		if _, listed := mentionSenderLockSites[name]; !listed {
			unlisted = append(unlisted, `"`+name+`" takes the sender's lock and the list does not name it`)
		}
	}
	slices.Sort(gone)
	slices.Sort(unlisted)
	return append(gone, unlisted...)
}

// parseSources parses sources.
func parseSources(t *testing.T, sources map[string][]byte) []*ast.File {
	t.Helper()
	positions := token.NewFileSet()
	var files []*ast.File
	for name, source := range sources {
		file, err := parser.ParseFile(positions, name, source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}
	return files
}

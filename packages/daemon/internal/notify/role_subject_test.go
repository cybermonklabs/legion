package notify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/subjectforms"
)

// daemonModuleRoot is packages/daemon, the module whose every role topic the census reads, from
// this package's directory.
var daemonModuleRoot = filepath.Join("..", "..")

// notifyImportPath is this package as the daemon's other packages import it.
const notifyImportPath = "github.com/sjawhar/legion/daemon/internal/notify"

// subjectformsImportPath is Envoy's public door onto its subject builders and readers.
const subjectformsImportPath = "github.com/sjawhar/envoy/subjectforms"

// Every role topic the daemon publishes names its project: it is RoleTopic's, which is the role's
// lane under the project its own name says (subjectforms.RoleSubject of subjectforms.RoleProject),
// never the bare old form, the prefix and the role alone. A Legion role token's project is the one
// Envoy's contracts define, so a deployment whose Legion project is not its Dispatch key cannot
// split a role's topic from its claim.
//
// The census reads every non-test Go file of the daemon module and refuses each place that builds
// a role topic by hand: a use of RoleTopicPrefix that is neither a reader (an argument of
// strings.CutPrefix, HasPrefix or TrimPrefix) nor a subscription (a concatenation that ends in the
// wildcard ">"), and every string literal that begins a role lane. That covers publishToRole, the
// executors that publish to a role (deliverNotice, roleMessage, hookPrompt, updateAsk, and
// deliverHeld for mergeQueueRemoval and mergeForwardMessage), the notice re-hold's topic, the
// addressing line a launch is told and the reviewer's topic a review-stuck notice names.
//
// Its control is in the same run: the census applied to a source that builds the bare role topic
// the way every executor did before this rule must name it, and must leave a reader and a
// subscription alone. Negative control, run when this was written: put deliverNotice's publish
// back on notify.RoleTopicPrefix+string(architect); the census fails naming outbox.go and that
// expression.
func TestEveryRoleTopicTheDaemonPublishesNamesItsProject(t *testing.T) {
	t.Run("the topic names the role's own project", func(t *testing.T) {
		for _, tc := range []struct{ role, want, project string }{
			{"legion-legion-legion-208-architect", "notifications.role.LEGION.legion-legion-legion-208-architect", "LEGION"},
			// A deployment whose Legion project is acme works issues of another Dispatch project:
			// the topic takes the token's project, the one its claim is keyed by.
			{"legion-acme-legion-41-reviewer", "notifications.role.ACME.legion-acme-legion-41-reviewer", "ACME"},
			{"legion-legion-controller", "notifications.role.LEGION.legion-legion-controller", "LEGION"},
			// A token whose upper case is longer than a project key works for no project.
			{"legion-acmewidgetsinc-legion-1-architect", "notifications.role._.legion-acmewidgetsinc-legion-1-architect", "_"},
			{"merge-queue", "notifications.role._.merge-queue", "_"},
		} {
			got := RoleTopic(tc.role)
			if got != tc.want {
				t.Errorf("RoleTopic(%q) = %q, want %q", tc.role, got, tc.want)
				continue
			}
			lane, _ := strings.CutPrefix(got, RoleTopicPrefix)
			if project, name, legacy := subjectforms.CanonicalRole(lane); project != tc.project || name != tc.role || legacy {
				t.Errorf("the listener reads %q as project %q, role %q, legacy %v; want %q, %q, false", got, project, name, legacy, tc.project, tc.role)
			}
		}
	})

	t.Run("no file builds a bare role topic", func(t *testing.T) {
		var findings []string
		files, roleTopicCalls := 0, map[string]int{}
		err := filepath.WalkDir(daemonModuleRoot, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if name := entry.Name(); name == "testdata" || name == "node_modules" || (strings.HasPrefix(name, ".") && path != daemonModuleRoot) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			files++
			rel, err := filepath.Rel(daemonModuleRoot, path)
			if err != nil {
				return err
			}
			census := roleTopicCensus(fset, file, filepath.ToSlash(rel))
			findings = append(findings, census.bare...)
			roleTopicCalls[filepath.ToSlash(rel)] += census.roleTopicCalls
			return nil
		})
		if err != nil {
			t.Fatalf("read the daemon module's sources: %v", err)
		}
		if len(findings) != 0 {
			t.Errorf("a role topic is built bare, with no project, in %d places; build it with notify.RoleTopic:\n%s", len(findings), strings.Join(findings, "\n"))
		}
		// A census that read nothing, or could not see the executors' own calls, has not passed: the
		// files that publish to a role must each show RoleTopic, or the walk is blind.
		for _, publisher := range []string{"internal/daemon/outbox.go", "internal/daemon/merge_queue_removal.go"} {
			if roleTopicCalls[publisher] == 0 {
				t.Errorf("UNKNOWN: the census read %d files and saw no RoleTopic call in %s, which publishes to a role", files, publisher)
			}
		}
	})

	t.Run("control: the census names a bare role topic and leaves readers alone", func(t *testing.T) {
		const source = `package daemon

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/notify"
)

const exceptions = "notifications.envoy.exceptions." + notify.RoleTopicPrefix + ">"

func (r *outbox) deliver(ctx context.Context, architect claim.Token, topic string) error {
	if _, ok := strings.CutPrefix(topic, notify.RoleTopicPrefix); ok {
		return nil
	}
	_ = fmt.Sprintf("%s%s", notify.RoleTopicPrefix, architect)
	_ = r.publish(ctx, "notifications.role.merge-queue", "m", nil, "k")
	_ = r.publish(ctx, notify.RoleTopic(string(architect)), "m", nil, "k")
	return r.publish(ctx, notify.RoleTopicPrefix+string(architect), "m", nil, "k")
}
`
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "control.go", source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse the control's source: %v", err)
		}
		census := roleTopicCensus(fset, file, "control.go")
		want := []string{
			`control.go:16: fmt.Sprintf("%s%s", notify.RoleTopicPrefix, architect)`,
			`control.go:17: "notifications.role.merge-queue"`,
			`control.go:19: notify.RoleTopicPrefix + string(architect)`,
		}
		if !slices.Equal(census.bare, want) || census.roleTopicCalls != 1 {
			t.Fatalf("the census of the control found %q and %d RoleTopic calls, want %q and 1", census.bare, census.roleTopicCalls, want)
		}
	})
}

// roleTopicReading is what the census reads of one file: each place that builds a role topic
// bare, as "<path>:<line>: <expression>", and how many times the file calls RoleTopic.
type roleTopicReading struct {
	bare           []string
	roleTopicCalls int
}

// roleTopicCensus reads one parsed file for the role topics it builds (the test above says what
// it refuses), path naming the file in what it reports.
func roleTopicCensus(fset *token.FileSet, file *ast.File, path string) roleTopicReading {
	notifyName, subjectformsName := importName(file, notifyImportPath), importName(file, subjectformsImportPath)
	inNotify := file.Name.Name == "notify"
	// stack holds the ancestors of the node the inspection is at.
	var stack []ast.Node
	// isPrefix is whether expr names the role lanes' prefix: notify's, or subjectforms' own. An
	// identifier that is a selector's part (subjectforms.RoleTopicPrefix's last) is read with its
	// selector, never on its own.
	isPrefix := func(expr ast.Expr) bool {
		switch e := expr.(type) {
		case *ast.Ident:
			if len(stack) > 0 {
				if _, partOfSelector := stack[len(stack)-1].(*ast.SelectorExpr); partOfSelector {
					return false
				}
			}
			return inNotify && e.Name == "RoleTopicPrefix"
		case *ast.SelectorExpr:
			x, ok := e.X.(*ast.Ident)
			return ok && e.Sel.Name == "RoleTopicPrefix" && ((notifyName != "" && x.Name == notifyName) || (subjectformsName != "" && x.Name == subjectformsName))
		}
		return false
	}
	isRoleTopic := func(call *ast.CallExpr) bool {
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			return inNotify && fun.Name == "RoleTopic"
		case *ast.SelectorExpr:
			x, ok := fun.X.(*ast.Ident)
			return ok && fun.Sel.Name == "RoleTopic" && notifyName != "" && x.Name == notifyName
		}
		return false
	}

	var reading roleTopicReading
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if call, ok := n.(*ast.CallExpr); ok && isRoleTopic(call) {
			reading.roleTopicCalls++
		}
		expr, isExpr := n.(ast.Expr)
		if isExpr && (isPrefix(expr) || isRoleLane(expr)) && !isPrefixDeclaration(stack, inNotify) && !isReader(stack, expr) {
			if built, subscription := builtFrom(stack, expr); !subscription {
				reading.bare = append(reading.bare, path+":"+strconv.Itoa(fset.Position(expr.Pos()).Line)+": "+types.ExprString(built))
			}
		}
		stack = append(stack, n)
		return true
	})
	return reading
}

// importName is the name file refers to the package at path by, "" when it does not import it.
func importName(file *ast.File, path string) string {
	for _, spec := range file.Imports {
		if imported, err := strconv.Unquote(spec.Path.Value); err != nil || imported != path {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}

// isRoleLane is whether expr is a string literal that begins a role lane's name.
func isRoleLane(expr ast.Expr) bool {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(literal.Value)
	return err == nil && strings.HasPrefix(value, RoleTopicPrefix)
}

// isPrefixDeclaration is whether the expression on top of stack is the value notify's own
// RoleTopicPrefix is declared as.
func isPrefixDeclaration(stack []ast.Node, inNotify bool) bool {
	if !inNotify || len(stack) == 0 {
		return false
	}
	spec, ok := stack[len(stack)-1].(*ast.ValueSpec)
	return ok && slices.ContainsFunc(spec.Names, func(name *ast.Ident) bool { return name.Name == "RoleTopicPrefix" })
}

// readers are the calls of package strings a role lane's prefix is read through.
var readers = []string{"CutPrefix", "HasPrefix", "TrimPrefix"}

// isReader is whether expr is an argument of a reader, strings.CutPrefix, HasPrefix or
// TrimPrefix, stack holding its ancestors.
func isReader(stack []ast.Node, expr ast.Expr) bool {
	for i := len(stack) - 1; i >= 0; i-- {
		switch parent := stack[i].(type) {
		case *ast.ParenExpr:
			continue
		case *ast.CallExpr:
			fun, ok := parent.Fun.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			x, ok := fun.X.(*ast.Ident)
			return ok && x.Name == "strings" && slices.Contains(readers, fun.Sel.Name) && slices.ContainsFunc(parent.Args, func(arg ast.Expr) bool { return arg.Pos() <= expr.Pos() && expr.End() <= arg.End() })
		default:
			return false
		}
	}
	return false
}

// builtFrom is the whole expression expr builds a name in, stack holding its ancestors: the
// concatenation it is part of, else the fmt call that formats it, else expr itself; and whether
// that concatenation ends in the wildcard ">", which names a subscription, not a topic.
func builtFrom(stack []ast.Node, expr ast.Expr) (ast.Expr, bool) {
	built := expr
	for i := len(stack) - 1; i >= 0; i-- {
		parent, ok := stack[i].(ast.Expr)
		if !ok {
			break
		}
		if binary, isBinary := parent.(*ast.BinaryExpr); isBinary && binary.Op == token.ADD {
			built = binary
			continue
		}
		if _, isParen := parent.(*ast.ParenExpr); isParen {
			built = parent
			continue
		}
		if call, isCall := parent.(*ast.CallExpr); isCall && built == expr {
			if fun, isSelector := call.Fun.(*ast.SelectorExpr); isSelector {
				if x, isIdent := fun.X.(*ast.Ident); isIdent && x.Name == "fmt" {
					return call, false
				}
			}
		}
		break
	}
	if binary, ok := built.(*ast.BinaryExpr); ok {
		last := binary.Y
		for {
			if paren, isParen := last.(*ast.ParenExpr); isParen {
				last = paren.X
				continue
			}
			if inner, isBinary := last.(*ast.BinaryExpr); isBinary && inner.Op == token.ADD {
				last = inner.Y
				continue
			}
			break
		}
		if literal, isLiteral := last.(*ast.BasicLit); isLiteral && literal.Kind == token.STRING {
			value, err := strconv.Unquote(literal.Value)
			return built, err == nil && strings.HasSuffix(value, ">")
		}
	}
	return built, false
}

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

// roleLaneStem is what every role lane's name begins with: the prefix without its last dot, so a
// name joined from the prefix's words is read as the prefix it spells.
var roleLaneStem = strings.TrimSuffix(RoleTopicPrefix, ".")

// unknownText stands, in the text the census reads a name as, for each part only a run would know.
const unknownText = "\x00"

// Every role topic the daemon publishes names its project: it is RoleTopic's, which is the role's
// lane under the project its own name says (subjectforms.RoleSubject of subjectforms.RoleProject),
// never the bare old form, the prefix and the role alone, and never a lane under a project chosen
// anywhere else. A Legion role token's project is the one Envoy's contracts define, so a
// deployment whose Legion project is not its Dispatch key cannot split a role's topic from its
// claim.
//
// The census reads every non-test Go file of the daemon module and refuses each place that builds
// a role topic by hand, not by RoleTopic:
//   - a use of subjectforms.RoleSubject anywhere but in RoleTopic's own body, since any other use
//     picks the project itself and can name one the role's claim is not keyed by; a call whose
//     role is the wildcard "*" or ">" names a subscription, which no publish can take, and is
//     left alone;
//   - a name that begins a role lane, read as far as its source spells it out: a string literal,
//     the prefix, and a concatenation, a strings.Join or a fmt formatting of those, so the prefix
//     joined from its words is read as the prefix. A use of the prefix as a reader (an argument of
//     strings.CutPrefix, HasPrefix or TrimPrefix), a subscription (a name that ends in the
//     wildcard ">") and notify's own declaration of RoleTopicPrefix build no topic.
//
// That covers publishToRole, the executors that publish to a role (deliverNotice, roleMessage,
// hookPrompt, updateAsk, and deliverHeld for mergeQueueRemoval and mergeForwardMessage), the
// notice re-hold's topic, the addressing line a launch is told and the reviewer's topic a
// review-stuck notice names. A name put together across statements, or through a variable, is
// beyond what the census reads; the executors' own tests compare each topic with RoleTopic's.
//
// Its control is in the same run: the census applied to sources that build a role topic by hand
// in each of those ways must name each one, a RoleSubject call whose role is a literal that is no
// wildcard among them, and must leave a reader, a subscription (by the prefix or by a wildcard
// role), a join that begins no role lane and RoleTopic's own body alone. Negative controls, run
// when this was written, each failing naming its file and expression: deliverNotice's publish put
// back on notify.RoleTopicPrefix+string(architect); publishToRole's on
// strings.Join([]string{"notifications", "role", bare}, "."); and roleMessage's and deliverNotice's
// on subjectforms.RoleSubject(r.dispatchProject, …), a lane under the wrong project.
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

	t.Run("no file builds a role topic by hand", func(t *testing.T) {
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
			findings = append(findings, census.byHand...)
			roleTopicCalls[filepath.ToSlash(rel)] += census.roleTopicCalls
			return nil
		})
		if err != nil {
			t.Fatalf("read the daemon module's sources: %v", err)
		}
		if len(findings) != 0 {
			t.Errorf("a role topic is built by hand, not by notify.RoleTopic, in %d places; build it with notify.RoleTopic:\n%s", len(findings), strings.Join(findings, "\n"))
		}
		// A census that read nothing, or could not see the executors' own calls, has not passed: the
		// files that publish to a role must each show RoleTopic, or the walk is blind.
		for _, publisher := range []string{"internal/daemon/outbox.go", "internal/daemon/merge_queue_removal.go"} {
			if roleTopicCalls[publisher] == 0 {
				t.Errorf("UNKNOWN: the census read %d files and saw no RoleTopic call in %s, which publishes to a role", files, publisher)
			}
		}
	})

	t.Run("control: the census names each role topic built by hand and leaves readers alone", func(t *testing.T) {
		for _, control := range []struct {
			path, source   string
			want           []string
			roleTopicCalls int
		}{
			{
				path: "control.go",
				source: `package daemon

import (
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/subjectforms"
	"github.com/sjawhar/legion/daemon/internal/notify"
)

const exceptions = "notifications.envoy.exceptions." + notify.RoleTopicPrefix + ">"

func (r *outbox) deliver(ctx context.Context, architect claim.Token, topic string) error {
	if _, ok := strings.CutPrefix(topic, notify.RoleTopicPrefix); ok {
		return nil
	}
	r.log.Info("deliver", "role", architect, "lane", strings.Join([]string{"notifications", "agent", "s"}, "."))
	_ = fmt.Sprintf("%s%s", notify.RoleTopicPrefix, architect)
	_ = r.publish(ctx, "notifications.role.merge-queue", "m", nil, "k")
	_ = r.publish(ctx, strings.Join([]string{"notifications", "role", string(architect)}, "."), "m", nil, "k")
	_ = r.publish(ctx, "notifications."+"role."+string(architect), "m", nil, "k")
	_ = r.publish(ctx, fmt.Sprintf("%s.%s.%s", "notifications", "role", architect), "m", nil, "k")
	_ = r.publish(ctx, subjectforms.RoleSubject(r.dispatchProject, string(architect)), "m", nil, "k")
	_ = r.publish(ctx, subjectforms.RoleSubject(r.dispatchProject, "merge-queue"), "m", nil, "k")
	_ = r.subscribe(ctx, "notifications.envoy.exceptions."+subjectforms.RoleSubject(r.dispatchProject, "*"))
	_ = r.subscribe(ctx, subjectforms.RoleSubject(r.dispatchProject, (">")))
	_ = r.publish(ctx, notify.RoleTopic(string(architect)), "m", nil, "k")
	return r.publish(ctx, notify.RoleTopicPrefix+string(architect), "m", nil, "k")
}
`,
				want: []string{
					`control.go:18: fmt.Sprintf("%s%s", notify.RoleTopicPrefix, architect)`,
					`control.go:19: "notifications.role.merge-queue"`,
					`control.go:20: strings.Join([]string{…}, ".")`,
					`control.go:21: "notifications." + "role." + string(architect)`,
					`control.go:22: fmt.Sprintf("%s.%s.%s", "notifications", "role", architect)`,
					`control.go:23: subjectforms.RoleSubject(r.dispatchProject, string(architect))`,
					`control.go:24: subjectforms.RoleSubject(r.dispatchProject, "merge-queue")`,
					`control.go:28: notify.RoleTopicPrefix + string(architect)`,
				},
				roleTopicCalls: 1,
			},
			{
				// In notify itself RoleTopic's own body is the one place the builder is called.
				path: "notify.go",
				source: `package notify

import "github.com/sjawhar/envoy/subjectforms"

const RoleTopicPrefix = subjectforms.RoleTopicPrefix

func RoleTopic(role string) string {
	return subjectforms.RoleSubject(subjectforms.RoleProject(role), role)
}

func architectTopic(project, role string) string {
	return subjectforms.RoleSubject(project, role)
}

func controllerTopic(role string) string {
	return RoleTopicPrefix + role
}
`,
				want: []string{
					`notify.go:12: subjectforms.RoleSubject(project, role)`,
					`notify.go:16: RoleTopicPrefix + role`,
				},
			},
		} {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, control.path, control.source, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse the control %s: %v", control.path, err)
			}
			census := roleTopicCensus(fset, file, control.path)
			if !slices.Equal(census.byHand, control.want) || census.roleTopicCalls != control.roleTopicCalls {
				t.Errorf("the census of the control %s found %q and %d RoleTopic calls, want %q and %d",
					control.path, census.byHand, census.roleTopicCalls, control.want, control.roleTopicCalls)
			}
		}
	})
}

// roleTopicReading is what the census reads of one file: each place that builds a role topic by
// hand, as "<path>:<line>: <expression>", and how many times the file calls RoleTopic.
type roleTopicReading struct {
	byHand         []string
	roleTopicCalls int
}

// roleTopicCensus reads one parsed file for the role topics it builds by hand (the test above says
// what it refuses), path naming the file in what it reports.
func roleTopicCensus(fset *token.FileSet, file *ast.File, path string) roleTopicReading {
	notifyName, subjectformsName := importName(file, notifyImportPath), importName(file, subjectformsImportPath)
	inNotify := file.Name.Name == "notify"
	// stack holds the ancestors of the node the inspection is at.
	var stack []ast.Node
	// namesPrefix is whether expr names the role lanes' prefix: notify's, or subjectforms' own.
	namesPrefix := func(expr ast.Expr) bool {
		switch e := expr.(type) {
		case *ast.Ident:
			return inNotify && e.Name == "RoleTopicPrefix"
		case *ast.SelectorExpr:
			x, ok := e.X.(*ast.Ident)
			return ok && e.Sel.Name == "RoleTopicPrefix" && ((notifyName != "" && x.Name == notifyName) || (subjectformsName != "" && x.Name == subjectformsName))
		}
		return false
	}
	// isPrefix is namesPrefix for the node the inspection is at. An identifier that is a
	// selector's part (subjectforms.RoleTopicPrefix's last) is read with its selector, never on
	// its own.
	isPrefix := func(expr ast.Expr) bool {
		if _, isIdent := expr.(*ast.Ident); isIdent && len(stack) > 0 {
			if _, partOfSelector := stack[len(stack)-1].(*ast.SelectorExpr); partOfSelector {
				return false
			}
		}
		return namesPrefix(expr)
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
	isRoleSubject := func(expr ast.Expr) bool {
		selector, ok := expr.(*ast.SelectorExpr)
		if !ok || subjectformsName == "" {
			return false
		}
		x, ok := selector.X.(*ast.Ident)
		return ok && x.Name == subjectformsName && selector.Sel.Name == "RoleSubject"
	}
	// inRoleTopic is whether the inspection is inside notify's own RoleTopic, the one place the
	// builder is called.
	inRoleTopic := func() bool {
		return inNotify && slices.ContainsFunc(stack, func(n ast.Node) bool {
			decl, ok := n.(*ast.FuncDecl)
			return ok && decl.Recv == nil && decl.Name.Name == "RoleTopic"
		})
	}
	// text is what expr builds as far as its source spells it out, unknownText standing for each
	// part only a run would know: a string literal, the prefix, and a concatenation, a
	// strings.Join of a listed slice or a fmt.Sprintf or Errorf of a literal format, of those.
	var text func(ast.Expr) string
	text = func(expr ast.Expr) string {
		switch e := expr.(type) {
		case *ast.BasicLit:
			if value, err := strconv.Unquote(e.Value); err == nil && e.Kind == token.STRING {
				return value
			}
		case *ast.ParenExpr:
			return text(e.X)
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				return text(e.X) + text(e.Y)
			}
		case *ast.CallExpr:
			if parts, ok := joinedParts(e); ok {
				read := make([]string, len(parts.Elts))
				for i, part := range parts.Elts {
					read[i] = text(part)
				}
				return strings.Join(read, text(e.Args[1]))
			}
			if (isCall(e, "fmt", "Sprintf") || isCall(e, "fmt", "Errorf")) && len(e.Args) > 0 {
				args := make([]string, len(e.Args)-1)
				for i, arg := range e.Args[1:] {
					args[i] = text(arg)
				}
				return formatted(text(e.Args[0]), args)
			}
		}
		if namesPrefix(expr) {
			return RoleTopicPrefix
		}
		return unknownText
	}

	var reading roleTopicReading
	reported := map[ast.Expr]bool{}
	report := func(built ast.Expr) {
		if !reported[built] {
			reported[built] = true
			reading.byHand = append(reading.byHand, path+":"+strconv.Itoa(fset.Position(built.Pos()).Line)+": "+types.ExprString(built))
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if call, ok := n.(*ast.CallExpr); ok && isRoleTopic(call) {
			reading.roleTopicCalls++
		}
		expr, isExpr := n.(ast.Expr)
		switch {
		case !isExpr:
		case isRoleSubject(expr):
			if inRoleTopic() {
				break
			}
			built := expr
			if call, ok := stack[len(stack)-1].(*ast.CallExpr); ok && call.Fun == expr {
				// A wildcard role builds a subscription to the project's role lanes, not a topic.
				if len(call.Args) == 2 && slices.Contains([]string{"*", ">"}, text(call.Args[1])) {
					break
				}
				built = call
			}
			report(built)
		case isPrefix(expr) || isStringLiteral(expr):
			if isPrefixDeclaration(stack, inNotify) || isReader(stack, expr) {
				break
			}
			built := builtFrom(stack, expr)
			name := text(built)
			if (strings.HasPrefix(text(expr), roleLaneStem) || strings.HasPrefix(name, roleLaneStem)) && !strings.HasSuffix(name, ">") {
				report(built)
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

// isStringLiteral is whether expr is a string literal.
func isStringLiteral(expr ast.Expr) bool {
	literal, ok := expr.(*ast.BasicLit)
	return ok && literal.Kind == token.STRING
}

// isCall is whether call calls the function name of the package its source names pkg.
func isCall(call *ast.CallExpr, pkg, name string) bool {
	fun, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := fun.X.(*ast.Ident)
	return ok && x.Name == pkg && fun.Sel.Name == name
}

// isFmtCall is whether call calls a function of package fmt.
func isFmtCall(call *ast.CallExpr) bool {
	fun, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := fun.X.(*ast.Ident)
	return ok && x.Name == "fmt"
}

// joinedParts is the listed slice call joins, when call is strings.Join of one and a separator.
func joinedParts(call *ast.CallExpr) (*ast.CompositeLit, bool) {
	if !isCall(call, "strings", "Join") || len(call.Args) != 2 {
		return nil, false
	}
	parts, ok := call.Args[0].(*ast.CompositeLit)
	return parts, ok
}

// formatted is format with each verb replaced by its argument's text: a %s or %v by the text, a
// %q by the text quoted, a %% by a percent sign, and any other verb, or one past the arguments, by
// unknownText. A verb that takes its width, precision or argument from the arguments makes the
// rest of the text unknown, since which argument each later verb takes is then a run's to know.
func formatted(format string, args []string) string {
	if strings.Contains(format, unknownText) {
		return unknownText
	}
	var out strings.Builder
	next := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			out.WriteByte(format[i])
			continue
		}
		j := i + 1
		for j < len(format) && strings.IndexByte("+-# 0123456789.", format[j]) >= 0 {
			j++
		}
		if j == len(format) || format[j] == '*' || format[j] == '[' {
			out.WriteString(unknownText)
			return out.String()
		}
		i = j
		switch verb := format[j]; {
		case verb == '%':
			out.WriteByte('%')
			continue
		case next == len(args):
			out.WriteString(unknownText)
		case verb == 's' || verb == 'v':
			out.WriteString(args[next])
		case verb == 'q':
			out.WriteString(`"` + args[next] + `"`)
		default:
			out.WriteString(unknownText)
		}
		next++
	}
	return out.String()
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

// builtFrom is the whole expression expr builds a name in, stack holding its ancestors: it climbs
// from expr through each concatenation, parenthesis, fmt call and strings.Join of a listed slice
// that holds it, and stops at anything else, which takes the name as built.
func builtFrom(stack []ast.Node, expr ast.Expr) ast.Expr {
	built := expr
	for i := len(stack) - 1; i >= 0; i-- {
		switch parent := stack[i].(type) {
		case *ast.BinaryExpr:
			if parent.Op != token.ADD {
				return built
			}
			built = parent
		case *ast.ParenExpr:
			built = parent
		case *ast.CompositeLit:
			// A part of the slice strings.Join joins builds the join, the call just above.
			if i == 0 {
				return built
			}
			call, overCall := stack[i-1].(*ast.CallExpr)
			if !overCall {
				return built
			}
			if parts, joined := joinedParts(call); !joined || parts != parent {
				return built
			}
		case *ast.CallExpr:
			if _, joined := joinedParts(parent); !joined && !isFmtCall(parent) {
				return built
			}
			built = parent
		default:
			return built
		}
	}
	return built
}

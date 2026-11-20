// Command guarded-url-check verifies the small set of reviewed escapes from URL setting values.
// It loads one package at a time. Its target package's syntax is available to inspect, while
// go/packages obtains imported types from export data rather than retaining every module's syntax.
// Every package is inspected, including callers that receive a guarded value through another
// package. A Raw method value is an escape even when its eventual invocation is elsewhere.
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

const (
	guardedPackage = "github.com/sjawhar/envoy/urlsetting"
	guardedType    = "Value"
	guardedRaw     = "Raw"
)

type judgment struct {
	Package     string
	Function    string
	Destination string
	Reason      string
}

var judged = []judgment{
	{
		Package:     "github.com/sjawhar/envoy/internal/broker/wake",
		Function:    "(wake.Envoy).Notify",
		Destination: "requestURL",
		Reason:      "builds the listener request URL; its transport error is passed to redact.RequestError",
	},
}

type finding struct {
	Position token.Position
	Message  string
}

func (f finding) String() string {
	return fmt.Sprintf("%s:%d: %s", f.Position.Filename, f.Position.Line, f.Message)
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: guarded-url-check <repository root>")
		os.Exit(2)
	}
	findings, err := run(os.Args[1], judged)
	if err != nil {
		fmt.Fprintln(os.Stderr, "guarded-url-check: UNKNOWN:", err)
		os.Exit(2)
	}
	for _, finding := range findings {
		fmt.Println(finding)
	}
	if len(findings) != 0 {
		os.Exit(1)
	}
}

func run(root string, judgments []judgment) ([]finding, error) {
	if err := validateJudgments(judgments); err != nil {
		return nil, err
	}
	modules, err := goModules(root)
	if err != nil {
		return nil, err
	}
	var findings []finding
	uses := make(map[int]bool, len(judgments))
	for _, module := range modules {
		packagesInModule, err := listedPackages(module)
		if err != nil {
			return nil, err
		}
		for _, packagePath := range packagesInModule {
			checked, used, err := checkPackage(module, packagePath, judgments)
			if err != nil {
				return nil, err
			}
			findings = append(findings, checked...)
			for index := range used {
				uses[index] = true
			}
		}
	}
	for index, judgment := range judgments {
		if !uses[index] {
			findings = append(findings, finding{Message: fmt.Sprintf("judged raw extraction %s %s -> %s no longer exists", judgment.Package, judgment.Function, judgment.Destination)})
		}
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].String() < findings[j].String() })
	return findings, nil
}

func validateJudgments(judgments []judgment) error {
	seen := make(map[string]bool, len(judgments))
	for _, judgment := range judgments {
		if judgment.Package == "" || judgment.Function == "" || judgment.Destination == "" || judgment.Reason == "" {
			return fmt.Errorf("each judged raw extraction needs package, function, destination and reason")
		}
		key := judgment.Package + "\x00" + judgment.Function + "\x00" + judgment.Destination
		if seen[key] {
			return fmt.Errorf("judged raw extraction %s %s -> %s is repeated", judgment.Package, judgment.Function, judgment.Destination)
		}
		seen[key] = true
	}
	return nil
}

func goModules(root string) ([]string, error) {
	command := exec.Command("git", "-C", root, "ls-files", "--", "*go.mod")
	output, err := command.Output()
	if err == nil {
		var modules []string
		for _, name := range strings.Fields(string(output)) {
			modules = append(modules, filepath.Join(root, filepath.Dir(name)))
		}
		if len(modules) != 0 {
			return modules, nil
		}
	}
	var modules []string
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if !entry.IsDir() && entry.Name() == "go.mod" {
			modules = append(modules, filepath.Dir(path))
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if len(modules) == 0 {
		return nil, fmt.Errorf("no go.mod below %s", root)
	}
	sort.Strings(modules)
	return modules, nil
}

func listedPackages(module string) ([]string, error) {
	command := exec.Command("go", "list", "-e", "-f", "{{.ImportPath}}", "./...")
	command.Dir = module
	command.Env = withoutGoWork(os.Environ())
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list packages in %s: %w\n%s", module, err, output)
	}
	var names []string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		name := strings.TrimSpace(scanner.Text())
		if name == "" || name == "command-line-arguments" {
			continue
		}
		names = append(names, name)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read package list in %s: %w", module, err)
	}
	sort.Strings(names)
	return slices.Compact(names), nil
}

func checkPackage(module, packagePath string, judgments []judgment) ([]finding, map[int]bool, error) {
	configuration := &packages.Config{
		Dir: module,
		Env: withoutGoWork(os.Environ()),
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports,
	}
	loaded, err := packages.Load(configuration, packagePath)
	if err != nil {
		return nil, nil, fmt.Errorf("load %s: %w", packagePath, err)
	}
	if len(loaded) != 1 {
		return nil, nil, fmt.Errorf("load %s: got %d packages", packagePath, len(loaded))
	}
	pkg := loaded[0]
	if len(pkg.Errors) != 0 {
		return nil, nil, fmt.Errorf("load %s: %s", packagePath, joinPackageErrors(pkg.Errors))
	}
	if len(pkg.Syntax) == 0 {
		// go list includes a package whose directory holds tests only, although this check loads
		// production code alone. There is no source for a default-build check to inspect.
		return nil, map[int]bool{}, nil
	}
	if pkg.Types == nil {
		return nil, nil, fmt.Errorf("load %s: no type information", packagePath)
	}
	findings, used := inspectPackage(pkg, judgments)
	return findings, used, nil
}

func joinPackageErrors(errors []packages.Error) string {
	messages := make([]string, 0, len(errors))
	for _, err := range errors {
		messages = append(messages, err.Error())
	}
	return strings.Join(messages, "; ")
}

func withoutGoWork(environment []string) []string {
	filtered := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "GOWORK=") {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, "GOWORK=off")
}

func inspectPackage(pkg *packages.Package, judgments []judgment) ([]finding, map[int]bool) {
	var findings []finding
	used := make(map[int]bool)
	for _, file := range pkg.Syntax {
		parents := parentNodes(file)
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				function := functionAt(file, node.Pos(), pkg.TypesInfo)
				if isSettingRead(node, pkg.TypesInfo) && !isGuardedConstructor(parents[node], node, pkg.TypesInfo) {
					findings = append(findings, at(pkg.Fset, node.Pos(), "BROKER_ENVOY_URL is read into a plain string in "+function+" outside the configuration boundary"))
				}
				if name, ok := reflectiveOrUnsafeCall(node, pkg.TypesInfo); ok {
					findings = append(findings, at(pkg.Fset, node.Pos(), name+" in "+function+" bypasses urlsetting.Value's guarded representation"))
				}
			case *ast.SelectorExpr:
				if !isRawSelector(node, pkg.TypesInfo) {
					return true
				}
				function := functionAt(file, node.Pos(), pkg.TypesInfo)
				var expression ast.Node = node
				for {
					parent, ok := parents[expression].(*ast.ParenExpr)
					if !ok {
						break
					}
					expression = parent
				}
				call, ok := parents[expression].(*ast.CallExpr)
				if !ok || call.Fun != expression {
					findings = append(findings, at(pkg.Fset, node.Pos(), "urlsetting.Value.Raw method reference in "+function+" escapes its reviewed call site"))
					return true
				}
				destination := destinationOf(call, parents, pkg.Fset)
				index := matchingJudgment(judgments, pkg.PkgPath, function, destination)
				if index < 0 {
					findings = append(findings, at(pkg.Fset, node.Pos(), fmt.Sprintf("urlsetting.Value.Raw in %s -> %s is not judged", function, destination)))
					return true
				}
				used[index] = true
			}
			return true
		})
	}
	return findings, used
}

func at(files *token.FileSet, position token.Pos, message string) finding {
	return finding{Position: files.Position(position), Message: message}
}

func parentNodes(file *ast.File) map[ast.Node]ast.Node {
	parents := make(map[ast.Node]ast.Node)
	var stack []ast.Node
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) != 0 {
			parents[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})
	return parents
}

func isRawSelector(selector *ast.SelectorExpr, info *types.Info) bool {
	selection := info.Selections[selector]
	if selection == nil || (selection.Kind() != types.MethodVal && selection.Kind() != types.MethodExpr) {
		return false
	}
	method, ok := selection.Obj().(*types.Func)
	if !ok || method.Name() != guardedRaw || method.Pkg() == nil || method.Pkg().Path() != guardedPackage {
		return false
	}
	signature, ok := method.Type().(*types.Signature)
	return ok && signature.Recv() != nil && isGuardedType(signature.Recv().Type())
}

func isSettingRead(call *ast.CallExpr, info *types.Info) bool {
	if len(call.Args) != 1 {
		return false
	}
	setting := info.Types[call.Args[0]].Value
	if setting == nil || setting.Kind() != constant.String || constant.StringVal(setting) != "BROKER_ENVOY_URL" {
		return false
	}
	callee := info.TypeOf(call.Fun)
	if callee == nil {
		return false
	}
	signature, ok := callee.Underlying().(*types.Signature)
	if !ok || signature.Variadic() || signature.Params().Len() != 1 || !types.Identical(signature.Params().At(0).Type(), types.Typ[types.String]) {
		return false
	}
	results := signature.Results()
	return (results.Len() == 1 || results.Len() == 2 && types.Identical(results.At(1).Type(), types.Typ[types.Bool])) && types.Identical(results.At(0).Type(), types.Typ[types.String])
}

func isGuardedConstructor(parent ast.Node, read *ast.CallExpr, info *types.Info) bool {
	call, ok := parent.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 || call.Args[0] != read {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "New" {
		return false
	}
	function, ok := info.ObjectOf(selector.Sel).(*types.Func)
	if !ok || function.Pkg() == nil || function.Pkg().Path() != guardedPackage {
		return false
	}
	return isGuardedType(info.TypeOf(call))
}

func reflectiveOrUnsafeCall(call *ast.CallExpr, info *types.Info) (string, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	object := info.ObjectOf(selector.Sel)
	if object == nil || object.Pkg() == nil || (object.Pkg().Path() != "reflect" && object.Pkg().Path() != "unsafe") {
		return "", false
	}
	for _, argument := range call.Args {
		if isGuardedType(info.TypeOf(argument)) {
			return object.Pkg().Path() + "." + object.Name(), true
		}
	}
	return "", false
}

func isGuardedType(value types.Type) bool {
	for value != nil {
		value = types.Unalias(value)
		switch current := value.(type) {
		case *types.Named:
			object := current.Obj()
			return object.Pkg() != nil && object.Pkg().Path() == guardedPackage && object.Name() == guardedType
		case *types.Pointer:
			value = current.Elem()
		default:
			return false
		}
	}
	return false
}

func functionAt(file *ast.File, position token.Pos, info *types.Info) string {
	name := "<package>"
	span := token.Pos(0)
	ast.Inspect(file, func(node ast.Node) bool {
		declaration, ok := node.(*ast.FuncDecl)
		if !ok || position < declaration.Pos() || position > declaration.End() || declaration.Pos() < span {
			return true
		}
		span = declaration.Pos()
		name = declaration.Name.Name
		if declaration.Recv != nil && len(declaration.Recv.List) == 1 {
			name = "(" + types.TypeString(info.TypeOf(declaration.Recv.List[0].Type), shortQualifier) + ")." + name
		}
		return true
	})
	return name
}

func shortQualifier(packagePath *types.Package) string { return packagePath.Name() }

func destinationOf(call *ast.CallExpr, parents map[ast.Node]ast.Node, files *token.FileSet) string {
	var child ast.Node = call
	for parent := parents[child]; parent != nil; parent = parents[child] {
		switch parent := parent.(type) {
		case *ast.ParenExpr, *ast.BinaryExpr, *ast.UnaryExpr, *ast.CompositeLit:
			child = parent
			continue
		case *ast.AssignStmt:
			for index, right := range parent.Rhs {
				if contains(right, call) && index < len(parent.Lhs) {
					return expressionName(parent.Lhs[index], files)
				}
			}
		case *ast.ValueSpec:
			for index, right := range parent.Values {
				if contains(right, call) && index < len(parent.Names) {
					return parent.Names[index].Name
				}
			}
		case *ast.CallExpr:
			for index, argument := range parent.Args {
				if contains(argument, call) {
					return expressionName(parent.Fun, files) + " argument " + strconv.Itoa(index+1)
				}
			}
		}
		child = parent
	}
	return "<unknown destination>"
}

func contains(root ast.Node, want ast.Node) bool {
	found := false
	ast.Inspect(root, func(node ast.Node) bool {
		if node == want {
			found = true
			return false
		}
		return !found
	})
	return found
}

func expressionName(expression ast.Expr, files *token.FileSet) string {
	if identifier, ok := expression.(*ast.Ident); ok {
		return identifier.Name
	}
	return strings.TrimSpace(types.ExprString(expression))
}

func matchingJudgment(judgments []judgment, packagePath, function, destination string) int {
	for index, judgment := range judgments {
		if judgment.Package == packagePath && judgment.Function == function && judgment.Destination == destination {
			return index
		}
	}
	return -1
}

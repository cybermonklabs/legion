// Daemon-pane is the skill-scenario rig's one reading of the Legion daemon: what the daemon gives a
// phase-worker pane, computed by the daemon's own functions, for worker-pane.ts and
// daemon-standin.ts to run Oh My Pi and the stand-in daemon under. It belongs to no module:
// daemon-pane.ts runs it with `go run -overlay` as a main inside a checkout's packages/daemon, the
// one place the daemon's internal packages can be imported from. The daemon's routes, and how it
// takes a completion, are read by daemon-routes.go and daemon-completion.go instead, programs of
// the standard library alone, since those readings need nothing of the daemon but its files and
// building this one first builds the daemon module's whole dependency graph (daemon-routes.go
// says what that cost).
//
//	phase <role>             prints the phase the role's worker runs in, then on a second line the
//	                         word of the handoff record that phase ends with (handoffWords), empty
//	                         for a phase that ends with none: the first phase workflow.RoleFor
//	                         gives to the role
//	refusal not-current-phase <role> <phase> <issue> [<engine source> [<intake package dir>]]
//	                         prints the code of the daemon's refusal of a completion of a phase
//	                         the role does not run, then on a second line the daemon's own
//	                         message for it, read out of the daemon's source (notCurrentPhase)
//	count-default <key> [<config package dir>]
//	                         prints the default of the count setting `key` (legion.yaml), read
//	                         out of the daemon's source (countDefault)
//	claim-refusal <name> [<claim package dir>]
//	                         prints the status of the claim refusal `name` (claim.InvalidSecret,
//	                         claim.StaleGeneration, …), then on a second line its message, read out
//	                         of the daemon's source (claimRefusal)
//	post-check               reads a message body on stdin and prints nothing where Dispatch takes
//	                         the longest post the daemon's outbox could make of it, else the kind of
//	                         Dispatch's refusal (`decision` or `bound`), then on a second line the
//	                         refusal's own sentence: the daemon's own reading (postCheck)
//	pane                     reads a paneRequest (JSON) on stdin, writes the claim's gh bundle
//	                         under its LEGION_GH_OWNERS_DIR, and prints its paneResponse (JSON)
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/dispatchbody"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const usage = "usage: daemon-pane phase <role> | refusal not-current-phase <role> <phase> <issue> [<engine source> [<intake package dir>]] | count-default <key> [<config package dir>] | claim-refusal <name> [<claim package dir>] | post-check < body | pane < request.json"

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 2 && args[0] == "phase":
		err = printPhase(claim.Role(args[1]))
	case len(args) >= 5 && len(args) <= 7 && args[0] == "refusal" && args[1] == "not-current-phase":
		// The engine source the refusal is read out of, and the intake package the engine's
		// shared refusals are built in: the daemon's own, and the ones a caller names otherwise.
		// A caller names them so that a test can hold this reading to substitutes of its own and
		// watch each shape and each guard below fire — the guards answer a daemon that writes
		// the refusal in some other shape, which today's daemon does not, and an older daemon's
		// shape is one today's daemon no longer writes, so without sources of its own not one of
		// them can be reached (daemon-pane.test.ts). The rig itself names none and reads the
		// daemon's files.
		engine, intakeDir := defaultEngineFile, defaultIntakeDir
		if len(args) >= 6 {
			engine = args[5]
		}
		if len(args) == 7 {
			intakeDir = args[6]
		}
		err = printNotCurrentPhase(args[2], args[3], args[4], engine, intakeDir)
	case (len(args) == 2 || len(args) == 3) && args[0] == "count-default":
		dir := defaultConfigDir
		if len(args) == 3 {
			dir = args[2]
		}
		err = printCountDefault(args[1], dir)
	case (len(args) == 2 || len(args) == 3) && args[0] == "claim-refusal":
		dir := defaultClaimDir
		if len(args) == 3 {
			dir = args[2]
		}
		err = printClaimRefusal(args[1], dir)
	case len(args) == 1 && args[0] == "post-check":
		err = postCheck()
	case len(args) == 1 && args[0] == "pane":
		err = pane()
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-pane %s: %v\n", args[0], err)
		os.Exit(1)
	}
}

// handoffWords is the word of the handoff each handoff phase ends with: this program's one
// restatement of the daemon, and its own copy rather than the daemon's phase table because one
// program runs against each label's checkout of the daemon module, and that table has had more
// than one name (phase.HandoffFile, then phase.Handoff). Every other answer here is the daemon's
// own function (daemon-pane.ts).
var handoffWords = map[phase.Phase]string{
	phase.Planning: "plan", phase.Implementing: "implement", phase.Testing: "test", phase.Reviewing: "review",
}

// printPhase prints the first phase of the transition table `role` works and that phase's handoff
// word.
func printPhase(role claim.Role) error {
	for _, p := range phase.All {
		if workflow.RoleFor(p) != role {
			continue
		}
		fmt.Println(p)
		fmt.Println(handoffWords[p])
		return nil
	}
	return fmt.Errorf("%q works no phase", role)
}

// postCheck prints whether Dispatch takes a message the daemon posts with the body read on stdin,
// as the daemon decides it before it records a move whose reason it posts (workflow's
// backwardReasonRefusal, api's closeRoot): the longest post its outbox could make of the body,
// the body under the marker of the largest row id (record.LongestPost), read by Dispatch's own
// reader (dispatchbody.Check). Nothing is printed for a post Dispatch takes; a refusal prints its
// kind, `decision` for a body holding a decision block and `bound` for one past a bound, then the
// refusal's sentence, which the daemon quotes in its own. The kind is read off the refusal's type,
// the *itemblocks.DecisionBlock Check returns unwrapped, rather than through
// dispatchbody.IsDecisionBlock, which main ff2a10ef's envoy module has not got: the rig builds
// this program in that checkout's module too, whose daemon refuses no reason (daemon-pane.ts's
// postRefusal says who asks).
func postCheck() error {
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read the body: %w", err)
	}
	over := dispatchbody.Check(record.LongestPost(string(body)))
	if over == nil {
		return nil
	}
	kind := "bound"
	if strings.HasSuffix(fmt.Sprintf("%T", over), ".DecisionBlock") {
		kind = "decision"
	}
	fmt.Println(kind)
	fmt.Println(over.Error())
	return nil
}

// defaultConfigDir is the package the daemon reads legion.yaml in and keeps each setting's default
// in, read relative to the module, and taken as an argument as defaultEngineFile is.
const defaultConfigDir = "internal/config"

// printCountDefault prints the default of the count setting `key` (countDefault).
func printCountDefault(key, dir string) error {
	value, err := countDefault(key, dir)
	if err != nil {
		return err
	}
	fmt.Println(value)
	return nil
}

// countDefault is the default the daemon gives the count setting `key` when legion.yaml leaves it
// out, read out of the config package at `dir`, every file of it but its tests. The rig runs with
// no legion.yaml, so that default is the value the daemon runs with. The daemon has kept that
// default in one of two shapes (countEntry), and this reads both, since one program reads each
// label's checkout. A default that is no integer literal, none in either shape, or two that
// disagree is an error, never a number this invents.
func countDefault(key, dir string) (int, error) {
	sources, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return 0, fmt.Errorf("list the daemon's %s: %w", dir, err)
	}
	type reading struct {
		value int
		at    string
	}
	var readings []reading
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		files := token.NewFileSet()
		parsed, err := parser.ParseFile(files, source, nil, 0)
		if err != nil {
			return 0, fmt.Errorf("read the daemon's %s: %w", source, err)
		}
		var read error
		ast.Inspect(parsed, func(node ast.Node) bool {
			if read != nil {
				return false
			}
			entry, isLiteral := node.(*ast.CompositeLit)
			if !isLiteral {
				return true
			}
			given, named := countEntry(entry, key)
			if !named {
				return true
			}
			number, isNumber := given.(*ast.BasicLit)
			if !isNumber || number.Kind != token.INT {
				read = fmt.Errorf("%s gives %s a default of %s, which is no integer literal", source, key, types.ExprString(given))
				return false
			}
			value, err := strconv.Atoi(number.Value)
			if err != nil {
				read = err
				return false
			}
			readings = append(readings, reading{value, files.Position(entry.Pos()).String()})
			return false
		})
		if read != nil {
			return 0, read
		}
	}
	if len(readings) == 0 {
		return 0, fmt.Errorf("%s holds no {%q, <default>, …} entry and no {Name: %q, …, Positive: &positiveKey{Value: <default>, …}} row: the daemon keeps that default some other way now", dir, key, key)
	}
	for _, other := range readings[1:] {
		if other.value != readings[0].value {
			return 0, fmt.Errorf("%s gives %s a default of %d at %s and of %d at %s: which one the daemon runs with is not read here", dir, key, readings[0].value, readings[0].at, other.value, other.at)
		}
	}
	return readings[0].value, nil
}

// countEntry is the expression `entry` gives the count setting `key` as its default, in either
// shape the daemon has kept count defaults in, and false for a literal that gives `key` none:
//   - `{"<key>", <default>, …}`, an entry of the table of count keys in config.go, an older
//     daemon's (main `ff2a10ef`'s);
//   - `{Name: "<key>", …, Positive: &positiveKey{Value: <default>, …}}`, a row of the loader's
//     key table in keytable.go, this checkout's.
func countEntry(entry *ast.CompositeLit, key string) (ast.Expr, bool) {
	if len(entry.Elts) >= 2 {
		if named, isText := stringLiteral(entry.Elts[0]); isText && named == key {
			return entry.Elts[1], true
		}
	}
	if named, isText := stringLiteral(keyedField(entry, "Name")); !isText || named != key {
		return nil, false
	}
	pointer, isPointer := keyedField(entry, "Positive").(*ast.UnaryExpr)
	if !isPointer {
		return nil, false
	}
	positive, isLiteral := pointer.X.(*ast.CompositeLit)
	if !isLiteral {
		return nil, false
	}
	given := keyedField(positive, "Value")
	return given, given != nil
}

// keyedField is the value `literal` gives its field `name` (`name: <value>`), and nil when it
// gives that field none.
func keyedField(literal *ast.CompositeLit, name string) ast.Expr {
	for _, element := range literal.Elts {
		field, isField := element.(*ast.KeyValueExpr)
		if !isField {
			continue
		}
		if key, isKey := field.Key.(*ast.Ident); isKey && key.Name == name {
			return field.Value
		}
	}
	return nil
}

// defaultEngineFile is where the daemon builds the workflow's refusals, read relative to the
// module this program is run inside (daemon-pane.ts). Every reader below takes the file as an
// argument rather than this constant, so a caller — a test — can hold the reading to a
// substitute of its own.
const defaultEngineFile = "internal/workflow/engine.go"

// defaultIntakeDir is the package a refusal the engine shares with the completion route is built
// in (`intake.NotCurrentPhase`), read relative to the module as defaultEngineFile is, and taken
// as an argument for the same reason.
const defaultIntakeDir = "internal/intake"

// notCurrentPhaseCode is the refusal the daemon answers a completion of a phase the completing
// role does not run with.
const notCurrentPhaseCode = "HANDOFF_NOT_CURRENT_PHASE"

// printNotCurrentPhase prints that refusal's code and the daemon's own message for it, for the
// stand-in to answer such a completion with (daemon-standin.ts).
func printNotCurrentPhase(role, issuePhase, issue, engineFile, intakeDir string) error {
	message, err := notCurrentPhase(role, issuePhase, issue, engineFile, intakeDir)
	if err != nil {
		return err
	}
	fmt.Println(notCurrentPhaseCode)
	fmt.Println(message)
	return nil
}

// notCurrentPhase is the sentence the daemon sends a worker whose completion names a phase its
// role does not run, read out of the daemon's own code rather than restated here: the skill has
// the worker quote that sentence to its architect, so a stand-in answering a sentence of its own
// would teach every agent a message the daemon never sends, and a copy goes stale the day the
// daemon's wording moves with nothing to say so. The engine builds it in one place, in one of two
// shapes (refusalMessage), so this reads that call's format and fills it with the arguments the
// engine itself names. A daemon that writes the refusal in some other shape is an error here,
// never a sentence this invents.
func notCurrentPhase(role, issuePhase, issue, engineFile, intakeDir string) (string, error) {
	format, arguments, err := refusalMessage(notCurrentPhaseCode, engineFile, intakeDir)
	if err != nil {
		return "", err
	}
	named := map[string]string{"fact.Role": role, "issue.Phase": issuePhase, "issue.Key": issue}
	values := make([]any, 0, len(arguments))
	for _, argument := range arguments {
		value, ok := named[argument]
		if !ok {
			return "", fmt.Errorf("%s fills the %s message with %s, which this program has no value for", engineFile, notCurrentPhaseCode, argument)
		}
		values = append(values, value)
	}
	message := fmt.Sprintf(format, values...)
	if strings.Contains(message, "%!") {
		return "", fmt.Errorf("%s's %s message %q takes other arguments than the %d it names", engineFile, notCurrentPhaseCode, format, len(arguments))
	}
	return message, nil
}

// refusalMessage is the format string of the engine's refusal `code` and the argument
// expressions the engine fills it with, as the daemon writes them, in one of the two shapes the
// engine has built that refusal in:
//   - `refused(code, fmt.Sprintf(<format>, <arguments>...))` in the engine itself, an older
//     daemon's (main `ff2a10ef`'s);
//   - `intake.NotCurrentPhase(<arguments>...)` in the engine, this checkout's, whose sentence the
//     intake package builds, shared with the completion route: its `func NotCurrentPhase` returns
//     a refusal of `code` whose `Message` is `fmt.Sprintf(<format>, <parameters>...)`, and each
//     parameter it fills the format with is replaced by the engine's argument in that place.
//
// The first is read first; finding neither is an error naming both.
func refusalMessage(code, engineFile, intakeDir string) (string, []string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), engineFile, nil, 0)
	if err != nil {
		return "", nil, fmt.Errorf("read the daemon's %s: %w", engineFile, err)
	}
	format, arguments, found := "", []string(nil), false
	var shared *ast.CallExpr
	ast.Inspect(file, func(node ast.Node) bool {
		if found {
			return false
		}
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		if shared == nil && isIntakeNotCurrentPhase(call) {
			shared = call
			return true
		}
		name, isName := call.Fun.(*ast.Ident)
		if !isName || name.Name != "refused" || len(call.Args) != 2 {
			return true
		}
		if refused, isText := stringLiteral(call.Args[0]); !isText || refused != code {
			return true
		}
		text, filled, isSprintf := sprintfOf(call.Args[1])
		if !isSprintf {
			return true
		}
		format, found = text, true
		for _, argument := range filled {
			arguments = append(arguments, types.ExprString(argument))
		}
		return false
	})
	if found {
		return format, arguments, nil
	}
	if shared == nil {
		return "", nil, fmt.Errorf("%s holds no refused(%q, fmt.Sprintf(…)) call and no intake.NotCurrentPhase(…) call: the daemon writes that refusal some other way now", engineFile, code)
	}
	return sharedRefusal(code, shared, intakeDir)
}

// isIntakeNotCurrentPhase says whether `call` is `intake.NotCurrentPhase(…)`.
func isIntakeNotCurrentPhase(call *ast.CallExpr) bool {
	selector, isSelector := call.Fun.(*ast.SelectorExpr)
	if !isSelector || selector.Sel.Name != "NotCurrentPhase" {
		return false
	}
	pkg, isIdent := selector.X.(*ast.Ident)
	return isIdent && pkg.Name == "intake"
}

// sharedRefusal is the format and the engine's arguments of the refusal `code` the engine builds
// with `call`, `intake.NotCurrentPhase(…)`: the format of the `Message` the intake package's
// `func NotCurrentPhase` writes beside `Code: code`, and, for each parameter that format is
// filled with, the engine's argument in that parameter's place.
func sharedRefusal(code string, call *ast.CallExpr, intakeDir string) (string, []string, error) {
	sources, err := filepath.Glob(filepath.Join(intakeDir, "*.go"))
	if err != nil {
		return "", nil, fmt.Errorf("list the daemon's %s: %w", intakeDir, err)
	}
	missing := fmt.Errorf("%s holds no func NotCurrentPhase returning a %s refusal whose Message is fmt.Sprintf(…) of its parameters: the daemon writes that refusal some other way now", intakeDir, code)
	var function *ast.FuncDecl
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
		if err != nil {
			return "", nil, fmt.Errorf("read the daemon's %s: %w", source, err)
		}
		for _, decl := range file.Decls {
			if fn, isFunc := decl.(*ast.FuncDecl); isFunc && fn.Recv == nil && fn.Name.Name == "NotCurrentPhase" && fn.Body != nil {
				function = fn
			}
		}
	}
	if function == nil {
		return "", nil, missing
	}
	var parameters []string
	for _, field := range function.Type.Params.List {
		for _, name := range field.Names {
			parameters = append(parameters, name.Name)
		}
	}
	if len(parameters) != len(call.Args) {
		return "", nil, fmt.Errorf("the engine calls intake.NotCurrentPhase with %d arguments, and %s's takes %d", len(call.Args), intakeDir, len(parameters))
	}
	format, filled, found := "", []ast.Expr(nil), false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if found {
			return false
		}
		literal, isLiteral := node.(*ast.CompositeLit)
		if !isLiteral {
			return true
		}
		var coded bool
		var message ast.Expr
		for _, element := range literal.Elts {
			field, isField := element.(*ast.KeyValueExpr)
			if !isField {
				continue
			}
			key, isKey := field.Key.(*ast.Ident)
			if !isKey {
				continue
			}
			switch key.Name {
			case "Code":
				text, isText := stringLiteral(field.Value)
				coded = isText && text == code
			case "Message":
				message = field.Value
			}
		}
		if !coded || message == nil {
			return true
		}
		format, filled, found = sprintfOf(message)
		return !found
	})
	if !found {
		return "", nil, missing
	}
	arguments := make([]string, 0, len(filled))
	for _, expression := range filled {
		name, isName := expression.(*ast.Ident)
		place := -1
		if isName {
			for at, parameter := range parameters {
				if parameter == name.Name {
					place = at
				}
			}
		}
		if place < 0 {
			return "", nil, fmt.Errorf("%s fills the %s message with %s, which is none of NotCurrentPhase's parameters", intakeDir, code, types.ExprString(expression))
		}
		arguments = append(arguments, types.ExprString(call.Args[place]))
	}
	return format, arguments, nil
}

// sprintfOf is the format literal and the arguments of `expression` when it is
// `fmt.Sprintf(<format literal>, <arguments>...)`, and false for any other expression.
func sprintfOf(expression ast.Expr) (string, []ast.Expr, bool) {
	sprintf, isCall := expression.(*ast.CallExpr)
	if !isCall {
		return "", nil, false
	}
	printer, isSelector := sprintf.Fun.(*ast.SelectorExpr)
	if !isSelector || printer.Sel.Name != "Sprintf" || len(sprintf.Args) == 0 {
		return "", nil, false
	}
	text, isText := stringLiteral(sprintf.Args[0])
	if !isText {
		return "", nil, false
	}
	return text, sprintf.Args[1:], true
}

// defaultClaimDir is the package the daemon keeps its claim refusals in, read relative to the
// module, and taken as an argument as defaultEngineFile is.
const defaultClaimDir = "internal/claim"

// statuses are the HTTP statuses a claim refusal is written with, by the name net/http gives each:
// the standard library's own constants, so a refusal of another status is an error here.
var statuses = map[string]int{
	"StatusBadRequest": http.StatusBadRequest, "StatusUnauthorized": http.StatusUnauthorized,
	"StatusForbidden": http.StatusForbidden, "StatusConflict": http.StatusConflict,
	"StatusGone": http.StatusGone, "StatusPreconditionFailed": http.StatusPreconditionFailed,
}

// printClaimRefusal prints the claim refusal `name`'s status and message (claimRefusal).
func printClaimRefusal(name, dir string) error {
	status, message, err := claimRefusal(name, dir)
	if err != nil {
		return err
	}
	fmt.Println(status)
	fmt.Println(message)
	return nil
}

// claimRefusal is the status and the message of the claim refusal the package at `dir` declares
// as `<name> = Refusal{Status: http.<Status>, Message: "<message>"}`: the answer a claim route
// gives a caller it refuses, read out of the source rather than restated in the rig, so the
// stand-in's refusal is the daemon's own sentence (daemon-standin.ts). A declaration of another
// shape, or none, is an error, never a sentence this invents.
func claimRefusal(name, dir string) (int, string, error) {
	sources, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return 0, "", fmt.Errorf("list the daemon's %s: %w", dir, err)
	}
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
		if err != nil {
			return 0, "", fmt.Errorf("read the daemon's %s: %w", source, err)
		}
		for _, decl := range file.Decls {
			general, isGeneral := decl.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.VAR {
				continue
			}
			for _, spec := range general.Specs {
				value, isValue := spec.(*ast.ValueSpec)
				if !isValue || len(value.Names) != 1 || value.Names[0].Name != name || len(value.Values) != 1 {
					continue
				}
				status, message, read := refusalLiteral(value.Values[0])
				if !read {
					return 0, "", fmt.Errorf("%s declares %s as %s, which is no Refusal{Status: http.<Status>, Message: \"<message>\"}", source, name, types.ExprString(value.Values[0]))
				}
				return status, message, nil
			}
		}
	}
	return 0, "", fmt.Errorf("%s declares no claim refusal %s: the daemon refuses that caller some other way now", dir, name)
}

// refusalLiteral is the status and the message of `Refusal{Status: http.<Status>, Message:
// "<message>"}`, and false for any other expression.
func refusalLiteral(expression ast.Expr) (int, string, bool) {
	literal, isLiteral := expression.(*ast.CompositeLit)
	if !isLiteral {
		return 0, "", false
	}
	if kind, isIdent := literal.Type.(*ast.Ident); !isIdent || kind.Name != "Refusal" {
		return 0, "", false
	}
	status, message := 0, ""
	for _, element := range literal.Elts {
		field, isField := element.(*ast.KeyValueExpr)
		if !isField {
			return 0, "", false
		}
		key, isKey := field.Key.(*ast.Ident)
		if !isKey {
			return 0, "", false
		}
		switch key.Name {
		case "Status":
			named, isSelector := field.Value.(*ast.SelectorExpr)
			if !isSelector {
				return 0, "", false
			}
			status = statuses[named.Sel.Name]
		case "Message":
			text, isText := stringLiteral(field.Value)
			if !isText {
				return 0, "", false
			}
			message = text
		}
	}
	return status, message, status != 0 && message != ""
}

// stringLiteral is the text of an interpreted string literal, and false for any other expression.
func stringLiteral(expression ast.Expr) (string, bool) {
	literal, isLiteral := expression.(*ast.BasicLit)
	if !isLiteral || literal.Kind != token.STRING {
		return "", false
	}
	text, err := strconv.Unquote(literal.Value)
	return text, err == nil
}

// paneRequest is the claim a pane is launched for, and the rig's stand-ins for what the daemon
// knows at boot. The pane's tree is its issue.
type paneRequest struct {
	Project string `json:"project"`
	Issue   string `json:"issue"`
	Role    string `json:"role"`
	// StateDir stands for the daemon's state_dir, Workspace for the issue's workspace.
	StateDir  string   `json:"stateDir"`
	Workspace string   `json:"workspace"`
	DaemonURL string   `json:"daemonUrl"`
	EnvoyURL  string   `json:"envoyUrl"`
	NATSURLs  []string `json:"natsUrls"`
	// BootTokenFile is the rig's boot token file, the one its stand-in daemon checks.
	BootTokenFile string `json:"bootTokenFile"`
	// GHToken is the GitHub token the rig stands in for the claim's App token: it is rendered
	// into the claim's gh bundle (runtime.GHConfigDir), the one directory of Repository's owner,
	// which the pane's GH_CONFIG_DIR names, as the daemon's tmux runtime writes it at spawn; the
	// pane's LEGION_GH_PATH is realGH's: GHPath when the rig names one, else the first gh on
	// Path, refused when that is a script, as the daemon resolves it at boot. "" is a daemon
	// with no GitHub credential: no files, and no gh variable on the pane.
	GHToken string `json:"ghToken"`
	// Path is the PATH the daemon would start under; the pane's PATH is it behind the pane's
	// launcher directory (workerbin.Path).
	Path string `json:"path"`
	// Repository is the GitHub repository of the run's own world and DefaultBranch the branch
	// that world opens its pull request against. The pane's addressing names them, as a real
	// pane's names its issue's, and its gh bundle is the directory of this repository's owner.
	// Why they are the world's own and no name of this program's: the worker world comment of
	// rig.sh, which holds that story once.
	Repository    string `json:"repository"`
	DefaultBranch string `json:"defaultBranch"`
	// GHPath is the real gh the rig names for the pane, as an operator names one with
	// LEGION_GH_PATH: its recording stand-in. "" is the daemon's own resolution, the first gh on
	// Path, refused when that is a script.
	GHPath string `json:"ghPath"`
	// SystemPrompt asks for the pane's system prompt, composed from the role prompts the daemon
	// module embeds and written to the claim's prompt file under StateDir, as a pane's launch
	// writes it (runtime.WriteSystemPrompt).
	SystemPrompt bool `json:"systemPrompt"`
}

type paneResponse struct {
	// Env is every variable the daemon tells the pane, PATH included.
	Env map[string]string `json:"env"`
	// SystemPromptArgument is the pane's one --append-system-prompt argument, as shell text: the
	// flag and the path of the file the prompt was written to; "" without SystemPrompt.
	SystemPromptArgument string `json:"systemPromptArgument,omitempty"`
}

func pane() error {
	var req paneRequest
	decoder := json.NewDecoder(os.Stdin)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return fmt.Errorf("read the request: %w", err)
	}
	repository, err := ghrepo.Parse("the rig's repository", req.Repository)
	if err != nil {
		return err
	}
	if req.DefaultBranch == "" {
		return errors.New("the rig's world names no default branch, which the pane's addressing names")
	}
	role := claim.Role(req.Role)
	token, err := claim.NewToken(req.Project, req.Issue, role)
	if err != nil {
		return err
	}
	owner := repository.Owner()
	ghRoot, gh := "", ""
	if req.GHToken != "" {
		// ghconfig's one writer of a role's gh bundle, so the rig's directory is the daemon's layout;
		// the App label and expiry only feed the runtime's log lines, which the rig does not write. The
		// rig's project serves one repository of one owner, so its bundle holds that owner's directory
		// alone, where a daemon serving more holds a `<owner>/<repo>` directory beside it per sibling
		// repository.
		ghRoot = runtime.GHConfigDir(req.StateDir, token)
		bundle := ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{owner: ghconfig.Render(req.GHToken, "", time.Time{})}}
		if _, err := ghconfig.WriteBundle(ghRoot, bundle); err != nil {
			return fmt.Errorf("write the claim's gh files: %w", err)
		}
		if gh, err = realGH(req.GHPath, req.Path); err != nil {
			return err
		}
	}
	spec := runtime.SpawnSpec{Claim: token, Project: req.Project, Tree: req.Issue, Issue: req.Issue, Role: role, Generation: 1}
	variables, err := tmux.PaneVariables(spec, tmux.PaneInputs{
		StateDir: req.StateDir, Workspace: req.Workspace, DaemonURL: req.DaemonURL, EnvoyURL: req.EnvoyURL,
		NATSURLs: req.NATSURLs, GHOwnersDir: ghRoot, GHDefault: owner, GH: gh,
	}, []runtime.SecretFile{{Variable: "LEGION_BOOT_TOKEN", Path: req.BootTokenFile}})
	if err != nil {
		return err
	}
	res := paneResponse{Env: map[string]string{"PATH": workerbin.Path(req.Path, req.StateDir)}}
	for _, variable := range variables {
		name, value, _ := strings.Cut(variable, "=")
		res.Env[name] = value
	}
	if req.SystemPrompt {
		if res.SystemPromptArgument, err = systemPromptArgument(req, token, repository); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(res)
}

// realGH is the real gh the pane is told as LEGION_GH_PATH: named, the gh the rig names (its
// stand-in, as an operator's LEGION_GH_PATH is taken as given), else the daemon's resolution at
// boot, the first executable gh on path, refused when it is a script (daemon.RefuseScriptGH's
// rule). It is this program's own copy, since the rig runs it against each label's checkout of the
// daemon module, an older one among them.
func realGH(named, path string) (string, error) {
	if named != "" {
		if info, err := os.Stat(named); err != nil || !filepath.IsAbs(named) || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("the rig's gh %q is not an absolute executable path", named)
		}
		return named, nil
	}
	for _, dir := range filepath.SplitList(path) {
		gh := filepath.Join(dir, "gh")
		if info, err := os.Stat(gh); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			head, err := firstBytes(gh, 2)
			if err != nil {
				return "", fmt.Errorf("read %s, the first gh on the daemon's PATH: %w", gh, err)
			}
			if string(head) == "#!" {
				return "", fmt.Errorf("the first gh on the daemon's PATH, %s, is a script, not the real gh, which the daemon refuses to boot on", gh)
			}
			return gh, nil
		}
	}
	return "", fmt.Errorf("no gh on the daemon's PATH %q: the daemon resolves the real gh every pane's `legion gh` runs at boot", path)
}

// firstBytes is up to n bytes from the start of path.
func firstBytes(path string, n int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	head := make([]byte, n)
	read, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return head[:read], nil
}

// systemPromptArgument composes the pane's role prompt as the daemon does (specs.SpawnSpec): the
// role prompts snapshotted under the state directory, the role's parts, then its addressing; it
// writes the text to the claim's prompt file, as a pane's launch does, and answers the argument
// that names that file. The join and the write are this program's own copies of the daemon's
// (runtime.ComposePrompt, runtime.WriteSystemPrompt), since the rig runs it against each label's
// checkout of the daemon module, an older one among them, which has neither. The
// addressing names `repository`, the world's own, the world's default branch and the reach of the
// role's App, that repository's owner, whose directory alone the pane's gh bundle holds
// (addressingOf), so the repository a worker is told to work in is the one its gh calls reach and
// the one the architect's note in its prompt names.
func systemPromptArgument(req paneRequest, token claim.Token, repository ghrepo.Repository) (string, error) {
	role := claim.Role(req.Role)
	if claim.IsTreeArchitect(role, req.Issue, req.Issue) {
		return "", errors.New("a tree architect is also told its design gate policy, which the rig does not stand in for")
	}
	composer, err := prompts.New(req.StateDir)
	if err != nil {
		return "", err
	}
	parts, err := composer.Compose(role, claim.IsTreeRoot(req.Issue, req.Issue))
	if err != nil {
		return "", err
	}
	addressing, err := addressingOf(req.Project, supervise.Claim{
		Token: token, Project: req.Project, Tree: req.Issue, Issue: req.Issue, Role: role, Generation: 1,
	}, repository, req.DefaultBranch)
	if err != nil {
		return "", err
	}
	var body strings.Builder
	for _, path := range parts.RolePromptPaths {
		text, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("prompt file: %w", err)
		}
		body.Write(text)
	}
	prompt := strings.TrimRight(body.String(), "\n") + "\n\n" + addressing
	promptFile := filepath.Join(req.StateDir, "secrets", string(token)+"-system-prompt.md")
	if err := os.MkdirAll(filepath.Dir(promptFile), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(promptFile, []byte(prompt), 0o600); err != nil {
		return "", err
	}
	return "--append-system-prompt " + shellprefix.Word(promptFile), nil
}

// addressingOf is the daemon's own addressing of c (daemon.AddressingFragment) for a pane on
// repository and defaultBranch whose role's App is installed on that repository's owner alone.
// The fragment's last parameter is not one type across the checkouts the rig runs this program
// against: since INFRA-2543 it is the owners the role's App reaches ([]string), and before that
// change it was the project's mapping view (api.RepositoriesView, a Mapped list of Repo and
// Default), which the older checkout of daemon-standin.test.ts's older-daemon row still takes. A
// call written for either type does not build against the other, so the call is made through the
// checkout's own signature, with the reach that checkout's daemon gives this pane: the one owner,
// or the one repository mapped as the project's default.
func addressingOf(project string, c supervise.Claim, repository ghrepo.Repository, defaultBranch string) (string, error) {
	fragment := reflect.ValueOf(daemon.AddressingFragment)
	if fragment.Type().NumIn() != 5 {
		return "", fmt.Errorf("daemon.AddressingFragment is %s, which this program does not call", fragment.Type())
	}
	reachType := fragment.Type().In(4)
	var reach reflect.Value
	switch {
	case reachType.Kind() == reflect.Slice && reachType.Elem().Kind() == reflect.String:
		reach = reflect.ValueOf([]string{repository.Owner()}).Convert(reachType)
	case reachType.Kind() == reflect.Struct:
		view := reflect.New(reachType).Elem()
		mapped := view.FieldByName("Mapped")
		if mapped.Kind() != reflect.Slice || mapped.Type().Elem().Kind() != reflect.Struct {
			break
		}
		entry := reflect.New(mapped.Type().Elem()).Elem()
		repo, isDefault := entry.FieldByName("Repo"), entry.FieldByName("Default")
		if repo.Kind() != reflect.String || isDefault.Kind() != reflect.Bool {
			break
		}
		repo.SetString(repository.String())
		isDefault.SetBool(true)
		mapped.Set(reflect.Append(mapped, entry))
		reach = view
	}
	if !reach.IsValid() {
		return "", fmt.Errorf("daemon.AddressingFragment takes the reach as %s, which this program builds no value of", reachType)
	}
	out := fragment.Call([]reflect.Value{
		reflect.ValueOf(project), reflect.ValueOf(c), reflect.ValueOf(repository), reflect.ValueOf(defaultBranch), reach,
	})
	if err, _ := out[1].Interface().(error); err != nil {
		return "", err
	}
	return out[0].String(), nil
}

package natscallout

//ci:reads packages/envoy/scripts/lib/legacy-subjects-gate-callout-publishers.json the gate's list of publishers, read through ../../../scripts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/natsgrant"
)

// calloutPublishersFile is the list legacy-subjects-gate.sh's fact (a3), the publishers, reads for
// a hub behind this auth service: each kind of BROKER_NATS_SERVICES entry with whether the grant
// the service admits it with may publish a notification subject, and the same for a session's
// token. The gate counts every entry of a kind marked true as a publisher, and reads an entry of a
// kind the list does not name as unknown.
const calloutPublishersFile = "../../../scripts/lib/legacy-subjects-gate-callout-publishers.json"

// kindConversion is the one place config makes a NATSServiceKind from a string at run time: the
// parser's reading of an entry's first field, which its switch then holds to the constants.
const kindConversion = "parseNATSService: NATSServiceKind(parts[0])"

// noConversion says a package makes no value of a type from a string at run time. natsgrant makes
// no OperatorPhase so: the hold row's reader makes one (HoldView.verified), and its switch holds it
// to natsgrant's constants.
const noConversion = ""

// declaredValues is every value of T, a named string type, that the source of T's package holds as
// a constant, in any of its files and in any spelling: a typed constant, a conversion of a literal,
// or an untyped constant a switch case or a composite literal gives the type. It type-checks the
// package from its source and its imports from their export data, so a value added there is read
// here with no list of this test's own. A T made from a string at run time anywhere in the package
// but conversion fails the test, since the value it holds cannot be read.
func declaredValues[T ~string](t *testing.T, conversion string) []T {
	t.Helper()
	named := reflect.TypeFor[T]()
	path, typeName := named.PkgPath(), named.Name()
	module, err := filepath.Abs("../../..")
	require.NoError(t, err)
	list := exec.Command("go", "list", "-deps", "-export", "-json=ImportPath,Dir,GoFiles,Export", path)
	list.Dir = module
	var stderr strings.Builder
	list.Stderr = &stderr
	output, err := list.Output()
	require.NoError(t, err, "go list %s's packages: %s", path, stderr.String())
	type listedPackage struct {
		ImportPath, Dir, Export string
		GoFiles                 []string
	}
	listed := map[string]listedPackage{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var pkg listedPackage
		err := decoder.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		listed[pkg.ImportPath] = pkg
	}
	source, ok := listed[path]
	require.True(t, ok, "go list named no %s", path)

	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range source.GoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(source.Dir, name), nil, 0)
		require.NoError(t, err)
		files = append(files, file)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	check := types.Config{Importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		pkg, ok := listed[path]
		if !ok || pkg.Export == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(pkg.Export)
	})}
	checked, err := check.Check(path, fset, files, info)
	require.NoError(t, err)
	typeObject := checked.Scope().Lookup(typeName)
	require.NotNil(t, typeObject, "%s declares no %s", path, typeName)
	valueType := typeObject.Type()

	seen := map[T]bool{}
	for expr, value := range info.Types {
		if value.Value == nil || !types.Identical(value.Type, valueType) {
			continue
		}
		require.Equal(t, constant.String, value.Value.Kind(), "a constant of %s's type %s is no string: %s", path, typeName, types.ExprString(expr))
		seen[T(constant.StringVal(value.Value))] = true
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			ast.Inspect(decl, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) != 1 || !info.Types[call.Fun].IsType() || !types.Identical(info.Types[call.Fun].Type, valueType) || info.Types[call.Args[0]].Value != nil {
					return true
				}
				site := "a declaration outside any function: " + types.ExprString(call)
				if function, ok := decl.(*ast.FuncDecl); ok {
					site = function.Name.Name + ": " + types.ExprString(call)
				}
				require.Equal(t, conversion, site, "%s makes a value of its type %s from a string at run time at %s, so the values it holds cannot be read from its source", path, typeName, fset.Position(call.Pos()))
				return true
			})
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// admittedServiceGrants are the grants this service answers a connection of an entry of kind with,
// as decideService builds them for the widest entry of that kind (every project, or, for an edge
// listener, one project it lists), less those scopeGrant and guard refuse: the kind's one grant
// (serviceGrant), or, for the operator, natsgrant.Operator of each of phases (decideOperator),
// since one operator entry is answered under whichever phase the hold row holds when it connects.
func admittedServiceGrants(kind config.NATSServiceKind, phases []natsgrant.OperatorPhase) []natsgrant.Permissions {
	entry := config.NATSService{Kind: kind, MachineID: "example-machine", AllProjects: true}
	if kind == config.NATSEdgeListener {
		entry.AllProjects, entry.Projects = false, []string{"PRIV"}
	}
	grants := []natsgrant.Permissions{serviceGrant(entry)}
	if kind == config.NATSOperator {
		grants = nil
		for _, phase := range phases {
			grants = append(grants, natsgrant.Operator(phase))
		}
	}
	var admitted []natsgrant.Permissions
	for _, grant := range grants {
		if scoped, err := scopeGrant(entry, grant); err == nil && guard(scoped) == nil {
			admitted = append(admitted, scoped)
		}
	}
	return admitted
}

// reachesNotifications and publishesNotifications are the gate's rule
// (lib/legacy-subjects-gate-publishers.jq, reaches_notifications and can_publish_notifications): a
// publish allow entry reaches a notification subject when it is `>` or has two tokens or more, the
// first notifications or `*`; an allow list that is empty or holds a blank entry allows every
// subject; and a deny list takes a grant's notification subjects away only through one entry that
// matches them all.
func reachesNotifications(pattern string) bool {
	pattern = strings.TrimSpace(pattern)
	tokens := strings.Split(pattern, ".")
	return pattern == ">" || (len(tokens) >= 2 && (tokens[0] == "notifications" || tokens[0] == "*"))
}

func publishesNotifications(grant natsgrant.Permissions) bool {
	open := len(grant.Publish) == 0 || slices.ContainsFunc(grant.Publish, func(entry string) bool { return strings.TrimSpace(entry) == "" })
	if !open && !slices.ContainsFunc(grant.Publish, reachesNotifications) {
		return false
	}
	return !slices.ContainsFunc(grant.PublishDeny, func(entry string) bool {
		entry = strings.TrimSpace(entry)
		return entry == ">" || entry == "*.>" || entry == "notifications.>"
	})
}

// calloutPublishers is the gate's list as the gate reads it. Session is a pointer so a list that
// leaves it out is told from one that says false.
type calloutPublishers struct {
	Kinds   map[string]bool `json:"kinds"`
	Session *bool           `json:"session"`
}

// TestTheGateCountsEveryCalloutKindWhoseGrantPublishesANotificationSubject: the gate's list
// classifies every kind config declares, and no other, by whether this service admits it with a
// grant that may publish a notification subject, and a session's token the same way, each read
// from the grants themselves (serviceGrant, scopeGrant, guard and natsgrant.Session), the
// operator's under every phase natsgrant declares for the hold row (natsgrant.Operator). So a kind
// config gains fails this test until the gate classifies it, and so does a kind whose grant, in
// any phase, gains or loses such a publish.
func TestTheGateCountsEveryCalloutKindWhoseGrantPublishesANotificationSubject(t *testing.T) {
	kinds := declaredValues[config.NATSServiceKind](t, kindConversion)
	for _, known := range []config.NATSServiceKind{config.NATSDispatch, config.NATSListener, config.NATSDaemon, config.NATSMeasure, config.NATSEdgeListener, config.NATSOperator, config.NATSProbe} {
		require.Contains(t, kinds, known, "the read of config's source found the kinds %q, and not %s", kinds, known)
	}
	phases := declaredValues[natsgrant.OperatorPhase](t, noConversion)
	for _, known := range []natsgrant.OperatorPhase{natsgrant.OperatorPhaseReleased, natsgrant.OperatorPhasePrepare, natsgrant.OperatorPhaseHeld} {
		require.Contains(t, phases, known, "the read of natsgrant's source found the phases %q, and not %s", phases, known)
	}
	want := map[string]bool{}
	for _, kind := range kinds {
		want[string(kind)] = slices.ContainsFunc(admittedServiceGrants(kind, phases), publishesNotifications)
	}
	session, err := natsgrant.Session("ses_a")
	require.NoError(t, err)
	wantSession := guard(session) == nil && publishesNotifications(session)
	wantJSON, err := json.Marshal(calloutPublishers{Kinds: want, Session: &wantSession})
	require.NoError(t, err)

	raw, err := os.ReadFile(calloutPublishersFile)
	require.NoError(t, err)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var list calloutPublishers
	require.NoError(t, decoder.Decode(&list))
	require.Equal(t, want, list.Kinds, "%s must classify every kind config declares, and no other, by whether its admitted grant may publish a notification subject: write %s", calloutPublishersFile, wantJSON)
	require.NotNil(t, list.Session, "%s must say whether a session's grant may publish a notification subject: write %s", calloutPublishersFile, wantJSON)
	require.Equal(t, wantSession, *list.Session, "%s must say whether a session's grant may publish a notification subject: write %s", calloutPublishersFile, wantJSON)
}

// The rule itself, held on the shapes the gate's rows hold it on (legacy-subjects-gate.test.sh).
func TestPublishesNotificationsReadsAllowAndDenyAsTheGateDoes(t *testing.T) {
	for _, row := range []struct {
		name  string
		grant natsgrant.Permissions
		want  bool
	}{
		{"notifications.>", natsgrant.Permissions{Publish: []string{"notifications.>"}}, true},
		{"one family", natsgrant.Permissions{Publish: []string{"notifications.agent.>"}}, true},
		{"any first token", natsgrant.Permissions{Publish: []string{"*.>"}}, true},
		{"every subject", natsgrant.Permissions{Publish: []string{">"}}, true},
		{"no allow list", natsgrant.Permissions{}, true},
		{"a blank entry", natsgrant.Permissions{Publish: []string{" "}}, true},
		{"a deny of one family", natsgrant.Permissions{Publish: []string{">"}, PublishDeny: []string{"notifications.agent.>"}}, true},
		{"a deny of every notification subject", natsgrant.Permissions{Publish: []string{">"}, PublishDeny: []string{"notifications.>"}}, false},
		{"one token", natsgrant.Permissions{Publish: []string{"notifications"}}, false},
		{"another family", natsgrant.Permissions{Publish: []string{"$JS.API.>", "_INBOX.>", "agentstream.*.frames"}}, false},
	} {
		require.Equal(t, row.want, publishesNotifications(row.grant), row.name)
	}
}

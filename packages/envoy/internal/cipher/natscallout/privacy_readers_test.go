package natscallout

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/api"
)

// privacyDecisions is the one decision a person makes for every kind of service: whether an entry
// of it, with the whole grant, reads every private project and its Slack channel ids at the
// broker's GET /v1/project-privacy. A listener reads it for its privacy decisions, and the privacy
// measurement times that read on the listener's path (plan C's measurement Job). Every other kind
// does not.
var privacyDecisions = map[Kind]bool{
	KindDispatch:       false,
	KindListener:       true,
	KindDaemon:         false,
	KindMeasure:        false,
	KindPrivacyMeasure: true,
}

// declaredKinds is every constant of type Kind this package declares, by name, as the type checker
// reads the package's own non-test files: a constant is one whatever way it is written (`X Kind =
// "x"`, `X = Kind("x")`), so a kind added there is a kind this test sees. The package's imports are
// not read: each resolves to an empty package and the checker's errors over them are dropped,
// since Kind is a string type of this package and no constant of it depends on an import.
func declaredKinds(t *testing.T) map[string]Kind {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	files := token.NewFileSet()
	var parsed []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, path, nil, 0)
		require.NoError(t, err)
		parsed = append(parsed, file)
	}
	config := types.Config{Importer: emptyImports{}, Error: func(error) {}}
	pkg, _ := config.Check("natscallout", files, parsed, nil)
	kind, ok := pkg.Scope().Lookup("Kind").(*types.TypeName)
	require.True(t, ok, "the package declares no type Kind")
	kinds := map[string]Kind{}
	for _, name := range pkg.Scope().Names() {
		value, ok := pkg.Scope().Lookup(name).(*types.Const)
		if !ok || !types.Identical(value.Type(), kind.Type()) {
			continue
		}
		require.Equal(t, constant.String, value.Val().Kind(), "the Kind constant %s has no string value", name)
		kinds[name] = Kind(constant.StringVal(value.Val()))
	}
	return kinds
}

// emptyImports answers every import with an empty package, for declaredKinds.
type emptyImports struct{}

func (emptyImports) Import(path string) (*types.Package, error) {
	pkg := types.NewPackage(path, filepath.Base(path))
	pkg.MarkComplete()
	return pkg, nil
}

// TestEveryKindHasAPrivacyDecision: every Kind constant the package declares has a row in
// privacyDecisions, and Kind.ReadsProjectPrivacy answers each as its row says. A kind added
// without a row fails here until someone decides whether it reads every private project; the
// default is no.
func TestEveryKindHasAPrivacyDecision(t *testing.T) {
	declared := declaredKinds(t)
	require.NotEmpty(t, declared, "this test read no Kind constant from the package's source")
	for name, kind := range declared {
		want, decided := privacyDecisions[kind]
		if !decided {
			t.Errorf("the kind %s (%q) has no row in privacyDecisions: decide whether it reads every private project at GET /v1/project-privacy (the default is no), and add its row", name, kind)
			continue
		}
		require.Equal(t, want, kind.ReadsProjectPrivacy(), "the kind %q reads project privacy: %v, want %v", kind, kind.ReadsProjectPrivacy(), want)
	}
	for kind := range privacyDecisions {
		require.True(t, slices.Contains(slices.Collect(maps.Values(declared)), kind), "privacyDecisions has a row for %q, which the package does not declare", kind)
	}
}

// TestOnlyWholeGrantReadersAreAnsweredTheRoute: PrivacyReaders keeps, from an entry of every kind
// and a listener held to its exchange projects, exactly the whole-grant listener and the privacy
// measurement, each named by its entry and the measurement marked as one; an account listed
// nowhere, a session and a scoped listener are none.
func TestOnlyWholeGrantReadersAreAnsweredTheRoute(t *testing.T) {
	services, err := ParseServices("listener/example-hub/*=" + listenerAccount +
		" listener/example-scoped/-=system:serviceaccount:example-ns:example-scoped-listener" +
		" dispatch/example-dispatch/*=system:serviceaccount:dispatch:dispatch" +
		" daemon/-/*=" + daemonAccount +
		" measure/-/*=google:" + measureAccount +
		" privacy-measure/project-privacy-measure/*=" + privacyMeasureAccount)
	require.NoError(t, err)
	require.Equal(t, api.PrivacyReaders{
		K8s: map[string]api.PrivacyReader{
			listenerAccount:       {Name: "listener/example-hub"},
			privacyMeasureAccount: {Name: "privacy-measure/project-privacy-measure", Measurement: true},
		},
		Google: map[string]api.PrivacyReader{},
	}, PrivacyReaders(services))
}

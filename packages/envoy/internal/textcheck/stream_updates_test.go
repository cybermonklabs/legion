package textcheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// testnatsImport is the package of the one function a test updates a stream through,
// testnats.UpdateStream, and streamUpdateFixture is that function, by its file.
// rawStreamUpdateSubject is the prefix of a raw JetStream stream-update request's subject.
const (
	testnatsImport         = envoyModule + "/internal/testnats"
	streamUpdateFixture    = "internal/testnats/streams.go"
	rawStreamUpdateSubject = "$JS.API.STREAM.UPDATE."
)

// streamUpdateMethods are the methods that send a stream update: the legacy JetStream context's
// and the jetstream package's UpdateStream, and the jetstream package's CreateOrUpdateStream.
var streamUpdateMethods = map[string]bool{"UpdateStream": true, "CreateOrUpdateStream": true}

// directStreamUpdate is one reference to a stream update method (streamUpdateMethods), or one raw
// JetStream stream-update request, that a test makes itself: path and line, and its top-level
// function.
type directStreamUpdate struct {
	path     string
	line     int
	function string
}

func (u directStreamUpdate) key() string { return u.path + " " + u.function }

func (u directStreamUpdate) String() string {
	return fmt.Sprintf("%s:%d %s", u.path, u.line, u.function)
}

// goFiles calls visit with every Go file under root (packages/envoy), by its path and its
// slash-separated path from root, and whether it is a test source: a test file, or a file of a test
// helper package (a non-test file of a package whose directory's name begins or ends with "test",
// as testnats and cmdtest do). It skips testdata, node_modules, vendor and hidden directories. Both
// source checks read the tree through it, so they judge the same files: the update check parses
// the test sources alone (testSources), and the create check every file, to type-check the packages
// the test sources are in (typedTestUnits).
func goFiles(root string, visit func(path, rel string, test bool) error) error {
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
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		dir := filepath.Base(filepath.Dir(path))
		return visit(path, filepath.ToSlash(rel), strings.HasSuffix(path, "_test.go") || strings.HasPrefix(dir, "test") || strings.HasSuffix(dir, "test"))
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", root, err)
	}
	return nil
}

// testSources calls visit with every test source under root (goFiles), parsed, by its
// slash-separated path from root.
func testSources(root string, visit func(rel string, fileSet *token.FileSet, file *ast.File)) error {
	fileSet := token.NewFileSet()
	return goFiles(root, func(path, rel string, test bool) error {
		if !test {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		visit(rel, fileSet, file)
		return nil
	})
}

// directStreamUpdates walks the test sources under root (testSources) and returns every reference
// to a stream update method (streamUpdateMethods), the legacy JetStream context's and the
// jetstream package's alike, and every raw $JS.API.STREAM.UPDATE request. It lets two kinds of
// reference pass. One is testnats.UpdateStream itself, in streamUpdateFixture, and every call to
// it. The other is a reference in a method named UpdateStream: such a method is a fake JetStream
// context that a test hands the code under test, and it stands in for the server under that
// code's own update.
func directStreamUpdates(root string) ([]directStreamUpdate, error) {
	type source struct {
		rel     string
		fileSet *token.FileSet
		file    *ast.File
	}
	var sources []source
	values := subjectValues{local: map[*ast.Object][]ast.Expr{}, global: map[string][]ast.Expr{}}
	if err := testSources(root, func(rel string, fileSet *token.FileSet, file *ast.File) {
		sources = append(sources, source{rel: rel, fileSet: fileSet, file: file})
		values.learn(path.Dir(rel)+" "+file.Name.Name, file)
	}); err != nil {
		return nil, err
	}
	var found []directStreamUpdate
	for _, source := range sources {
		rel, fileSet, file := source.rel, source.fileSet, source.file
		pkg := path.Dir(rel) + " " + file.Name.Name
		fixtureNames := map[string]bool{}
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			if imported != testnatsImport {
				continue
			}
			name := "testnats"
			if spec.Name != nil {
				name = spec.Name.Name
			}
			fixtureNames[name] = true
		}
		for _, decl := range file.Decls {
			function := ""
			if fn, ok := decl.(*ast.FuncDecl); ok {
				function = fn.Name.Name
				if fn.Name.Name == "UpdateStream" && (fn.Recv != nil || rel == streamUpdateFixture) {
					continue
				}
			}
			record := func(pos token.Pos) {
				found = append(found, directStreamUpdate{path: rel, line: fileSet.Position(pos).Line, function: function})
			}
			ast.Inspect(decl, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.CallExpr:
					if rawStreamUpdateRequest(node, values, pkg) {
						record(node.Pos())
					}
				case *ast.SelectorExpr:
					if !streamUpdateMethods[node.Sel.Name] {
						return true
					}
					if pkg, ok := node.X.(*ast.Ident); ok && fixtureNames[pkg.Name] {
						return true
					}
					record(node.Pos())
				}
				return true
			})
		}
	}
	return found, nil
}

// subjectValues are the values each variable and constant of the test sources is given, so that
// a subject held in one is read as what it holds: local, by the declaration the parser resolved a
// name to within its file (a declaration, a := and every later = of that name), and global, by a
// package's top-level name, for a file that reads a constant or a variable another file of its
// package declares, keyed "<directory> <package> <name>".
type subjectValues struct {
	local  map[*ast.Object][]ast.Expr
	global map[string][]ast.Expr
}

// learn records the values file, of package pkg ("<directory> <package>"), gives its names.
func (v subjectValues) learn(pkg string, file *ast.File) {
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok {
			for _, spec := range gen.Specs {
				if value, ok := spec.(*ast.ValueSpec); ok && len(value.Values) == len(value.Names) {
					for i, name := range value.Names {
						v.global[pkg+" "+name.Name] = append(v.global[pkg+" "+name.Name], value.Values[i])
					}
				}
			}
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.ValueSpec:
			if len(node.Values) == len(node.Names) {
				for i, name := range node.Names {
					if name.Obj != nil {
						v.local[name.Obj] = append(v.local[name.Obj], node.Values[i])
					}
				}
			}
		case *ast.AssignStmt:
			if len(node.Lhs) == len(node.Rhs) {
				for i, left := range node.Lhs {
					if name, ok := left.(*ast.Ident); ok && name.Obj != nil {
						v.local[name.Obj] = append(v.local[name.Obj], node.Rhs[i])
					}
				}
			}
		}
		return true
	})
}

// of is the values name can hold: those of the declaration the parser resolved it to, or, for a
// name its file does not declare, those of the top-level name of pkg.
func (v subjectValues) of(name *ast.Ident, pkg string) []ast.Expr {
	if name.Obj != nil {
		return v.local[name.Obj]
	}
	return v.global[pkg+" "+name.Name]
}

// rawSendMethods are nats.Conn's sends that take their subject as an argument.
var rawSendMethods = map[string]bool{"Request": true, "RequestWithContext": true, "Publish": true, "PublishRequest": true}

// rawStreamUpdateRequest reports whether call, in package pkg, is a send (rawSendMethods) of a
// JetStream stream-update subject (streamUpdateSubject). Every such request bypasses
// testnats.UpdateStream's read-back and its narrowing guard. A watch of that subject, or a test of
// a string against it, sends nothing.
func rawStreamUpdateRequest(call *ast.CallExpr, values subjectValues, pkg string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !rawSendMethods[selector.Sel.Name] {
		return false
	}
	return slices.ContainsFunc(call.Args, func(arg ast.Expr) bool { return streamUpdateSubject(arg, values, pkg, map[ast.Expr]bool{}) })
}

// streamUpdateSubject reports whether subject is a JetStream stream-update subject as the source
// spells it: a string literal that begins with rawStreamUpdateSubject, such a literal with more
// joined after it by +, a fmt.Sprintf whose format is such a literal, or a variable or constant
// any of whose values (subjectValues) is one. seen holds the expressions already read, so a value
// that joins its own name ends.
func streamUpdateSubject(subject ast.Expr, values subjectValues, pkg string, seen map[ast.Expr]bool) bool {
	if seen[subject] {
		return false
	}
	seen[subject] = true
	switch subject := subject.(type) {
	case *ast.BasicLit:
		text, err := strconv.Unquote(subject.Value)
		return subject.Kind == token.STRING && err == nil && strings.HasPrefix(text, rawStreamUpdateSubject)
	case *ast.ParenExpr:
		return streamUpdateSubject(subject.X, values, pkg, seen)
	case *ast.BinaryExpr:
		return subject.Op == token.ADD && streamUpdateSubject(subject.X, values, pkg, seen)
	case *ast.Ident:
		return slices.ContainsFunc(values.of(subject, pkg), func(value ast.Expr) bool { return streamUpdateSubject(value, values, pkg, seen) })
	case *ast.CallExpr:
		selector, ok := subject.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Sprintf" || len(subject.Args) == 0 {
			return false
		}
		fmtPkg, ok := selector.X.(*ast.Ident)
		return ok && fmtPkg.Name == "fmt" && streamUpdateSubject(subject.Args[0], values, pkg, seen)
	}
	return false
}

// A test updates a stream only through testnats.UpdateStream. A test that sends the update itself
// and waits for its answer fails when the answer never comes: nats-server answers an update only
// from the member that leads the stream's group when it applies it, so an update applied while
// the group elects a leader gets no answer. testnats.UpdateStream reads the stream until the
// update shows, under the test's own deadline, and never sends the update again.
func TestNoTestUpdatesAStreamExceptThroughTestnatsUpdateStream(t *testing.T) {
	found, err := directStreamUpdates(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		var lines []string
		for _, update := range found {
			lines = append(lines, update.String())
		}
		t.Errorf("%d stream updates a test sends itself and waits on for an answer that may never come; send each through testnats.UpdateStream:\n%s",
			len(found), strings.Join(lines, "\n"))
	}
}

// plantedStreamUpdates is a tree under the envoy module's path for the control below: demo's tests
// update a stream themselves in each shape the check names, a raw subject held in a variable, a
// constant of the same file and one of another file of the package among them, and through the
// fixture and a fake JetStream context, which it lets pass; one watches the update subject, one
// tests a string against it and one sends to a variable that holds another subject, which send no
// update; stacktest is a test helper that updates one itself, and production a package that is
// neither a test nor a helper.
var plantedStreamUpdates = map[string]string{
	"internal/testnats/streams.go": `package testnats

import natsgo "github.com/nats-io/nats.go"

func UpdateStream(js natsgo.JetStreamContext, config natsgo.StreamConfig) {
	js.UpdateStream(&config)
}

func fixtureCaller(js natsgo.JetStreamContext, config natsgo.StreamConfig) { UpdateStream(js, config) }
`,
	"internal/demo/demo_test.go": `package demo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	fixture "github.com/sjawhar/envoy/internal/testnats"
)

type rig struct{ js nats.JetStreamContext }

type fake struct{ nats.JetStreamContext }

func (f fake) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return f.JetStreamContext.UpdateStream(cfg, opts...)
}

func operator(js nats.JetStreamContext, cfg nats.StreamConfig) { js.UpdateStream(&cfg) }

func (r rig) refuse(cfg nats.StreamConfig) { r.js.UpdateStream(&cfg) }

func narrow(js nats.JetStreamContext, cfg *nats.StreamConfig) {
	update := func() { js.UpdateStream(cfg) }
	update()
}

func newAPI(js jetstream.JetStream, cfg jetstream.StreamConfig) { js.UpdateStream(context.Background(), cfg) }

func createOrUpdate(js jetstream.JetStream, cfg jetstream.StreamConfig) { js.CreateOrUpdateStream(context.Background(), cfg) }

func methodValue(js nats.JetStreamContext, cfg nats.StreamConfig) {
	update := js.UpdateStream
	update(&cfg)
}

func rawRequest(conn *nats.Conn) { _, _ = conn.Request("$JS.API.STREAM.UPDATE.RAW", nil, time.Second) }

func rawJoined(conn *nats.Conn, stream string) { _, _ = conn.Request("$JS.API.STREAM.UPDATE."+stream, nil, time.Second) }

func rawFormatted(conn *nats.Conn, stream string) { _ = conn.Publish(fmt.Sprintf("$JS.API.STREAM.UPDATE.%s", stream), nil) }

func rawNamesTheSubjectElsewhere(conn *nats.Conn, subject string) { _ = conn.Publish(subject, []byte("$JS.API.STREAM.UPDATE.NOT_A_SUBJECT"+subject)) }

func watchesUpdates(conn *nats.Conn) { _, _ = conn.Subscribe("$JS.API.STREAM.UPDATE.>", func(*nats.Msg) {}) }

func isUpdate(subject string) bool { return strings.HasPrefix(subject, "$JS.API.STREAM.UPDATE.") }

func throughFixture(js nats.JetStreamContext, cfg nats.StreamConfig) { fixture.UpdateStream(js, cfg) }

func readOnly(js nats.JetStreamContext) { js.StreamInfo("S") }

const fileUpdateSubject = "$JS.API.STREAM.UPDATE.CONST"

func rawHeld(conn *nats.Conn, stream string) {
	subject := "$JS.API.STREAM.UPDATE." + stream
	_, _ = conn.Request(subject, nil, time.Second)
}

func rawHeldAfterAnother(conn *nats.Conn, stream string) {
	subject := "notifications.github.x"
	subject = "$JS.API.STREAM.UPDATE." + stream
	_ = conn.Publish(subject, nil)
}

func rawConstant(conn *nats.Conn) { _ = conn.Publish(fileUpdateSubject, nil) }

func rawOtherFilesConstant(conn *nats.Conn) { _ = conn.Publish(packageUpdateSubject, nil) }

func rawHeldOtherSubject(conn *nats.Conn) {
	subject := "notifications.github.x"
	_ = conn.Publish(subject, nil)
}
`,
	"internal/demo/subjects_test.go": `package demo

const packageUpdateSubject = "$JS.API.STREAM.UPDATE.PACKAGE"
`,
	"internal/stacktest/stack.go":   "package stacktest\n\nimport \"github.com/nats-io/nats.go\"\n\nfunc Start(js nats.JetStreamContext, cfg nats.StreamConfig) { js.UpdateStream(&cfg) }\n",
	"internal/production/update.go": "package production\n\nimport \"github.com/nats-io/nats.go\"\n\nfunc Keep(js nats.JetStreamContext, cfg nats.StreamConfig) { js.UpdateStream(&cfg) }\n",
}

// The control of the check above: in a planted tree it finds a stream update a test or a test
// helper sends itself in every shape it names, and nothing in the fixture, in a call to it, in a
// fake JetStream context's UpdateStream, in a watch of the update subject or a test of a string
// against it, or in a package that is neither a test nor a helper.
func TestTheStreamUpdateCheckFindsEveryShapeItNamesAndNothingElse(t *testing.T) {
	root := t.TempDir()
	for name, contents := range plantedStreamUpdates {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found, err := directStreamUpdates(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, update := range found {
		got = append(got, update.key())
	}
	slices.Sort(got)
	want := []string{
		"internal/demo/demo_test.go createOrUpdate",
		"internal/demo/demo_test.go methodValue",
		"internal/demo/demo_test.go narrow",
		"internal/demo/demo_test.go newAPI",
		"internal/demo/demo_test.go operator",
		"internal/demo/demo_test.go rawConstant",
		"internal/demo/demo_test.go rawFormatted",
		"internal/demo/demo_test.go rawHeld",
		"internal/demo/demo_test.go rawHeldAfterAnother",
		"internal/demo/demo_test.go rawJoined",
		"internal/demo/demo_test.go rawOtherFilesConstant",
		"internal/demo/demo_test.go rawRequest",
		"internal/demo/demo_test.go refuse",
		"internal/stacktest/stack.go Start",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the check found %q in the planted tree, want %q", got, want)
	}
}

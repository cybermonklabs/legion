package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// decisionsGoneBuildStamp is the commit the build this test boots registers under, so an
// announcement the test writes under another commit is the build a rollback reaches.
const decisionsGoneBuildStamp = "0d7d0d7d0d7d0d7d0d7d0d7d0d7d0d7d0d7d0d7d"

// The checks the boot's retirement narrows, each with a write of `decisions` it then refuses.
var decisionsChecks = []struct{ name, write string }{
	{"artifacts_role_check", `insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
		values ('GONE-1', 'GONE', 'refused-md', 'refused.md', 'doc', false, 'decisions', '{"kind":"user","id":"alice"}')`},
	{"projects_gated_steps_check", `update projects set gated_steps = '{decisions}' where key = 'GONE'`},
	{"design_gates_step_check", `insert into design_gates (issue, step, artifact_id) values ('GONE-1', 'decisions', 'refused')`},
}

// What stands at the root's head of what the release that retired each issue's decisions document
// kept for one release: nothing.
//
// The code goes with no condition. The model package exports no model.Decision and no
// model.DecisionLink, and the docs package declares no rollWindowAskAttributes, read from the
// packages' own declarations, since a test that named them would not compile once they are gone.
// The conversion's runner docs/decision_step.go is no file of the tree, and cmd/dispatch's
// subcommands hold neither restore-answered-asks nor convert-decisions, read from
// subcommands.go and from envoy-dispatch's own list. DocumentRoleSchema and GATED_STEPS of the
// contracts hold no `decisions`. Before each search for an absence the test finds a planted
// instance in a scratch copy, so a search that cannot find is no pass.
//
// The stored objects go in a step of every boot, over five databases this build boots on:
//   - one whose newest announcement of another build lists every kind this build lists,
//     stores_moved among them, and where decisions_documents() answers no document: the step
//     deletes the design_gates rows of the step `decisions`, drops the function and narrows the
//     three checks, each of which then refuses `decisions` by name, and a second boot changes
//     nothing. Over it the issue read answers no decisions_artifact_id,
//     GET /api/v1/issues/{key}/decisions answers 404 NOT_FOUND, a new document named decisions.md
//     or slugged decisions-md is refused 409 DECISIONS_ARE_NODES and the issue holds no new
//     document, a Keep of a `##` heading of a retired decisions document answers 404
//     TARGET_NOT_FOUND and writes no decision_keeps row, the gated-steps write of `decisions` is
//     400 INVALID_PROJECT and the design-gates write of it 400 INVALID_ISSUE, each naming the step,
//     and /healthz holds no decisions_step_owed;
//   - two whose newest such announcement is of a release before this one, which lists no
//     stores_moved: the release before the decisions release, which lists none of the kinds that
//     release added either, and the issue-room release, the release before this one, which lists
//     every other kind this build lists and whose issue read still calls decisions_documents().
//     Over each every object stays, a call of decisions_documents() answers after the boot, the
//     server serves, and the log names the announcement and stores_moved as what it lacks;
//   - one where decisions_documents() answers a document, a document with no role that its slug
//     names: the function and the checks stay, the server serves, and the log names the document;
//   - one this boot created from empty: the step deletes them at once.
func TestNothingAdmitsTheDecisionsDocumentAfterTheRoot(t *testing.T) {
	root := moduleRoot(t)
	t.Run("code", func(t *testing.T) { requireTheDecisionsCodeGone(t, root) })
	t.Run("contracts", func(t *testing.T) { requireTheContractListsHoldNoDecisions(t, root) })

	binary := cmdtest.BuildStamped(t, "envoy-dispatch", decisionsGoneBuildStamp)
	t.Run("released", func(t *testing.T) { requireTheReleasedDatabaseRetires(t, binary) })
	t.Run("release before", func(t *testing.T) { requireTheReleaseBeforeKeepsTheObjects(t, binary) })
	t.Run("document stands", func(t *testing.T) { requireAStandingDocumentKeepsTheObjects(t, binary) })
	t.Run("from empty", func(t *testing.T) { requireAnEmptyDatabaseRetiresAtOnce(t, binary) })
}

// moduleRoot is packages/envoy, the directory two above this package's.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// declaredNames is every top-level name the non-test Go files of dir declare: types, functions
// (methods by their name alone), and values.
func declaredNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := map[string]bool{}
	files := token.NewFileSet()
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, filepath.Join(dir, entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, declared := range file.Decls {
			switch declared := declared.(type) {
			case *ast.FuncDecl:
				names[declared.Name.Name] = true
			case *ast.GenDecl:
				for _, spec := range declared.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						names[spec.Name.Name] = true
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							names[name.Name] = true
						}
					}
				}
			}
		}
	}
	return names
}

// subcommandNames is the name of each entry of the subcommands table subcommands.go at path
// declares: the first field of each element of `var subcommands`.
func subcommandNames(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var names []string
	ast.Inspect(file, func(node ast.Node) bool {
		spec, isValue := node.(*ast.ValueSpec)
		if !isValue || len(spec.Names) != 1 || spec.Names[0].Name != "subcommands" || len(spec.Values) != 1 {
			return true
		}
		table, isTable := spec.Values[0].(*ast.CompositeLit)
		if !isTable {
			return false
		}
		for _, element := range table.Elts {
			entry, isEntry := element.(*ast.CompositeLit)
			if !isEntry || len(entry.Elts) == 0 {
				continue
			}
			if name, isName := entry.Elts[0].(*ast.BasicLit); isName && name.Kind == token.STRING {
				if unquoted, err := strconv.Unquote(name.Value); err == nil {
					names = append(names, unquoted)
				}
			}
		}
		return false
	})
	return names
}

// scratchCopy writes text into a file named name under a scratch directory of its own, and
// answers the directory.
func scratchCopy(t *testing.T, name, text string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func requireTheDecisionsCodeGone(t *testing.T, root string) {
	model := filepath.Join(root, "internal", "dispatch", "model")
	docs := filepath.Join(root, "internal", "dispatch", "docs")
	planted := declaredNames(t, scratchCopy(t, "planted.go",
		"package model\n\ntype Decision struct{}\n\ntype DecisionLink struct{}\n\nfunc rollWindowAskAttributes() {}\n"))
	for _, name := range []string{"Decision", "DecisionLink", "rollWindowAskAttributes"} {
		if !planted[name] {
			t.Fatalf("the scan finds no %s in a scratch copy that declares it: it cannot find what it looks for", name)
		}
	}
	declaredModel := declaredNames(t, model)
	for _, name := range []string{"Decision", "DecisionLink"} {
		if declaredModel[name] {
			t.Errorf("internal/dispatch/model exports model.%s, a compatibility object the root deletes", name)
		}
	}
	if declaredNames(t, docs)["rollWindowAskAttributes"] {
		t.Errorf("internal/dispatch/docs declares rollWindowAskAttributes, which the root deletes")
	}

	runner := filepath.Join(scratchCopy(t, "decision_step.go", "package docs\n"), "decision_step.go")
	if _, err := os.Stat(runner); err != nil {
		t.Fatalf("the scan finds no decision_step.go in a scratch copy that holds it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(docs, "decision_step.go")); err == nil {
		t.Errorf("internal/dispatch/docs/decision_step.go, the conversion's runner, is a file of the tree")
	} else if !os.IsNotExist(err) {
		t.Fatalf("read internal/dispatch/docs/decision_step.go: %v", err)
	}

	source, err := os.ReadFile(filepath.Join(root, "cmd", "dispatch", "subcommands.go"))
	if err != nil {
		t.Fatal(err)
	}
	plantedTable := strings.Replace(string(source), "var subcommands = []subcommand{",
		"var subcommands = []subcommand{\n\t{\"restore-answered-asks\", nil},\n\t{\"convert-decisions\", nil},", 1)
	plantedNames := subcommandNames(t, filepath.Join(scratchCopy(t, "subcommands.go", plantedTable), "subcommands.go"))
	if !slices.Contains(plantedNames, "restore-answered-asks") || !slices.Contains(plantedNames, "convert-decisions") {
		t.Fatalf("the scan finds %v in a scratch copy that plants both hand commands: it cannot find what it looks for", plantedNames)
	}
	names := subcommandNames(t, filepath.Join(root, "cmd", "dispatch", "subcommands.go"))
	if len(names) == 0 {
		t.Fatalf("subcommands.go declares no subcommand the scan reads")
	}
	var listed strings.Builder
	runSubcommand(context.Background(), []string{"no-such-subcommand"}, settingValues{}, &strings.Builder{}, &listed)
	for _, gone := range []string{"restore-answered-asks", "convert-decisions"} {
		if slices.Contains(names, gone) {
			t.Errorf("subcommands.go names %q, a hand command of the conversion the root deletes", gone)
		}
		if strings.Contains(listed.String(), gone) {
			t.Errorf("envoy-dispatch lists %q among its subcommands: %s", gone, listed.String())
		}
	}
}

// enumMembers is the quoted members of the first array literal after marker in text.
func enumMembers(text, marker string) ([]string, bool) {
	at := strings.Index(text, marker)
	if at < 0 {
		return nil, false
	}
	rest := text[at+len(marker):]
	open, close := strings.Index(rest, "["), strings.Index(rest, "]")
	if open < 0 || close < open {
		return nil, false
	}
	return regexp.MustCompile(`"([^"]*)"`).FindAllString(rest[open:close], -1), true
}

// gatedSteps is the members GATED_STEPS holds in legion-roles.ts' text, following one name it
// is set to (GATED_STEPS = DESIGN_GATE_STEPS).
func gatedSteps(t *testing.T, text string) []string {
	t.Helper()
	set := regexp.MustCompile(`export const GATED_STEPS = ([A-Z_]+);`).FindStringSubmatch(text)
	if set != nil {
		members, found := enumMembers(text, "export const "+set[1]+" =")
		if !found {
			t.Fatalf("legion-roles.ts sets GATED_STEPS to %s, which it does not declare", set[1])
		}
		return members
	}
	members, found := enumMembers(text, "export const GATED_STEPS =")
	if !found {
		t.Fatalf("legion-roles.ts declares no GATED_STEPS the scan reads")
	}
	return members
}

func requireTheContractListsHoldNoDecisions(t *testing.T, root string) {
	contracts := filepath.Join(root, "..", "contracts", "src")
	documents, err := os.ReadFile(filepath.Join(contracts, "dispatch-api-documents.ts"))
	if err != nil {
		t.Fatal(err)
	}
	roles, err := os.ReadFile(filepath.Join(contracts, "legion-roles.ts"))
	if err != nil {
		t.Fatal(err)
	}
	const roleMarker = "export const DocumentRoleSchema = z.enum("
	plantedDocuments, err := os.ReadFile(filepath.Join(scratchCopy(t, "dispatch-api-documents.ts",
		strings.Replace(string(documents), roleMarker+"[", roleMarker+`["decisions", `, 1)), "dispatch-api-documents.ts"))
	if err != nil {
		t.Fatal(err)
	}
	plantedRoles, found := enumMembers(string(plantedDocuments), roleMarker)
	if !found || !slices.Contains(plantedRoles, `"decisions"`) {
		t.Fatalf("the scan finds %v in a scratch copy of DocumentRoleSchema that plants decisions", plantedRoles)
	}
	plantedRolesFile := regexp.MustCompile(`export const GATED_STEPS = [^;]*;`).ReplaceAllString(string(roles),
		`export const GATED_STEPS = ["decisions", ...DESIGN_GATE_STEPS] as const;`)
	plantedText, err := os.ReadFile(filepath.Join(scratchCopy(t, "legion-roles.ts", plantedRolesFile), "legion-roles.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if planted := gatedSteps(t, string(plantedText)); !slices.Contains(planted, `"decisions"`) {
		t.Fatalf("the scan finds %v in a scratch copy of GATED_STEPS that plants decisions", planted)
	}
	if members, found := enumMembers(string(documents), roleMarker); !found || slices.Contains(members, `"decisions"`) {
		t.Errorf("DocumentRoleSchema holds %v (found %t), want no decisions", members, found)
	}
	if members := gatedSteps(t, string(roles)); slices.Contains(members, `"decisions"`) || len(members) == 0 {
		t.Errorf("GATED_STEPS holds %v, want the design-gate steps and no decisions", members)
	}
}

// decisionsDatabase is one database this build boots on, as the test reads it after the boot.
type decisionsDatabase struct {
	url     string
	process *dispatchProcess
}

func (d *decisionsDatabase) exec(t *testing.T, statement string, args ...any) error {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), d.url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(context.Background(), statement, args...)
	return err
}

func (d *decisionsDatabase) scan(t *testing.T, statement string, into ...any) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), d.url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	if err := conn.QueryRow(context.Background(), statement).Scan(into...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// seedGone writes, as a release before this one left them, the project GONE with an issue GONE-1,
// its spec, a design_gates row of the step spec and one of the step `decisions`, a retired
// decisions document holding one `##` heading, and an announcement of another build under commit
// listing kinds.
func seedGone(t *testing.T, d *decisionsDatabase, commit string, kinds []string) (spec, retired string) {
	t.Helper()
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee, gated_steps) values ('GONE', 'Gone', 'alice', '{criteria,spec}')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
		 values ('GONE-1', 'GONE', 1, 'What the root deletes', '{"kind":"user","id":"alice"}', 'U')`,
	} {
		if err := d.exec(t, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	d.scan(t, `insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('GONE-1', 'GONE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}') returning id::text`, &spec)
	d.scan(t, `insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, retired_at)
		values ('GONE-1', 'GONE', 'decisions-md', 'decisions.md', 'doc', false, '{"kind":"user","id":"alice"}', now())
		returning id::text`, &retired)
	for _, statement := range []string{
		`insert into artifact_versions (artifact_id, number, markdown, authors)
		 values ('` + retired + `', 1, E'# Decisions\n\n## The store holds the corpus\n\nWe chose Postgres.\n', '[{"kind":"user","id":"alice"}]')`,
		`insert into design_gates (issue, step, artifact_id) values ('GONE-1', 'spec', '` + spec + `'), ('GONE-1', 'decisions', '` + retired + `')`,
	} {
		if err := d.exec(t, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	if err := d.exec(t, `insert into server_build_announcements (commit, tuple_kinds) values ($1, $2)`, commit, kinds); err != nil {
		t.Fatalf("announce %s: %v", commit, err)
	}
	return spec, retired
}

// boot starts the build on d and waits for it to serve.
func (d *decisionsDatabase) boot(t *testing.T, binary string) {
	t.Helper()
	d.process = startDispatchProcessWith(t, d.url, dispatchOptions{Binary: binary})
	d.process.waitHealthy(t)
}

func (d *decisionsDatabase) stop(t *testing.T) {
	t.Helper()
	d.process.Terminate(t)
	d.process.WaitExit(t, "SIGTERM")
}

// objects is what stands of the objects the boot's retirement deletes: whether the function
// stands, how many design_gates rows of the step `decisions` there are, and each check's
// definition.
func (d *decisionsDatabase) objects(t *testing.T) (bool, int, map[string]string) {
	t.Helper()
	var function bool
	var rows int
	d.scan(t, `select to_regprocedure('decisions_documents()') is not null`, &function)
	d.scan(t, `select count(*) from design_gates where step = 'decisions'`, &rows)
	checks := map[string]string{}
	for _, check := range decisionsChecks {
		var definition string
		d.scan(t, `select pg_get_constraintdef(oid) from pg_constraint where conname = '`+check.name+`'`, &definition)
		checks[check.name] = definition
	}
	return function, rows, checks
}

// requireRetired fails unless the boot's retirement deleted every object over d: no function, no
// design_gates row of the step `decisions`, and each check refusing `decisions` by name.
func requireRetired(t *testing.T, d *decisionsDatabase) {
	t.Helper()
	function, rows, checks := d.objects(t)
	if function {
		t.Errorf("decisions_documents() stands after the boot, want it dropped")
	}
	if rows != 0 {
		t.Errorf("%d design_gates rows of the step decisions stand after the boot, want none", rows)
	}
	for _, check := range decisionsChecks {
		if strings.Contains(checks[check.name], "decisions") {
			t.Errorf("%s still admits decisions after the boot: %s", check.name, checks[check.name])
			continue
		}
		err := d.exec(t, check.write)
		if err == nil || !strings.Contains(err.Error(), check.name) {
			t.Errorf("a write of decisions = %v, want %s to refuse it by name", err, check.name)
		}
	}
}

// requireKept fails unless every object stands over d, naming the first check it finds narrowed.
func requireKept(t *testing.T, d *decisionsDatabase, rowsWant int) {
	t.Helper()
	function, rows, checks := d.objects(t)
	for _, check := range decisionsChecks {
		if !strings.Contains(checks[check.name], "decisions") {
			t.Fatalf("%s is narrow when the server starts (%s), want it kept while its guard does not hold", check.name, checks[check.name])
		}
	}
	if !function {
		t.Errorf("decisions_documents() is dropped, want it kept while its guard does not hold")
	}
	if rows != rowsWant {
		t.Errorf("%d design_gates rows of the step decisions stand, want the %d seeded", rows, rowsWant)
	}
}

func newDecisionsDatabase(t *testing.T) *decisionsDatabase {
	t.Helper()
	return &decisionsDatabase{url: storetest.Open(t).Pool.Config().ConnString()}
}

func requireTheReleasedDatabaseRetires(t *testing.T, binary string) {
	d := newDecisionsDatabase(t)
	_, retired := seedGone(t, d, "1150115011501150115011501150115011501150", refs.TupleKinds())
	d.boot(t, binary)
	requireRetired(t, d)
	function, rows, checks := d.objects(t)

	p := d.process
	var issue map[string]any
	if err := json.Unmarshal(p.call(t, http.MethodGet, "/api/v1/issues/GONE-1", "", http.StatusOK), &issue); err != nil {
		t.Fatal(err)
	}
	if _, named := issue["decisions_artifact_id"]; named {
		t.Errorf("the issue read answers decisions_artifact_id %v, a member the root deletes", issue["decisions_artifact_id"])
	}
	if answer := p.send(t, http.MethodGet, "/api/v1/issues/GONE-1/decisions", "", asUser(dispatchTestLogin)); answer.status != http.StatusNotFound || answer.code() != "NOT_FOUND" {
		t.Errorf("GET /api/v1/issues/GONE-1/decisions = %s, want 404 NOT_FOUND", answer)
	}
	for _, name := range []string{"decisions.md", "decisions-md"} {
		body, _ := json.Marshal(map[string]string{"name": name, "content": "# Decisions\n\nA new decisions document.\n"})
		if answer := p.send(t, http.MethodPost, "/api/v1/issues/GONE-1/artifacts", string(body), asUser(dispatchTestLogin)); answer.status != http.StatusConflict || answer.code() != "DECISIONS_ARE_NODES" {
			t.Errorf("a new document named %s = %s, want 409 DECISIONS_ARE_NODES", name, answer)
		}
	}
	var standing int
	d.scan(t, `select count(*) from artifacts where issue_key = 'GONE-1' and retired_at is null and slug like 'decisions%'`, &standing)
	if standing != 0 {
		t.Errorf("the issue holds %d new decisions documents after the refusals, want none", standing)
	}
	if answer := p.send(t, http.MethodPost, "/api/v1/artifacts/"+retired+"/blocks/the-store-holds-the-corpus/keep", `{"text_sha256":"sha"}`, asUser(dispatchTestLogin)); answer.status != http.StatusNotFound || answer.code() != "TARGET_NOT_FOUND" {
		t.Errorf("a Keep of a heading of the retired decisions document = %s, want 404 TARGET_NOT_FOUND", answer)
	}
	var keeps int
	d.scan(t, `select count(*) from decision_keeps`, &keeps)
	if keeps != 0 {
		t.Errorf("%d decision_keeps rows after the Keep, want none", keeps)
	}
	if answer := p.send(t, http.MethodPut, "/api/v1/projects/GONE/gated-steps", `{"steps":["decisions"]}`, asUser(dispatchTestLogin)); answer.status != http.StatusBadRequest || answer.code() != "INVALID_PROJECT" || !strings.Contains(string(answer.body), "decisions") {
		t.Errorf("the gated-steps write of decisions = %s, want 400 INVALID_PROJECT naming the step", answer)
	}
	if answer := p.send(t, http.MethodPut, "/api/v1/issues/GONE-1/design-gates", `{"gates":[{"step":"decisions","artifact_id":"`+retired+`"}]}`, asUser(dispatchTestLogin)); answer.status != http.StatusBadRequest || answer.code() != "INVALID_ISSUE" || !strings.Contains(string(answer.body), "decisions") {
		t.Errorf("the design-gates write of decisions = %s, want 400 INVALID_ISSUE naming the step", answer)
	}
	var health map[string]any
	if err := json.Unmarshal(p.call(t, http.MethodGet, "/healthz", "", http.StatusOK), &health); err != nil {
		t.Fatal(err)
	}
	if _, owed := health["decisions_step_owed"]; owed {
		t.Errorf("/healthz answers decisions_step_owed %v, a member the root deletes", health["decisions_step_owed"])
	}

	d.stop(t)
	d.boot(t, binary)
	again, rowsAgain, checksAgain := d.objects(t)
	if again != function || rowsAgain != rows || !mapsEqual(checksAgain, checks) ||
		strings.Contains(d.process.Output.String(), "the boot's retirement deleted its objects") {
		t.Errorf("a second boot changed the objects (function %t, %d rows, %v) or deleted again:\n%s", again, rowsAgain, checksAgain, d.process.Output.String())
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// requireTheReleaseBeforeKeepsTheObjects boots over two databases whose newest announcement of
// another build is of a release before this one: the release before the decisions release, which
// lists none of the kinds that release added, and the issue-room release, the release before this
// one, which lists every other kind this build lists and still reads decisions_documents(). Neither
// lists stores_moved, so the step keeps every object over each, a call of decisions_documents()
// still answers after the boot, as the issue-room release's issue read calls it, and the step logs
// one line naming the announcement and what it lacks.
func requireTheReleaseBeforeKeepsTheObjects(t *testing.T, binary string) {
	for _, before := range []struct {
		name, commit string
		unlisted     []string
	}{
		{"before the decisions release", "1149114911491149114911491149114911491149",
			[]string{"followed_by", "from_ask", "decided_by", processes.CapabilityStoresMoved}},
		{"the issue-room release", "1253125312531253125312531253125312531253", []string{processes.CapabilityStoresMoved}},
	} {
		t.Run(before.name, func(t *testing.T) {
			d := newDecisionsDatabase(t)
			seedGone(t, d, before.commit, slices.DeleteFunc(slices.Clone(refs.TupleKinds()), func(kind string) bool {
				return slices.Contains(before.unlisted, kind)
			}))
			d.boot(t, binary)
			// The function first: a step that dropped it fails here, naming it.
			var standing int
			d.scan(t, `select count(*) from decisions_documents()`, &standing)
			requireKept(t, d, 1)
			output := d.process.Output.String()
			logged := slices.ContainsFunc(strings.Split(output, "\n"), func(line string) bool {
				return strings.Contains(line, `retirement="the decisions document's kept objects"`) && strings.Contains(line, "kept its objects") &&
					strings.Contains(line, "announcement_commit="+before.commit) && strings.Contains(line, "missing=["+processes.CapabilityStoresMoved+"]")
			})
			if !logged {
				t.Errorf("the log names no announcement %s lacking %s for the kept objects:\n%s", before.commit, processes.CapabilityStoresMoved, output)
			}
			d.process.call(t, http.MethodGet, "/api/v1/issues/GONE-1", "", http.StatusOK)
		})
	}
}

func requireAStandingDocumentKeepsTheObjects(t *testing.T, binary string) {
	d := newDecisionsDatabase(t)
	seedGone(t, d, "1150115011501150115011501150115011501150", refs.TupleKinds())
	// GONE-1's own decisions document is retired, and holds the slug, so the one that stands is
	// GONE-2's: stored before the role existed, it holds no role, and its slug alone names it.
	if err := d.exec(t, `insert into issues (key, project_key, number, title, created_by, rank)
		values ('GONE-2', 'GONE', 2, 'Unconverted', '{"kind":"user","id":"alice"}', 'V')`); err != nil {
		t.Fatal(err)
	}
	var standing string
	d.scan(t, `insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('GONE-2', 'GONE', 'decisions-md', 'decisions.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text`, &standing)
	d.boot(t, binary)
	requireKept(t, d, 1)
	output := d.process.Output.String()
	if !strings.Contains(output, "decisions_document="+standing) {
		t.Errorf("the log names no document %s, a decisions document with no role its slug names, which decisions_documents() answers:\n%s", standing, output)
	}
	d.process.call(t, http.MethodGet, "/api/v1/issues/GONE-1", "", http.StatusOK)
}

func requireAnEmptyDatabaseRetiresAtOnce(t *testing.T, binary string) {
	d := &decisionsDatabase{url: storetest.Empty(t)}
	d.boot(t, binary)
	if err := d.exec(t, `insert into projects (key, name, default_assignee) values ('GONE', 'Gone', 'alice')`); err != nil {
		t.Fatal(err)
	}
	if err := d.exec(t, `insert into issues (key, project_key, number, title, created_by, rank)
		values ('GONE-1', 'GONE', 1, 'From empty', '{"kind":"user","id":"alice"}', 'U')`); err != nil {
		t.Fatal(err)
	}
	requireRetired(t, d)
	if !strings.Contains(d.process.Output.String(), "created_from_empty=true") {
		t.Errorf("the log says nothing of a database created from empty:\n%s", d.process.Output.String())
	}
}

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The old layout as main held it the moment routes moved into files of their own (843a9f45),
// frozen: a branch made before the move holds this layout, and the splitter must read it.
const (
	oldTableFixture  = "testdata/old_routes_table.go.txt"
	oldShapesFixture = "testdata/old_routes_shapes.go.txt"
)

// The one row of the frozen table whose pattern is a constant another file of package api declares.
const goingPathsFile = "package api\n\nconst goingCataloguePath = \"/api/v1/going/catalogue\"\n"

// A row of the old layout a branch adds beside GET /api/v1/limits, with its shape: two branches
// that each add one there conflict in the old layout, since both insert at one line.
func addedRoute(name string) (row, shape string) {
	return "\t\t{http.MethodGet, \"/api/v1/adopt-test-" + name + "\", authAny, \"Route " + name + ".\", s.getAdoptTest" + strings.ToUpper(name) + "},\n",
		"\t\"GET /api/v1/adopt-test-" + name + "\": {Status: http.StatusOK, Response: routeIndex{}},\n"
}

// scratchRepo is a git repository holding package api in the old layout on main, with its
// commits made by a fixed author.
type scratchRepo struct {
	t    *testing.T
	root string
}

func newScratchRepo(t *testing.T) *scratchRepo {
	t.Helper()
	for name, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid",
		"JJ_CONFIG": "/dev/null", "JJ_USER": "t", "JJ_EMAIL": "t@example.invalid",
	} {
		t.Setenv(name, value)
	}
	repo := &scratchRepo{t: t, root: t.TempDir()}
	repo.git("init", "-q", "-b", "main", ".")
	repo.write(tableFile, fixture(t, oldTableFixture))
	repo.write(shapesFile, fixture(t, oldShapesFixture))
	repo.write("going_paths.go", goingPathsFile)
	repo.commit("the old layout")
	return repo
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// git runs git in the repository and answers its stdout, failing the test on an error.
func (r *scratchRepo) git(args ...string) string {
	r.t.Helper()
	output, err := r.run("git", args...)
	if err != nil {
		r.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return output
}

// run runs a command in the repository and answers its combined output and error.
func (r *scratchRepo) run(name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	command.Dir = r.root
	output, err := command.CombinedOutput()
	return string(output), err
}

func (r *scratchRepo) path(name string) string { return filepath.Join(r.root, apiDir, name) }

func (r *scratchRepo) write(name, content string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(r.path(name)), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(r.path(name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *scratchRepo) read(name string) string {
	r.t.Helper()
	content, err := os.ReadFile(r.path(name))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(content)
}

func (r *scratchRepo) commit(message string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "-m", message)
	return strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

// moveMain makes main the new layout: the old layout split into its route files, and the two
// files the split leaves holding shared code alone.
func (r *scratchRepo) moveMain() map[string][]byte {
	r.t.Helper()
	r.git("checkout", "-q", "main")
	files, err := splitOldLayout(dirTree(filepath.Join(r.root, apiDir)))
	if err != nil {
		r.t.Fatalf("split the old layout: %v", err)
	}
	for name, content := range files {
		r.write(name, string(content))
	}
	r.write(tableFile, "package api\n\n// The new layout's shared code.\n")
	r.write(shapesFile, "package api\n\n// The new layout's shared shapes.\n")
	r.commit("one file per route")
	return files
}

// addRouteOn makes a branch from the base commit that adds one route in the old layout.
func (r *scratchRepo) addRouteOn(branch, base, name string) string {
	r.t.Helper()
	r.git("checkout", "-q", "-b", branch, base)
	row, shape := addedRoute(name)
	table, shapes := r.read(tableFile), r.read(shapesFile)
	limitsRow := strings.Index(table, "{http.MethodGet, \"/api/v1/limits\"")
	limitsShape := strings.Index(shapes, "\t\"GET /api/v1/limits\":")
	if limitsRow < 0 || limitsShape < 0 {
		r.t.Fatal("the frozen old layout holds no GET /api/v1/limits row and shape to add beside")
	}
	rowEnd := limitsRow + strings.Index(table[limitsRow:], "\n") + 1
	shapeEnd := limitsShape + strings.Index(shapes[limitsShape:], "\n") + 1
	r.write(tableFile, table[:rowEnd]+row+table[rowEnd:])
	r.write(shapesFile, shapes[:shapeEnd]+shape+shapes[shapeEnd:])
	return r.commit("add GET /api/v1/adopt-test-" + name)
}

// adoptBranch merges main into the branch, runs the adopt step and commits the merge.
func (r *scratchRepo) adoptBranch(branch string) (*adoptReport, string) {
	r.t.Helper()
	r.git("checkout", "-q", branch)
	if output, err := r.run("git", "merge", "--no-edit", "main"); err == nil {
		r.t.Fatalf("merging main into %s stopped on no conflict, so there is nothing to adopt: %s", branch, output)
	}
	repo, err := findRepo(r.root)
	if err != nil {
		r.t.Fatal(err)
	}
	report, err := adopt(repo)
	if err != nil {
		r.t.Fatalf("adopt on %s: %v", branch, err)
	}
	r.git("commit", "-q", "--no-edit")
	return report, strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

// Two branches that each add a route beside one row conflict in the old layout. Each takes main's
// move with the adopt step: it writes the route the branch added, leaves nothing for the person,
// and leaves no conflict; and then the two adopted branches merge with each other cleanly, which
// is what the move is for. The control is the merge of the two branches before adoption, which
// must conflict for the clean merge after it to mean anything.
func TestTheAdoptStepCarriesARouteAcrossTheMoveAndTwoAddedRoutesMerge(t *testing.T) {
	repo := newScratchRepo(t)
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	headA := repo.addRouteOn("branch-a", base, "a")
	headB := repo.addRouteOn("branch-b", base, "b")
	if _, err := repo.run("git", "merge-tree", "--write-tree", headA, headB); err == nil {
		t.Fatal("the control failed: the two branches merge cleanly in the old layout, so a clean merge after adoption proves nothing")
	}
	repo.moveMain()

	adopted := map[string]string{}
	for _, name := range []string{"a", "b"} {
		report, head := repo.adoptBranch("branch-" + name)
		file, err := routeFileName("GET", "/api/v1/adopt-test-"+name)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(report.written, []string{file}) {
			t.Fatalf("branch %s: the adopt step wrote %v, want only %s", name, report.written, file)
		}
		if report.left() {
			var out bytes.Buffer
			report.print(&out)
			t.Fatalf("branch %s: the adopt step left work for the person:\n%s", name, out.String())
		}
		split, err := splitOldLayout(gitTree{gitRepo{root: repo.root}, map[string]string{"a": headA, "b": headB}[name]})
		if err != nil {
			t.Fatal(err)
		}
		if got := repo.read(file); got != string(split[file]) {
			t.Fatalf("branch %s: %s holds\n%s\nwant the branch's route as the splitter writes it:\n%s", name, file, got, split[file])
		}
		if got := repo.read(tableFile); got != "package api\n\n// The new layout's shared code.\n" {
			t.Fatalf("branch %s: %s is not main's: %q", name, tableFile, got)
		}
		adopted[name] = head
	}
	if output, err := repo.run("git", "merge-tree", "--write-tree", "--name-only", adopted["a"], adopted["b"]); err != nil {
		t.Fatalf("the two adopted branches conflict: %v\n%s", err, output)
	}
}

// editOn makes a branch from the base commit whose one change to the old layout is edit's.
func (r *scratchRepo) editOn(branch, base string, edit func(table, shapes string) (string, string)) {
	r.t.Helper()
	r.git("checkout", "-q", "-b", branch, base)
	table, shapes := edit(r.read(tableFile), r.read(shapesFile))
	r.write(tableFile, table)
	r.write(shapesFile, shapes)
	r.commit("edit " + branch)
}

// mergeMainAndAdopt merges main into the branch checked out and runs the adopt step once.
func (r *scratchRepo) mergeMainAndAdopt() (*adoptReport, error) {
	r.t.Helper()
	if output, err := r.run("git", "merge", "--no-edit", "main"); err == nil {
		r.t.Fatalf("merging main stopped on no conflict, so there is nothing to adopt: %s", output)
	}
	found, err := findRepo(r.root)
	if err != nil {
		r.t.Fatal(err)
	}
	return adopt(found)
}

// cutLine removes the one line of text that holds marker.
func cutLine(t *testing.T, text, marker string) string {
	t.Helper()
	at := strings.Index(text, marker)
	if at < 0 {
		t.Fatalf("no line holds %q", marker)
	}
	start := strings.LastIndex(text[:at], "\n") + 1
	return text[:start] + text[at+strings.Index(text[at:], "\n")+1:]
}

// A route the branch removes is a file main's side added, which git's index holds as a staged
// addition; the step removes it, so the merge commit does not hold it.
func TestTheAdoptStepRemovesARouteTheBranchRemoved(t *testing.T) {
	repo := newScratchRepo(t)
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	repo.editOn("branch-removes", base, func(table, shapes string) (string, string) {
		return cutLine(t, table, `{http.MethodGet, "/api/v1/limits"`), cutLine(t, shapes, "\t\"GET /api/v1/limits\":")
	})
	repo.moveMain()
	repo.git("checkout", "-q", "branch-removes")
	report, err := repo.mergeMainAndAdopt()
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	file, _ := routeFileName("GET", "/api/v1/limits")
	if !slices.Equal(report.removed, []string{file}) || report.left() {
		var out bytes.Buffer
		report.print(&out)
		t.Fatalf("the adopt step did not remove %s alone:\n%s", file, out.String())
	}
	if _, err := os.Stat(repo.path(file)); !os.IsNotExist(err) {
		t.Fatalf("%s is still on disk: %v", file, err)
	}
	repo.git("commit", "-q", "--no-edit")
	if listed := repo.git("ls-files", "--", filepath.Join(apiDir, file)); listed != "" {
		t.Fatalf("the merge commit holds %s", listed)
	}
}

// The step runs once per merge. Once it has resolved routes_table.go and routes_shapes.go, a
// second run is refused, so what the person carried into them is kept. And a route file a run
// left in conflict is not merged a second time when the shared files are in conflict again: it
// keeps one conflict, not one nested in another.
func TestTheAdoptStepIsNotRunTwiceOverOneMerge(t *testing.T) {
	repo := newScratchRepo(t)
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	const limits = "The bounds a write is held to"
	repo.editOn("branch-edits", base, func(table, shapes string) (string, string) {
		return strings.Replace(table, limits, "The branch's bounds a write is held to", 1), shapes
	})
	repo.moveMain()
	file, _ := routeFileName("GET", "/api/v1/limits")
	repo.write(file, strings.Replace(repo.read(file), limits, "Main's bounds a write is held to", 1))
	repo.commit("main edits the limits route")
	repo.git("checkout", "-q", "branch-edits")
	report, err := repo.mergeMainAndAdopt()
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !slices.Equal(report.conflicted, []string{file}) {
		t.Fatalf("the first run left %v in conflict, want %s alone", report.conflicted, file)
	}
	// The conflict is git's, so git refuses the commit until the person resolves it. Without
	// that, main's version of the route sits in the index and a plain commit records it.
	if output, err := repo.run("git", "commit", "-q", "--no-edit"); err == nil {
		t.Fatalf("git committed a merge holding an unresolved route: %s", output)
	}
	if listed := repo.git("ls-files", "-u", "--", filepath.Join(apiDir, file)); strings.Count(listed, "\n") != 3 {
		t.Fatalf("git's index holds %q for %s, want its three stages", listed, file)
	}

	carried := repo.read(tableFile) + "\n// carried by hand\n"
	repo.write(tableFile, carried)
	found, err := findRepo(repo.root)
	if err != nil {
		t.Fatal(err)
	}
	// Every route the branch changed is in its file now, in conflict, so a second run is refused
	// and what was carried into routes_table.go stays.
	if _, err := adopt(found); err == nil || !strings.Contains(err.Error(), "has run on it already") {
		t.Fatalf("a second run over the adopted merge was not refused: %v", err)
	}
	if repo.read(tableFile) != carried {
		t.Fatal("the refused second run changed what was carried into routes_table.go")
	}
	if markers := strings.Count(repo.read(file), "<<<<<<< "); markers != 1 {
		t.Fatalf("%s holds %d conflict markers after the refused second run, want the first run's one", file, markers)
	}
}

// Three states the step leaves alone, each of which once counted as a route waiting to be
// written, so every run was a first run and a second would take routes_table.go and
// routes_shapes.go from the new side again and throw away what was carried into them: a route
// main removed and the branch changes, a route the branch adds exactly as main did, and a route
// the branch removes that main has changed. The step reports each and writes nothing, so a run
// after it must be refused.
func TestARouteTheStepLeavesAloneIsNotPendingForEver(t *testing.T) {
	limitsFile, _ := routeFileName("GET", "/api/v1/limits")
	const limits = "The bounds a write is held to"
	addedFile, _ := routeFileName("GET", "/api/v1/adopt-test-same")

	t.Run("main removed the route the branch changes", func(t *testing.T) {
		repo := newScratchRepo(t)
		base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
		repo.editOn("branch-a", base, func(table, shapes string) (string, string) {
			return strings.Replace(table, limits, "The branch's bounds", 1), shapes
		})
		repo.moveMain()
		repo.git("rm", "-q", "--", filepath.Join(apiDir, limitsFile))
		repo.commit("main removes the limits route")
		repo.refusesASecondRun(t, limitsFile, func(t *testing.T, report *adoptReport) {
			if len(report.decide) != 1 || !strings.Contains(report.decide[0], limitsFile) {
				t.Fatalf("the step reported %v, want one decide naming %s", report.decide, limitsFile)
			}
		})
	})

	t.Run("the branch adds the route main added", func(t *testing.T) {
		repo := newScratchRepo(t)
		base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
		head := repo.addRouteOn("branch-a", base, "same")
		// The branch's own route, as the splitter writes it, so main's file and the branch's
		// split are the same bytes.
		split, err := splitOldLayout(gitTree{gitRepo{root: repo.root}, head})
		if err != nil {
			t.Fatal(err)
		}
		repo.git("checkout", "-q", "main")
		repo.moveMain()
		repo.write(addedFile, string(split[addedFile]))
		repo.commit("main adds the same route")
		repo.refusesASecondRun(t, addedFile, func(t *testing.T, report *adoptReport) {
			if written := len(report.written) + len(report.updated) + len(report.merged); written != 0 {
				t.Fatalf("the step wrote %d routes, want none: main added the same one", written)
			}
		})
	})

	t.Run("the branch removes the route main changed", func(t *testing.T) {
		repo := newScratchRepo(t)
		base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
		repo.editOn("branch-a", base, func(table, shapes string) (string, string) {
			return cutLine(t, table, `{http.MethodGet, "/api/v1/limits"`), cutLine(t, shapes, "\t\"GET /api/v1/limits\":")
		})
		repo.moveMain()
		repo.write(limitsFile, strings.Replace(repo.read(limitsFile), limits, "Main's bounds", 1))
		repo.commit("main changes the limits route")
		repo.refusesASecondRun(t, limitsFile, func(t *testing.T, report *adoptReport) {
			if len(report.decide) != 1 || !strings.Contains(report.decide[0], limitsFile) {
				t.Fatalf("the step reported %v, want one decide naming %s", report.decide, limitsFile)
			}
		})
	})
}

// refusesASecondRun merges main into branch-a, runs the step, holds its report to expect, and
// then holds a second run to being refused with nothing carried thrown away.
func (r *scratchRepo) refusesASecondRun(t *testing.T, file string, expect func(*testing.T, *adoptReport)) {
	t.Helper()
	r.git("checkout", "-q", "branch-a")
	report, err := r.mergeMainAndAdopt()
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	expect(t, report)
	kept, _, err := readFileIfPresent(r.path(file))
	if err != nil {
		t.Fatal(err)
	}
	carried := r.read(tableFile) + "\n// carried by hand\n"
	r.write(tableFile, carried)
	found, err := findRepo(r.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adopt(found); err == nil || !strings.Contains(err.Error(), "has run on it already") {
		t.Fatalf("a second run was not refused, so every run of the step is a first run: %v", err)
	}
	if r.read(tableFile) != carried {
		t.Fatal("the second run threw away what was carried into routes_table.go")
	}
	if now, _, err := readFileIfPresent(r.path(file)); err != nil {
		t.Fatal(err)
	} else if string(now) != string(kept) {
		t.Fatalf("the second run changed %s, which the first left alone", file)
	}
}

// A person resolves the conflict this step left in a route file by hand, writing text that is
// neither side's, and then carries an edit into routes_table.go. A second run must be refused:
// the route is nobody's to write again, and the run would take routes_table.go from the new side
// and throw the carried edit away. A route file holding conflict markers and one a person has
// resolved are the same case, and only the markers were covered before.
func TestASecondRunIsRefusedAfterAConflictIsResolvedByHand(t *testing.T) {
	repo := newScratchRepo(t)
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	const limits = "The bounds a write is held to"
	repo.editOn("branch-edits", base, func(table, shapes string) (string, string) {
		return strings.Replace(table, limits, "The branch's bounds a write is held to", 1), shapes
	})
	repo.moveMain()
	file, _ := routeFileName("GET", "/api/v1/limits")
	repo.write(file, strings.Replace(repo.read(file), limits, "Main's bounds a write is held to", 1))
	repo.commit("main edits the limits route")
	repo.git("checkout", "-q", "branch-edits")
	report, err := repo.mergeMainAndAdopt()
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !slices.Equal(report.conflicted, []string{file}) {
		t.Fatalf("the first run left %v in conflict, want %s alone", report.conflicted, file)
	}

	// The person resolves the route by hand, keeping neither side's words, and stages it as git
	// asks; then carries an edit into the shared file the step took from the new side.
	resolved := strings.Replace(repo.read(file), limits, "The bounds both sides meant", 1)
	for _, marker := range []string{"<<<<<<< ", "||||||| ", "=======", ">>>>>>> "} {
		var kept []string
		for _, line := range strings.Split(resolved, "\n") {
			if !strings.HasPrefix(line, marker) && line != marker {
				kept = append(kept, line)
			}
		}
		resolved = strings.Join(kept, "\n")
	}
	if strings.Contains(resolved, "<<<<<<<") {
		t.Fatal("the hand resolution still holds conflict markers")
	}
	repo.write(file, resolved)
	repo.git("add", "--", filepath.Join(apiDir, file))
	carried := repo.read(tableFile) + "\n// carried by hand\n"
	repo.write(tableFile, carried)

	found, err := findRepo(repo.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adopt(found); err == nil || !strings.Contains(err.Error(), "has run on it already") {
		t.Fatalf("a second run after a hand resolution was not refused: %v", err)
	}
	if repo.read(tableFile) != carried {
		t.Fatal("the refused second run threw away what was carried into routes_table.go")
	}
	if repo.read(file) != resolved {
		t.Fatal("the refused second run overwrote the hand resolution")
	}
}

// A person may resolve routes_table.go and routes_shapes.go by hand before running the step, as
// `git checkout --theirs` does. The branch's routes are then in no file, so the step runs: it is
// what writes them. The control is the same merge with the step already run, which is refused.
func TestTheAdoptStepRunsAfterTheSharedFilesAreResolvedByHand(t *testing.T) {
	repo := newScratchRepo(t)
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	repo.addRouteOn("branch-a", base, "a")
	repo.moveMain()
	repo.git("checkout", "-q", "branch-a")
	if output, err := repo.run("git", "merge", "--no-edit", "main"); err == nil {
		t.Fatalf("merging main stopped on no conflict: %s", output)
	}
	for _, name := range []string{tableFile, shapesFile} {
		repo.git("checkout", "--theirs", "--", filepath.Join(apiDir, name))
		repo.git("add", "--", filepath.Join(apiDir, name))
	}
	found, err := findRepo(repo.root)
	if err != nil {
		t.Fatal(err)
	}
	report, err := adopt(found)
	if err != nil {
		t.Fatalf("adopt after the shared files were resolved by hand: %v", err)
	}
	file, _ := routeFileName("GET", "/api/v1/adopt-test-a")
	if !slices.Equal(report.written, []string{file}) || report.left() {
		var out bytes.Buffer
		report.print(&out)
		t.Fatalf("the step did not write %s alone:\n%s", file, out.String())
	}
	if _, err := adopt(found); err == nil || !strings.Contains(err.Error(), "has run on it already") {
		t.Fatalf("the control failed: a run after the routes were written was not refused: %v", err)
	}
}

// A row under any condition but the hooks' own is refused, not split as always mounted: a branch
// that narrowed the hooks' `if` would otherwise have its 15 hooks written ungated, and they mount
// in production. The control is the same table unchanged, which splits.
func TestARowUnderAnotherConditionIsRefused(t *testing.T) {
	t.Parallel()
	table := fixture(t, oldTableFixture)
	if _, err := splitOldLayout(oneFileTree{tableFile: table, shapesFile: fixture(t, oldShapesFixture), "going_paths.go": goingPathsFile}); err != nil {
		t.Fatalf("the control failed: the frozen table does not split: %v", err)
	}
	for _, condition := range []string{
		"s.deps.TestHooksEnabled && s.deps.Store != nil",
		"s.deps.TestHooksEnabled || s.deps.Store == nil",
		"hooksEnabled(s.deps)",
	} {
		narrowed := strings.Replace(table, "if s.deps.TestHooksEnabled {", "if "+condition+" {", 1)
		if narrowed == table {
			t.Fatal("the frozen table has no test hooks' block to narrow")
		}
		_, err := splitOldLayout(oneFileTree{tableFile: narrowed, shapesFile: fixture(t, oldShapesFixture), "going_paths.go": goingPathsFile})
		if err == nil || !strings.Contains(err.Error(), "a condition this reader does not know the meaning of") {
			t.Errorf("splitting a table whose hooks stand under `if %s` answered %v, want a refusal", condition, err)
		}
	}
}

// oneFileTree is package api as a map of file names to their text.
type oneFileTree map[string]string

func (t oneFileTree) read(name string) ([]byte, error) {
	content, held := t[name]
	if !held {
		return nil, os.ErrNotExist
	}
	return []byte(content), nil
}

func (t oneFileTree) filesNaming(word string) ([]string, error) {
	var names []string
	for name, content := range t {
		if strings.Contains(content, word) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// A statement the branch writes in the test hooks' block, beside their rows, is shared code the
// person must carry: the hooks' routeRow operands alone are cut before the comparison, so a
// planted statement and an edit to another append's operands both show, and the rows do not.
func TestAStatementInTheHooksBlockIsSharedCode(t *testing.T) {
	table := fixture(t, oldTableFixture)
	const hooks = "if s.deps.TestHooksEnabled {\n"
	at := strings.Index(table, hooks)
	if at < 0 {
		t.Fatal("the frozen table has no test hooks' block")
	}
	inHooks := func(statement string) string {
		return table[:at+len(hooks)] + "\t\t" + statement + "\n" + table[at+len(hooks):]
	}
	shared := func(text string) string {
		t.Helper()
		code, err := sharedCode(tableFile, []byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	before := shared(table)
	if strings.Contains(before, "/api/v1/live/_test/disconnect") {
		t.Fatal("the hooks' rows are not cut out of the shared code")
	}
	if after := shared(inHooks(`panic("shared-hook-change")`)); after == before || !strings.Contains(after, "shared-hook-change") {
		t.Fatal("a statement planted in the hooks' block is cut out with the hooks' rows, so its edit would be dropped unreported")
	}
	noteBefore := shared(inHooks(`notes := append([]string{}, "before-shared-hook-value"); _ = notes`))
	noteAfter := shared(inHooks(`notes := append([]string{}, "after-shared-hook-value"); _ = notes`))
	if noteBefore == noteAfter {
		t.Fatal("an edit to another append's operands in the hooks' block is cut out with the hooks' rows")
	}
}

// The same step under jj, where a merge is the working-copy commit `jj new <branch> main` makes
// and jj reads no git index: the route the branch added is written, routes_table.go and
// routes_shapes.go are main's, and jj holds no conflict afterwards.
func TestTheAdoptStepResolvesAJJMerge(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj is not installed; the git path is tested above")
	}
	repo := newScratchRepo(t)
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	repo.addRouteOn("branch-a", base, "a")
	repo.moveMain()
	if output, err := repo.run("jj", "git", "init", "--colocate"); err != nil {
		t.Fatalf("jj git init: %v: %s", err, output)
	}
	if output, err := repo.run("jj", "new", "branch-a", "main", "-m", "merge main"); err != nil {
		t.Fatalf("jj new: %v: %s", err, output)
	}
	found, err := findRepo(repo.root)
	if err != nil {
		t.Fatal(err)
	}
	if !found.jj {
		t.Fatal("findRepo took a jj workspace for a git checkout")
	}
	report, err := adopt(found)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	file, _ := routeFileName("GET", "/api/v1/adopt-test-a")
	if !slices.Equal(report.written, []string{file}) || report.left() {
		var out bytes.Buffer
		report.print(&out)
		t.Fatalf("the adopt step under jj:\n%s", out.String())
	}
	conflicts, err := repo.run("jj", "file", "list", "-r", "@", "-T", `if(conflict, path ++ "\n")`)
	if err != nil || strings.TrimSpace(conflicts) != "" {
		t.Fatalf("jj still holds conflicts after the adopt step: %v %q", err, conflicts)
	}
	listed, err := repo.run("jj", "file", "list", "-r", "@", "--", filepath.Join(apiDir, file))
	if err != nil || strings.TrimSpace(listed) == "" {
		t.Fatalf("jj's merge commit does not hold %s: %v %q", file, err, listed)
	}
}

// Splitting the frozen old layout writes one route file per row, each named by routeFileName and
// no two alike, and splitting it again writes the same bytes.
func TestTheSplitterIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		tableFile: fixture(t, oldTableFixture), shapesFile: fixture(t, oldShapesFixture), "going_paths.go": goingPathsFile,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	first, err := splitOldLayout(dirTree(dir))
	if err != nil {
		t.Fatal(err)
	}
	second, err := splitOldLayout(dirTree(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 222 {
		t.Fatalf("the frozen table splits into %d route files, want its 222 rows", len(first))
	}
	for name, content := range first {
		if !isRouteFile(name) {
			t.Errorf("%s is no route file name", name)
		}
		if !bytes.Equal(content, second[name]) {
			t.Errorf("%s: two splits of one table differ", name)
		}
	}
}

// The read moved between the two layouts #4557 made: a branch made before it states the read as a
// name in readScopeRoutes, and one made after states it as an entry of readClasses in a file of
// its own. The splitter reads whichever the branch has, and writes the same declaration field,
// so an old-layout branch adopts either way. The controls are a tree holding neither, which is
// refused, and the pre-#4557 fixture, which still splits.
func TestTheSplitterReadsTheReadFromEitherLayout(t *testing.T) {
	t.Parallel()
	before := oneFileTree{
		tableFile: fixture(t, oldTableFixture), shapesFile: fixture(t, oldShapesFixture), "going_paths.go": goingPathsFile,
	}
	after := oneFileTree{
		tableFile: fixture(t, "testdata/main_routes_table.go.txt"), shapesFile: fixture(t, "testdata/main_routes_shapes.go.txt"),
		"read_classes.go": fixture(t, "testdata/old_read_classes.go.txt"), "going_paths.go": goingPathsFile,
	}
	read := func(t *testing.T, src tree, name string) string {
		t.Helper()
		files, err := splitOldLayout(src)
		if err != nil {
			t.Fatalf("split: %v", err)
		}
		file, err := routeFileName("GET", name)
		if err != nil {
			t.Fatal(err)
		}
		content, written := files[file]
		if !written {
			t.Fatalf("the split wrote no %s", file)
		}
		return string(content)
	}
	// A route the list named before #4557 and the map classes as a read after it.
	const listed = "/api/v1/projects/{key}/architecture"
	if got := read(t, before, listed); !strings.Contains(got, "Read:") || !strings.Contains(got, "readWritesNothing") {
		t.Errorf("the pre-#4557 layout wrote %s without a read class:\n%s", listed, got)
	}
	if got := read(t, after, listed); !strings.Contains(got, "Read:") || !strings.Contains(got, "readWritesNothing") {
		t.Errorf("the post-#4557 layout wrote %s without a read class:\n%s", listed, got)
	}
	// A GET the map classes as a write, which the list could not say at all.
	const writes = "/api/v1/delivery/timeline"
	got := read(t, after, writes)
	if !strings.Contains(got, "readWritesState") || !strings.Contains(got, "WriteNote") {
		t.Errorf("the post-#4557 layout wrote %s without its write class and note:\n%s", writes, got)
	}
	// The control: neither the list nor the map, which the splitter refuses rather than writing
	// every route as stating no read.
	neither := oneFileTree{
		tableFile:  strings.Replace(fixture(t, "testdata/main_routes_table.go.txt"), "var routeScopes = map[string]routeScopeSet{", "var readScopeRoutes = []string{}\n\nvar routeScopes = map[string]routeScopeSet{", 1),
		shapesFile: fixture(t, "testdata/main_routes_shapes.go.txt"), "going_paths.go": goingPathsFile,
	}
	if _, err := splitOldLayout(neither); err != nil {
		t.Fatalf("a tree whose read is an empty list is no refusal, and this one answered: %v", err)
	}
}

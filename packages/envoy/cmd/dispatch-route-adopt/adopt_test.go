package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	repo, err := findRepo(r.root)
	if err != nil {
		r.t.Fatal(err)
	}
	report, err := adopt(repo, "main")
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

// adoptMain runs the step on the branch checked out, which makes the merge of main itself.
func (r *scratchRepo) adoptMain() (*adoptReport, error) {
	r.t.Helper()
	found, err := findRepo(r.root)
	if err != nil {
		r.t.Fatal(err)
	}
	return adopt(found, "main")
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
	report, err := repo.adoptMain()
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

// The step makes the merge itself and refuses a tree it did not start, so a second run over its
// own output cannot happen: the tree is mid-merge, and the refusal names `git merge --abort` and
// what that costs. The conflict it left is git's, so git refuses the commit until the person
// resolves it, and its three index stages are the merge's own sides in git's order: stage 2 holds
// HEAD's side, the branch's route as the branch wrote it, and stage 3 MERGE_HEAD's, main's, never
// the conflict-marked text, so `git checkout --ours` answers the branch's route. The markers in
// the working tree are in the same order: HEAD's side first.
func TestASecondRunIsRefusedBecauseTheStepOwnsTheMerge(t *testing.T) {
	repo := newScratchRepo(t)
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	const limits = "The bounds a write is held to"
	repo.editOn("branch-edits", base, func(table, shapes string) (string, string) {
		return strings.Replace(table, limits, "The branch's bounds a write is held to", 1), shapes
	})
	repo.moveMain()
	file, _ := routeFileName("GET", "/api/v1/limits")
	branchRoute := func() string {
		split, err := splitOldLayout(gitTree{gitRepo{root: repo.root}, strings.TrimSpace(repo.git("rev-parse", "branch-edits"))})
		if err != nil {
			t.Fatal(err)
		}
		return string(split[file])
	}()
	repo.write(file, strings.Replace(repo.read(file), limits, "Main's bounds a write is held to", 1))
	repo.commit("main edits the limits route")
	repo.git("checkout", "-q", "branch-edits")
	report, err := repo.adoptMain()
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !slices.Equal(report.conflicted, []string{file}) {
		t.Fatalf("the run left %v in conflict, want %s alone", report.conflicted, file)
	}
	if output, err := repo.run("git", "commit", "-q", "--no-edit"); err == nil {
		t.Fatalf("git committed a merge holding an unresolved route: %s", output)
	}
	// The index stages: 1 the merge base, 2 HEAD's (the branch's own bytes), 3 MERGE_HEAD's (main's).
	staged := repo.git("ls-files", "-u", "--", filepath.Join(apiDir, file))
	if strings.Count(staged, "\n") != 3 {
		t.Fatalf("git's index holds %q for %s, want its three stages", staged, file)
	}
	// git's own stage order for a merge made while the branch is checked out: stage 2 is HEAD's
	// side, the branch, and stage 3 is MERGE_HEAD's, what it merges. So `--ours` answers the
	// branch's route, as it does for every other path of this merge, and neither side's stage
	// holds the conflict-marked text the working tree carries.
	ours := strings.TrimSpace(repo.git("show", ":2:"+filepath.Join(apiDir, file)))
	if ours != strings.TrimSpace(branchRoute) {
		t.Fatalf("stage 2 holds\n%s\nwant the branch's own route, which is HEAD's side:\n%s", ours, branchRoute)
	}
	theirs := strings.TrimSpace(repo.git("show", ":3:"+filepath.Join(apiDir, file)))
	if theirs == strings.TrimSpace(branchRoute) {
		t.Fatal("stage 3 holds the branch's route, and git's stage 3 is MERGE_HEAD's side")
	}
	for stage, text := range map[string]string{"2": ours, "3": theirs} {
		if strings.Contains(text, "<<<<<<<") {
			t.Fatalf("stage %s holds conflict markers, so `git checkout --ours`/`--theirs` answer marked text", stage)
		}
	}
	// The working tree's markers put HEAD's side first, labelled HEAD, as git's own conflicts do,
	// so an editor's "current change" is the branch's route here as in every other file.
	marked := repo.read(file)
	at := strings.Index(marked, "<<<<<<< ")
	if at < 0 {
		t.Fatalf("%s holds no conflict marker:\n%s", file, marked)
	}
	top, rest, _ := strings.Cut(marked[at:], "\n")
	if !strings.HasPrefix(top, "<<<<<<< HEAD") {
		t.Fatalf("the first conflict marker of %s is %q, want HEAD's side first", file, top)
	}
	if first, _, _ := strings.Cut(rest, "\n=======\n"); !strings.Contains(first, "The branch's bounds") || strings.Contains(first, "Main's bounds") {
		t.Fatalf("the first side of %s's conflict is not the branch's route:\n%s", file, marked)
	}

	carried := repo.read(tableFile) + "\n// carried by hand\n"
	repo.write(tableFile, carried)
	found, err := findRepo(repo.root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = adopt(found, "main")
	if err == nil || !strings.Contains(err.Error(), "a merge is in progress") || !strings.Contains(err.Error(), "git -C "+repo.root+" merge --abort") {
		t.Fatalf("a second run was not refused by the merge in progress: %v", err)
	}
	if !strings.Contains(err.Error(), "is lost") {
		t.Fatalf("the refusal does not say what aborting costs: %v", err)
	}
	if repo.read(tableFile) != carried {
		t.Fatal("the refused second run changed what was carried into routes_table.go")
	}
	if markers := strings.Count(repo.read(file), "<<<<<<< "); markers != 1 {
		t.Fatalf("%s holds %d conflict markers after the refused run, want the first run's one", file, markers)
	}
}

// Three states the step leaves alone: a route main removed and the branch changes, a route the
// branch adds exactly as main did, and a route the branch removes that main has changed. Each is
// reported and nothing is written for it, and a run after the step is refused because the step
// owns the merge. Four readings of file content tried to answer "has this run already?" and each
// missed one of these or another state; the step no longer asks.
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
	report, err := r.adoptMain()
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	expect(t, report)
	onDisk := dirTree(filepath.Join(r.root, apiDir))
	kept, _, err := readIfPresent(onDisk, file)
	if err != nil {
		t.Fatal(err)
	}
	carried := r.read(tableFile) + "\n// carried by hand\n"
	r.write(tableFile, carried)
	found, err := findRepo(r.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adopt(found, "main"); err == nil || !strings.Contains(err.Error(), "a merge is in progress") {
		t.Fatalf("a second run was not refused by the merge in progress: %v", err)
	}
	if r.read(tableFile) != carried {
		t.Fatal("the second run threw away what was carried into routes_table.go")
	}
	if now, _, err := readIfPresent(onDisk, file); err != nil {
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
	report, err := repo.adoptMain()
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
	if _, err := adopt(found, "main"); err == nil || !strings.Contains(err.Error(), "a merge is in progress") {
		t.Fatalf("a second run after a hand resolution was not refused: %v", err)
	}
	if repo.read(tableFile) != carried {
		t.Fatal("the refused second run threw away what was carried into routes_table.go")
	}
	if repo.read(file) != resolved {
		t.Fatal("the refused second run overwrote the hand resolution")
	}
}

// The step begins from a tree nobody has touched, so it refuses one that is mid-merge or holds a
// change, and names what the person must do and what it costs. These are the refusals that make a
// second run impossible, rather than a reading of file content that judges one.
func TestTheStepRefusesATreeItDidNotStart(t *testing.T) {
	t.Run("a merge is in progress", func(t *testing.T) {
		repo := newScratchRepo(t)
		base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
		repo.addRouteOn("branch-a", base, "a")
		repo.moveMain()
		repo.git("checkout", "-q", "branch-a")
		if output, err := repo.run("git", "merge", "--no-edit", "main"); err == nil {
			t.Fatalf("merging main stopped on no conflict: %s", output)
		}
		_, err := repo.adoptMain()
		if err == nil || !strings.Contains(err.Error(), "a merge is in progress") || !strings.Contains(err.Error(), "git -C "+repo.root+" merge --abort") || !strings.Contains(err.Error(), "is lost") {
			t.Fatalf("the step ran in a merge it did not start, or said nothing useful: %v", err)
		}
		// And it runs once the merge is ended by the commands the refusal names.
		repo.runNamed(err.Error(), "git")
		if _, err := repo.adoptMain(); err != nil {
			t.Fatalf("adopt after the refusal's own commands: %v", err)
		}
	})

	t.Run("the tree holds a change", func(t *testing.T) {
		repo := newScratchRepo(t)
		base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
		repo.addRouteOn("branch-a", base, "a")
		repo.moveMain()
		repo.git("checkout", "-q", "branch-a")
		repo.write("scratch_note.go", "package api\n\n// A change nobody committed.\n")
		_, err := repo.adoptMain()
		if err == nil || !strings.Contains(err.Error(), "holds changes") || !strings.Contains(err.Error(), "scratch_note.go") {
			t.Fatalf("the step ran in a tree holding a change, or named none: %v", err)
		}
	})
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
	// No merge is made here: the step makes it, as it does with git. `jj new` is how a person
	// sits on a branch, on a fresh empty commit whose parent is the branch's work.
	if output, err := repo.run("jj", "new", "branch-a"); err != nil {
		t.Fatalf("jj new: %v: %s", err, output)
	}
	found, err := findRepo(repo.root)
	if err != nil {
		t.Fatal(err)
	}
	if !found.jj {
		t.Fatal("findRepo took a jj workspace for a git checkout")
	}
	report, err := adopt(found, "main")
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

// mainMovesOn gives main a commit past the branch's merge base and checks the branch out again: a
// merge of main is then a real merge, which `git merge --no-commit` leaves in progress, so a step
// that merged before it refused would leave MERGE_HEAD behind.
func (r *scratchRepo) mainMovesOn(branch string) {
	r.t.Helper()
	r.git("checkout", "-q", "main")
	r.write("main_moves_on.go", "package api\n\n// A commit of main's past the merge base.\n")
	r.commit("main moves on")
	r.git("checkout", "-q", branch)
}

// Nothing is merged until both sides are read: a branch the step cannot adopt is refused with the
// tree as it was, not left mid-merge. The rows are the two layout refusals, each with main moved
// past the merge base so that a merge made before the refusal would leave MERGE_HEAD behind; the
// merge base's refusal; and a merge git makes nothing of, which leaves no MERGE_HEAD for the step
// to write into.
func TestTheStepRefusesBeforeItMerges(t *testing.T) {
	for _, row := range []struct {
		why, into, says string
		set             func(repo *scratchRepo)
	}{
		{
			why: "both sides hold the old table", into: "main", says: "neither side has the layout to adopt",
			set: func(repo *scratchRepo) {
				base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
				repo.addRouteOn("branch-a", base, "a")
				repo.mainMovesOn("branch-a")
			},
		},
		{
			why: "both sides hold the route files", into: "main", says: "there is nothing to adopt",
			set: func(repo *scratchRepo) {
				repo.moveMain()
				repo.git("checkout", "-q", "-b", "branch-a", "main")
				repo.write("note.go", "package api\n\n// A change of the branch's own.\n")
				repo.commit("the branch adds a file")
				repo.mainMovesOn("branch-a")
			},
		},
		{
			why: "the revision shares no history", into: "unrelated", says: "share no merge base",
			set: func(repo *scratchRepo) {
				base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
				repo.addRouteOn("branch-a", base, "a")
				repo.moveMain()
				repo.git("checkout", "-q", "--orphan", "unrelated")
				repo.git("rm", "-rqf", "--ignore-unmatch", ".")
				// It holds the new layout, so every other check passes and the missing merge
				// base is the one thing between the step and its writes.
				for _, name := range strings.Fields(repo.git("ls-tree", "-r", "--name-only", "main")) {
					if strings.HasPrefix(name, apiDir+"/") {
						repo.write(strings.TrimPrefix(name, apiDir+"/"), repo.git("show", "main:"+name))
					}
				}
				repo.git("add", "-A")
				repo.git("commit", "-q", "-m", "unrelated")
				repo.git("checkout", "-qf", "branch-a")
			},
		},
		{
			// The branch holds the route files and main is its ancestor in the old layout, so
			// every check of the layouts passes, and `git merge` answers "Already up to date"
			// and leaves no MERGE_HEAD: the step must write nothing into a tree no merge holds.
			why: "git makes no merge", into: "main", says: "left no MERGE_HEAD",
			set: func(repo *scratchRepo) {
				base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
				repo.moveMain()
				repo.git("checkout", "-q", "-b", "branch-moved", "main")
				repo.git("branch", "-f", "main", base)
			},
		},
	} {
		t.Run(row.why, func(t *testing.T) {
			repo := newScratchRepo(t)
			row.set(repo)
			before := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
			found, err := findRepo(repo.root)
			if err != nil {
				t.Fatal(err)
			}
			_, err = adopt(found, row.into)
			if err == nil || !strings.Contains(err.Error(), row.says) {
				t.Fatalf("the step answered %v, want a refusal saying %q", err, row.says)
			}
			if !strings.Contains(err.Error(), "nothing is merged") {
				t.Fatalf("the refusal does not say the tree is untouched: %v", err)
			}
			if _, err := os.Stat(filepath.Join(repo.root, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
				t.Fatal("the step started a merge before refusing")
			}
			if now := strings.TrimSpace(repo.git("rev-parse", "HEAD")); now != before {
				t.Fatalf("HEAD moved from %s to %s", before, now)
			}
			if dirty := repo.git("status", "--porcelain"); strings.TrimSpace(dirty) != "" {
				t.Fatalf("the refused step left the tree dirty:\n%s", dirty)
			}
		})
	}
}

// jjOn makes the scratch repository a colocated jj workspace sitting on a fresh empty commit over
// branch, with main as a remote would have it, which jj names main@origin.
func (r *scratchRepo) jjOn(branch string) gitRepo {
	r.t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		r.t.Skip("jj is not installed; the git path is tested above")
	}
	r.git("update-ref", "refs/remotes/origin/main", "main")
	for _, args := range [][]string{{"git", "init", "--colocate"}, {"new", branch}} {
		if output, err := r.run("jj", args...); err != nil {
			r.t.Fatalf("jj %s: %v: %s", strings.Join(args, " "), err, output)
		}
	}
	found, err := findRepo(r.root)
	if err != nil {
		r.t.Fatal(err)
	}
	if !found.jj {
		r.t.Fatal("findRepo took a jj workspace for a git checkout")
	}
	return found
}

// jjParents is the working-copy commit's parents, in order.
func (r *scratchRepo) jjParents() []string {
	r.t.Helper()
	output, err := r.run("jj", "log", "--no-graph", "-r", "@", "-T", `parents.map(|c| c.commit_id()).join(" ")`)
	if err != nil {
		r.t.Fatalf("jj log: %v: %s", err, output)
	}
	return strings.Fields(output)
}

// runNamed runs each command a message names in backquotes, in order: the recovery a refusal or
// a failure gives the person, run as written.
func (r *scratchRepo) runNamed(message, tool string) {
	r.t.Helper()
	var ran int
	for _, match := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(message, -1) {
		fields := strings.Fields(match[1])
		if fields[0] != tool {
			continue
		}
		if output, err := r.run(fields[0], fields[1:]...); err != nil {
			r.t.Fatalf("%s, which the message names: %v: %s", match[1], err, output)
		}
		ran++
	}
	if ran == 0 {
		r.t.Fatalf("the message names no %s command to run:\n%s", tool, message)
	}
}

// Under jj the step merges main@origin when it is given no revision: jj names a remote bookmark
// <name>@<remote>, and origin/main names nothing it knows.
func TestUnderJJTheStepMergesMainAtOriginByDefault(t *testing.T) {
	repo := newScratchRepo(t)
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	repo.addRouteOn("branch-a", base, "a")
	repo.moveMain()
	repo.jjOn("branch-a")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-C", repo.root}, &stdout, &stderr); code != 0 {
		t.Fatalf("the step with no revision exited %d under jj:\n%s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "main@origin") {
		t.Fatalf("the step did not merge main@origin:\n%s", stdout.String())
	}
	if parents := repo.jjParents(); len(parents) != 2 {
		t.Fatalf("@ has parents %v, want the merge's two", parents)
	}
}

// Under jj a second run is refused because @ is a merge, and the refusal's own commands return the
// tree to the branch side, so the run after them merges again. `jj abandon @` alone would put a
// new working copy on the merge's two parents, and every later run would be refused.
func TestUnderJJTheRefusalReturnsToTheBranchSide(t *testing.T) {
	repo := newScratchRepo(t)
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	repo.addRouteOn("branch-a", base, "a")
	repo.moveMain()
	found := repo.jjOn("branch-a")
	if _, err := adopt(found, "main@origin"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	_, err := adopt(found, "main@origin")
	if err == nil || !strings.Contains(err.Error(), "is a merge already") || !strings.Contains(err.Error(), "is lost") {
		t.Fatalf("a second run under jj was not refused, or did not say what it costs: %v", err)
	}
	repo.runNamed(err.Error(), "jj")
	if parents := repo.jjParents(); len(parents) != 1 {
		t.Fatalf("after the refusal's commands @ has parents %v, want one: the branch side", parents)
	}
	report, err := adopt(found, "main@origin")
	if err != nil {
		t.Fatalf("the run after the refusal's commands: %v", err)
	}
	file, _ := routeFileName("GET", "/api/v1/adopt-test-a")
	if !slices.Equal(report.written, []string{file}) {
		t.Fatalf("the run after the refusal's commands wrote %v, want %s", report.written, file)
	}
}

// Under jj the merge joins the commit the step read as its own, not whatever @ is when the merge
// is made: a merge on another commit would join a tree whose layout nobody read.
func TestUnderJJTheMergeJoinsTheCommitTheStepRead(t *testing.T) {
	repo := newScratchRepo(t)
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	repo.addRouteOn("branch-a", base, "a")
	repo.moveMain()
	found := repo.jjOn("branch-a")
	here, there, err := found.sidesOf("main@origin")
	if err != nil {
		t.Fatal(err)
	}
	// @ moves between the read and the merge.
	if output, err := repo.run("jj", "new", "-m", "@ moved after the read"); err != nil {
		t.Fatalf("jj new: %v: %s", err, output)
	}
	if err := found.startMerge("main@origin", here, there); err != nil {
		t.Fatalf("startMerge: %v", err)
	}
	if parents := repo.jjParents(); !slices.Equal(parents, []string{here, there}) {
		t.Fatalf("the merge joins %v, want the commit the step read and main@origin, %v", parents, []string{here, there})
	}
}

// A failure after the merge is made names the commands that return the tree to the branch, and
// running them does. The failure is a local exclude naming the route file the step writes, so its
// `git add` fails: nothing the step reads from the commits before the merge can foresee it.
func TestAFailureAfterTheMergeNamesHowToReturnTheTree(t *testing.T) {
	repo := newScratchRepo(t)
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	head := repo.addRouteOn("branch-a", base, "a")
	repo.moveMain()
	repo.git("checkout", "-q", "branch-a")
	file, _ := routeFileName("GET", "/api/v1/adopt-test-a")
	exclude := filepath.Join(repo.root, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exclude, []byte(apiDir+"/"+file+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := repo.adoptMain()
	if err == nil {
		t.Fatal("the step staged a route file a local exclude names")
	}
	if _, statErr := os.Stat(filepath.Join(repo.root, ".git", "MERGE_HEAD")); statErr != nil {
		t.Fatalf("the failure came before the merge, so this row does not test a failure after it: %v", err)
	}
	if !strings.Contains(err.Error(), "git -C "+repo.root+" merge --abort") {
		t.Fatalf("a failure after the merge does not name `git merge --abort`: %v", err)
	}
	repo.runNamed(err.Error(), "git")
	if _, statErr := os.Stat(filepath.Join(repo.root, ".git", "MERGE_HEAD")); !os.IsNotExist(statErr) {
		t.Fatal("the failure's commands left the merge in progress")
	}
	if now := strings.TrimSpace(repo.git("rev-parse", "HEAD")); now != head {
		t.Fatalf("HEAD moved from %s to %s", head, now)
	}
	if dirty := repo.git("status", "--porcelain"); strings.TrimSpace(dirty) != "" {
		t.Fatalf("the failure's commands left the tree holding changes:\n%s", dirty)
	}
}

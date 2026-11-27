package main

//ci:reads .github/workflows/release.yaml TestTheReleaseListNamesWhatLegionIsBuiltFrom reads LEGION_VERSION_ARGS under ../../../.. and holds it to go list -deps and go env GOWORK
//ci:reads .github/go-release-targets.json TestTheReleaseListNamesWhatLegionIsBuiltFrom reads the legion targets, one go list -deps for each

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The paths release.yaml's LEGION_VERSION_ARGS lists are what the legion binary is built from
// (.github/ci-notes/release.md), for each target the release builds
// (.github/go-release-targets.json's legion list, with CGO_ENABLED=0 as release-build-legion.sh
// builds): every envoy package it links, as `go list -deps ./cmd/legion` names them, and the
// module files that decide what those packages compile with, which are the workspace's go.work
// (`go env GOWORK`) and the go.mod of each first-party module it links. The release finds no
// commit in the listed paths when one it misses alone changes (release-push.sh version), so no
// legion carries that change, and an envoy package it names that no target links releases a
// legion for nothing. A package one target alone links, by a file with a build constraint, counts
// as linked. The list is written by hand, so this row names each package found on one side only,
// and each module file the build reads that no listed path covers.
func TestTheReleaseListNamesWhatLegionIsBuiltFrom(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..", "..")
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	paths := legionReleasePaths(t, string(workflow))
	listed := legionReleaseEnvoyPackages(t, paths)
	linked, moduleFiles := legionBuildInputs(t, root, legionReleaseTargets(t, filepath.Join(root, ".github", "go-release-targets.json")))
	for pkg, targets := range linked {
		if !slices.Contains(listed, pkg) {
			t.Errorf("legion links %s (%s), which LEGION_VERSION_ARGS in release.yaml does not list: a change to it alone would release no legion", pkg, strings.Join(targets, ", "))
		}
	}
	for _, pkg := range listed {
		if linked[pkg] == nil {
			t.Errorf("LEGION_VERSION_ARGS in release.yaml lists %s, which legion does not link for any release target", pkg)
		}
	}
	for file, targets := range moduleFiles {
		if !slices.ContainsFunc(paths, func(path string) bool { return pathReads(path, file) }) {
			t.Errorf("legion's build reads %s (%s), which no path of LEGION_VERSION_ARGS in release.yaml covers: a change to it alone would release no legion", file, strings.Join(targets, ", "))
		}
	}
}

// pathReads reports whether the listed path, a pathspec release-bump.sh hands to `git log --`,
// reads file, a path from the repository's root: the same path, or a directory ("/" at its end)
// that holds it.
func pathReads(path, file string) bool {
	return path == file || strings.HasSuffix(path, "/") && strings.HasPrefix(file, path)
}

// legionReleaseTargets is the GOOS/GOARCH pairs the release builds legion for, read from path,
// .github/go-release-targets.json's legion list, as release-build-legion.sh reads it.
func legionReleaseTargets(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var targets struct{ Legion []string }
	if err := json.Unmarshal(raw, &targets); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if len(targets.Legion) == 0 {
		t.Fatalf("%s lists no legion target, so this row would compare nothing", path)
	}
	for _, target := range targets.Legion {
		if goos, goarch, ok := strings.Cut(target, "/"); !ok || goos == "" || goarch == "" {
			t.Fatalf("%s lists %q, which is no GOOS/GOARCH", path, target)
		}
	}
	return targets.Legion
}

// legionReleasePaths is the paths LEGION_VERSION_ARGS lists after its "--", read from
// release.yaml's text: the folded value's lines, each indented under the key.
func legionReleasePaths(t *testing.T, workflow string) []string {
	t.Helper()
	const key = "  LEGION_VERSION_ARGS: >-"
	_, value, found := strings.Cut(workflow, "\n"+key+"\n")
	if !found {
		t.Fatalf("release.yaml has no line %q", key)
	}
	var words []string
	for line := range strings.Lines(value) {
		if !strings.HasPrefix(line, "    ") {
			break
		}
		words = append(words, strings.Fields(line)...)
	}
	dash := slices.Index(words, "--")
	if dash < 0 {
		t.Fatalf("LEGION_VERSION_ARGS in release.yaml has no \"--\" before its paths: %q", words)
	}
	return words[dash+1:]
}

// legionReleaseEnvoyPackages is the envoy package directories among paths, as
// packages/envoy/<path>, the trailing "/" of each directory dropped. The module's go.mod and
// go.sum, which the list names too, are no package.
func legionReleaseEnvoyPackages(t *testing.T, paths []string) []string {
	t.Helper()
	var packages []string
	for _, path := range paths {
		if strings.HasPrefix(path, "packages/envoy/") && strings.HasSuffix(path, "/") {
			packages = append(packages, strings.TrimSuffix(path, "/"))
		}
	}
	if len(packages) == 0 {
		t.Fatalf("LEGION_VERSION_ARGS in release.yaml lists no envoy package directory, so this row would compare nothing: %q", paths)
	}
	return packages
}

// legionBuildInputs is what `go list -deps` finds this package (the legion binary) built from for
// any of targets, each with the targets that read it: every package of the envoy module it links,
// as packages/envoy/<path>, and the module files, from the repository's root at root: the go.mod
// of each first-party module it links (a module of the workspace, or one a replace takes from a
// directory) and the workspace's go.work. Each go command runs with CGO_ENABLED=0, the target's
// GOOS and GOARCH and no GOWORK of the caller's, as the release builds, with the go command of the
// toolchain that built this test, or the one on PATH.
func legionBuildInputs(t *testing.T, root string, targets []string) (packages, moduleFiles map[string][]string) {
	t.Helper()
	gocmd := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(gocmd); err != nil {
		if gocmd, err = exec.LookPath("go"); err != nil {
			t.Fatalf("no go command to list legion's packages with: %v", err)
		}
	}
	repo := realPath(t, root)
	fromRoot := func(file string) string {
		rel, err := filepath.Rel(repo, realPath(t, file))
		if err != nil || !filepath.IsLocal(rel) {
			t.Fatalf("%s, a module file legion's build reads, is outside the repository %s", file, repo)
		}
		return filepath.ToSlash(rel)
	}
	packages, moduleFiles = map[string][]string{}, map[string][]string{}
	const format = `{{.ImportPath}}{{with .Module}}{{if .Main}} {{.GoMod}}{{else}}{{with .Replace}}{{if not .Version}} {{.GoMod}}{{end}}{{end}}{{end}}{{end}}`
	for _, target := range targets {
		goos, goarch, _ := strings.Cut(target, "/")
		list := exec.Command(gocmd, "list", "-deps", "-f", format, ".")
		list.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch, "GOWORK=")
		out, err := list.Output()
		if err != nil {
			t.Fatalf("go list -deps ./cmd/legion for %s: %v", target, err)
		}
		found, gomods := 0, map[string]bool{}
		for line := range strings.Lines(string(out)) {
			imported, gomod, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
			if rest, ok := strings.CutPrefix(imported, "github.com/sjawhar/envoy/"); ok {
				packages["packages/envoy/"+rest] = append(packages["packages/envoy/"+rest], target)
				found++
			}
			if gomod != "" {
				gomods[fromRoot(gomod)] = true
			}
		}
		if found == 0 {
			t.Fatalf("go list -deps ./cmd/legion for %s names no envoy package, so this row would compare nothing", target)
		}
		if len(gomods) == 0 {
			t.Fatalf("go list -deps ./cmd/legion for %s names no first-party module, so this row would compare no go.mod", target)
		}
		for gomod := range gomods {
			moduleFiles[gomod] = append(moduleFiles[gomod], target)
		}
	}
	env := exec.Command(gocmd, "env", "GOWORK")
	env.Env = append(os.Environ(), "GOWORK=")
	out, err := env.Output()
	if err != nil {
		t.Fatalf("go env GOWORK: %v", err)
	}
	if gowork := strings.TrimSpace(string(out)); gowork != "" && gowork != "off" {
		moduleFiles[fromRoot(gowork)] = targets
	}
	return packages, moduleFiles
}

// realPath is path made absolute with every symbolic link resolved, so two paths to one file
// compare equal whichever way each was reached.
func realPath(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

package main

//ci:reads .github/workflows/release.yaml TestTheReleaseListNamesEveryEnvoyPackageLegionLinks reads LEGION_VERSION_ARGS under ../../../.. and holds it to go list -deps
//ci:reads .github/go-release-targets.json TestTheReleaseListNamesEveryEnvoyPackageLegionLinks reads the legion targets, one go list -deps for each

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

// The envoy packages release.yaml's LEGION_VERSION_ARGS lists are every envoy package the legion
// binary links, as `go list -deps ./cmd/legion` names them (.github/ci-notes/release.md), for each
// target the release builds (.github/go-release-targets.json's legion list, with CGO_ENABLED=0 as
// release-build-legion.sh builds): the release finds no commit in the listed paths when one it
// misses alone changes (release-push.sh version), so no legion carries that change, and one it
// names that no target links releases a legion for nothing. A package one target alone links, by
// a file with a build constraint, counts as linked. The list is written by hand, so this row names
// each package found on one side only.
func TestTheReleaseListNamesEveryEnvoyPackageLegionLinks(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	listed := legionReleaseEnvoyPackages(t, string(workflow))
	linked := linkedEnvoyPackages(t, legionReleaseTargets(t, filepath.Join(root, ".github", "go-release-targets.json")))
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

// legionReleaseEnvoyPackages is the envoy package directories LEGION_VERSION_ARGS lists after its
// "--", as packages/envoy/<path>, read from release.yaml's text: the folded value's lines, each
// indented under the key, the trailing "/" of each directory dropped. The module's go.mod and
// go.sum, which it lists too, are no package.
func legionReleaseEnvoyPackages(t *testing.T, workflow string) []string {
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
	var packages []string
	for _, path := range words[dash+1:] {
		if strings.HasPrefix(path, "packages/envoy/") && strings.HasSuffix(path, "/") {
			packages = append(packages, strings.TrimSuffix(path, "/"))
		}
	}
	if len(packages) == 0 {
		t.Fatalf("LEGION_VERSION_ARGS in release.yaml lists no envoy package directory, so this row would compare nothing: %q", words)
	}
	return packages
}

// linkedEnvoyPackages is every package of the envoy module `go list -deps` finds this package (the
// legion binary) linking for any of targets, as packages/envoy/<path>, each with the targets that
// link it. Each listing runs with CGO_ENABLED=0 and the target's GOOS and GOARCH, as the release
// builds, with the go command of the toolchain that built this test, or the one on PATH.
func linkedEnvoyPackages(t *testing.T, targets []string) map[string][]string {
	t.Helper()
	gocmd := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(gocmd); err != nil {
		if gocmd, err = exec.LookPath("go"); err != nil {
			t.Fatalf("no go command to list legion's packages with: %v", err)
		}
	}
	linked := map[string][]string{}
	for _, target := range targets {
		goos, goarch, _ := strings.Cut(target, "/")
		list := exec.Command(gocmd, "list", "-deps", ".")
		list.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
		out, err := list.Output()
		if err != nil {
			t.Fatalf("go list -deps ./cmd/legion for %s: %v", target, err)
		}
		found := 0
		for _, imported := range strings.Fields(string(out)) {
			if rest, ok := strings.CutPrefix(imported, "github.com/sjawhar/envoy/"); ok {
				linked["packages/envoy/"+rest] = append(linked["packages/envoy/"+rest], target)
				found++
			}
		}
		if found == 0 {
			t.Fatalf("go list -deps ./cmd/legion for %s names no envoy package, so this row would compare nothing", target)
		}
	}
	return linked
}

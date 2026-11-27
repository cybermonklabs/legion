//go:build twobuild

// The order of releases for an element a manifest stages: read in one release, created in a later
// one (twobuild_harness_test.go holds the previous build this reads).

package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A new element is readable one release before it is creatable (README.md, "Two builds at
// once"). So each element this tree's manifest stages - its creatable_since above its since - and
// creates - its creatable_since at or below the manifest's version - is one the previous release's
// manifest declares: a build of that release reads what this build writes. A manifest that makes
// a staged element creatable in the release that first reads it fails here, naming it. The check
// is of the order of releases, not of deployments.
//
// Its control is this tree's manifest at the version after its own, where plan_task's parent is
// creatable, against the previous release's manifest with that attribute taken out, as a release
// before the one that reads parent declares it: the check must fail, naming plan_task.parent. The
// attribute is taken out rather than read off the previous build, so the control still fails once
// the previous release reads parent.
func TestAStagedElementIsCreatableOnlyAfterAReleaseThatReadsIt(t *testing.T) {
	build := openPreviousBuild(t)
	previous := readStagedManifest(t, build.path("pmdoc", "schema", "blocks.json"))
	currentPath := filepath.Join("..", "..", "internal", "dispatch", "pmdoc", "schema", "blocks.json")
	current := readStagedManifest(t, currentPath)
	if unread := createdBeforeRead(current, previous); len(unread) > 0 {
		t.Errorf("this tree's manifest (version %d) creates %s, which it stages, and the previous release's manifest (version %d, commit %s) does not declare. "+
			"A build of that release would meet it unread. Make each creatable only in a release after one that reads it.",
			current.Version, strings.Join(unread, ", "), previous.Version, shortCommit(build.commit))
	}

	raw, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read this tree's manifest: %v", err)
	}
	var raised map[string]any
	if err := json.Unmarshal(raw, &raised); err != nil {
		t.Fatalf("decode this tree's manifest: %v", err)
	}
	raised["version"] = current.Version + 1
	encoded, err := json.Marshal(raised)
	if err != nil {
		t.Fatalf("encode this tree's manifest at version %d: %v", current.Version+1, err)
	}
	raisedPath := filepath.Join(t.TempDir(), "blocks.json")
	if err := os.WriteFile(raisedPath, encoded, 0o600); err != nil {
		t.Fatalf("write this tree's manifest at version %d: %v", current.Version+1, err)
	}
	before := stagedManifest{Version: previous.Version, declared: maps.Clone(previous.declared)}
	delete(before.declared, "plan_task.parent")
	if unread := createdBeforeRead(readStagedManifest(t, raisedPath), before); !slices.Contains(unread, "plan_task.parent") {
		t.Errorf("control: this tree's manifest at version %d against a release that does not read plan_task.parent creates %v unread, want plan_task.parent among them",
			current.Version+1, unread)
	}
}

// stagedVersions is one element's two numbers, each 1 where the manifest names none.
type stagedVersions struct{ since, creatableSince int }

// stagedManifest is a manifest's version and every element it declares, by name: a node or a
// mark by its own name after "node " or "mark ", a typed block by its name, an attribute as
// <block>.<attribute>, and an enum choice as <block>.<attribute>=<choice>.
type stagedManifest struct {
	Version  int
	declared map[string]stagedVersions
}

// stagedElement is what the manifest's JSON says of one element.
type stagedElement struct {
	Name           string `json:"name"`
	Since          *int   `json:"since"`
	CreatableSince *int   `json:"creatable_since"`
}

func (e stagedElement) versions() stagedVersions {
	versions := stagedVersions{since: 1, creatableSince: 1}
	if e.Since != nil {
		versions.since = *e.Since
	}
	if e.CreatableSince != nil {
		versions.creatableSince = *e.CreatableSince
	}
	return versions
}

// readStagedManifest reads the manifest at path.
func readStagedManifest(t *testing.T, path string) stagedManifest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the block manifest %s: %v", path, err)
	}
	var value struct {
		Version int             `json:"version"`
		Nodes   []stagedElement `json:"nodes"`
		Marks   []stagedElement `json:"marks"`
		Types   []struct {
			stagedElement
			Attributes map[string]struct {
				stagedElement
				Choices        []string                 `json:"choices"`
				ChoiceVersions map[string]stagedElement `json:"choice_versions"`
			} `json:"attributes"`
		} `json:"types"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode the block manifest %s: %v", path, err)
	}
	manifest := stagedManifest{Version: value.Version, declared: map[string]stagedVersions{}}
	for _, node := range value.Nodes {
		manifest.declared["node "+node.Name] = node.versions()
	}
	for _, mark := range value.Marks {
		manifest.declared["mark "+mark.Name] = mark.versions()
	}
	for _, typed := range value.Types {
		manifest.declared[typed.Name] = typed.versions()
		for name, attribute := range typed.Attributes {
			manifest.declared[typed.Name+"."+name] = attribute.versions()
			for _, choice := range attribute.Choices {
				manifest.declared[fmt.Sprintf("%s.%s=%s", typed.Name, name, choice)] = attribute.ChoiceVersions[choice].versions()
			}
		}
	}
	return manifest
}

// createdBeforeRead is each element current stages and creates that previous does not declare,
// sorted.
func createdBeforeRead(current, previous stagedManifest) []string {
	var unread []string
	for name, versions := range current.declared {
		staged := versions.creatableSince > versions.since
		creates := versions.creatableSince <= current.Version
		if _, read := previous.declared[name]; staged && creates && !read {
			unread = append(unread, name)
		}
	}
	slices.Sort(unread)
	return unread
}

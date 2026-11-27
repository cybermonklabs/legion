//go:build twobuild

// The order of releases for an element a manifest stages: read in one release, created in a later
// one (twobuild_harness_test.go holds the previous build this reads).

package main

import (
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
	previous := readManifest(t, build.path("pmdoc", "schema", "blocks.json"))
	current := readManifest(t, filepath.Join("..", "..", "internal", "dispatch", "pmdoc", "schema", "blocks.json"))
	if unread := createdBeforeRead(current, previous.declared()); len(unread) > 0 {
		t.Errorf("this tree's manifest (version %d) creates %s, which it stages, and the previous release's manifest (version %d, commit %s) does not declare. "+
			"A build of that release would meet it unread. Make each creatable only in a release after one that reads it.",
			current.Version, strings.Join(unread, ", "), previous.Version, shortCommit(build.commit))
	}

	raised := current
	raised.Version++
	before := previous.declared()
	delete(before, "plan_task.parent")
	if unread := createdBeforeRead(raised, before); !slices.Contains(unread, "plan_task.parent") {
		t.Errorf("control: this tree's manifest at version %d against a release that does not read plan_task.parent creates %v unread, want plan_task.parent among them",
			raised.Version, unread)
	}
}

// createdBeforeRead is each element current stages and creates that previous, the elements a
// manifest declares (blockManifest.declared), does not name, sorted.
func createdBeforeRead(current blockManifest, previous map[string]elementVersions) []string {
	var unread []string
	for name, versions := range current.declared() {
		since, creatableSince := versions.numbers()
		staged := creatableSince > since
		creates := creatableSince <= current.Version
		if _, read := previous[name]; staged && creates && !read {
			unread = append(unread, name)
		}
	}
	slices.Sort(unread)
	return unread
}

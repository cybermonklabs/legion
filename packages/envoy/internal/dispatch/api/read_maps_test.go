package api

import (
	"maps"
	"slices"
	"testing"
)

// readMapsDisagree names every GET row that one of the two read maps classes and the other does
// not, sorted: readClasses says what a GET row writes (the read-only gate's map, #4557) and
// readSources says what it reads (the visibility census's map). Both are keyed "GET <pattern>", one
// entry per GET row of the table, and register refuses a table that either fails to class
// (readClassRefusals, undecidedReads). This holds the two to each other directly, so a row added to
// one map and not the other is named by the key it is missing from.
func readMapsDisagree(classes map[string]readEntry, sources map[string]readSource) []string {
	var disagree []string
	for name := range classes {
		if _, ok := sources[name]; !ok {
			disagree = append(disagree, name+": in readClasses, missing from readSources")
		}
	}
	for name := range sources {
		if _, ok := classes[name]; !ok {
			disagree = append(disagree, name+": in readSources, missing from readClasses")
		}
	}
	slices.Sort(disagree)
	return disagree
}

// TestTheTwoReadMapsClassTheSameGETRows holds readClasses and readSources to exactly one set of
// GET rows. Two maps keyed the same way drift: a later change that adds a route and fills only one
// map is caught here by name, as register's two refusals would catch it at its next start.
//
// Its controls show the comparison can fail: each map with one entry removed, in turn, is named
// as missing that entry, and the two maps as they stand are named as disagreeing nowhere.
func TestTheTwoReadMapsClassTheSameGETRows(t *testing.T) {
	if disagree := readMapsDisagree(readClasses, readSources); len(disagree) > 0 {
		t.Fatalf("the two read maps class different GET rows:\n%v", disagree)
	}
	if len(readClasses) == 0 || len(readSources) == 0 {
		t.Fatalf("readClasses holds %d entries and readSources %d: a comparison of an empty map proves nothing",
			len(readClasses), len(readSources))
	}

	names := slices.Sorted(maps.Keys(readClasses))
	removed := names[len(names)/2]

	fewerClasses := maps.Clone(readClasses)
	delete(fewerClasses, removed)
	want := []string{removed + ": in readSources, missing from readClasses"}
	if got := readMapsDisagree(fewerClasses, readSources); !slices.Equal(got, want) {
		t.Errorf("control: readClasses without %q compares as %v, want %v", removed, got, want)
	}

	fewerSources := maps.Clone(readSources)
	delete(fewerSources, removed)
	want = []string{removed + ": in readClasses, missing from readSources"}
	if got := readMapsDisagree(readClasses, fewerSources); !slices.Equal(got, want) {
		t.Errorf("control: readSources without %q compares as %v, want %v", removed, got, want)
	}
}

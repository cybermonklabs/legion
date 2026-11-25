package replay

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// window.json names missingAncestors only where the window has one, as every optional field is
// written and read: each fixture window written before collect read ancestors, whose window.json
// names none, still reads, with none; p0-recorded-descendant-update reads with the one it names;
// and a window written with missing ancestors reads them back, in their order, while one written
// with none writes no member of that name.
func TestAWindowNamesItsMissingAncestorsOnlyWhereItHasThem(t *testing.T) {
	t.Parallel()
	for _, name := range allFixtureWindowNames(t) {
		dir := filepath.Join(fixtureWindowsDir, name)
		w, err := ReadWindow(dir)
		if err != nil {
			t.Errorf("window %s: %v", name, err)
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, windowFile))
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		if name == "p0-recorded-descendant-update" {
			want = []string{"ACMEAUX-1"}
		}
		if names := bytes.Contains(raw, []byte(`"missingAncestors"`)); !slices.Equal(w.MissingAncestors, want) || names != (want != nil) {
			t.Errorf("window %s reads with missing ancestors %v (window.json names the member: %t), want %v", name, w.MissingAncestors, names, want)
		}
	}

	at := func(minute int) time.Time { return time.Date(2001, time.March, 5, 9, minute, 0, 0, time.UTC) }
	w := Window{Project: "ACME", From: at(0), To: at(30), Caps: []CapAt{{At: at(0), Cap: 1}},
		Parts:        []Part{{From: at(0), To: at(30), Release: "fixture", Commit: record.MainRule.Commit, Rule: record.MainRule.Name}},
		TickInterval: Duration(time.Hour), GateSteps: []record.DesignStep{}, Linger: Duration(2 * time.Hour), ReviewRoundCap: 3,
		StalledTreeNotice: Duration(6 * time.Hour), WorkerSilence: Duration(4 * time.Hour), WorkerBackground: Duration(4 * time.Hour),
		Experiments: []config.Experiment{}, Lines: []string{}, Collector: record.MainRule.Commit}
	for _, missing := range [][]string{{"ACMEAUX-7", "BETAX-2"}, nil} {
		dir := filepath.Join(t.TempDir(), "window")
		written := w
		written.MissingAncestors = missing
		if err := written.Write(dir); err != nil {
			t.Fatal(err)
		}
		read, err := ReadWindow(dir)
		if err != nil {
			t.Fatalf("ReadWindow of a window written with missing ancestors %v: %v", missing, err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, windowFile))
		if err != nil {
			t.Fatal(err)
		}
		if names := bytes.Contains(raw, []byte(`"missingAncestors"`)); !slices.Equal(read.MissingAncestors, missing) || names != (missing != nil) {
			t.Errorf("a window written with missing ancestors %v reads with %v (window.json names the member: %t)", missing, read.MissingAncestors, names)
		}
	}
}

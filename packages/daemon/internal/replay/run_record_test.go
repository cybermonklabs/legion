package replay

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A run's run.json names the code that made it and when it started: buildCommit, the commit of
// the legion binary that ran it (RunConfig.BuildCommit, which `legion replay` sets from the
// binary's own commit), or unknown where none is given; and startedAt, the moment the run started
// on the wall clock, in RFC 3339 and UTC. The test replays clockWindow, whose every time is in 2001,
// so a start read from the simulated clock lies far outside the test's own span. It replays the
// window once with a commit given and once with none, reads each run.json back by its JSON names,
// and fails where its buildCommit is not the commit given (or unknown), or where its startedAt is
// not a UTC time from the moment the test read the wall clock before the run to the moment after it.
func TestRunJSONNamesTheBuildCommitAndTheStart(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	window := fixtureWindow(t, clockWindow)
	const given = "0123456789abcdef0123456789abcdef01234567"
	for _, c := range []struct {
		name, commit, want string
	}{
		{name: "a commit given", commit: given, want: given},
		{name: "no commit given", want: "unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out := t.TempDir()
			before := time.Now()
			if _, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
				PGDSN: dsn, Out: out, BuildCommit: c.commit}); err != nil {
				t.Fatalf("replay the window %s: %v", clockWindow, err)
			}
			after := time.Now()
			var ran struct {
				BuildCommit string `json:"buildCommit"`
				StartedAt   string `json:"startedAt"`
			}
			if err := readJSON(filepath.Join(out, runFile), &ran); err != nil {
				t.Fatal(err)
			}
			if ran.BuildCommit != c.want {
				t.Errorf("run.json's buildCommit = %q, want %q", ran.BuildCommit, c.want)
			}
			started, err := time.Parse(time.RFC3339, ran.StartedAt)
			if err != nil || !strings.HasSuffix(ran.StartedAt, "Z") {
				t.Fatalf("run.json's startedAt = %q, want an RFC 3339 time in UTC (%v)", ran.StartedAt, err)
			}
			if started.Before(before) || started.After(after) {
				t.Errorf("run.json's startedAt = %s, want a time from %s to %s, the test's own span on the wall clock", ran.StartedAt,
					before.UTC().Format(time.RFC3339Nano), after.UTC().Format(time.RFC3339Nano))
			}
		})
	}
}

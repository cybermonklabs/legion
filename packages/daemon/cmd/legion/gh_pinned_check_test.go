package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// ghCheckReadingLikeThePinnedCheck is a stand-in shared check for the call args, which does with
// it what the pinned check does, by ghroutetest.PinnedCheckReads, decided here for the call the
// test runs: for a call that check reads as a read, nothing; for any other, what that check does
// for one whose text matched a rule other than a credential's on a repository it must look up (a
// call that begins with an option it refuses instead, and no test gives this one such a call). It
// reads the target's visibility, through gh, in the argv and under the variable that check's
// vis_of uses, and records `lookup <slug>` in its own record before the read and what the read
// did after it: its exit status, when it started and ended, and what it printed (ghCheckReadOf).
// The target is LEAK_CHECK_STANDIN_TARGET, which stands for the slug the check's own targets
// would find. Either way it then clears the call, as that check clears such a call to a private
// repository, and it records each run, as leakguardtest.Recording does, so a test reads that it
// was handed args. `legion gh` answers its visibility read itself (answerVisibilityRead), by the
// guard's REST read, so it reaches the stand-in gh as ghGuardLookup, as the guard's own read does:
// the record is what tells the two apart.
func ghCheckReadingLikeThePinnedCheck(args []string) string {
	script := "cat >/dev/null 2>&1\n"
	if !ghroutetest.PinnedCheckReads(args) {
		script += `if [ -n "${LEAK_CHECK_STANDIN_TARGET-}" ]; then
	printf 'lookup %s\n' "$LEAK_CHECK_STANDIN_TARGET" >>"${LEAK_CHECK_RECORD:-/dev/null}"
	started=$(date +%s%N)
	said=$(PUBLIC_LEAK_CHECK_VISIBILITY_READ=$LEAK_CHECK_STANDIN_TARGET GH_HOST=github.com \
		gh api --hostname github.com "repos/$LEAK_CHECK_STANDIN_TARGET" --jq .visibility 2>&1 </dev/null)
	status=$?
	printf 'read exit %s started %s ended %s said %s\n' "$status" "$started" "$(date +%s%N)" "$(printf '%s' "$said" | tr '\n' ' ')" >>"${LEAK_CHECK_RECORD:-/dev/null}"
fi
`
	}
	return leakguardtest.Recording(script + "exit 0\n")
}

// ghCheckRead is what ghCheckReadingLikeThePinnedCheck recorded of its visibility read: the exit
// status of its gh, how long that gh ran, and what it printed, standard output and error together
// on one line.
type ghCheckRead struct {
	status  int
	ran     time.Duration
	printed string
}

// ghCheckReadOf is the visibility read the stand-in recorded in checked, its record, and whether
// it recorded one.
func ghCheckReadOf(t *testing.T, checked string) (ghCheckRead, bool) {
	t.Helper()
	for line := range strings.Lines(checked) {
		rest, ok := strings.CutPrefix(line, "read exit ")
		if !ok {
			continue
		}
		head, printed, _ := strings.Cut(strings.TrimSuffix(rest, "\n"), " said ")
		var read ghCheckRead
		var started, ended int64
		if _, err := fmt.Sscanf(head, "%d started %d ended %d", &read.status, &started, &ended); err != nil {
			t.Fatalf("the shared check recorded its visibility read as %q, which this cannot read: %v", line, err)
		}
		read.ran, read.printed = time.Duration(ended-started), strings.TrimSpace(printed)
		return read, true
	}
	return ghCheckRead{}, false
}

// ghBoundStop is the words internal/leakguard says of a child its context's deadline stopped
// (stoppedError.said), which `legion gh` prints when visibilityAnswerBound stops the read it
// answers the shared check's own visibility read with. Were the wording to change, such a read
// would still fail ghCheckRead.failure, by its status and what it printed.
const ghBoundStop = "was stopped by its bound"

// failure is how the read failed, and "" where it answered public: the stand-in gh's answer,
// which `legion gh` hands back from the guard's REST read.
func (r ghCheckRead) failure() string {
	switch {
	case strings.Contains(r.printed, ghBoundStop):
		return fmt.Sprintf("the shared check's visibility read was stopped by its bound after %.1f s: %q", r.ran.Seconds(), r.printed)
	case r.status != 0 || r.printed != "public":
		return fmt.Sprintf("the shared check's visibility read exited %d after %.1f s and printed %q; want exit 0 and public, the stand-in gh's answer, which `legion gh` hands back from the guard's REST read", r.status, r.ran.Seconds(), r.printed)
	}
	return ""
}

// ghGuardLookup is what the stand-in gh records of a REST visibility read of acme/widgets: the
// guard's own, which asks for the response's headers, and the shared check's, which `legion gh`
// answers by that same read (answerVisibilityRead).
const ghGuardLookup = "visibility api --hostname github.com repos/acme/widgets --include --jq .visibility\n"

// A call counts as a read only where the shared check counts it as one, so no read spends a
// visibility read in the check either. That every call ghroute admits as a read is one the pinned
// check reads as a read too, over generated spellings and without a process, is ghroute's
// TestGhReadsOnlyWhatTheSharedCheckReadsAsARead.
//
// And end to end, against ghCheckReadingLikeThePinnedCheck, a call outside the read grammar is
// guarded whole. Where the check reads it as no read — a listed leaf by its alias `pr ls`, a gh
// api endpoint written as a whole URL, GitHub's GraphQL endpoint with a query or a fragment after
// it (`graphql?x=1`, `/graphql#x`), `-i=` with a false value, which collect reads as `-f`, `--`
// before a word collect reads as a method, an empty value before a second option collect then
// takes as that value, so that the word after it is the method or the endpoint, a method other
// than GET, a field and an input — the check looks up its target's visibility, and the guard reads
// it too where the call names acme/widgets, so it costs what it cost on main and no less. Where
// the check reads it as a read — `--` before an endpoint, an empty value before a method, which
// collect takes as that value while gh sends the method, a value after `=` or joined to its short
// option (`--method=GET`, `-XGET`), an empty header and one beginning with `-`, and a listed leaf
// given a value after `=` — it costs the guard's read alone. The shared check's lookup reaches gh
// as the guard's REST read, which `legion gh` answers it with, and the check records it. A lookup
// that did not answer public fails as what it was, stopped by visibilityAnswerBound after so many
// seconds or ending with its own status and words, and not as a read missing from gh's count.
func TestLegionGhReadsOnlyWhatTheSharedCheckReadsAsARead(t *testing.T) {
	// The guard's own lookup is a whole guard's, which reads the visibility only where the
	// deployment names its rule file.
	run := ghGuardRig(t, leakguardtest.WriteRules(t, t.TempDir()))
	for _, tc := range []struct {
		args                   []string
		checkLooks, guardLooks bool
	}{
		{[]string{"pr", "ls", "-R", "acme/widgets"}, true, true},
		{[]string{"api", "https://api.github.com/repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "graphql?x=1"}, true, false},
		{[]string{"api", "/graphql#x"}, true, false},
		{[]string{"api", "-i=false", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "repos/acme/widgets/pulls", "-i=f"}, true, true},
		{[]string{"api", "-i=F", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "-i=FALSE", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "-i=False", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "--", "-XPOST"}, true, false},
		{[]string{"api", "--", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "-q", "", "-q", "-XPOST", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "--jq", "", "--jq", "graphql", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "repos/acme/widgets/pulls", "--jq", "", "-XPOST"}, false, true},
		{[]string{"api", "--jq=.x", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "-q.x", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "-H", "", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "-H", "-x", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "--method=GET", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "-XGET", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "-X", "POST", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "-f", "x=y", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "--input", "-", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"pr", "view", "1", "--repo=acme/widgets"}, false, true},
	} {
		t.Run("guarded whole: "+strings.Join(tc.args, " "), func(t *testing.T) {
			got := run(t, ghCheckAt(t, ghCheckReadingLikeThePinnedCheck(tc.args)), "", tc.args...)
			if got.code != 7 {
				t.Errorf("legion gh %q = exit %d, stderr %q; want gh's own 7", tc.args, got.code, got.stderr)
			}
			if want := ghGuardRun(tc.args); !strings.HasPrefix(got.checked, want) {
				t.Errorf("the shared check recorded %q, want its run of the call first, %q", got.checked, want)
			}
			looked := strings.Contains(got.checked, "lookup acme/widgets\n")
			if looked != tc.checkLooks {
				t.Errorf("the shared check read the visibility: %v, want %v: %q", looked, tc.checkLooks, got.checked)
			}
			read, recorded := ghCheckReadOf(t, got.checked)
			if looked && !recorded {
				t.Errorf("the shared check looked up the visibility and recorded no read after it, so what that read did cannot be told: %q", got.checked)
			}
			wantReads := 0
			for _, looks := range []bool{tc.checkLooks, tc.guardLooks} {
				if looks {
					wantReads++
				}
			}
			if recorded && read.failure() != "" {
				// gh's count would then miss a read the record already explains: one the bound
				// stopped before it reached gh, or one that reached gh and failed after.
				t.Error(read.failure())
			} else if reads := strings.Count(got.gh, ghGuardLookup); reads != wantReads {
				t.Errorf("gh was asked the visibility %d times, want %d (the check's read %v, the guard's %v): %q", reads, wantReads, tc.checkLooks, tc.guardLooks, got.gh)
			}
			if strings.Contains(got.log, `visibility="checked-only"`) {
				t.Errorf("the call log reads a call the guard read whole as checked-only: %q", got.log)
			}
		})
	}
}

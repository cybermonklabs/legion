package workflow

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// skippedRun is a check run as intake's decoder reads it from a settlement whose skipped group
// names it and whose check_runs entry carries GitHub's word for it (decodeChecks): skipped, the
// one conclusion the decoder puts on a run, with the word skipped.
func skippedRun(name string, id int64) record.AttemptRun {
	return record.AttemptRun{Name: name, ID: id, Conclusion: record.RunSkipped, GitHubConclusion: "skipped"}
}

// wordedRun is a check run as intake's decoder reads it from a settlement whose skipped group does
// not name it: GitHub's word for it and no conclusion, which classify then takes from the failed
// and cancelled groups (stampedRuns).
func wordedRun(name string, id int64, word string) record.AttemptRun {
	return record.AttemptRun{Name: name, ID: id, GitHubConclusion: word}
}

// concluded is check with conclusion, as a text names a check that did not pass: GitHub's word
// for its run, or the daemon's conclusion where the run carries none.
func concluded(check, conclusion string) texts.CheckConclusion {
	return texts.CheckConclusion{Check: check, Conclusion: conclusion}
}

// The suites a pull request holds back for the merge queue skip on its head, and the CI message
// names each of them in its list of skipped checks (skipped), never among the checks that passed,
// and counts the checks that ran and passed - build and lint, which GitHub concluded success, and
// advice, which it concluded neutral (dispatch://INFRA-4135 R1, and its plan's forward-merge rule
// for dispatch://INFRA-3686's records). The seven are the ones GitHub concluded skipped at
// cybermonklabs/legion#3981's head fb63391ff0d3 (`gh api --paginate
// "repos/cybermonklabs/legion/commits/fb63391ff0d3/check-runs?filter=latest&per_page=100"`). The
// settlement is the one the decoder reads from the listener's: the seven in its skipped group,
// and each run with its word.
func TestASettlementNamesAHeldBackSuiteSkipped(t *testing.T) {
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	applyRefusingNothing(t, pool, mergingEngine(), settledRunsAt("head", 1, []record.AttemptRun{
		wordedRun("advice", 1, "neutral"),
		wordedRun("build", 2, "success"),
		skippedRun("dispatch-memory", 3),
		skippedRun("envoy-go", 4),
		skippedRun("envoy-go-docs-finalizers", 5),
		skippedRun("envoy-go-race", 6),
		skippedRun("envoy-go-race-api", 7),
		skippedRun("envoy-go-race-docs", 8),
		skippedRun("envoy-smoke", 9),
		wordedRun("lint", 10, "success"),
	}))
	assertRoleMessages(t, pool, "LEGION-208", tell(t, nil, settledCI{passed: 3, skipped: []string{"dispatch-memory", "envoy-go",
		"envoy-go-docs-finalizers", "envoy-go-race", "envoy-go-race-api", "envoy-go-race-docs", "envoy-smoke"}}))
}

// A draft pull request's CI runs almost nothing: at cybermonklabs/legion#4180's head 061ca442
// GitHub's 52 check runs are 46 skipped, 2 failure and 4 success (`gh api --paginate
// "repos/cybermonklabs/legion/commits/061ca442/check-runs?filter=latest&per_page=100"`), and the
// message the daemon sent for that head named the skipped ones succeeded. The CI message names
// the 46 skipped in name order in its list of skipped checks, names CodeQL and ci-ok as not passed
// by GitHub's word, failure, and counts the four Analyze jobs as passed; no check is named
// succeeded.
func TestASettlementShapedAs4180NamesFortySixChecksSkipped(t *testing.T) {
	skipped := []string{"airlock-lean", "broker-client", "broker-multiprocess", "browser-image", "cancel-started", "changes",
		"ci-filters", "ci-images", "claude-envoy", "conflicts-script", "contracts", "daemon-go", "dispatch-browser",
		"dispatch-memory", "dispatch-multiprocess", "dispatch-two-builds", "dispatch-ui", "docs", "envoy-client",
		"envoy-e2e-local", "envoy-go", "envoy-go-checks", "envoy-go-docs-finalizers", "envoy-go-race", "envoy-go-race-api",
		"envoy-go-race-docs", "envoy-infra-go", "envoy-session-stall", "envoy-smoke", "lint", "listener-server-change",
		"pi-envoy", "pi-legion", "pi-shared", "plugin-pack", "pr-title", "private-names-port", "proof-sdk-offline",
		"sessions-script", "test", "test-fixtures", "text-checks", "tool-fallback", "tool-fallback-arm", "typecheck",
		"worker-image"}
	failed := []string{"CodeQL", "ci-ok"}
	succeeded := []string{"Analyze (actions)", "Analyze (go)", "Analyze (javascript-typescript)", "Analyze (python)"}
	if len(skipped) != 46 || len(failed) != 2 || len(succeeded) != 4 {
		t.Fatalf("the head's runs are %d skipped, %d failure and %d success, want 46, 2 and 4", len(skipped), len(failed), len(succeeded))
	}
	// The settlement as the decoder reads it: the skipped group's runs skipped with their word,
	// every other run with its word alone, and the failed group in Failing.
	var runs []record.AttemptRun
	for _, name := range skipped {
		runs = append(runs, skippedRun(name, int64(len(runs)+1)))
	}
	for _, group := range []struct {
		names []string
		word  string
	}{{failed, "failure"}, {succeeded, "success"}} {
		for _, name := range group.names {
			runs = append(runs, wordedRun(name, int64(len(runs)+1), group.word))
		}
	}
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	applyRefusingNothing(t, pool, mergingEngine(), settledRunsAt("head", 1, runs, failed...))

	assertRoleMessages(t, pool, "LEGION-208", tell(t, nil, settledCI{passed: 4,
		notPassed: []texts.CheckConclusion{concluded("CodeQL", "failure"), concluded("ci-ok", "failure")},
		skipped:   slices.Sorted(slices.Values(skipped))}))
	message := CISettledMessage("LEGION-208", recordedPR(t, pool, "LEGION-208"), phase.Testing)
	if len(message.Skipped) != 46 || len(message.NotPassed) != 2 || message.Passed != 4 {
		t.Errorf("the CI message names %d skipped and %d not passed and counts %d passed, want 46, 2 and 4: %+v",
			len(message.Skipped), len(message.NotPassed), message.Passed, message)
	}
	for _, check := range message.NotPassed {
		if check.Conclusion == string(record.RunSucceeded) || slices.Contains(skipped, check.Check) {
			t.Errorf("the CI message names %+v as not passed, want no check succeeded and no skipped one there", check)
		}
	}
}

// A listener of an earlier release sends neither a skipped group nor a word, and its settlement
// decodes to runs with neither (intake's
// TestDecodeChecksReadsASettlementWithNoSkippedGroupOrConclusionWordAsToday): the CI message is
// the record it was before the daemon read either, each check named by the daemon's conclusion,
// and it names no check skipped.
func TestASettlementWithNoSkippedGroupOrConclusionWordTellsTodaysLine(t *testing.T) {
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	applyRefusingNothing(t, pool, mergingEngine(),
		settledRunsAt("head", 1, []record.AttemptRun{{Name: "build", ID: 1}, {Name: "unit", ID: 2}}, "unit"))
	assertRoleMessages(t, pool, "LEGION-208", tell(t, nil, settledCI{passed: 1, notPassed: []texts.CheckConclusion{failed("unit")}}))
}

// The listener as it stands before it carries a word sends the skipped group and no word: the CI
// message names the check the group names skipped and every other one by its group
// (dispatch://INFRA-4135 R1, a listener that sends no conclusion word).
func TestASettlementWithASkippedGroupAndNoWordNamesThoseSkipped(t *testing.T) {
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	applyRefusingNothing(t, pool, mergingEngine(), settledRunsAt("head", 1, []record.AttemptRun{
		{Name: "build", ID: 1}, {Name: "envoy-go", ID: 2, Conclusion: record.RunSkipped}, {Name: "unit", ID: 3}}, "unit"))
	assertRoleMessages(t, pool, "LEGION-208", tell(t, nil,
		settledCI{passed: 1, notPassed: []texts.CheckConclusion{failed("unit")}, skipped: []string{"envoy-go"}}))
}

// settledOutcome is what a settlement of LEGION-208's head left beside what the CI message says:
// the issue's phase, the implementer's pass, every outbox row but the role messages (the notices,
// the supervise rows, the Dispatch statuses), each as its kind and payload in the order queued,
// and the role topic of each role message, in order.
type settledOutcome struct {
	phase  phase.Phase
	pass   int
	outbox []string
	told   []string
}

// outcomeOf reads the settledOutcome pool holds.
func outcomeOf(t *testing.T, pool *pgxpool.Pool) settledOutcome {
	t.Helper()
	outcome := settledOutcome{phase: issuePhase(t, pool), pass: implementerPass(t, pool), told: roleMessageTokens(t, pool)}
	rows, err := pool.Query(t.Context(), "select kind || ' ' || payload::text from outbox where kind <> 'role_message' order by id")
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("scan an outbox row: %v", err)
		}
		outcome.outbox = append(outcome.outbox, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the outbox: %v", err)
	}
	return outcome
}

// A settlement whose only checks that did not pass are skipped leaves what the same settlement
// with those runs succeeded leaves, in every phase TestASettlementMovesNothingAndTellsTheThreeRoles
// covers: a skipped run decides as a succeeded one (dispatch://INFRA-4135 R2), and only the CI
// message, which names the skipped checks skipped, differs. After either the issue is in the
// phase it was in, the implementer's pass and every outbox row but the role messages - the
// notices, the supervise rows, the Dispatch statuses - are the same, and the same roles are told.
func TestAHeadWhoseOnlyNonSuccessRunsAreSkippedMovesNoIssueSkipped(t *testing.T) {
	skipped := []record.AttemptRun{wordedRun("build", 1, "success"), skippedRun("dispatch-memory", 2), skippedRun("envoy-go", 3)}
	succeeded := []record.AttemptRun{wordedRun("build", 1, "success"), wordedRun("dispatch-memory", 2, "success"), wordedRun("envoy-go", 3, "success")}
	for _, tc := range []struct {
		from   phase.Phase
		status string
		// roles are the roles told, in the order tellChecks addresses them.
		roles []claim.Role
	}{
		{phase.Admitted, "in_progress", []claim.Role{claim.RoleArchitect, claim.RoleImplementer}},
		{phase.Implementing, "in_progress", []claim.Role{claim.RoleArchitect, claim.RoleImplementer}},
		{phase.Testing, "testing", []claim.Role{claim.RoleArchitect, claim.RoleTester, claim.RoleImplementer}},
		{phase.Reviewing, "needs_review", []claim.Role{claim.RoleArchitect, claim.RoleReviewer, claim.RoleImplementer}},
		{phase.Accepting, "needs_review", []claim.Role{claim.RoleArchitect, claim.RoleImplementer}},
		{phase.Retro, "retro", []claim.Role{claim.RoleArchitect, claim.RoleImplementer}},
		{phase.Merging, "retro", []claim.Role{claim.RoleArchitect, claim.RoleMerger, claim.RoleImplementer}},
	} {
		t.Run(string(tc.from), func(t *testing.T) {
			settle := func(runs []record.AttemptRun) (*pgxpool.Pool, settledOutcome) {
				pool := migratedPool(t)
				seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
					Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
				seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
					Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
				if role := RoleFor(tc.from); role != "" && role != claim.RoleImplementer {
					seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: role, Claim: claim.Token(string(role) + "-claim")})
				}
				applyRefusingNothing(t, pool, mergingEngine(), settledRunsAt("head", 1, runs))
				return pool, outcomeOf(t, pool)
			}
			pool, got := settle(skipped)
			_, want := settle(succeeded)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("a settlement with skipped runs left %+v, want what the same runs succeeded leave: %+v", got, want)
			}
			if got.phase != tc.from {
				t.Fatalf("after the settlement the issue is in %s, want it left in %s", got.phase, tc.from)
			}
			assertRoleMessages(t, pool, "LEGION-208", statusesTold(t, tc.roles, tc.from,
				settledCI{passed: 1, skipped: []string{"dispatch-memory", "envoy-go"}}))
		})
	}
}

// A settlement that changes only what a run is - the same run ids at a newer generation, envoy-go
// reported first with no skipped group and no word and then in the skipped group with the word
// skipped - is a new judgement of the head (classify.JudgementChanged compares runs whole): it is
// recorded, the roles are told the new record, and it moves nothing.
func TestASettlementThatOnlyNamesARunSkippedIsToldAndMovesNothing(t *testing.T) {
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	engine := mergingEngine()
	applyRefusingNothing(t, pool, engine, settledRunsAt("head", 1, []record.AttemptRun{{Name: "build", ID: 1}, {Name: "envoy-go", ID: 2}}))
	messages := tell(t, nil, settledCI{passed: 2})
	assertRoleMessages(t, pool, "LEGION-208", messages)

	applyRefusingNothing(t, pool, engine, settledRunsAt("head", 2, []record.AttemptRun{{Name: "build", ID: 1}, skippedRun("envoy-go", 2)}))
	assertRoleMessages(t, pool, "LEGION-208", tell(t, messages, settledCI{passed: 1, skipped: []string{"envoy-go"}}))
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("after the settlement the issue is in %s, want it left in testing", got)
	}
	if got := noticeKinds(t, pool, "LEGION-208"); len(got) != 0 {
		t.Fatalf("the architect was told %v, want nothing but the role messages", got)
	}
	assertOutboxCount(t, pool, "supervise", 0)
	assertOutboxCount(t, pool, "dispatch_status", 0)
	if pass := implementerPass(t, pool); pass != 0 {
		t.Fatalf("pass = %d, want none counted: nothing went back", pass)
	}
	pr := recordedPR(t, pool, "LEGION-208")
	runs := []record.AttemptRun{{Name: "build", ID: 1, Head: "head", Conclusion: record.RunSucceeded},
		{Name: "envoy-go", ID: 2, Head: "head", Conclusion: record.RunSkipped, GitHubConclusion: "skipped"}}
	if pr.Generation != 2 || !reflect.DeepEqual(pr.CheckRuns, runs) || len(pr.Failing) != 0 || len(pr.Cancelled) != 0 {
		t.Fatalf("the pull request keeps generation %d, runs %+v, failing %v, cancelled %v; want generation 2, %+v, and no name red",
			pr.Generation, pr.CheckRuns, pr.Failing, pr.Cancelled, runs)
	}
}

// The notice of a head GitHub will not merge, and the reviewer's message of it, name each check
// run GitHub skipped on the head in their list of skipped checks, and count the head's check runs
// and commit statuses that passed without it: a required check GitHub skipped there never ran,
// and the architect or the reviewer reading it must not take it for a pass
// (dispatch://INFRA-4135 R1).
func TestTheStuckNoticeNamesASkippedCheckSkipped(t *testing.T) {
	pool := migratedPool(t)
	seedMergeStateRound(t, pool, record.MergeStateBlocked)
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	apply("settled", naming(settledRunsAt(headOne, 1, []record.AttemptRun{wordedRun("unit tests", 2, "success"), skippedRun("envoy-go", 3)}),
		status("deploy", "success", 1)))
	apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: headOne,
		HeadSHA: headOne, Author: testReviewApp, Body: reviewBody("ship it", headOne)})
	told := reviewStuckNotices(t, pool)
	if len(told) != 1 || told[0].MergeState != string(record.MergeStateBlocked) || !slices.Equal(told[0].Skipped, []string{"envoy-go"}) ||
		len(told[0].NotPassed) != 0 || len(told[0].NotPassedStatuses) != 0 || told[0].Passed == nil || *told[0].Passed != 2 {
		t.Fatalf("review-stuck notices %+v, want one of the blocked head naming envoy-go skipped, nothing not passed, and 2 passed", told)
	}
	message, found := stuckApprovedRound(t, pool)
	if !found || !slices.Equal(message.Skipped, []string{"envoy-go"}) || len(message.NotPassed) != 0 || len(message.NotPassedStatuses) != 0 ||
		message.Passed != 2 {
		t.Fatalf("the reviewer's approved-round-stuck message %+v (found %t), want envoy-go skipped, nothing not passed, and 2 passed", message, found)
	}
}

// A released worker woken by a settlement whose only checks that did not pass are skipped is told
// they were skipped, in name order, in the task's list of skipped checks, and never counts them
// among the checks that passed (dispatch://INFRA-4135 R1). A run is skipped by its conclusion or
// by GitHub's word: the listener as it stands sends a skipped group and no word, which gives runs
// with the skipped conclusion and no word, and a settlement whose skipped group the decoder could
// not read gives runs with the word skipped and no skipped conclusion; the CI message names either
// skipped, and the task names them skipped too. A failed or cancelled check is named with GitHub's
// word for its run, as the CI message names it, beside the skipped checks and the failed
// statuses. A skipped run the settlement names failed or cancelled is that and not skipped, which
// no listener sends. A settlement with no skipped run counts every check that passed by its word,
// success or neutral; one with no word either wakes the worker with the record it gave before.
func TestTheWakeTaskNamesSkippedChecksSkipped(t *testing.T) {
	checks := func(runs []record.AttemptRun, failing, cancelled []string, statuses ...record.CommitStatus) intake.PullRequestChecks {
		fact := settlement("head1", failing...)
		fact.CheckRuns, fact.Cancelled, fact.Statuses = runs, cancelled, statuses
		return fact
	}
	onlySkipped := checks([]record.AttemptRun{wordedRun("build", 1, "success"), skippedRun("envoy-go", 2), skippedRun("envoy-go-race", 3)}, nil, nil)
	for _, shape := range []struct {
		name      string
		fact      intake.PullRequestChecks
		notPassed []texts.CheckConclusion
		skipped   []string
		statuses  []texts.StatusState
		passed    int
	}{
		{"only skipped checks did not pass", onlySkipped, nil, []string{"envoy-go", "envoy-go-race"}, nil, 1},
		{"in name order", checks([]record.AttemptRun{skippedRun("envoy-smoke", 3), skippedRun("dispatch-memory", 2), wordedRun("build", 1, "success")}, nil, nil),
			nil, []string{"dispatch-memory", "envoy-smoke"}, nil, 1},
		{"a skipped group and no word", checks([]record.AttemptRun{{Name: "build", ID: 1}, {Name: "envoy-go", ID: 2, Conclusion: record.RunSkipped}}, nil, nil),
			nil, []string{"envoy-go"}, nil, 1},
		{"a skipped group the decoder could not read", checks([]record.AttemptRun{wordedRun("build", 1, "success"),
			wordedRun("envoy-go", 2, "skipped"), wordedRun("dispatch-memory", 3, "skipped")}, nil, nil),
			nil, []string{"dispatch-memory", "envoy-go"}, nil, 1},
		{"a failed check run", checks([]record.AttemptRun{skippedRun("envoy-go", 2), wordedRun("unit", 3, "failure")}, []string{"unit"}, nil),
			[]texts.CheckConclusion{concluded("unit", "failure")}, []string{"envoy-go"}, nil, 0},
		{"a cancelled check run", checks([]record.AttemptRun{wordedRun("e2e", 1, "cancelled"), skippedRun("envoy-go", 2)}, nil, []string{"e2e"}),
			[]texts.CheckConclusion{concluded("e2e", "cancelled")}, []string{"envoy-go"}, nil, 0},
		{"a failed commit status", checks([]record.AttemptRun{wordedRun("build", 1, "success"), skippedRun("envoy-go", 2)}, nil, nil,
			status("deploy", "success", 1), status("ci", "failure", 1)),
			nil, []string{"envoy-go"}, []texts.StatusState{red("ci", "failure")}, 2},
		{"a skipped run the settlement names failed", checks([]record.AttemptRun{skippedRun("envoy-go", 2)}, []string{"envoy-go"}, nil),
			[]texts.CheckConclusion{failed("envoy-go")}, nil, nil, 0},
		{"a skipped run it names cancelled", checks([]record.AttemptRun{skippedRun("envoy-go", 2)}, nil, []string{"envoy-go"}),
			[]texts.CheckConclusion{concluded("envoy-go", string(record.RunCancelled))}, nil, nil, 0},
		{"a failed run with its word failure", checks([]record.AttemptRun{wordedRun("unit", 1, "failure")}, []string{"unit"}, nil),
			[]texts.CheckConclusion{concluded("unit", "failure")}, nil, nil, 0},
		{"a failed run with its word timed_out", checks([]record.AttemptRun{wordedRun("unit", 1, "timed_out")}, []string{"unit"}, nil),
			[]texts.CheckConclusion{concluded("unit", "timed_out")}, nil, nil, 0},
		{"a cancelled run with its word cancelled", checks([]record.AttemptRun{wordedRun("e2e", 1, "cancelled")}, nil, []string{"e2e"}),
			[]texts.CheckConclusion{concluded("e2e", "cancelled")}, nil, nil, 0},
		{"words and no skipped run", checks([]record.AttemptRun{wordedRun("advice", 1, "neutral"), wordedRun("build", 2, "success")}, nil, nil),
			nil, nil, nil, 2},
		{"no skipped run and no word", settlement("head1"), nil, nil, nil, 1},
	} {
		got := SettlementWake("LEGION-208", phase.Implementing, "sjawhar/legion", shape.fact)
		if !reflect.DeepEqual(got.NotPassed, shape.notPassed) || !reflect.DeepEqual(got.Skipped, shape.skipped) ||
			!reflect.DeepEqual(got.NotPassedStatuses, shape.statuses) || got.Passed == nil || *got.Passed != shape.passed {
			t.Errorf("%s: the settlement's wake = %+v, want the checks %+v not passed, %v skipped, the statuses %+v not passed and %d passed",
				shape.name, got, shape.notPassed, shape.skipped, shape.statuses, shape.passed)
		}
	}
	for _, wake := range []struct {
		name string
		fact intake.PullRequestChecks
		// line is the line of the woken role's task that names its skipped checks, "" for none.
		line string
	}{
		{"the wake on only skipped checks", onlySkipped, "skipped[2]: envoy-go,envoy-go-race"},
		{"the wake on no skipped run and no word", settlement("head1"), ""},
	} {
		t.Run(wake.name, func(t *testing.T) {
			rig := newWakeRig(t)
			rig.apply("settled", wake.fact)
			want := SettlementWake("LEGION-208", phase.Implementing, "sjawhar/legion", wake.fact)
			// The wake brings back a session an idle release stopped (newWakeRig), which its task names.
			want.StoppedBy = StoppedByRelease.word()
			oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, taskText(want), "passed: 1")
			task := wakeStarts(t, rig.pool)[0].Task
			if named := strings.Contains(task, "skipped"); wake.line == "" && named || wake.line != "" && !strings.Contains(task, wake.line) {
				t.Fatalf("the woken role's task = %q, want its skipped checks named as %q", task, wake.line)
			}
		})
	}
}

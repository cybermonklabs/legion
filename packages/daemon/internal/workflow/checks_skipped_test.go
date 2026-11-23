package workflow

import (
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// skippedSaid is the sentence each text the roles read of a head's CI says once where it names a
// check skipped (dispatch://INFRA-4135 R1), spelled out here so a change to it fails these tests.
const skippedSaid = "a skipped check ran nothing on this head"

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

// The suites a pull request holds back for the merge queue skip on its head, and the CI message
// names each of them skipped, by the word GitHub gave it, beside the checks that ran, each by its
// own word, and says once, after the checks, that a skipped check ran nothing on this head
// (dispatch://INFRA-4135 R1). The seven are the ones GitHub concluded skipped at
// cybermonklabs/legion#3981's head fb63391ff0d3 (`gh api --paginate
// "repos/cybermonklabs/legion/commits/fb63391ff0d3/check-runs?filter=latest&per_page=100"`),
// beside build and lint, which GitHub concluded success, and advice, which it concluded neutral.
// The settlement is the one the decoder reads from the listener's: the seven in its skipped group,
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
	assertRoleMessages(t, pool, "LEGION-208", tell(t, nil, `CI settled at head: "advice" neutral, "build" success, `+
		`"dispatch-memory" skipped, "envoy-go" skipped, "envoy-go-docs-finalizers" skipped, "envoy-go-race" skipped, `+
		`"envoy-go-race-api" skipped, "envoy-go-race-docs" skipped, "envoy-smoke" skipped, "lint" success; `+skippedSaid))
}

// A draft pull request's CI runs almost nothing: at cybermonklabs/legion#4180's head 061ca442
// GitHub's 52 check runs are 46 skipped, 2 failure and 4 success (`gh api --paginate
// "repos/cybermonklabs/legion/commits/061ca442/check-runs?filter=latest&per_page=100"`), and the
// message the daemon sent for that head named the skipped ones succeeded. The CI message names each
// of the 52 by its word, in name order - the 46 skipped, CodeQL and ci-ok failure, the four Analyze
// jobs success - and says once that a skipped check ran nothing on this head.
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
	words := make(map[string]string, 52)
	var runs []record.AttemptRun
	for _, name := range skipped {
		runs = append(runs, skippedRun(name, int64(len(runs)+1)))
		words[name] = "skipped"
	}
	for _, group := range []struct {
		names []string
		word  string
	}{{failed, "failure"}, {succeeded, "success"}} {
		for _, name := range group.names {
			runs = append(runs, wordedRun(name, int64(len(runs)+1), group.word))
			words[name] = group.word
		}
	}
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	applyRefusingNothing(t, pool, mergingEngine(), settledRunsAt("head", 1, runs, failed...))

	names := slices.Sorted(maps.Keys(words))
	named := make([]string, len(names))
	for i, name := range names {
		named[i] = strconv.Quote(name) + " " + words[name]
	}
	want := "CI settled at head: " + strings.Join(named, ", ") + "; " + skippedSaid
	assertRoleMessages(t, pool, "LEGION-208", tell(t, nil, want))
	line := settledChecks(recordedPR(t, pool, "LEGION-208"))
	for _, count := range []struct {
		said string
		want int
	}{{`" skipped`, 46}, {`" failure`, 2}, {`" success`, 4}, {`" succeeded`, 0}, {skippedSaid, 1}} {
		if got := strings.Count(line, count.said); got != count.want {
			t.Errorf("the CI line says %q %d times, want %d: %q", count.said, got, count.want, line)
		}
	}
}

// A listener of an earlier release sends neither a skipped group nor a word, and its settlement
// decodes to runs with neither (intake's
// TestDecodeChecksReadsASettlementWithNoSkippedGroupOrConclusionWordAsToday): the CI message reads
// word for word as it did before the daemon read either, and says nothing of a skipped check.
func TestASettlementWithNoSkippedGroupOrConclusionWordTellsTodaysLine(t *testing.T) {
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	applyRefusingNothing(t, pool, mergingEngine(),
		settledRunsAt("head", 1, []record.AttemptRun{{Name: "build", ID: 1}, {Name: "unit", ID: 2}}, "unit"))
	assertRoleMessages(t, pool, "LEGION-208", tell(t, nil, `CI settled at head: "build" succeeded, "unit" failed`))
}

// The listener as it stands before it carries a word sends the skipped group and no word: the CI
// message names the check the group names skipped and every other one by its group, and says once
// that a skipped check ran nothing on this head (dispatch://INFRA-4135 R1, a listener that sends
// no conclusion word).
func TestASettlementWithASkippedGroupAndNoWordNamesThoseSkipped(t *testing.T) {
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	applyRefusingNothing(t, pool, mergingEngine(), settledRunsAt("head", 1, []record.AttemptRun{
		{Name: "build", ID: 1}, {Name: "envoy-go", ID: 2, Conclusion: record.RunSkipped}, {Name: "unit", ID: 3}}, "unit"))
	assertRoleMessages(t, pool, "LEGION-208", tell(t, nil,
		`CI settled at head: "build" succeeded, "envoy-go" skipped, "unit" failed; `+skippedSaid))
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
		// In merging the implementer is not told (cybermonklabs/legion#4979).
		{phase.Merging, "retro", []claim.Role{claim.RoleArchitect, claim.RoleMerger}},
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
			told := make([]record.RoleMessage, 0, len(tc.roles))
			for _, role := range tc.roles {
				told = append(told, record.RoleMessage{Role: roleToken(t, role),
					Body: toldBody(`CI settled at head: "build" success, "dispatch-memory" skipped, "envoy-go" skipped; ` + skippedSaid)})
			}
			assertRoleMessages(t, pool, "LEGION-208", told)
		})
	}
}

// A settlement that changes only what a run is - the same run ids at a newer generation, envoy-go
// reported first with no skipped group and no word and then in the skipped group with the word
// skipped - is a new judgement of the head (classify.JudgementChanged compares runs whole): it is
// recorded, the roles are told the new line, and it moves nothing.
func TestASettlementThatOnlyNamesARunSkippedIsToldAndMovesNothing(t *testing.T) {
	pool := migratedPool(t)
	seedSettlingRound(t, pool)
	engine := mergingEngine()
	applyRefusingNothing(t, pool, engine, settledRunsAt("head", 1, []record.AttemptRun{{Name: "build", ID: 1}, {Name: "envoy-go", ID: 2}}))
	messages := tell(t, nil, `CI settled at head: "build" succeeded, "envoy-go" succeeded`)
	assertRoleMessages(t, pool, "LEGION-208", messages)

	applyRefusingNothing(t, pool, engine, settledRunsAt("head", 2, []record.AttemptRun{{Name: "build", ID: 1}, skippedRun("envoy-go", 2)}))
	assertRoleMessages(t, pool, "LEGION-208", tell(t, messages, `CI settled at head: "build" succeeded, "envoy-go" skipped; `+skippedSaid))
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

// The notice of a head GitHub will not merge names each check run GitHub reported on the head by
// the word GitHub gave it, a skipped one skipped, and says once, after the commit statuses, that a
// skipped check ran nothing on this head: a required check GitHub skipped there never ran, and
// the architect reading the notice must not take it for a pass (dispatch://INFRA-4135 R1).
func TestTheStuckNoticeNamesASkippedCheckSkipped(t *testing.T) {
	pool := migratedPool(t)
	seedMergeStateRound(t, pool, record.MergeStateBlocked)
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	apply("settled", naming(settledRunsAt(headOne, 1, []record.AttemptRun{wordedRun("unit tests", 2, "success"), skippedRun("envoy-go", 3)}),
		status("deploy", "success", 1)))
	apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: headOne,
		HeadSHA: headOne, Author: testReviewApp, Body: reviewBody("ship it", headOne)})
	told := reviewStuckNotices(t, pool)
	want := "GitHub reported on head " + headOne + `: "envoy-go" skipped, "unit tests" success; commit statuses: "deploy" success; ` +
		skippedSaid + ". The daemon reads the base's rules only on a conflicting read"
	if len(told) != 1 || !strings.Contains(told[0].Reason, want) {
		t.Fatalf("review-stuck notices %+v, want one reading %q", told, want)
	}
	if said := strings.Count(told[0].Reason, skippedSaid); said != 1 {
		t.Fatalf("the notice says %q %d times, want once: %q", skippedSaid, said, told[0].Reason)
	}
}

// A released worker woken by a settlement whose only checks that did not pass are skipped is told
// they were skipped, each name quoted, in name order, and that a skipped check ran nothing on this
// head, and never that the head passed every check (dispatch://INFRA-4135 R1). A run is skipped by
// its conclusion or by GitHub's word: the listener as it stands sends a skipped group and no word,
// which gives runs with the skipped conclusion and no word, and a settlement whose skipped group
// the decoder could not read gives runs with the word skipped and no skipped conclusion; the CI
// message names either skipped, and the task names them skipped too. With a check failed or
// cancelled, or a failed commit status, the skipped checks follow the failed or cancelled ones,
// which stay unquoted, and come before the statuses. A skipped run the settlement names failed or
// cancelled is that and not skipped, which no listener sends, and a run it names failed or
// cancelled with its own word (failure, timed_out or cancelled) is named failed or cancelled, as
// before. A settlement with no skipped run gives the text it gave before, whatever other word
// GitHub gave each run; one with no word either wakes the worker with the task it gave before,
// word for word.
func TestTheWakeTaskNamesSkippedChecksSkipped(t *testing.T) {
	const woken = "Your issue's pod had been released while it was idle, and this wake brought it back. " +
		"Your session, your workspace and any work you had not pushed are as you left them. " +
		"The wake is a CI settlement on your pull request's head: "
	checks := func(runs []record.AttemptRun, failing, cancelled []string, statuses ...record.CommitStatus) intake.PullRequestChecks {
		fact := settlement("head1", failing...)
		fact.CheckRuns, fact.Cancelled, fact.Statuses = runs, cancelled, statuses
		return fact
	}
	onlySkipped := checks([]record.AttemptRun{wordedRun("build", 1, "success"), skippedRun("envoy-go", 2), skippedRun("envoy-go-race", 3)}, nil, nil)
	const onlySkippedDetail = `head1 had no failed or cancelled check; skipped "envoy-go", "envoy-go-race": ` + skippedSaid
	for _, shape := range []struct {
		name   string
		fact   intake.PullRequestChecks
		detail string
	}{
		{"only skipped checks did not pass", onlySkipped, onlySkippedDetail},
		{"in name order", checks([]record.AttemptRun{skippedRun("envoy-smoke", 3), skippedRun("dispatch-memory", 2), wordedRun("build", 1, "success")}, nil, nil),
			`head1 had no failed or cancelled check; skipped "dispatch-memory", "envoy-smoke": ` + skippedSaid},
		{"a skipped group and no word", checks([]record.AttemptRun{{Name: "build", ID: 1}, {Name: "envoy-go", ID: 2, Conclusion: record.RunSkipped}}, nil, nil),
			`head1 had no failed or cancelled check; skipped "envoy-go": ` + skippedSaid},
		{"a skipped group the decoder could not read", checks([]record.AttemptRun{wordedRun("build", 1, "success"),
			wordedRun("envoy-go", 2, "skipped"), wordedRun("dispatch-memory", 3, "skipped")}, nil, nil),
			`head1 had no failed or cancelled check; skipped "dispatch-memory", "envoy-go": ` + skippedSaid},
		{"a failed check run", checks([]record.AttemptRun{skippedRun("envoy-go", 2), wordedRun("unit", 3, "failure")}, []string{"unit"}, nil),
			`head1 failed unit; skipped "envoy-go": ` + skippedSaid},
		{"a cancelled check run", checks([]record.AttemptRun{wordedRun("e2e", 1, "cancelled"), skippedRun("envoy-go", 2)}, nil, []string{"e2e"}),
			`head1 had e2e cancelled; skipped "envoy-go": ` + skippedSaid},
		{"a failed commit status", checks([]record.AttemptRun{wordedRun("build", 1, "success"), skippedRun("envoy-go", 2)}, nil, nil,
			status("deploy", "success", 1), status("ci", "failure", 1)),
			`head1 had no failed or cancelled check; skipped "envoy-go": ` + skippedSaid + "; failed commit status ci"},
	} {
		got := settlementDetail(shape.fact)
		if got != shape.detail {
			t.Errorf("%s: settlementDetail = %q, want %q", shape.name, got, shape.detail)
		}
		if strings.Contains(got, "passed every check") || strings.Count(got, skippedSaid) != 1 {
			t.Errorf("%s: settlementDetail = %q, want %q once and no %q", shape.name, got, skippedSaid, "passed every check")
		}
	}
	for _, today := range []struct {
		name   string
		fact   intake.PullRequestChecks
		detail string
	}{
		{"a skipped run the settlement names failed", checks([]record.AttemptRun{skippedRun("envoy-go", 2)}, []string{"envoy-go"}, nil), "head1 failed envoy-go"},
		{"a skipped run it names cancelled", checks([]record.AttemptRun{skippedRun("envoy-go", 2)}, nil, []string{"envoy-go"}), "head1 had envoy-go cancelled"},
		{"a failed run with its word failure", checks([]record.AttemptRun{wordedRun("unit", 1, "failure")}, []string{"unit"}, nil), "head1 failed unit"},
		{"a failed run with its word timed_out", checks([]record.AttemptRun{wordedRun("unit", 1, "timed_out")}, []string{"unit"}, nil), "head1 failed unit"},
		{"a cancelled run with its word cancelled", checks([]record.AttemptRun{wordedRun("e2e", 1, "cancelled")}, nil, []string{"e2e"}), "head1 had e2e cancelled"},
		{"words and no skipped run", checks([]record.AttemptRun{wordedRun("advice", 1, "neutral"), wordedRun("build", 2, "success")}, nil, nil),
			"head1 passed every check"},
		{"no skipped run and no word", settlement("head1"), "head1 passed every check"},
	} {
		if got := settlementDetail(today.fact); got != today.detail {
			t.Errorf("%s: settlementDetail = %q, want today's %q", today.name, got, today.detail)
		}
	}
	t.Run("the wake on only skipped checks", func(t *testing.T) {
		rig := newWakeRig(t)
		rig.apply("settled", onlySkipped)
		oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, woken+onlySkippedDetail)
		if task := wakeStarts(t, rig.pool)[0].Task; strings.Contains(task, "passed every check") {
			t.Fatalf("the woken role's task = %q, want no %q: a skipped check ran nothing", task, "passed every check")
		}
	})
	t.Run("the wake on no skipped run and no word", func(t *testing.T) {
		rig := newWakeRig(t)
		rig.apply("settled", settlement("head1"))
		oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks)
		want := woken + "head1 passed every check. Read the checks before you decide anything."
		if task := wakeStarts(t, rig.pool)[0].Task; task != want {
			t.Fatalf("the woken role's task = %q, want today's %q", task, want)
		}
	})
}

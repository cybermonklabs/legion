package workflow

// What a worker-died notice names of a failed claim on the two paths that named nothing of it: the
// tree architect's, and the held phase's. Each now carries the sentence the supervisor wrote when
// it gave up on the claim (detail), and the pod end the claim's process was last observed to have
// died of (pod_end), which is the cause no role chose — an eviction for the node's disk above all
// (INFRA-2009) — or, where there is none and the claim failed because its process died, the first
// line of the runtime's account of that death (death). Each is a field of its own
// (texts.WorkerDiedRecord, workflow's diedOf), so nothing in one reads as another.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// evictionMessage is the kubelet's message on a pod it evicted for its node's disk.
const evictionMessage = "The node was low on resource: ephemeral-storage. Container roles was using 48Gi, which exceeds its request of 0."

// evictedPodEnd is a claim's stored pod end as the runtime read it off an evicted pod
// (supervise.Claim.PodEnd, runtime.Observation.PodEnd).
const evictedPodEnd = "Evicted: " + evictionMessage

// budgetRanOut is the sentence the supervisor writes when a claim's relaunches run out
// (supervise's fail, Claim.Failure).
const budgetRanOut = "deaths with work outstanding ran out"

// roleLogTail is the tail of the log the runtime quotes at the end of a death's detail
// (sandbox's roleLog): the role's own last lines with the launcher's prefix dropped, then the
// container's own under the line that introduces them.
const roleLogTail = "   Compiling widgets v0.4.1 (/legion/workspaces/acme/widgets/legion-208)\n" +
	"error: failed to write /legion/scratch/target/debug/deps/libwidgets-3f2a9c.rlib: No space left on device (os error 28)\n" +
	"error: could not compile `widgets` (lib) due to 1 previous error\n" +
	"the container's own last lines, no role's:\n" +
	"volume trim: the volume is 97.2% full (used 50331648000 B, free 1073741824 B): at or above the high mark 85%, sweeping to 70%"

// evictedDeath is a claim's stored account of its process's last death when the cluster evicted
// its pod, in the shape the runtime writes it (supervise.Claim.LastDeathCause): supervise's
// deathCause puts the verdict before the detail sandbox's Runtime.ended writes, which is the pod
// and its phase, the pod's reason and the kubelet's message, the container's end, and role's
// last lines of the log. Neither function is reachable from this package, so the shape is
// copied from the two.
func evictedDeath(role claim.Role) string {
	return "gone: pod legion-legion-208 (uid 8a1f) Failed: " + evictedPodEnd +
		": container roles terminated (Error, exit code 137); last lines of " + string(role) + ":\n" + roleLogTail
}

// oomKilledDeath is a claim's stored account of a death whose pod gives no reason of its own,
// in the shape the runtime writes it: the pod still runs and its container ended, which the
// Sandbox runtime observes with no pod end (sandbox's observation of a running pod's terminated
// container), so the claim holds none beside it.
func oomKilledDeath(role claim.Role) string {
	return "gone: pod legion-legion-208 (uid 8a1f) container roles terminated (OOMKilled, exit code 137); last lines of " +
		string(role) + ":\n" + roleLogTail
}

// failedRoot seeds LEGION-208 at p and applies fact to it, and answers the one worker-died notice
// the failure wrote.
func failedRoot(t *testing.T, p phase.Phase, fact intake.ClaimFailed) record.Notice {
	t.Helper()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: p, Generation: 1, Status: "in_progress", Rank: "U"})
	fact.Issue = "LEGION-208"
	if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "claim-failed", fact,
		testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the %s's failed claim: %v", fact.Role, err)
	}
	notices := noticesOfKind(t, pool, "LEGION-208", "worker-died")
	if len(notices) != 1 {
		t.Fatalf("the outbox holds %d worker-died notices, want one", len(notices))
	}
	return notices[0]
}

// The tree architect's own claim failing is told to that architect and to the controller, and the
// notice names both of the things the supervisor knows: the sentence and the pod end. The
// negative control is the code at the merge base, where this path names a role and a phase and
// nothing of the failure.
func TestATreeArchitectsFailedClaimNamesWhatEndedIt(t *testing.T) {
	died := failedRoot(t, phase.Planning, intake.ClaimFailed{Role: claim.RoleArchitect, Reason: budgetRanOut, PodEnd: evictedPodEnd})
	if died.Detail != budgetRanOut || died.PodEnd != evictedPodEnd {
		t.Errorf("the tree architect's worker-died names detail %q and pod end %q, want %q and %q", died.Detail, died.PodEnd, budgetRanOut, evictedPodEnd)
	}
}

// A pod that died with no pod-level reason leaves the claim a sentence and no pod end, and the
// tree architect's worker-died still names that sentence as its detail: the notice carries no
// failure record, so its detail is the only place the architect and the controller read why the
// claim failed. The negative control is diedOf naming nothing for a fact with no pod end, which
// left this notice saying nothing of the failure at all.
func TestATreeArchitectsFailedClaimNamesTheSentenceWithNoPodEnd(t *testing.T) {
	died := failedRoot(t, phase.Planning, intake.ClaimFailed{Role: claim.RoleArchitect, Reason: budgetRanOut, Cause: supervise.CauseDied})
	if died.Detail != budgetRanOut || died.DetailCut || died.PodEnd != "" || died.Death != "" {
		t.Errorf("the tree architect's worker-died is %+v, want the supervisor's sentence as its detail, whole, and no pod end or death", died)
	}
	if died.Failure != nil {
		t.Errorf("the tree architect's worker-died carries the failure record %+v, want none: its detail is where the sentence goes", died.Failure)
	}
}

// The held path is the other one that named nothing: the role that works the issue's phase died,
// the phase is held for its architect to decide, and the worker-died beside that hold now names
// what ended the worker. The negative control is the same code at the merge base.
func TestAHeldPhasesWorkerDiedNamesWhatEndedIt(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})

	if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "implementer-failed",
		intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleImplementer, Reason: budgetRanOut, PodEnd: evictedPodEnd},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the implementer's failed claim: %v", err)
	}
	var held phase.Phase
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-208'").Scan(&held); err != nil {
		t.Fatalf("read the root: %v", err)
	}
	if held != phase.Held {
		t.Fatalf("the issue stands at %s, want it held for its architect to decide", held)
	}
	notices := noticesOfKind(t, pool, "LEGION-208", "worker-died")
	if len(notices) != 1 {
		t.Fatalf("the outbox holds %d worker-died notices, want one beside the hold", len(notices))
	}
	if died := notices[0]; died.Detail != budgetRanOut || died.PodEnd != evictedPodEnd {
		t.Errorf("the held phase's worker-died names detail %q and pod end %q, want %q and %q", died.Detail, died.PodEnd, budgetRanOut, evictedPodEnd)
	}
}

// The held path keeps from repeating the sentence its failure record already carries: with a
// cause and no pod end, the worker-died's failure record names the sentence in its detail and the
// notice's own detail is empty. A held phase whose fact names no cause — a failure recorded before
// the daemon kept one, replayed at a boot — carries no failure record at all, so its detail is the
// sentence, as the tree architect's is.
func TestAHeldPhasesWorkerDiedNamesTheSentenceOnceWithNoPodEnd(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cause      supervise.FailureCause
		wantDetail string
	}{
		{"a fact with a cause, whose failure record carries the sentence", supervise.CauseDied, ""},
		{"a fact with no cause, which carries no failure record", "", budgetRanOut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			died := failedRoot(t, phase.Implementing, intake.ClaimFailed{Role: claim.RoleImplementer, Reason: budgetRanOut, Cause: tc.cause})
			if died.Detail != tc.wantDetail {
				t.Errorf("the held phase's worker-died names the detail %q, want %q", died.Detail, tc.wantDetail)
			}
			recorded := ""
			if died.Failure != nil {
				recorded = died.Failure.Detail
			}
			if said := strings.Count(died.Detail+"\n"+recorded, budgetRanOut); said != 1 {
				t.Errorf("the held phase's worker-died names the sentence %d times across its detail %q and its failure detail %q, want once",
					said, died.Detail, recorded)
			}
		})
	}
}

// A held phase whose fact names a cause and a pod end still names the sentence once: the failure
// record's detail carries it, so the notice names only the pod end beside it, which the record
// has no field for. The runtime's account of the death is the record's own `death`, so the notice
// does not repeat that either. The negative control is diedOf naming the sentence whatever the
// notice carried beside it, which named the sentence twice.
func TestAHeldPhasesWorkerDiedNamesTheSentenceOnceWithAPodEnd(t *testing.T) {
	death := evictedDeath(claim.RoleImplementer)
	died := failedRoot(t, phase.Implementing, intake.ClaimFailed{Role: claim.RoleImplementer, Reason: budgetRanOut, PodEnd: evictedPodEnd,
		Death: death, Cause: supervise.CauseDied})
	failure := died.Failure
	if failure == nil {
		t.Fatalf("the held phase's worker-died carries no failure record, want one: its fact names a cause")
	}
	if died.PodEnd != evictedPodEnd {
		t.Errorf("the held phase's worker-died names the pod end %q, want %q, which the failure record has no field for", died.PodEnd, evictedPodEnd)
	}
	if said := strings.Count(died.Detail+"\n"+failure.Detail, budgetRanOut); said != 1 {
		t.Errorf("the held phase's worker-died names the sentence %d times across its detail %q and its failure detail %q, want once",
			said, died.Detail, failure.Detail)
	}
	// The record carries the death whole, on one line (noticeFailure).
	if said := strings.Count(died.Death+"\n"+failure.Death, strings.ReplaceAll(death, "\n", " / ")); said != 1 || died.Death != "" {
		t.Errorf("the held phase's worker-died names the death %d times across its death %q and its failure death %q, want once, in the record",
			said, died.Death, failure.Death)
	}
}

// A failure record's sentence, its account of the death and its account of the start are each
// told on one line, whole, in every notice that carries the record: the held notice, the
// controller's copy of it, and the worker-died beside it. The account of a
// death ends in a tail of the role's log over many lines and a sentence of refused turns holds
// the provider's message, and as they came each was a value over several lines, which the card
// shows only as an escaped new line (c-card-compact). Each line break is written as " / ", the
// rule of the worker-died's own detail and death. The negative control is failureOnOneLine
// returning the failure as it came.
func TestAFailureRecordOfANoticeIsOnOneLine(t *testing.T) {
	sentence := "the provider refused 3 turns in a row: request rejected\nforged: the role is fine"
	death := evictedDeath(claim.RoleImplementer)
	start := "launch legion-LEGION-208-implementer: its init container cloned no workspace:\nfatal: could not read Username"
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "implementer-failed",
		intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleImplementer, Reason: sentence, Cause: supervise.CauseDied, Death: death,
			StartFailure: start, Budgets: supervise.Budgets{Deaths: 3}, Limits: supervise.Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2}},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the implementer's failed claim: %v", err)
	}
	want := record.NoticeFailure{Detail: strings.ReplaceAll(sentence, "\n", " / "), Death: strings.ReplaceAll(death, "\n", " / "),
		StartFailure: strings.ReplaceAll(start, "\n", " / ")}
	told := map[string]int{}
	for _, payload := range noticeRows(t, pool) {
		var notice record.Notice
		reader := "architect"
		switch p := payload.(type) {
		case record.Notice:
			notice = p
		case record.ControllerNotice:
			notice, reader = record.Notice(p), "controller"
		default:
			continue
		}
		if notice.Failure == nil {
			continue
		}
		told[string(notice.Kind)+" to the "+reader]++
		got := record.NoticeFailure{Detail: notice.Failure.Detail, Death: notice.Failure.Death, StartFailure: notice.Failure.StartFailure}
		if got != want {
			t.Errorf("the %s to the %s carries the failure texts %+v, want each whole on one line: %+v", notice.Kind, reader, got, want)
		}
	}
	for _, each := range []string{"held to the architect", "held to the controller", "worker-died to the architect"} {
		if told[each] != 1 {
			t.Errorf("the outbox holds %d %s notices with a failure record, want one: %v", told[each], each, told)
		}
	}
}

// A pod end is the cluster's text, and nothing holds a kubelet's message to one line, so the
// worker-died writes each line break of it as " / ", as it writes its detail and its death. The
// negative control is diedOf naming the pod end as the fact gave it.
func TestAWorkerDiedsPodEndIsOnOneLine(t *testing.T) {
	const podEnd = "Evicted: The node was low on resource: ephemeral-storage.\nContainer roles was using 48Gi, which exceeds its request of 0."
	died := failedRoot(t, phase.Planning, intake.ClaimFailed{Role: claim.RoleArchitect, Reason: budgetRanOut, PodEnd: podEnd})
	if want := strings.ReplaceAll(podEnd, "\n", " / "); died.PodEnd != want {
		t.Errorf("the worker-died names the pod end %q, want it on one line: %q", died.PodEnd, want)
	}
}

// A death whose pod gives no reason of its own leaves the claim no pod end, and the runtime's own
// account of it is what the daemon last observed. The tree architect's notice carries no failure
// record, so where its claim failed because its process died it names that account's first line
// as its death beside the sentence, since nothing else in the notice carries it. The negative
// control is diedOf reading only the sentence and the pod end, which left the notice the sentence
// alone.
func TestAWorkerDiedWithNoFailureRecordNamesTheLastObservedDeath(t *testing.T) {
	died := failedRoot(t, phase.Planning, intake.ClaimFailed{Role: claim.RoleArchitect, Reason: budgetRanOut,
		Death: oomKilledDeath(claim.RoleArchitect), Cause: supervise.CauseDied})
	if died.Failure != nil {
		t.Fatalf("the worker-died carries the failure record %+v, want none: its own death is where the death goes", died.Failure)
	}
	const firstLine = "gone: pod legion-legion-208 (uid 8a1f) container roles terminated (OOMKilled, exit code 137)"
	if died.Detail != budgetRanOut || died.Death != firstLine || died.DeathCut || died.PodEnd != "" {
		t.Errorf("the worker-died names detail %q, death %q (cut %t), pod end %q; want %q, %q, not cut, and no pod end",
			died.Detail, died.Death, died.DeathCut, died.PodEnd, budgetRanOut, firstLine)
	}
}

// A claim keeps the runtime's account of its last death however long ago that death was: no
// launch clears it (supervise's died), and since INFRA-2009 its first line holds the pod's own
// reason and the kubelet's message. So a claim that was evicted, came back, and then failed for
// something else still holds that eviction, and a notice that named it would put an end that did
// not fail the claim beside the sentence of the one that did — and an eviction reads as the
// cluster's fault, which the architect escalates. Every cause but `died`, and a fact that names
// none, leaves the notice the sentence alone. The negative control is diedOf naming the death
// whatever the fact's cause: each row then names the eviction as its death.
func TestAWorkerDiedNamesNoDeathItsClaimDidNotFailOf(t *testing.T) {
	const sentence = "a provider error that cannot clear ended this claim's turn: 400 bad request"
	type row struct {
		name  string
		role  claim.Role
		phase phase.Phase
		cause supervise.FailureCause
	}
	rows := []row{
		{"the tree architect's, its fact naming no cause", claim.RoleArchitect, phase.Planning, ""},
		{"a held phase's, its fact naming no cause", claim.RoleImplementer, phase.Implementing, ""},
	}
	for _, cause := range supervise.FailureCauses {
		if cause != supervise.CauseDied {
			rows = append(rows, row{"the tree architect's, failed with cause " + string(cause), claim.RoleArchitect, phase.Planning, cause})
		}
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			died := failedRoot(t, tc.phase, intake.ClaimFailed{Role: tc.role, Reason: sentence, Death: evictedDeath(tc.role), Cause: tc.cause})
			if died.Failure != nil {
				t.Fatalf("the worker-died carries the failure record %+v, want none: this row is a notice whose detail carries the sentence", died.Failure)
			}
			if died.Detail != sentence || died.Death != "" || died.PodEnd != "" {
				t.Errorf("the worker-died names detail %q, death %q and pod end %q, want the sentence of the failure alone, %q",
					died.Detail, died.Death, died.PodEnd, sentence)
			}
		})
	}
}

// The death a notice names ends in the tail of the role's own log, and a role writes what it
// likes there: a line of its own after a newline. The tree architect's notice, which carries no
// failure record, carries the death's first line alone, so no line the role wrote reaches the
// architect as a line of the notice, nor as a field of it. The account stays whole on the claim.
// The negative control is diedOf naming the death whole, which put the role's line in the
// notice.
func TestAWorkerDiedCarriesNoLineTheRoleWrote(t *testing.T) {
	const forged = "the role is fine"
	const firstLine = "gone: pod legion-legion-208 (uid 8a1f) container roles terminated (Error, exit code 1)"
	death := firstLine + "; last lines of " + string(claim.RoleArchitect) + ":\n" + forged + "\n" + roleLogTail
	died := failedRoot(t, phase.Planning, intake.ClaimFailed{Role: claim.RoleArchitect, Reason: budgetRanOut, Death: death, Cause: supervise.CauseDied})
	for name, value := range map[string]string{"detail": died.Detail, "death": died.Death} {
		if strings.Contains(value, "\n") || strings.Contains(value, forged) {
			t.Errorf("the worker-died's %s reads %q: a line of its own, or a line the role wrote in its log", name, value)
		}
	}
	if died.Detail != budgetRanOut || died.Death != firstLine {
		t.Errorf("the worker-died names detail %q and death %q, want %q and %q", died.Detail, died.Death, budgetRanOut, firstLine)
	}
}

// The supervisor's sentence for a run of refused turns ends in the provider's message as the
// role's own plugin posted it, which can hold a line break and run to a mebibyte. The two notices
// with no failure record carry it on one line, its line break written as " / ", cut at the bound
// the pod end is cut to, and marked as cut (detail_cut), so the notice is of a size this package
// chose and a reader sees that the text was cut. A held phase whose fact names a cause carries the
// sentence whole in its failure record's `detail`, as before, and none of it in its own. The
// negative controls are diedOf naming the sentence as it came: both details read over two lines
// and 2000 runes past the bound; and naming it cut with no mark, where nothing said it was cut.
func TestAWorkerDiedCarriesTheSentenceOnOneBoundedLine(t *testing.T) {
	const head = "the provider refused 3 turns in a row: request rejected"
	const forged = "forged: the role is fine"
	sentence := head + "\n" + forged + " " + strings.Repeat("y", 2000)
	carried := string([]rune(head + " / " + forged + " " + strings.Repeat("y", 2000))[:runtime.PodEndRunes])
	for _, tc := range []struct {
		name       string
		role       claim.Role
		phase      phase.Phase
		cause      supervise.FailureCause
		wantDetail string
	}{
		{"the tree architect's", claim.RoleArchitect, phase.Planning, "", carried},
		{"a held phase's whose fact names no cause", claim.RoleImplementer, phase.Implementing, "", carried},
		{"a held phase's whose fact names a cause", claim.RoleImplementer, phase.Implementing, supervise.CauseProviderError, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			died := failedRoot(t, tc.phase, intake.ClaimFailed{Role: tc.role, Reason: sentence, Cause: tc.cause})
			if strings.Contains(died.Detail, "\n") {
				t.Errorf("the worker-died's detail reads over %d lines, want one: %.200q", strings.Count(died.Detail, "\n")+1, died.Detail)
			}
			if died.Detail != tc.wantDetail || died.DetailCut != (tc.wantDetail != "") {
				t.Errorf("the worker-died's detail is %.200q (cut %t), want %.200q, marked as cut where it is", died.Detail, died.DetailCut, tc.wantDetail)
			}
			if runes := utf8.RuneCountInString(died.Detail); runes > runtime.PodEndRunes {
				t.Errorf("the worker-died carries %d runes of the sentence, want at most the %d the pod end is cut to", runes, runtime.PodEndRunes)
			}
			if tc.cause != "" && (died.Failure == nil || died.Failure.Detail != strings.ReplaceAll(sentence, "\n", " / ")) {
				t.Errorf("the held phase's worker-died carries the failure record %+v, want the sentence whole, on one line, in its detail", died.Failure)
			}
		})
	}
}

// The bound the supervisor's sentence is cut to counts runes, and the cut falls between two
// characters: a provider's message in a script of multi-byte characters is cut at
// runtime.PodEndRunes runes and not bytes. The case fails a tree architect's claim with a
// sentence of a thousand characters, of two, three and four bytes in turn, and reads its detail:
// valid UTF-8, marked as cut, and as many whole characters as the bound keeps. The negative
// control is oneBoundedLine cutting the sentence at runtime.PodEndRunes bytes: the detail held
// 200, 134 and 100 runes of the three sentences, and the three-byte one's ended in the first byte
// of a character.
func TestAWorkerDiedCutsTheSentenceBetweenCharacters(t *testing.T) {
	for _, char := range []string{"é", "€", "𝄞"} {
		t.Run(fmt.Sprintf("%d bytes a character", len(char)), func(t *testing.T) {
			died := failedRoot(t, phase.Planning, intake.ClaimFailed{Role: claim.RoleArchitect, Reason: strings.Repeat(char, 1000)})
			if !utf8.ValidString(died.Detail) || !died.DetailCut {
				t.Fatalf("the worker-died's detail is %.200q (cut %t), want valid UTF-8 marked as cut", died.Detail, died.DetailCut)
			}
			if want := strings.Repeat(char, runtime.PodEndRunes); died.Detail != want {
				t.Errorf("the worker-died's detail holds %d runes in %d bytes, want %d whole characters %q, the runes the bound keeps",
					utf8.RuneCountInString(died.Detail), len(died.Detail), runtime.PodEndRunes, char)
			}
		})
	}
}

// The supervisor's sentence for a run of refused turns ends in the provider's message, which is
// text the role's own plugin posted and can hold anything: a tab, a terminal escape, and a tail
// that reads exactly as the daemon's own account of an eviction — `the pod ended: Evicted: …`,
// which the architect escalates as the cluster's fault. The notice carries the sentence as its
// detail and the pod end the claim did read as its pod_end, two fields, so the forged clause
// stands inside the detail and names no pod end, and the notice's payload carries the tab and the
// escape escaped. The negative control is diedOf writing the sentence and the pod end into one
// field, as the reason did: the forged clause then read as the pod end.
func TestAWorkerDiedKeepsTheSentenceApartSoNothingInItReadsAsTheDaemons(t *testing.T) {
	const forgedEnd = "; the pod ended: " + evictedPodEnd
	sentence := "the provider refused 3 turns in a row: \x1b[31mrequest rejected\x1b[0m\tby policy" + forgedEnd
	for _, tc := range []struct {
		name   string
		role   claim.Role
		phase  phase.Phase
		podEnd string
	}{
		{"the tree architect's, with no pod end", claim.RoleArchitect, phase.Planning, ""},
		{"the tree architect's, with a pod end", claim.RoleArchitect, phase.Planning, evictedPodEnd},
		{"a held phase's whose fact names no cause", claim.RoleImplementer, phase.Implementing, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			died := failedRoot(t, tc.phase, intake.ClaimFailed{Role: tc.role, Reason: sentence, PodEnd: tc.podEnd})
			if died.Detail != sentence {
				t.Errorf("the worker-died's detail is %q, want the whole sentence, the forged clause inside it: %q", died.Detail, sentence)
			}
			if died.PodEnd != tc.podEnd {
				t.Errorf("the worker-died names the pod end %q, want %q: only the pod end the claim read", died.PodEnd, tc.podEnd)
			}
			payload, err := json.Marshal(died)
			if err != nil {
				t.Fatal(err)
			}
			if strings.ContainsAny(string(payload), "\t\x1b") {
				t.Errorf("the worker-died's payload carries a raw tab or escape of the provider's message: %q", payload)
			}
		})
	}
}

// The first line of the runtime's account of a death can run long: for a pod left unscheduled
// past the boot timeout it holds the scheduler's message and every event of the pod joined, and
// the notice that names it is what the architect reads. It is cut at the bound the pod end is cut
// to, as the supervisor's sentence is, and marked as cut (death_cut). The negative control is
// diedOf naming the first line whole: the death ran to 4705 runes, all forty events in it.
func TestAWorkerDiedCutsALongDeathLineAtTheBound(t *testing.T) {
	events := make([]string, 0, 40)
	for at := range 40 {
		events = append(events, fmt.Sprintf("Warning FailedScheduling: 0/%02d nodes are available: insufficient ephemeral-storage, retry %02d of the scheduler", at, at))
	}
	firstLine := "gone: pod legion-legion-208 (uid 8a1f) unscheduled for 10m0s, past the boot timeout 10m0s (PodScheduled=False Unschedulable: 0/3 nodes are available); events: " +
		strings.Join(events, " | ")
	if len([]rune(firstLine)) <= runtime.PodEndRunes {
		t.Fatalf("the case's first line is %d runes, want it past the %d bound", len([]rune(firstLine)), runtime.PodEndRunes)
	}
	died := failedRoot(t, phase.Planning, intake.ClaimFailed{Role: claim.RoleArchitect, Reason: budgetRanOut, Death: firstLine, Cause: supervise.CauseDied})
	if want := string([]rune(firstLine)[:runtime.PodEndRunes]); died.Death != want || !died.DeathCut || died.Detail != budgetRanOut {
		t.Errorf("the worker-died names death of %d runes (cut %t) %.300q… and detail %q, want the first line's first %d runes marked as cut, and %q",
			utf8.RuneCountInString(died.Death), died.DeathCut, died.Death, died.Detail, runtime.PodEndRunes, budgetRanOut)
	}
}

// A pod the cluster evicted leaves the claim both a pod end and the runtime's account of the
// death, and that account is the runtime's whole detail: the pod's reason and the kubelet's
// message, which are the pod end, then the container's end and a tail of the role's log over
// many lines. The two notices with no failure record name the sentence and the pod end alone,
// so the kubelet's message is read once, with nothing of the log. The negative control is diedOf
// naming the death whatever the pod end held, which put the message in the notice twice and the
// log beside it.
func TestAWorkerDiedWithNoFailureRecordNamesAnEvictionOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		role  claim.Role
		phase phase.Phase
	}{
		{"the tree architect's", claim.RoleArchitect, phase.Planning},
		{"a held phase's whose fact names no cause", claim.RoleImplementer, phase.Implementing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			died := failedRoot(t, tc.phase, intake.ClaimFailed{Role: tc.role, Reason: budgetRanOut, PodEnd: evictedPodEnd, Death: evictedDeath(tc.role)})
			if died.Failure != nil {
				t.Fatalf("the worker-died carries the failure record %+v, want none: its own fields are where the pod end goes", died.Failure)
			}
			said := died.Detail + "\n" + died.PodEnd + "\n" + died.Death
			if count := strings.Count(said, evictionMessage); count != 1 {
				t.Errorf("the worker-died names the kubelet's message %d times, want once: %q", count, said)
			}
			if died.Detail != budgetRanOut || died.PodEnd != evictedPodEnd || died.Death != "" {
				t.Errorf("the worker-died names detail %q, pod end %q and death %q, want %q, %q and no death",
					died.Detail, died.PodEnd, died.Death, budgetRanOut, evictedPodEnd)
			}
		})
	}
}

// The path that already names what the failure meant keeps what it names: a role that does not
// work the issue's phase is told the phase the issue works, and nothing of the supervisor's
// sentence or the pod's end is added beside it. What that fact says is what the architect must
// act on, and it is more than either of those facts is.
func TestARoleThatWorksNoPhaseKeepsWhatItsPathNames(t *testing.T) {
	died := failedRoot(t, phase.Implementing, intake.ClaimFailed{Role: claim.RoleTester, Reason: budgetRanOut, PodEnd: evictedPodEnd})
	if died.Working != phase.Implementing || died.Detail != "" || died.PodEnd != "" || died.Death != "" {
		t.Fatalf("the notice of a role that works no phase is %+v, want the phase the issue works and nothing of the sentence or the pod end", died)
	}
}

// The worker-died a production check's look sends when the checking role's claim fails is a
// worker-died like the others, and what it carries of the supervisor's sentence is bounded and
// kept to one line as theirs is (the round-23 decision on R5, dispatch://INFRA-2009): the
// sentence, which for a run of refused turns holds the provider message the role's own plugin
// posts, of up to a mebibyte over any number of lines. The case fails the tester of a closed
// child whose look is open, with a sentence of two lines and two thousand bytes. A fact that names
// a cause carries the sentence whole in its failure record, as every failure record does, and no
// detail of the notice's own; a fact that names none carries it as the detail, one line and cut.
// The negative control is the path at dd846980, which carried the sentence whole.
func TestAProductionCheckLooksWorkerDiedDetailIsOneBoundedLine(t *testing.T) {
	const forged = "forged: the role is fine"
	sentence := "the provider refused 3 turns in a row: request rejected\n" + forged + " " + strings.Repeat("y", 2000)
	for _, tc := range []struct {
		name  string
		cause supervise.FailureCause
	}{
		{"a fact that names a cause", supervise.CauseProviderError},
		{"a fact that names no cause", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := childCheckRig(t)
			rig.apply("tester-failed", intake.ClaimFailed{Issue: "LEGION-209", Role: claim.RoleTester, Reason: sentence, Cause: tc.cause,
				Budgets: supervise.Budgets{LaunchFailures: 3}, Limits: supervise.Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2}})

			notices := noticesOfKind(t, rig.pool, "LEGION-209", "worker-died")
			if len(notices) != 1 {
				t.Fatalf("the outbox holds %d worker-died notices of the ended look, want one", len(notices))
			}
			died := notices[0]
			if died.Look != 1 || died.Role != claim.RoleTester {
				t.Errorf("the look's worker-died names look %d and role %q, want look 1 and the tester", died.Look, died.Role)
			}
			if strings.Contains(died.Detail, "\n") || utf8.RuneCountInString(died.Detail) > runtime.PodEndRunes {
				t.Errorf("the look's worker-died detail is %d runes over %d lines, want one line of at most %d: %.200q",
					utf8.RuneCountInString(died.Detail), strings.Count(died.Detail, "\n")+1, runtime.PodEndRunes, died.Detail)
			}
			switch {
			case tc.cause != "" && (died.Failure == nil || died.Failure.Detail != strings.ReplaceAll(sentence, "\n", " / ") || died.Detail != ""):
				t.Errorf("the look's worker-died carries failure %+v and detail %.80q, want the sentence whole, on one line, in the record and no detail", died.Failure, died.Detail)
			case tc.cause == "" && (died.Failure != nil || !died.DetailCut || !strings.HasPrefix(died.Detail, "the provider refused 3 turns in a row: request rejected / "+forged)):
				t.Errorf("the look's worker-died carries failure %+v and detail %.120q (cut %t), want the sentence on one line, cut", died.Failure, died.Detail, died.DetailCut)
			}
		})
	}
}

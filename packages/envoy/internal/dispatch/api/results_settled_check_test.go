// results_settled_check_test.go holds the results routes to one state of a check: the block as
// the block index holds it (results_settled_check.go). A browser's edit that has not settled
// changes nothing for the record route or the read, a check the index does not hold yet is no
// check of either, and a CI job's stored result counts only for the job the check names.
package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// otherCIJob is a CI job of the same workflow that no check of the rig names: its results token
// is minted in check run 98, at the commit the rig's job tested.
const otherCIJob = "a-different-job"

// newUnsettledCIJobRig is newCIJobRig with its first settlement done and a document service
// whose browser edits settle only when the test settles them, so an edit stays unsettled while
// the test reads and writes.
func newUnsettledCIJobRig(t *testing.T) (*ciJobRig, *docs.Service) {
	t.Helper()
	rig := newCIJobRig(t)
	if err := rig.deps.Docs.(*docs.Service).SettleIssueDocuments(context.Background(), rig.issue); err != nil {
		t.Fatal(err)
	}
	documents := docs.New(docs.Deps{Store: rig.database, Identity: headerIdentity(rig.database), ServerURL: testServerURL, Settle: time.Hour})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	rig.deps.Docs = documents
	mux := http.NewServeMux()
	served := register(mux, t.Context(), rig.deps)
	t.Cleanup(func() { waitBackgroundOf(t, served) })
	rig.handler = mux
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	rig.github.setCheckRun(98, otherCIJob, ciJobCommit)
	return rig, documents
}

// otherJobToken is a results token of otherCIJob's run at ciJobCommit on legion/ACME-1.
func otherJobToken(rig *ciJobRig) string {
	return rig.jobToken(func(answer *brokertoken.Introspection) { answer.CI.CheckRunID = "98" })
}

// withCICheck is the rig's Verification document with c1's ci_check naming job.
func withCICheck(job string) string {
	return strings.Replace(ciJobVerification, `ci_check="`+ciJobName+`"`, `ci_check="`+job+`"`, 1)
}

// TestAJobChangeABrowserHasNotSettledLendsNoJobTheOriginalDigest is the deep review's probe of
// 9fe80175cd, as it asserts. c1 names go-checks and is settled. A browser changes c1's ci_check to
// another job and the change does not settle; that job's results token records a pass of c1; the
// browser restores the document. The check must not read a pass from the other job, before the
// restored document settles or after. The record route reads the check as the block index holds
// it, so the other job's pass is refused 409 CHECK_NOT_THIS_JOBS while the change is unsettled, as
// it is before the change (the control).
func TestAJobChangeABrowserHasNotSettledLendsNoJobTheOriginalDigest(t *testing.T) {
	rig, documents := newUnsettledCIJobRig(t)
	original := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
	token := otherJobToken(rig)
	control := rig.post(t, rig.issue, token, jobResultOf("c1", "pass"))
	if code, _ := refusalOf(t, control); control.Code != http.StatusConflict || code != "CHECK_NOT_THIS_JOBS" {
		t.Fatalf("wrong-job control = %d %s", control.Code, control.Body.String())
	}
	writeBrowserDocument(t, documents, rig.verification, withCICheck(otherCIJob))
	live := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
	t.Logf("original: job=%q digest=%s; browser edit before settlement: job=%q digest=%s", original.CICheck, original.SHA256, live.CICheck, live.SHA256)
	wrote := rig.post(t, rig.issue, token, jobResultOf("c1", "pass"))
	t.Logf("different job's record = %d %s", wrote.Code, wrote.Body.String())
	if code, _ := refusalOf(t, wrote); wrote.Code != http.StatusConflict || code != "CHECK_NOT_THIS_JOBS" {
		t.Errorf("the other job's pass while the change of ci_check is unsettled = %d %s, want 409 CHECK_NOT_THIS_JOBS", wrote.Code, wrote.Body.String())
	}
	writeBrowserDocument(t, documents, rig.verification, ciJobVerification)
	restored := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
	t.Logf("restored: job=%q digest=%s pass_stands=%+v results=%+v", restored.CICheck, restored.SHA256, restored.PassStands, restored.Results)
	if restored.PassStands.Answer == model.PassStandsYes {
		t.Errorf("the original check %q reads pass from another job; original digest %s, restored digest %s", restored.CICheck, original.SHA256, restored.SHA256)
	}
	if err := documents.SettleIssueDocuments(context.Background(), rig.issue); err != nil {
		t.Fatal(err)
	}
	settled := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
	t.Logf("after settlement: job=%q digest=%s pass_stands=%+v", settled.CICheck, settled.SHA256, settled.PassStands)
	if settled.PassStands.Answer == model.PassStandsYes {
		t.Error("the wrong-job pass remains after settlement")
	}
}

// TestASettledChangeOfCICheckCountsTheNewJobsPassAndNothingAfterTheRestore is the probe's control:
// the change of c1's ci_check to another job settles before that job's pass, so the pass is
// recorded against the state that names that job, and it stands, from ci, at the head of the
// branch. Once the restore settles, c1 names go-checks again, under a digest of its own: the other
// job's pass is at words that moved and counts for nothing, and no pass of c1 stands.
func TestASettledChangeOfCICheckCountsTheNewJobsPassAndNothingAfterTheRestore(t *testing.T) {
	rig, documents := newUnsettledCIJobRig(t)
	writeBrowserDocument(t, documents, rig.verification, withCICheck(otherCIJob))
	if err := documents.SettleIssueDocuments(context.Background(), rig.issue); err != nil {
		t.Fatal(err)
	}
	pass := jobRecorded(t, "the other job's pass once c1 names it", rig.post(t, rig.issue, otherJobToken(rig), jobResultOf("c1", "pass")))
	changed := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
	if changed.CICheck != otherCIJob {
		t.Fatalf("c1 reads ci_check %q once the change settled, want %q", changed.CICheck, otherCIJob)
	}
	passStandsFrom(t, changed, "the other job's pass of the check that names it", model.PassStandsYes, "ci")

	writeBrowserDocument(t, documents, rig.verification, ciJobVerification)
	if err := documents.SettleIssueDocuments(context.Background(), rig.issue); err != nil {
		t.Fatal(err)
	}
	read := readResults(t, rig.handler, rig.issue)
	restored := checkOf(t, read, "c1")
	if restored.CICheck != ciJobName || restored.SHA256 == changed.SHA256 {
		t.Fatalf("the restore reads ci_check %q at digest %s, want %q at a digest other than %s", restored.CICheck, restored.SHA256, ciJobName, changed.SHA256)
	}
	standsAt(t, read, "c1", pass.ID, "the other job's pass once c1 names go-checks again", model.CommitAtBasis, true)
	if restored.PassStands.Answer == model.PassStandsYes {
		t.Fatalf("c1 reads %+v once it names go-checks again, want no pass from the other job's result", restored.PassStands)
	}
}

// TestWhileAnEditIsNotSettledTheResultsReadServesTheSettledCheck: a browser rewords k1 and changes
// c1's ci_check, and neither settles. The results read serves both checks as they settled: c1
// still names go-checks and k1 keeps its words and digest. Once the edit settles, the read serves
// the new words and job.
func TestWhileAnEditIsNotSettledTheResultsReadServesTheSettledCheck(t *testing.T) {
	rig, documents := newUnsettledCIJobRig(t)
	before := readResults(t, rig.handler, rig.issue)
	settledC1, settledK1 := checkOf(t, before, "c1"), checkOf(t, before, "k1")
	edited := ciCheckBlock("c1", `negative_control="`+ciJobNegativeControl+`" ci_check="`+otherCIJob+`"`) + "\n" +
		":::check{#k1 negative_control=\"" + ciJobNegativeControl + "\"}\nThe server starts at the head.\n:::\n"
	writeBrowserDocument(t, documents, rig.verification, edited)

	unsettled := readResults(t, rig.handler, rig.issue)
	c1, k1 := checkOf(t, unsettled, "c1"), checkOf(t, unsettled, "k1")
	if c1.CICheck != ciJobName || c1.SHA256 != settledC1.SHA256 || k1.Text != settledK1.Text || k1.SHA256 != settledK1.SHA256 {
		t.Fatalf("while the edit is unsettled the read serves c1 %q at %s and k1 %q at %s, want the settled %q at %s and %q at %s",
			c1.CICheck, c1.SHA256, k1.Text, k1.SHA256, ciJobName, settledC1.SHA256, settledK1.Text, settledK1.SHA256)
	}

	if err := documents.SettleIssueDocuments(context.Background(), rig.issue); err != nil {
		t.Fatal(err)
	}
	after := readResults(t, rig.handler, rig.issue)
	if c1, k1 := checkOf(t, after, "c1"), checkOf(t, after, "k1"); c1.CICheck != otherCIJob || !strings.Contains(k1.Text, "starts") || k1.SHA256 == settledK1.SHA256 {
		t.Fatalf("once the edit settles the read serves c1 %q and k1 %q at %s, want %q and the new words", c1.CICheck, k1.Text, k1.SHA256, otherCIJob)
	}
}

// TestTheResultsReadListsTheChecksInTheOrderOfTheNewestStoredVersion: a browser moves k1 above c1.
// While the edit has not settled, the read lists c1 then k1, the order of the newest stored
// version; once it settles, k1 then c1, the order of the version the settlement stores, which the
// order of the block ids would not give.
func TestTheResultsReadListsTheChecksInTheOrderOfTheNewestStoredVersion(t *testing.T) {
	rig, documents := newUnsettledCIJobRig(t)
	c1 := ciCheckBlock("c1", `negative_control="`+ciJobNegativeControl+`" ci_check="`+ciJobName+`"`)
	k1 := ciCheckBlock("k1", `negative_control="`+ciJobNegativeControl+`"`)
	if ciJobVerification != c1+"\n"+k1 {
		t.Fatalf("fixture: the Verification document is %q, want c1 then k1", ciJobVerification)
	}
	order := func() []string {
		var ids []string
		for _, check := range readResults(t, rig.handler, rig.issue).Checks {
			ids = append(ids, check.BlockID)
		}
		return ids
	}
	writeBrowserDocument(t, documents, rig.verification, k1+"\n"+c1)
	if got := order(); !slices.Equal(got, []string{"c1", "k1"}) {
		t.Fatalf("while the edit is unsettled the read lists %v, want the stored order [c1 k1]", got)
	}
	if err := documents.SettleIssueDocuments(context.Background(), rig.issue); err != nil {
		t.Fatal(err)
	}
	if got := order(); !slices.Equal(got, []string{"k1", "c1"}) {
		t.Fatalf("once the edit settles the read lists %v, want the new version's order [k1 c1]", got)
	}
}

// TestACheckTheIndexDoesNotHoldYetIsNoCheckOfEitherRoute: a browser adds check c9 and the edit does
// not settle. A tester's result of c9 is refused 400 UNKNOWN_CHECK, as the record route refused a
// check the index did not hold before it read a check's state from the index, and the results read
// lists no c9. The control: once the edit settles, the read lists c9 and the tester's result of it
// is recorded.
func TestACheckTheIndexDoesNotHoldYetIsNoCheckOfEitherRoute(t *testing.T) {
	rig, documents := newUnsettledCIJobRig(t)
	added := ciJobVerification + "\n" + ciCheckBlock("c9", `negative_control="`+ciJobNegativeControl+`"`)
	writeBrowserDocument(t, documents, rig.verification, added)
	tester := resultBody("c9", "pass", ciJobCommit, map[string]any{"role": "tester"})

	refused := rig.post(t, rig.issue, rig.sessionToken(), tester)
	if code, _ := refusalOf(t, refused); refused.Code != http.StatusBadRequest || code != "UNKNOWN_CHECK" {
		t.Fatalf("a tester's result of the unsettled c9 = %d %s, want 400 UNKNOWN_CHECK", refused.Code, refused.Body.String())
	}
	for _, check := range readResults(t, rig.handler, rig.issue).Checks {
		if check.BlockID == "c9" {
			t.Fatalf("the read lists the unsettled c9: %+v", check)
		}
	}

	if err := documents.SettleIssueDocuments(context.Background(), rig.issue); err != nil {
		t.Fatal(err)
	}
	checkOf(t, readResults(t, rig.handler, rig.issue), "c9")
	jobRecorded(t, "the control: the tester's result of c9 once it settled", rig.post(t, rig.issue, rig.sessionToken(), tester))
}

// TestACICheckABrowserGivesACheckHoldingNoResultTakesEffectWhenTheEditSettles is the window of a
// ci_check a person writes in the browser, on k1, an ordinary check that holds no result before
// the edit. While the edit that gives k1 ci_check has not settled, both routes take k1 as the
// block index holds it, an ordinary check: the read serves it with no ci_check, and a tester's pass
// of it is recorded. The settlement then finds k1 holding a role's result and holds it to the
// fourth rule: it takes k1's ci_check off with one block.invalid naming that rule, and the tester's
// pass stands. The control: the same edit, settled before the tester's pass, makes k1 CI's, so
// that pass is refused 409 CHECK_PROVED_BY_CI, and k1 keeps its ci_check with no block.invalid.
func TestACICheckABrowserGivesACheckHoldingNoResultTakesEffectWhenTheEditSettles(t *testing.T) {
	ordinary := ciCheckBlock("k1", `negative_control="`+ciJobNegativeControl+`"`)
	edited := strings.Replace(ciJobVerification, ordinary,
		ciCheckBlock("k1", `negative_control="`+ciJobNegativeControl+`" ci_check="`+ciJobName+`"`), 1)
	if edited == ciJobVerification {
		t.Fatalf("fixture: no ordinary k1 block %q in %q", ordinary, ciJobVerification)
	}
	tester := resultBody("k1", "pass", ciJobCommit, map[string]any{"role": "tester"})
	// edit is a rig whose pull request #70 is open at the commit the tester records, whose k1
	// holds no result, and whose browser has given k1 ci_check in an edit that has not settled.
	edit := func(t *testing.T) (*ciJobRig, *docs.Service) {
		t.Helper()
		rig, documents := newUnsettledCIJobRig(t)
		writeHead(t, rig.database, ciJobRepository, 70, ciJobCommit, "open", rig.issue)
		if k1 := checkOf(t, readResults(t, rig.handler, rig.issue), "k1"); k1.CICheck != "" || len(k1.Results) != 0 {
			t.Fatalf("k1 reads %+v before the edit, want an ordinary check holding no result", k1)
		}
		writeBrowserDocument(t, documents, rig.verification, edited)
		return rig, documents
	}

	t.Run("the tester records k1 while the edit is unsettled", func(t *testing.T) {
		rig, documents := edit(t)
		if k1 := checkOf(t, readResults(t, rig.handler, rig.issue), "k1"); k1.CICheck != "" {
			t.Fatalf("while the edit is unsettled the read serves k1 with ci_check %q, want the ordinary k1 it settled as", k1.CICheck)
		}
		jobRecorded(t, "a tester's pass of k1 while the edit giving it ci_check is unsettled", rig.post(t, rig.issue, rig.sessionToken(), tester))
		if err := documents.SettleIssueDocuments(context.Background(), rig.issue); err != nil {
			t.Fatal(err)
		}
		k1 := checkOf(t, readResults(t, rig.handler, rig.issue), "k1")
		if k1.CICheck != "" {
			t.Fatalf("k1 once settled reads ci_check %q, want it taken off, since k1 holds a role's result", k1.CICheck)
		}
		passStandsFrom(t, k1, "the tester's pass of k1 once the edit settled", model.PassStandsYes, "tester")
		want := []ciCheckRefusalEvent{{BlockID: "k1", Reason: ciCheckRecordedRule, BlockType: "check"}}
		if got := ciCheckRefusalEvents(t, rig.handler, rig.issue); !slices.Equal(got, want) {
			t.Fatalf("block.invalid events = %+v, want %+v", got, want)
		}
	})

	t.Run("the control: the edit settles before the tester records k1", func(t *testing.T) {
		rig, documents := edit(t)
		if err := documents.SettleIssueDocuments(context.Background(), rig.issue); err != nil {
			t.Fatal(err)
		}
		refused := rig.post(t, rig.issue, rig.sessionToken(), tester)
		if code, _ := refusalOf(t, refused); refused.Code != http.StatusConflict || code != "CHECK_PROVED_BY_CI" {
			t.Fatalf("a tester's pass of k1 once the edit settled = %d %s, want 409 CHECK_PROVED_BY_CI", refused.Code, refused.Body.String())
		}
		if k1 := checkOf(t, readResults(t, rig.handler, rig.issue), "k1"); k1.CICheck != ciJobName {
			t.Fatalf("k1 once settled reads ci_check %q, want %q", k1.CICheck, ciJobName)
		}
		if got := ciCheckRefusalEvents(t, rig.handler, rig.issue); len(got) != 0 {
			t.Fatalf("block.invalid events = %+v, want none", got)
		}
	})
}

// TestACICheckAVersionGivesBetweenAWritesResolveAndItsLocksRefusesARolesResult: k1 is an
// ordinary check holding no result when a tester's pass of it resolves, so the judgement the write
// makes before its transaction opens lets the pass through. A version of the document then gives
// k1 ci_check before the write takes its locks, held by the block index as a settlement's version
// is (writeVerification). The write judges the pass again by the check its locks find
// (recordResultUnderOwnerLocks), which CI proves now, and refuses it 409 CHECK_PROVED_BY_CI with
// nothing stored. The control: the same version written before the tester's pass resolves is
// refused the same way, by the judgement made before the transaction opens.
func TestACICheckAVersionGivesBetweenAWritesResolveAndItsLocksRefusesARolesResult(t *testing.T) {
	ordinary := ciCheckBlock("k1", `negative_control="`+ciJobNegativeControl+`"`)
	edited := strings.Replace(ciJobVerification, ordinary,
		ciCheckBlock("k1", `negative_control="`+ciJobNegativeControl+`" ci_check="`+ciJobName+`"`), 1)
	if edited == ciJobVerification {
		t.Fatalf("fixture: no ordinary k1 block %q in %q", ordinary, ciJobVerification)
	}
	tester := resultBody("k1", "pass", heldWriteCommit, map[string]any{"role": "tester"})
	ordinaryK1 := func(t *testing.T, rig *ciJobRig) {
		t.Helper()
		if k1 := checkOf(t, readResults(t, rig.handler, rig.issue), "k1"); k1.CICheck != "" || len(k1.Results) != 0 {
			t.Fatalf("k1 reads %+v before the version, want an ordinary check holding no result", k1)
		}
	}

	t.Run("the version lands between the write's resolve and its locks", func(t *testing.T) {
		rig := newCIJobRig(t)
		ordinaryK1(t, rig)
		held := holdWriteBeforeItsLocks(t, rig, rig.sessionToken(), tester)
		writeVerification(t, rig, edited)
		if k1 := checkOf(t, readResults(t, rig.handler, rig.issue), "k1"); k1.CICheck != ciJobName {
			t.Fatalf("fixture: k1 reads ci_check %q once the version landed, want %q", k1.CICheck, ciJobName)
		}
		held.release()
		rig.refusedWith(t, "a tester's pass of k1 that resolved before the version landed", <-held.answered,
			http.StatusConflict, "CHECK_PROVED_BY_CI")
	})

	t.Run("the control: the version lands before the write resolves", func(t *testing.T) {
		rig := newCIJobRig(t)
		ordinaryK1(t, rig)
		writeVerification(t, rig, edited)
		rig.refusedWith(t, "a tester's pass of k1 once the version landed", rig.post(t, rig.issue, rig.sessionToken(), tester),
			http.StatusConflict, "CHECK_PROVED_BY_CI")
	})
}

// TestAJobsResultCountsOnlyForTheJobItsCheckNames is the second line of the binding of a CI job's
// result to its check: a row of ci_check_results that another job's run recorded, stored against
// the digest c1 carries now, as no route of this build writes one, counts for nothing on c1, which
// names go-checks, and says why. The control: the same row named for go-checks's run stands, from
// ci, at the head of the branch.
func TestAJobsResultCountsOnlyForTheJobItsCheckNames(t *testing.T) {
	for _, row := range []struct {
		name, run, notCounted, answer string
	}{
		{"another job's row", otherCIJob, model.ResultNotCountedAnotherJob, model.PassStandsNo},
		{"the control: the named job's row", ciJobName, "", model.PassStandsYes},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newCIJobRig(t)
			rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
			check := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
			if _, err := rig.database.Pool.Exec(context.Background(), `
				insert into ci_check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version, check_sha256, run, commit_sha, branch, verdict, attestation, session, actor)
				values ($1, $1, $2::uuid, 'c1', $3, $4, $5, $6, $7, 'pass', '{}', $8, '{"kind":"session","id":"ci"}')
			`, rig.issue, check.ArtifactID, check.Version, check.SHA256, row.run, ciJobCommit, ciJobBranch, ciJobSession); err != nil {
				t.Fatalf("store %s: %v", row.name, err)
			}
			check = checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
			if len(check.Results) != 1 || check.Results[0].NotCounted != row.notCounted || check.PassStands.Answer != row.answer {
				t.Fatalf("%s reads %+v with pass_stands %+v, want not_counted %q and %s", row.name, check.Results, check.PassStands, row.notCounted, row.answer)
			}
		})
	}
}

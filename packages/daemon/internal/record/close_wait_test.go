package record

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// The commits the close's facts name: the issue's newest merge (cwMergeSHA, the merge commit of
// pull request 7, whose final reviewed head is cwMergeHead), an earlier merge of the same issue,
// the merge commit of the pull request that landed an integration branch on main, and the head of
// another pull request of the issue that is still open.
const (
	cwRepo        = "acme/widgets"
	cwLinkedRepo  = "acme/infra"
	cwMergeSHA    = "1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a"
	cwEarlierSHA  = "2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b"
	cwLandingSHA  = "3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c"
	cwMergeHead   = "4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d"
	cwOtherHead   = "5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"
	cwArtifact    = "art-verification"
	cwGeneration  = uint64(2)
	cwReleasePost = "c-release-post-is-up"
)

// cwEntered is the issue's newest entry into verifying: a read counts only after it.
var cwEntered = time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)

// cwMerge is the issue's newest merge: its pull request 7 merged into main of its own repository.
func cwMerge() Merge {
	return Merge{Number: 7, Head: cwMergeHead, SHA: cwMergeSHA, Base: "main", Repo: cwRepo, MergedAt: cwEntered}
}

// cwBranch is a branch basis of the issue's own repository, as Dispatch names one: with the merge
// commit it holds of the issue's work there, "" for none.
func cwBranch(branch, mergeCommit string) dispatch.CheckBasis {
	return dispatch.CheckBasis{Kind: dispatch.BasisBranch, Branch: branch, MergeCommit: mergeCommit}
}

// cwLinkedBranch is a branch basis of a repository the issue links a pull request in.
func cwLinkedBranch(repo, branch, mergeCommit string) dispatch.CheckBasis {
	basis := cwBranch(branch, mergeCommit)
	basis.Repo = repo
	return basis
}

// cwStands is Dispatch's answer for a check: a fail and a pass and fail name their deciding
// results newest first, and every answer that names results names their commit.
func cwStands(answer, reason string, results ...string) dispatch.PassStands {
	stands := dispatch.PassStands{Answer: answer, Reason: reason, Results: results}
	if len(results) > 0 {
		stands.Commit = cwMergeSHA
	}
	return stands
}

var (
	cwPass        = cwStands(dispatch.PassStandsYes, "", "r-pass")
	cwNoResult    = cwStands(dispatch.PassStandsNo, dispatch.PassStandsReasonNoResult)
	cwFail        = cwStands(dispatch.PassStandsNo, dispatch.PassStandsReasonFail, "r-fail", "r-pass")
	cwPassAndFail = cwStands(dispatch.PassStandsUnknown, dispatch.PassStandsReasonPassAndFail, "r-pass", "r-fail")
)

// cwCheck is one check of the issue's Verification document as the results read answers it.
func cwCheck(id, when string, stands dispatch.PassStands, bases ...dispatch.CheckBasis) dispatch.CheckBlock {
	return dispatch.CheckBlock{BlockID: id, ArtifactID: cwArtifact, When: when, ComparedWith: bases, PassStands: stands}
}

// cwAfterMerge is an after_merge check compared with main at the newest merge.
func cwAfterMerge(id string, stands dispatch.PassStands) dispatch.CheckBlock {
	return cwCheck(id, dispatch.CheckAfterMerge, stands, cwBranch("main", cwMergeSHA))
}

// cwBeforeMerge is a before_merge check with one subject for each head it names, each with the
// answer the check has there.
func cwBeforeMerge(id string, subjects ...dispatch.PassStandsSubject) dispatch.CheckBlock {
	check := cwCheck(id, dispatch.CheckBeforeMerge, dispatch.PassStands{Answer: dispatch.PassStandsYes, Subjects: subjects})
	for _, subject := range subjects {
		check.ComparedWith = append(check.ComparedWith, subject.Basis)
		if subject.Answer != dispatch.PassStandsYes {
			check.PassStands.Answer = subject.Answer
		}
	}
	return check
}

// cwSubject is a before_merge check's state at the head of pull request number.
func cwSubject(head string, number int, merged bool, answer, reason string, results ...string) dispatch.PassStandsSubject {
	return dispatch.PassStandsSubject{
		Basis:  dispatch.CheckBasis{Kind: dispatch.BasisPullRequest, Commit: head, PullRequest: &number, Merged: merged},
		Answer: answer, Reason: reason, Results: results,
	}
}

// cwResults is the results read of the issue's Verification document, holding checks.
func cwResults(checks ...dispatch.CheckBlock) dispatch.IssueResults {
	return dispatch.IssueResults{Issue: "ACME-1", VerificationArtifactID: cwArtifact, Checks: checks}
}

// cwProduction is the issue's production check of generation, opened for the merge mergeSHA, open
// after a pending report where outcome is "", and closed with outcome otherwise.
func cwProduction(generation uint64, mergeSHA string, outcome CheckOutcome) *ProductionCheck {
	opened := cwEntered.Add(2 * time.Minute)
	check := &ProductionCheck{Issue: "ACME-1", Project: "ACME", Tree: "ACME-1", Repo: cwRepo, Number: 7, MergeSHA: mergeSHA,
		OpenedAt: opened, BoundAt: opened.Add(336 * time.Hour), Role: claim.RoleTester, Looks: 1, Generation: generation}
	if outcome == "" {
		wake := opened.Add(24 * time.Hour)
		check.WakeAt = &wake
		return check
	}
	closed := opened.Add(time.Hour)
	check.ClosedAt, check.Outcome = &closed, outcome
	return check
}

// cwBlock is one check block of a version of the Verification document, as Dispatch reads its
// blocks from that version's text: its attributes are the four every check here writes, beside
// marks, the attributes subject and program the version gives it.
func cwBlock(id string, marks map[string]string) dispatch.DocumentBlock {
	attributes := map[string]string{"serves": "dispatch://ACME-1/spec#b-r-close", "surface": "api", "run": "go test ./...", "when": "after_merge"}
	for name, value := range marks {
		attributes[name] = value
	}
	return dispatch.DocumentBlock{ID: id, Type: "check", Attributes: attributes, Text: "The widget closes when its checks pass.\n"}
}

// cwApproval is the Verification document approved at version 3 holding approved, whose latest
// version holds current.
func cwApproval(approved, current []dispatch.DocumentBlock) *CheckApproval {
	return &CheckApproval{Version: 3, Approved: approved, Current: current}
}

// TestCloseWaitOfEachCase holds the one predicate of an issue's close to each case the plan
// names: a row is the facts the workflow reads in one transaction, and the wait they give. A read
// counts only after the issue's newest entry into verifying, and each check only where the read
// holds the newest merge for its class. Every row's answer is in Dispatch's shape, as the
// client's types decode it, and stored as the close read keeps it. The close is done exactly where
// the wait lists nothing.
func TestCloseWaitOfEachCase(t *testing.T) {
	read := func(answer CheckReadAnswer) *CheckRead {
		t.Helper()
		encoded, err := json.Marshal(answer)
		if err != nil {
			t.Fatalf("encode the read's answer: %v", err)
		}
		at := cwEntered.Add(time.Minute)
		return &CheckRead{Issue: "ACME-1", Generation: cwGeneration, ReadAt: at, Answer: encoded, RecordedAt: at.Add(time.Second)}
	}
	// passing is facts on which the issue closes: a read after its entry into verifying of checks
	// that each pass at its newest merge, and its production check of its generation recorded
	// for that merge.
	passing := func(answer CheckReadAnswer) CloseFacts {
		return CloseFacts{Repo: cwRepo, Merge: cwMerge(), Entered: cwEntered, Read: read(answer), Generation: cwGeneration,
			Production: cwProduction(cwGeneration, cwMergeSHA, CheckRecorded), ProductionOn: true}
	}
	allPass := CheckReadAnswer{Results: cwResults(
		cwAfterMerge("c-after", cwPass),
		cwBeforeMerge("c-before", cwSubject(cwMergeHead, 7, true, dispatch.PassStandsYes, "", "r-head")),
		cwCheck("c-trunk", dispatch.CheckDefaultBranch, cwPass, cwBranch("main", cwMergeSHA)),
	)}
	oneCheck := func(check dispatch.CheckBlock) CheckReadAnswer {
		return CheckReadAnswer{Results: cwResults(check)}
	}
	with := func(facts CloseFacts, change func(*CloseFacts)) CloseFacts {
		change(&facts)
		return facts
	}
	withProduction := func(check *ProductionCheck) func(*CloseFacts) {
		return func(f *CloseFacts) { f.Production = check }
	}
	at := func(answer CheckReadAnswer, readAt time.Time) func(*CloseFacts) {
		return func(f *CloseFacts) {
			f.Read = read(answer)
			f.Read.ReadAt = readAt
		}
	}
	// approved reads the current document's check c-after with the marks current, the approved
	// version holding it with the marks approved.
	approved := func(approvedMarks, currentMarks map[string]string) CloseFacts {
		check := cwAfterMerge("c-after", cwPass)
		check.Subject, check.Program = currentMarks["subject"], currentMarks["program"]
		return passing(CheckReadAnswer{Results: cwResults(check),
			Approval: cwApproval([]dispatch.DocumentBlock{cwBlock("c-after", approvedMarks)}, []dispatch.DocumentBlock{cwBlock("c-after", currentMarks)})})
	}
	// summarized is answer beside an architect's handoff summary "done" in the issue's log, which
	// the stored read carries as one more key no reader of the close takes.
	summarized := func(answer CheckReadAnswer) CloseFacts {
		facts := passing(answer)
		var raw map[string]any
		if err := json.Unmarshal(facts.Read.Answer, &raw); err != nil {
			t.Fatalf("decode the read's answer: %v", err)
		}
		raw["log"] = []map[string]string{{"kind": "handoff", "role": "architect", "summary": "done"}}
		encoded, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("encode the read's answer: %v", err)
		}
		facts.Read.Answer = encoded
		return facts
	}
	withdrawn, merged := cwEntered.Add(10*time.Minute), cwEntered.Add(20*time.Minute)
	linkedMerge := cwMerge()
	linkedMerge.Repo = cwLinkedRepo
	stackedMerge := cwMerge()
	stackedMerge.Base = "release"
	// live is an after_merge check of production that names no program, where Dispatch holds the
	// current deploy of more than one program in the issue's repository.
	live := cwAfterMerge("c-live", cwStands(dispatch.PassStandsUnknown, dispatch.PassStandsReasonProgramNotNamed))
	live.Subject = dispatch.CheckSubjectProduction

	for _, tc := range []struct {
		name  string
		facts CloseFacts
		want  CloseWait
	}{
		{name: "row 1: every check passes and production is recorded for the newest merge", facts: passing(allPass)},
		{name: "row 2: an after_merge check with no result waits",
			facts: passing(oneCheck(cwAfterMerge("c-after", cwNoResult))),
			want: CloseWait{Checks: []CheckWait{{Check: "c-after", State: CheckStateNoResult, Answer: dispatch.PassStandsNo,
				Reason: dispatch.PassStandsReasonNoResult}}}},
		{name: "row 3: a failed check waits as a fail with its deciding result",
			facts: passing(oneCheck(cwAfterMerge("c-after", cwFail))),
			want: CloseWait{Checks: []CheckWait{{Check: "c-after", State: CheckStateFail, Answer: dispatch.PassStandsNo,
				Reason: dispatch.PassStandsReasonFail, Result: "r-fail", Commit: cwMergeSHA}}}},
		{name: "row 4: a pass and a fail at one commit waits as unknown",
			facts: passing(oneCheck(cwAfterMerge("c-after", cwPassAndFail))),
			want: CloseWait{Checks: []CheckWait{{Check: "c-after", State: CheckStateUnknown, Answer: dispatch.PassStandsUnknown,
				Reason: dispatch.PassStandsReasonPassAndFail}}}},
		{name: "row 5: no read", facts: with(passing(allPass), func(f *CloseFacts) { f.Read = nil }), want: CloseWait{Unread: true}},
		{name: "row 5: a read that failed",
			facts: with(passing(allPass), func(f *CloseFacts) { f.Read.Answer, f.Read.Failure = nil, "502 BAD_GATEWAY" }),
			want:  CloseWait{Unread: true}},
		{name: "row 6: a read made before the newest entry into verifying",
			facts: with(passing(allPass), at(allPass, cwEntered.Add(-time.Minute))), want: CloseWait{Unread: true}},
		{name: "row 7: a read whose check names another merge commit",
			facts: passing(oneCheck(cwCheck("c-after", dispatch.CheckAfterMerge, cwPass, cwBranch("main", cwEarlierSHA)))),
			want:  CloseWait{Unread: true}},
		{name: "row 8: the production check open after a pending report",
			facts: with(passing(allPass), withProduction(cwProduction(cwGeneration, cwMergeSHA, ""))),
			want:  CloseWait{Production: ProductionOpen}},
		{name: "row 8: the production check closed not_run",
			facts: with(passing(allPass), withProduction(cwProduction(cwGeneration, cwMergeSHA, CheckNotRun))),
			want:  CloseWait{Production: ProductionNotRun}},
		{name: "row 8: the production check closed defect",
			facts: with(passing(allPass), withProduction(cwProduction(cwGeneration, cwMergeSHA, CheckDefect))),
			want:  CloseWait{Production: ProductionDefect}},
		{name: "row 8: the production check recorded for an earlier merge commit",
			facts: with(passing(allPass), withProduction(cwProduction(cwGeneration, cwEarlierSHA, CheckRecorded))),
			want:  CloseWait{Production: ProductionEarlierMerge}},
		{name: "row 9: no production check run and no Verification document",
			facts: with(passing(CheckReadAnswer{Results: dispatch.IssueResults{Issue: "ACME-1"}}), func(f *CloseFacts) {
				f.Production, f.ProductionOn = nil, false
			})},
		{name: "row 10: a Verification document with no check and production recorded",
			facts: passing(CheckReadAnswer{Results: cwResults()})},
		{name: "row 11: a Verification document with no check and production open",
			facts: with(passing(CheckReadAnswer{Results: cwResults()}), withProduction(cwProduction(cwGeneration, cwMergeSHA, ""))),
			want:  CloseWait{Production: ProductionOpen}},
		{name: "row 12: a check removed since the approved version",
			facts: passing(CheckReadAnswer{Results: cwResults(cwAfterMerge("c-after", cwPass)),
				Approval: cwApproval([]dispatch.DocumentBlock{cwBlock("c-after", nil), cwBlock("c-removed", nil)}, []dispatch.DocumentBlock{cwBlock("c-after", nil)})}),
			want: CloseWait{Unapproved: true}},
		{name: "row 13: an issue beneath in verifying",
			facts: with(passing(allPass), func(f *CloseFacts) { f.Beneath = []string{"ACME-2"} }),
			want:  CloseWait{Beneath: []string{"ACME-2"}}},
		{name: "row 14: a filed child in backlog that the record does not hold",
			facts: with(passing(CheckReadAnswer{Results: allPass.Results, Children: []dispatch.IssueChild{{Key: "ACME-3", Status: "backlog"}}}),
				func(f *CloseFacts) { f.OpenChildren = []string{"ACME-3"} }),
			want: CloseWait{OpenChildren: []string{"ACME-3"}}},
		{name: "row 15: one open defect",
			facts: with(passing(allPass), func(f *CloseFacts) {
				f.Defects = []Defect{{ID: 41, Issue: "ACME-1", Generation: cwGeneration, Kind: DefectKindDefect, Text: "the widget drops its last row",
					Role: claim.RoleImplementer, Head: cwMergeHead, ReportedAt: cwEntered}}
			}),
			want: CloseWait{Defects: []string{"41"}}},
		{name: "row 15: a withdrawn defect alone",
			facts: with(passing(allPass), func(f *CloseFacts) {
				f.Defects = []Defect{{ID: 42, Issue: "ACME-1", Generation: cwGeneration, Kind: DefectKindDefect, Text: "same as 41",
					ReportedAt: cwEntered, WithdrawnAt: &withdrawn, WithdrawReason: "same as 41"}}
			})},
		{name: "row 15: a defect a merged change carried, alone",
			facts: with(passing(allPass), func(f *CloseFacts) {
				f.Defects = []Defect{{ID: 43, Issue: "ACME-1", Generation: 1, Kind: DefectKindDefect, Text: "the widget drops its last row",
					ReportedAt: cwEntered, CarriedBy: cwGeneration, MergedAt: &merged}}
			})},
		{name: "row 16: an open next change alone",
			facts: with(passing(allPass), func(f *CloseFacts) { f.OpenChange, f.Production, f.ProductionOn = true, nil, false }),
			want:  CloseWait{OpenChange: true}},
		{name: "row 17: TestAnAfterMergeCheckOfAReleasePostHoldsTheClose",
			facts: passing(oneCheck(cwAfterMerge(cwReleasePost, cwNoResult))),
			want: CloseWait{Checks: []CheckWait{{Check: cwReleasePost, State: CheckStateNoResult, Answer: dispatch.PassStandsNo,
				Reason: dispatch.PassStandsReasonNoResult}}}},
		{name: "row 17: the release post's check removed without approval",
			facts: passing(CheckReadAnswer{Results: cwResults(),
				Approval: cwApproval([]dispatch.DocumentBlock{cwBlock(cwReleasePost, nil)}, nil)}),
			want: CloseWait{Unapproved: true}},
		{name: "row 18: row 1's facts beside an architect's summary done", facts: summarized(allPass)},
		{name: "row 18: row 2's facts beside an architect's summary done",
			facts: summarized(oneCheck(cwAfterMerge("c-after", cwNoResult))),
			want: CloseWait{Checks: []CheckWait{{Check: "c-after", State: CheckStateNoResult, Answer: dispatch.PassStandsNo,
				Reason: dispatch.PassStandsReasonNoResult}}}},
		{name: "row 19: a check reason of an open next change whose check now passes",
			facts: with(passing(oneCheck(cwAfterMerge("c-after", cwPass))), func(f *CloseFacts) {
				f.Generation, f.OpenChange, f.Production, f.ProductionOn = 3, true, nil, false
				f.Defects = []Defect{{ID: 44, Issue: "ACME-1", Generation: cwGeneration, Kind: DefectKindCheck, Text: "c-after fails",
					Check: "c-after", Result: "r-fail", ReportedAt: cwEntered, CarriedBy: 3}}
			}),
			want: CloseWait{OpenChange: true}},
		{name: "row 20: an after_merge check whose only merge is linked, read with the linked entry",
			facts: with(passing(oneCheck(cwCheck("c-after", dispatch.CheckAfterMerge, cwPass, cwLinkedBranch(cwLinkedRepo, "main", cwMergeSHA)))),
				func(f *CloseFacts) { f.Merge = linkedMerge })},
		{name: "row 20: the same read with an entry of the issue's own repository and no merge commit",
			facts: with(passing(oneCheck(cwCheck("c-after", dispatch.CheckAfterMerge, cwPass,
				cwBranch("main", ""), cwLinkedBranch(cwLinkedRepo, "main", cwMergeSHA)))),
				func(f *CloseFacts) { f.Merge = linkedMerge })},
		{name: "row 20: the linked entry with no merge commit, which Dispatch could not order",
			facts: with(passing(oneCheck(cwCheck("c-after", dispatch.CheckAfterMerge, cwPass, cwLinkedBranch(cwLinkedRepo, "main", "")))),
				func(f *CloseFacts) { f.Merge = linkedMerge }),
			want: CloseWait{Unread: true}},
		{name: "row 21: a default_branch check of an issue merged into another branch, the default-branch entry naming a merge commit",
			facts: with(passing(oneCheck(cwCheck("c-trunk", dispatch.CheckDefaultBranch, cwPass, cwBranch("main", cwLandingSHA)))),
				func(f *CloseFacts) { f.Merge = stackedMerge })},
		{name: "row 21: the default-branch entry naming none",
			facts: with(passing(oneCheck(cwCheck("c-trunk", dispatch.CheckDefaultBranch, cwNoResult, cwBranch("main", "")))),
				func(f *CloseFacts) { f.Merge = stackedMerge }),
			want: CloseWait{Checks: []CheckWait{{Check: "c-trunk", State: CheckStateNoResult, Answer: dispatch.PassStandsNo,
				Reason: dispatch.PassStandsReasonNoResult}}}},
		{name: "row 22: a before_merge check whose subject is the newest merge's final head",
			facts: passing(oneCheck(cwBeforeMerge("c-before",
				cwSubject(cwMergeHead, 7, true, dispatch.PassStandsYes, "", "r-head"))))},
		{name: "row 22: a read whose only subject is another head, with no read at the merge's head",
			facts: passing(oneCheck(cwBeforeMerge("c-before",
				cwSubject(cwOtherHead, 9, false, dispatch.PassStandsNo, dispatch.PassStandsReasonNoResult)))),
			want: CloseWait{Unread: true}},
		{name: "row 22: that read beside the read at the merge's head",
			facts: passing(CheckReadAnswer{
				Results: cwResults(cwBeforeMerge("c-before", cwSubject(cwOtherHead, 9, false, dispatch.PassStandsNo, dispatch.PassStandsReasonNoResult))),
				AtMergeHead: &dispatch.IssueResults{Issue: "ACME-1", VerificationArtifactID: cwArtifact, Checks: []dispatch.CheckBlock{
					cwCheck("c-before", dispatch.CheckBeforeMerge, dispatch.PassStands{Answer: dispatch.PassStandsNo, Reason: dispatch.PassStandsReasonFail,
						Results: []string{"r-fail-at-head"}, Commit: cwMergeHead}, dispatch.CheckBasis{Kind: dispatch.BasisNamed, Commit: cwMergeHead}),
				}},
			}),
			want: CloseWait{Checks: []CheckWait{{Check: "c-before", State: CheckStateFail, Answer: dispatch.PassStandsNo,
				Reason: dispatch.PassStandsReasonFail, Result: "r-fail-at-head", Commit: cwMergeHead}}}},
		{name: "row 23: a Verification document with no approved version",
			facts: passing(CheckReadAnswer{Results: cwResults(cwAfterMerge("c-after", cwPass)),
				Approval: &CheckApproval{Current: []dispatch.DocumentBlock{cwBlock("c-after", nil)}}})},
		{name: "row 24: a live check that reads unknown for program_not_named waits with no result yet",
			facts: passing(oneCheck(live)),
			want: CloseWait{Checks: []CheckWait{{Check: "c-live", State: CheckStateNoResult, Answer: dispatch.PassStandsUnknown,
				Reason: dispatch.PassStandsReasonProgramNotNamed}}}},
		{name: "row 25: a check block that only gained subject production",
			facts: approved(nil, map[string]string{"subject": dispatch.CheckSubjectProduction})},
		{name: "row 26: a check block that gained subject production and program dispatch",
			facts: approved(nil, map[string]string{"subject": dispatch.CheckSubjectProduction, "program": "dispatch"})},
		{name: "row 27: a check block whose program changed",
			facts: approved(map[string]string{"subject": dispatch.CheckSubjectProduction, "program": "dispatch"},
				map[string]string{"subject": dispatch.CheckSubjectProduction, "program": "legion"}),
			want: CloseWait{Unapproved: true}},
		{name: "row 28: a check block whose approved version held subject production and that gained program",
			facts: approved(map[string]string{"subject": dispatch.CheckSubjectProduction},
				map[string]string{"subject": dispatch.CheckSubjectProduction, "program": "dispatch"}),
			want: CloseWait{Unapproved: true}},
		{name: "row 29: a check block whose subject changed from production to code",
			facts: approved(map[string]string{"subject": dispatch.CheckSubjectProduction}, map[string]string{"subject": dispatch.CheckSubjectCode}),
			want:  CloseWait{Unapproved: true}},
		{name: "row 30: a check block whose subject was removed",
			facts: approved(map[string]string{"subject": dispatch.CheckSubjectProduction}, nil),
			want:  CloseWait{Unapproved: true}},
		{name: "row 31: production recorded at the newest merge, of an earlier generation",
			facts: with(passing(allPass), withProduction(cwProduction(cwGeneration-1, cwMergeSHA, CheckRecorded))),
			want:  CloseWait{Production: ProductionOpening}},
		{name: "row 32: an open next change and a check of an earlier generation closed defect",
			facts: with(passing(allPass), func(f *CloseFacts) {
				f.Generation, f.OpenChange, f.Production = 3, true, cwProduction(cwGeneration, cwMergeSHA, CheckDefect)
			}),
			want: CloseWait{Production: ProductionAwaitingChange, OpenChange: true}},
		{name: "row 33: a check of generation 0 recorded at the newest merge, on an issue of generation 2",
			facts: with(passing(allPass), withProduction(cwProduction(0, cwMergeSHA, CheckRecorded))),
			want:  CloseWait{Production: ProductionOpening}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CloseWaitOf(tc.facts)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("CloseWaitOf =\n%+v\nwant\n%+v", got, tc.want)
			}
			if done := reflect.DeepEqual(tc.want, CloseWait{}); got.Done() != done {
				t.Fatalf("Done() = %t for %+v, want %t", got.Done(), got, done)
			}
		})
	}
}

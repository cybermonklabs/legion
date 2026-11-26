package intake

import "testing"

// Each fact names its issue, or names none: a fact that names one issue answers its key, and a
// fact that names none or several answers false. Every fact type must answer it to be a Fact at
// all, so the compiler, not this test, holds that each type is decided; this test holds what each
// answers.
func TestIssueOfAnswersEachFactThatNamesOneIssue(t *testing.T) {
	t.Parallel()
	const issue = "LEGION-1"
	namesOne := map[string]Fact{
		"DispatchIssue":           DispatchIssue{Key: issue},
		"DispatchArtifact":        DispatchArtifact{Key: issue},
		"GateAboveDispatch":       GateAboveDispatch{Read: DispatchArtifact{Key: issue}},
		"DispatchAskOpened":       DispatchAskOpened{Key: issue},
		"DispatchAskRestored":     DispatchAskRestored{Key: issue},
		"DispatchAskClosed":       DispatchAskClosed{Key: issue},
		"DispatchAskReply":        DispatchAskReply{Key: issue},
		"DispatchApprovalMoved":   DispatchApprovalMoved{Key: issue},
		"DispatchClaim":           DispatchClaim{Key: issue},
		"Handover":                Handover{Issue: issue},
		"MergeQueueReadDone":      MergeQueueReadDone{Issue: issue},
		"ConflictConfirmDone":     ConflictConfirmDone{Issue: issue},
		"PullRequestMissing":      PullRequestMissing{Issue: issue},
		"HandoffComplete":         HandoffComplete{Issue: issue},
		"BackwardMove":            BackwardMove{Issue: issue},
		"RetryOrEscalate":         RetryOrEscalate{Issue: issue},
		"RestartPhase":            RestartPhase{Issue: issue},
		"SignOff":                 SignOff{Issue: issue},
		"GateRegistered":          GateRegistered{Issue: issue},
		"CloseRoot":               CloseRoot{Issue: issue},
		"ClaimFailed":             ClaimFailed{Issue: issue},
		"SessionLost":             SessionLost{Issue: issue},
		"SessionStoreFault":       SessionStoreFault{Issue: issue},
		"TextsFellBack":           TextsFellBack{Issue: issue},
		"MemoryGuardKill":         MemoryGuardKill{Issue: issue},
		"VolumeNearFull":          VolumeNearFull{Issue: issue},
		"ClaimReady":              ClaimReady{Issue: issue},
		"TaskConfirmed":           TaskConfirmed{Issue: issue},
		"GateRequestFound":        GateRequestFound{Issue: issue},
		"CatchUpApplied":          CatchUpApplied{Issue: issue},
		"LingerExpired":           LingerExpired{Issue: issue},
		"CheckReport":             CheckReport{Issue: issue},
		"CheckWakeDue":            CheckWakeDue{Issue: issue},
		"CheckLookEnded":          CheckLookEnded{Issue: issue},
		"CheckBound":              CheckBound{Issue: issue},
		"PodBackstopDue":          PodBackstopDue{Issue: issue},
		"WorkerSilent":            WorkerSilent{Issue: issue},
		"WorkerBackgroundStalled": WorkerBackgroundStalled{Issue: issue},
	}
	namesNone := map[string]Fact{
		"ChildAdoption":               ChildAdoption{Tree: issue, Children: []AdoptedChild{{Issue: issue}}},
		"DispatchConsumerPosition":    DispatchConsumerPosition{},
		"ControllerTick":              ControllerTick{},
		"ControllerMemoryGuardKill":   ControllerMemoryGuardKill{},
		"EntrySweep":                  EntrySweep{},
		"ProjectResumed":              ProjectResumed{},
		"PullRequestOpened":           PullRequestOpened{Branch: "legion/" + issue},
		"PullRequestSynchronized":     PullRequestSynchronized{Branch: "legion/" + issue},
		"PullRequestReview":           PullRequestReview{},
		"PullRequestChecks":           PullRequestChecks{},
		"PullRequestMergeability":     PullRequestMergeability{},
		"PullRequestMerged":           PullRequestMerged{},
		"PullRequestClosed":           PullRequestClosed{},
		"PullRequestDequeued":         PullRequestDequeued{},
		"PullRequestReviewRequests":   PullRequestReviewRequests{},
		"PullRequestMergeStateUnread": PullRequestMergeStateUnread{},
		"Push":                        Push{},
		"BasePush":                    BasePush{},
		"DaemonBooted":                DaemonBooted{},
		"CutoverMergerRepair":         CutoverMergerRepair{},
		"ListenerWake":                ListenerWake{},
	}
	for name, fact := range namesOne {
		if got, ok := IssueOf(fact); got != issue || !ok {
			t.Errorf("IssueOf(%s naming %s) = %q, %v; want %q, true", name, issue, got, ok, issue)
		}
	}
	for name, fact := range namesNone {
		if got, ok := IssueOf(fact); got != "" || ok {
			t.Errorf("IssueOf(%s) = %q, %v; want \"\", false: it names no one issue", name, got, ok)
		}
	}
}

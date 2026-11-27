package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The verdict on a seed's read (judgeBasisRead) asks the ancestor's leave of R1's first condition
// where nothing else the read shows decides. An approval of the version that names nobody, which
// only a Dispatch that records no reviewer gives, decides nothing: on an ancestor that has left, as
// the read or the daemon's own store shows it, the wait ends with the leave's words, and on one
// still meeting the condition nothing changes. A request no longer open with no approval of the
// version ends the wait by whoever closed it, as the seed's read of the close names them, and on
// an ancestor that has left the end names the leave and then that close. The closer is the end's
// actor; a close that names no actor is named as Someone Dispatch does not name, and its end has no
// actor. A read of a closed request that carries no read of its close is no read the seed makes,
// and an error.
func TestTheVerdictAsksTheAncestorsLeaveWhereNothingElseDecides(t *testing.T) {
	t.Parallel()
	gate := record.PendOnBasis(record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 4}, *pendingBasis(), "ask-100")
	nobody := dispatch.Approval{IssueKey: basisAncestor, State: "approved", LatestVersion: 4, Version: new(4), At: basisApprovedAt.Add(24 * time.Hour), Seq: 60}
	done, unlabelled := intake.AncestorRead{Status: record.DoneStatus, HandedOver: true}, intake.AncestorRead{Status: "in_progress"}
	bySession := &intake.BasisClose{Actor: dispatch.Actor{Kind: "session", ID: "session-other"}, State: "resolved"}
	for _, tc := range []struct {
		name     string
		approval dispatch.Approval
		ancestor intake.AncestorRead
		// closed is the seed's read of the request's close, nil for none.
		closed *intake.BasisClose
		// accepted is what the daemon's own store shows of the ancestor's acceptance.
		accepted string
		want     basisVerdict
		// fails says the verdict is an error.
		fails bool
	}{
		{name: "an approval naming nobody, the ancestor done", approval: nobody, ancestor: done,
			want: basisVerdict{kind: basisEnds, text: "Dispatch shows LEGION-100 done, so LEGION-100 is accepted."}},
		{name: "an approval naming nobody, the ancestor without the legion label", approval: nobody, ancestor: unlabelled,
			want: basisVerdict{kind: basisEnds, text: "LEGION-100 no longer carries the legion label."}},
		{name: "an approval naming nobody, the store showing the ancestor accepted", approval: nobody, ancestor: ancestorAtWork,
			accepted: "The daemon records LEGION-100 in retro", want: basisVerdict{kind: basisEnds, text: "The daemon records LEGION-100 in retro, so LEGION-100 is accepted."}},
		{name: "an approval naming nobody, the ancestor at work", approval: nobody, ancestor: ancestorAtWork,
			want: basisVerdict{text: "an approval of the ancestor's version that names nobody opens and ends no step waiting on it"}},
		{name: "the request closed, the ancestor done", approval: requestClosed(), ancestor: done, closed: bySession,
			want: basisVerdict{kind: basisEnds, text: "Dispatch shows LEGION-100 done, so LEGION-100 is accepted. " + closedSentence("session:session-other"),
				actor: "session:session-other"}},
		{name: "the request closed, the ancestor without the legion label", approval: requestClosed(), ancestor: unlabelled, closed: bySession,
			want: basisVerdict{kind: basisEnds, text: "LEGION-100 no longer carries the legion label. " + closedSentence("session:session-other"),
				actor: "session:session-other"}},
		{name: "the request closed, the store showing the ancestor accepted", approval: requestClosed(), ancestor: ancestorAtWork, closed: bySession,
			accepted: "The daemon records LEGION-100 in retro", want: basisVerdict{kind: basisEnds,
				text: "The daemon records LEGION-100 in retro, so LEGION-100 is accepted. " + closedSentence("session:session-other"), actor: "session:session-other"}},
		{name: "the request closed, the ancestor at work", approval: requestClosed(), ancestor: ancestorAtWork, closed: bySession,
			want: basisVerdict{kind: basisEnds, text: closedSentence("session:session-other"), actor: "session:session-other"}},
		{name: "the request closed by an actor Dispatch does not name", approval: requestClosed(), ancestor: ancestorAtWork,
			closed: &intake.BasisClose{State: "resolved"}, want: basisVerdict{kind: basisEnds, text: closedSentence("Someone Dispatch does not name")}},
		{name: "the request closed, with no read of its close", approval: requestClosed(), ancestor: ancestorAtWork, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := judgeBasisRead(gate, intake.BasisRead{Approval: tc.approval, Ancestor: tc.ancestor, Closed: tc.closed},
				func() (string, error) { return tc.accepted, nil })
			if (err != nil) != tc.fails || got != tc.want {
				t.Fatalf("the verdict = %+v, %v; want %+v, failing %t", got, err, tc.want, tc.fails)
			}
		})
	}
}

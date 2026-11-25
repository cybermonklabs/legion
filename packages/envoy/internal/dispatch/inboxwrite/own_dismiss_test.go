package inboxwrite

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAnOwnAnswerOrReplyDismissesOnlyWhereAllFourConditionsHold holds PlanRecord's rule of D5
// (the own-answer rule, R1, ownDismissal) to one row each way for each of its four conditions: the
// handler marked the write its writer's own (Write.Own), the writer is a person, the tuples' writer
// carries no agent acting for them (Who.Act), and the write is an answer or a reply rather than the
// write that starts its thread. Only a reply under a message thread on no project dismisses an Agent
// thread at its root; every other write that dismisses does so on its writer's own row. A write that
// leaves OwnWrite false - the carried approval answer, the restore of a retracted ask, the seed's
// and the backfill's writes - dismisses nothing.
func TestAnOwnAnswerOrReplyDismissesOnlyWhereAllFourConditionsHold(t *testing.T) {
	at := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	alice := model.Actor{Kind: "user", ID: "alice"}
	acting := model.Actor{Kind: "user", ID: "alice", Act: &model.ActorAct{}}
	session := model.Actor{Kind: "session", ID: "s1"}
	issue := "CORE-1"
	root := thread{kind: graphstore.KindComment, id: "root-comment"}
	askRoot := thread{kind: graphstore.KindAsk, id: "the-ask"}
	directRoot := thread{kind: graphstore.KindMessage, id: "direct-root"}
	issueRoot := thread{kind: graphstore.KindMessage, id: "issue-root"}

	reply := Comment("reply-comment", alice, at, &issue, nil, "CORE").RepliesTo(graphstore.KindComment, root.id)
	answer := Answered(Ask{id: askRoot.id, issueKey: &issue, project: "CORE"}, model.AskAnswer{User: "alice", At: at}, alice)
	directReply := Message("direct-reply", alice, at, nil, "").RepliesTo(graphstore.KindMessage, directRoot.id)
	issueReply := Message("issue-reply", alice, at, &issue, "CORE").RepliesTo(graphstore.KindMessage, issueRoot.id)

	for _, row := range []struct {
		name   string
		write  Write
		person string
		root   thread
		want   dismissal
	}{
		{name: "a person's own reply as themselves dismisses", write: reply.Own(), person: "alice", root: root, want: dismissesRow},
		{name: "a reply its handler did not mark its writer's own dismisses nothing", write: reply, person: "alice", root: root},
		{name: "a person's own answer dismisses", write: answer.Own(), person: "alice", root: askRoot, want: dismissesRow},
		{name: "an answer its handler did not mark its writer's own dismisses nothing", write: answer, person: "alice", root: askRoot},
		{name: "a session's own reply dismisses nothing",
			write:  Comment("session-reply", session, at, &issue, nil, "CORE").RepliesTo(graphstore.KindComment, root.id).Own(),
			person: "", root: root},
		{name: "a reply an agent writes in the person's name dismisses nothing",
			write:  Comment("acting-reply", acting, at, &issue, nil, "CORE").RepliesTo(graphstore.KindComment, root.id).Own(),
			person: "alice", root: root},
		{name: "an answer an agent writes in the person's name dismisses nothing",
			write:  Answered(Ask{id: askRoot.id, issueKey: &issue, project: "CORE"}, model.AskAnswer{User: "alice", At: at}, acting).Own(),
			person: "alice", root: askRoot},
		{name: "the comment that starts a thread dismisses nothing",
			write: Comment(root.id, alice, at, &issue, nil, "CORE").Own(), person: "alice", root: root},
		{name: "a decline dismisses nothing", write: Declined(askRoot.id, alice, at).Own(), person: "alice", root: askRoot},
		{name: "a person's own reply under an Agent thread dismisses that thread at its root", write: directReply.Own(), person: "alice",
			root: directRoot, want: dismissesAgentRoot},
		{name: "a person's own reply to an issue message dismisses a Reply thread", write: issueReply.Own(), person: "alice",
			root: issueRoot, want: dismissesRow},
		{name: "a direct message that starts a conversation dismisses nothing",
			write: Message(directRoot.id, alice, at, nil, "").Own(), person: "alice", root: directRoot},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := ownDismissal(row.write, row.person, row.root); got != row.want {
				t.Errorf("ownDismissal = %v, want %v (nothing %v, the writer's row %v, the Agent root %v)",
					got, row.want, dismissesNothing, dismissesRow, dismissesAgentRoot)
			}
		})
	}
}

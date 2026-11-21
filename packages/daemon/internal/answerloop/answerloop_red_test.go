// Red tests for INFRA-204 (#2230), package github.com/sjawhar/legion/daemon/internal/answerloop.
// Drop this file in packages/daemon/internal/answerloop/ as answerloop_red_test.go; it uses the
// branch's own rig (rig_test.go). Both tests fail on 651fcff6.
//
//	cd packages/daemon && go test ./internal/answerloop/ \
//	  -run 'TestAFailedAppendedMarkDoesNotAppendTheExchangeTwice|TestOneRoleEnvelopeHandedOffAgainAfterANewerSessionIsAnsweredOnce' -count=1 -v

package answerloop

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// failAppendedMark is the loop's store with one failure in it: the write that marks a question
// appended fails the first time, as a connection reset, a statement timeout or the daemon's own
// stop fails it. Everything else is the real store.
type failAppendedMark struct {
	Store
	failed bool
}

func (f *failAppendedMark) UpdateQuestion(ctx context.Context, q store.AnswerQuestion, from ...store.QuestionState) error {
	if q.State == store.QuestionAppended && !f.failed {
		f.failed = true
		return errors.New("the store is unreachable")
	}
	return f.Store.UpdateQuestion(ctx, q, from...)
}

// Finding 1. generate() writes the exchange into the agent's conversation (question.go:122-124)
// and only then marks the question appended (question.go:144). Nothing records that the write
// happened, so a kill or a store error in that window leaves the question `generating`, and the
// next pass answers it again and appends a second exchange. The agent resumes with the same
// question twice, two different answers and two legion-answer entries naming one question id, and
// the day's spend carries two model calls for one question.
func TestAFailedAppendedMarkDoesNotAppendTheExchangeTwice(t *testing.T) {
	r := newRig(t)
	path := r.conversation("ses_tester", testerToken, 0)
	r.cfg.Store = &failAppendedMark{Store: r.cfg.Store}
	r.restart()
	r.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
	r.drain()
	r.drain()
	content := fileContent(t, path)
	if got := strings.Count(content, `"Why plum?"`); got != 1 {
		t.Errorf("the conversation holds the question %d times; want once", got)
	}
	if got := strings.Count(content, "Plum, from Why plum?"); got != 1 {
		t.Errorf("the conversation holds %d answers to it; want one", got)
	}
	if got := strings.Count(content, `"customType":"legion-answer"`); got != 1 {
		t.Errorf("the conversation holds %d legion-answer entries for one question; want one", got)
	}
	if r.answerCount() != 1 {
		t.Errorf("the model was called %d times; want once", r.answerCount())
	}
	if posts := r.dispatch.posts(); len(posts) != 1 {
		t.Errorf("Dispatch posts = %d; want one", len(posts))
	}
	if spent, err := r.st.AnswerSpend(r.ctx, project, testNow); err != nil || spent != 0.25 {
		t.Errorf("the day's spend = %v, %v; want one answer's 0.25", spent, err)
	}
}

// Finding 2. A question's recorded identity is (project, identity, session) — the answering
// session is resolved at receive time from the role's newest conversation (answerloop.go:380,
// :350; store/answer_questions.go:82). One envelope to a role, handed off again after a newer
// conversation of that role is recorded, is a second question answered from a second conversation:
// the sender gets the same message answered twice, under two idempotency keys the listener cannot
// collapse. Plan 2.8 says the identity is the envelope's dedupe key and is stable across retries
// and restarts (A8).
func TestOneRoleEnvelopeHandedOffAgainAfterANewerSessionIsAnsweredOnce(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_old", testerToken, 0)
	roleTopic := "notifications.role." + string(testerToken)
	h := envoyHandoff(roleTopic, "ses_old", "evt-1", "publish.k1", "Which fruit did you test?")
	r.receive(h)
	r.drain()
	// The role ran again and stopped again; its newer conversation is recorded.
	r.conversation("ses_new", testerToken, 10)
	// JetStream delivers the same envelope again after the daemon comes back.
	r.restart()
	r.receive(h)
	r.drain()
	if sends := r.listener.posts(); len(sends) != 1 {
		t.Fatalf("the sender got %d answers to one envelope; want one\n%s", len(sends), describe(sends))
	}
	if r.answerCount() != 1 {
		t.Fatalf("the model was called %d times for one envelope; want once", r.answerCount())
	}
	if all := r.questions(); len(all) != 1 {
		t.Fatalf("questions = %d for one envelope; want one", len(all))
	}
}

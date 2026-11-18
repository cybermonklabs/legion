package bus

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// A session's consumer filters on every form of each topic it follows (contracts.TopicForms):
// an issue's topic in the old form takes the new form too, the session's own agent subject takes
// its new form under any project, and each message published in either form is delivered once. A
// message on another session's agent subject, in either form, is not. So the rename can move a
// producer to the new form without one session missing a message, whichever form it registered.
func TestASessionConsumerFiltersOnBothFormsOfEachTopic(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_forms"
	legacyIssue := contracts.DispatchIssueTopicPrefix + "INFRA-12.>"
	if _, err := consumers.Ensure(ctx, session, []string{contracts.LegacyAgentSubject(session), legacyIssue}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	info, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	want := []string{
		"notifications.agent.*.ses_forms",
		"notifications.agent.ses_forms",
		"notifications.dispatch.issue.INFRA-12.>",
		"notifications.dispatch.issue.INFRA.INFRA-12.>",
	}
	if !slices.Equal(info.Config.FilterSubjects, want) {
		t.Fatalf("filters = %q, want %q", info.Config.FilterSubjects, want)
	}

	pull := pullSession(t, consumers, session)
	publishStored(t, owner, contracts.LegacyAgentSubject("ses_other"), "another session's, old form")
	publishStored(t, owner, contracts.AgentSubject("INFRA", "ses_other"), "another session's, new form")
	var delivered []uint64
	for _, subject := range []string{
		contracts.LegacyAgentSubject(session),
		contracts.AgentSubject("PRIV", session),
		contracts.AgentSubject(contracts.NoProject, session),
		"notifications.dispatch.issue.INFRA-12.comment.created",
		contracts.DispatchIssueSubject("INFRA", "INFRA-12", "comment.created"),
	} {
		delivered = append(delivered, publishStored(t, owner, subject, subject))
	}
	for _, seq := range delivered {
		if msg := next(t, pull); msg.StreamSeq != seq {
			t.Fatalf("delivered %d on %s, want %d", msg.StreamSeq, msg.Subject, seq)
		}
	}
	requireNoMessage(t, consumers, pull, session)
}

// A topic registered in the new form takes the old form too, while the old form is read: a
// session that follows an issue the way the rename names it still receives the events Dispatch
// publishes in the form it publishes them in today.
func TestANewFormTopicTakesTheOldFormToo(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_new_form"
	follow := contracts.DispatchIssueSubject("INFRA", "INFRA-12", ">")
	if _, err := consumers.Ensure(ctx, session, []string{contracts.AgentSubject("INFRA", session), follow}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	pull := pullSession(t, consumers, session)
	old := publishStored(t, owner, "notifications.dispatch.issue.INFRA-12.issue.updated", "old form")
	if msg := next(t, pull); msg.StreamSeq != old {
		t.Fatalf("delivered %d on %s, want the old form's %d", msg.StreamSeq, msg.Subject, old)
	}
	own := publishStored(t, owner, contracts.LegacyAgentSubject(session), "old form, own")
	if msg := next(t, pull); msg.StreamSeq != own {
		t.Fatalf("delivered %d on %s, want the old own subject's %d", msg.StreamSeq, msg.Subject, own)
	}
}

// A consumer created with no position starts at the first message stored for the session on its
// own agent subject (create), and a message sent to it in the new form before it registered is
// one of those: it is the session's own, and nothing else would ever deliver it.
func TestAConsumerStartsAtTheFirstMessageOfItsOwnAgentSubjectInEitherForm(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_first_new_form"
	publishStored(t, owner, "notifications.github.o.r.push", "before the session")
	first := publishStored(t, owner, contracts.AgentSubject("PRIV", session), "sent in the new form before the session registered")
	publishStored(t, owner, contracts.LegacyAgentSubject(session), "sent in the old form after it")
	start, err := consumers.Ensure(ctx, session, []string{contracts.LegacyAgentSubject(session)}, 0)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !start.Created || start.StartSeq != first {
		t.Fatalf("Ensure = %+v, want created at the new-form message %d", start, first)
	}
}

// What the rename leaves alone keeps the filter the merge base gave it: topics of a family that
// is not renamed reduce to exactly the filters they reduced to before, and the one subject a
// registration gains is its own agent subject's new form.
func TestTopicsOfAFamilyTheRenameLeavesKeepTheirFilters(t *testing.T) {
	topics := []string{
		"notifications.github.o.r.>",
		"notifications.team.deploy-squad",
		"notifications.dispatch.document.INFRA.spec.>",
		"notifications.slack.T1.C1.>",
	}
	_, filters, err := sessionConsumerTarget("ses_same", topics)
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if want := ReduceFilterSubjects(topics); !slices.Equal(filters, want) {
		t.Fatalf("filters = %q, want the merge base's %q", filters, want)
	}
	_, filters, err = sessionConsumerTarget("ses_same", append(slices.Clone(topics), contracts.LegacyAgentSubject("ses_same")))
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	want := ReduceFilterSubjects(append(slices.Clone(topics), contracts.LegacyAgentSubject("ses_same"), "notifications.agent.*.ses_same"))
	if !slices.Equal(filters, want) {
		t.Fatalf("filters with the agent subject = %q, want %q", filters, want)
	}
}

// An issue's topic under a wildcard project position has no old form beside it
// (contracts.LegacyTwin): its old form would overlap it, the wildcard against the key and the key
// against what follows, and ReduceFilterSubjects would replace the two with their common prefix,
// notifications.dispatch.issue.>, a filter over every project's issues. So such a topic is its own
// one filter, as at the merge base, and the session's consumer reaches no other issue. The control
// is the issue's topic in either form under its own project, which keeps its two disjoint forms.
func TestAWildcardProjectIssueTopicKeepsItsOneFilter(t *testing.T) {
	const wildcard = "notifications.dispatch.issue.*.INFRA-12.>"
	if filters := SessionConsumerFilters([]string{wildcard}); !slices.Equal(filters, []string{wildcard}) {
		t.Fatalf("filters of %s = %q, want the topic alone", wildcard, filters)
	}
	both := []string{"notifications.dispatch.issue.INFRA-12.>", "notifications.dispatch.issue.INFRA.INFRA-12.>"}
	for _, topic := range both {
		if filters := SessionConsumerFilters([]string{topic}); !slices.Equal(filters, ReduceFilterSubjects(both)) {
			t.Fatalf("filters of %s = %q, want both forms %q", topic, filters, ReduceFilterSubjects(both))
		}
	}
}

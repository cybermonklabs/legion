package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The recipient count passes over the session's consumer filter (mayLeaveOut) only where that
// filter can leave none of the session's topics out: a set with no topic outside notifications.
// and fewer than two wildcard topics. The claim rests on reasoning about the reduction, so this
// test holds it to the filter itself (sessionFilter) over every set of one to three topics of a
// pool built to hold each case the reasoning names: literals of every family that has a second
// form (an agent subject, its exception lane, an issue's topic), the session's own agent subject
// in the new form under a wildcard project, wildcards that pass the private rule alone, one that
// does not, and topics outside notifications.
//
// For every set, a set the shortcut passes over has nothing its filter leaves out, and carriedFor
// answers as filterCarries, the judgement with no shortcut, for every subject of a second pool
// that a topic of the set matches. The pool holds sets the filter does leave topics out of - a
// topic outside notifications., the exception-lane pair, the chain of merges, a pair that merges
// only through a new form - and the test checks first that it leaves each of those out, so a
// shortcut that passed over one of them would answer otherwise and fail here.
func TestTheRecipientCountsShortcutLeavesOutNothingTheFullFilterWould(t *testing.T) {
	const (
		session  = "ses_shortcut"
		pairFoo  = "notifications.envoy.exceptions.foo.*"
		pairBar  = "notifications.envoy.exceptions.*.bar"
		chainA   = "notifications.envoy.exceptions.notifications.github.*.pr"
		chainB   = "notifications.envoy.exceptions.notifications.github.repo.*"
		chainC   = "notifications.envoy.exceptions.*.github.foo"
		issueOld = "notifications.envoy.exceptions.notifications.dispatch.issue.KEY-1.>"
		formMate = "notifications.envoy.exceptions.*.dispatch.issue.KEY.KEY-1.foo"
	)
	pool := []string{
		contracts.LegacyAgentSubject(session),
		contracts.LegacyAgentSubject("ses_other"),
		"notifications.envoy.exceptions.notifications.agent.ses_other",
		"notifications.team.deploy",
		"notifications.envoy.exceptions.foo.bar",
		"notifications.legion.infra.controller",
		"notifications.dispatch.issue.KEY-1.comment.created",
		"notifications.agent.*." + session,
		"notifications.team.*",
		"notifications.team.>",
		pairFoo, pairBar,
		"notifications.envoy.exceptions.notifications.github.>",
		chainA, chainB, chainC,
		issueOld, formMate,
		"notifications.dispatch.issue.KEY-1.>",
		"notifications.github.*.repo.pr",
		"notifications.agent.*.ses_other",
		"*.team.foo",
		"deploy.done",
	}
	subjects := []string{
		"notifications.team.foo", "notifications.team.deploy", "notifications.team.x.y",
		"notifications.envoy.exceptions.foo.bar",
		"notifications.envoy.exceptions.notifications.github.repo.pr",
		"notifications.envoy.exceptions.x.github.foo",
		"notifications.envoy.exceptions.notifications.dispatch.issue.KEY.KEY-1.foo",
		"notifications.dispatch.issue.KEY.KEY-1.comment.created",
		contracts.LegacyAgentSubject(session), "notifications.agent.ACME." + session,
		contracts.LegacyAgentSubject("ses_other"),
		"notifications.github.x.repo.pr",
		"notifications.legion.infra.controller",
		"deploy.done",
	}
	for _, leaking := range [][]string{{"*.team.foo"}, {pairFoo, pairBar}, {chainA, chainB, chainC}, {issueOld, formMate}} {
		if _, leftOut := sessionFilter(session, leaking); len(leftOut) == 0 {
			t.Fatalf("the filter leaves nothing out of %v, so the pool holds no case a shortcut could get wrong", leaking)
		}
	}
	matched := func(topics []string, subject string) bool {
		for _, topic := range topics {
			for _, form := range contracts.TopicForms(topic) {
				if bus.SubjectContains(form, subject) {
					return true
				}
			}
		}
		return false
	}
	sets, passedOver, judged := 0, 0, 0
	check := func(topics []string) {
		sets++
		_, leftOut := sessionFilter(session, topics)
		if !mayLeaveOut(topics) {
			passedOver++
			if len(leftOut) > 0 {
				t.Errorf("the shortcut passes over %v, whose filter leaves out %v", topics, leftOut)
			}
		}
		for _, subject := range subjects {
			if !matched(topics, subject) {
				continue
			}
			judged++
			if got, want := carriedFor(session, topics, subject), filterCarries(session, topics, subject); got != want {
				t.Errorf("a publish on %s counts the session following %v %t, where its filter counts it %t", subject, topics, got, want)
			}
		}
	}
	for i := range pool {
		check([]string{pool[i]})
		for j := i + 1; j < len(pool); j++ {
			check([]string{pool[i], pool[j]})
			for k := j + 1; k < len(pool); k++ {
				check([]string{pool[i], pool[j], pool[k]})
			}
		}
	}
	if passedOver == 0 || judged == 0 || passedOver == sets {
		t.Fatalf("of %d sets the shortcut passed over %d, and %d judgements were made: the test did not reach both paths", sets, passedOver, judged)
	}
	t.Logf("%d sets, %d of them passed over by the shortcut, %d judgements compared", sets, passedOver, judged)
}

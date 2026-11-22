package main

// edge_frame_verdict_test.go is the table test of edgeFrameVerdict (edge_frames.go), the one rule
// the relay and the relay socket both judge a frame by, with a row for each of its branches and a
// passing row beside each withholding one, so no branch can be deleted without a row failing. It
// calls the rule directly, with no NATS server, so it covers the branches the live frame-rule test
// (edge_frame_rule_test.go) cannot store a frame to reach: a GitHub relay header whose envelope
// names a topic that is no GitHub event's, the wildcard own subject, and the role mark a forged
// claim or a kept or taken copy carries (F1, F2).

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

func TestEdgeFrameVerdict(t *testing.T) {
	const session = "ses_v"
	own := contracts.LegacyAgentSubject(session)
	ownNew := contracts.AgentSubject("PRIV", session)
	otherAgent := contracts.LegacyAgentSubject("ses_other")
	previous := contracts.LegacyAgentSubject("ses_prev")
	const privRole, pubRole = "legion-priv-priv-1-tester", "legion-pub-pub-1-tester"
	const ownIssue = "notifications.dispatch.issue.PRIV.PRIV-1.comment.created"
	const otherIssue = "notifications.dispatch.issue.PUB.PUB-1.comment.created"
	const github = "notifications.github.acme.pub.pr.1"

	frame := func(topic, role string) []byte {
		fields := map[string]string{}
		if topic != "" {
			fields["topic"] = topic
		}
		if role != "" {
			fields["role"] = role
		}
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	projects := []string{"PRIV"}
	for _, tc := range []struct {
		name    string
		subject string
		topic   string
		role    string
		want    frameVerdict
	}{
		{"a send on the session's own subject passes", own, own, "", framePasses()},
		{"a send on the session's own new-form subject passes", ownNew, ownNew, "", framePasses()},
		{"a forged other-project role-marked send on the own subject is withheld", own, own, pubRole, frameDrops(withheldProjectNotHeld)},
		{"the session's own project's role-marked send kept on the previous holder's subject passes", own, previous, privRole, framePasses()},
		{"the session's own project's role-lane forward passes", own, contracts.RoleTopicPrefix + privRole, "", framePasses()},
		{"an other-project role-lane forward is withheld", own, contracts.RoleTopicPrefix + pubRole, "", frameDrops(withheldProjectNotHeld)},
		{"a frame on another session's agent subject is withheld", otherAgent, otherAgent, "", frameDrops(withheldProjectNotHeld)},
		{"a frame on the own subject with no envelope topic is withheld", own, "", "", frameDrops(withheldProjectNotHeld)},
		{"a non-role forward of a GitHub event on the own subject is withheld", own, github, "", frameDrops(withheldProjectNotHeld)},
		{"a non-role forward of another agent subject on the own subject is withheld", own, otherAgent, "", frameDrops(withheldProjectNotHeld)},
		{"the wildcard own subject is no one's and is withheld", contracts.AgentTopicPrefix + "*." + session, contracts.AgentTopicPrefix + "*." + session, "", frameDrops(withheldProjectNotHeld)},
		{"a GitHub header whose envelope names a topic that is no GitHub event's is withheld", github, ownIssue, "", frameDrops(withheldProjectNotHeld)},
		{"a GitHub header with no reader stands answer_unknown", github, "", "", frameAnswerUnknownVerdict()},
		{"a topic of the token's project passes", ownIssue, ownIssue, "", framePasses()},
		{"a topic of a project the token does not list is withheld", otherIssue, otherIssue, "", frameDrops(withheldProjectNotHeld)},
		{"a relay header of the token's project whose envelope names another project is withheld", ownIssue, otherIssue, "", frameDrops(withheldProjectNotHeld)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict := edgeFrameVerdict(nil, session, projects, tc.subject, frame(tc.topic, tc.role))
			if verdict != tc.want {
				t.Fatalf("edgeFrameVerdict(subject=%q, topic=%q, role=%q) = {action %d, reason %q}, want {action %d, reason %q}",
					tc.subject, tc.topic, tc.role, verdict.action, verdict.reason, tc.want.action, tc.want.reason)
			}
		})
	}
}

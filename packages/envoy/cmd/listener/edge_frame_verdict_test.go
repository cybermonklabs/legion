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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

func TestEdgeFrameVerdict(t *testing.T) {
	t.Parallel()
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

// TestEdgeFrameVerdictReadsEachFieldByItsExactKey holds the edge's frame rule to the fields the
// session's plugin reads: JSON.parse takes a frame's topic, role mark, payload and the payload's
// repo by their exact keys, past one byte order mark (TextDecoder), while encoding/json takes the
// last key of any case and refuses a byte order mark. A frame that names one of those fields under
// more than one key, or under a key that differs from its name only in case, is withheld on every
// branch under ambiguous_key: the probe frame, which encoding/json read as the session's own
// message and the plugin as another project's event, repeated keys, and "Topic", "TOPIC" and
// "tOpIc" beside "topic" on the generic, GitHub, role and agent branches. A frame with a top-level
// "__proto__" key, in either spelling, whose fields the plugin's zod read takes as the envelope's
// own, is withheld under prototype_key. A frame on an agent subject that encoding/json does not
// read and that is no role message whose topic is a role lane or an agent subject, and a GitHub
// envelope or payload encoding/json does not read, is withheld, as the encoding/json read withheld
// it. The controls, each with one exact key, get the verdict that read gave them. A row that names
// goTopic, goRole or goRepo first checks that the encoding/json read takes the frame that way, so
// no row passes on a frame that does not split the readers as it says.
func TestEdgeFrameVerdictReadsEachFieldByItsExactKey(t *testing.T) {
	t.Parallel()
	const session = "ses_v"
	own := contracts.LegacyAgentSubject(session)
	const privRole, pubRole = "legion-priv-priv-1-tester", "legion-pub-pub-1-tester"
	privLane, pubLane := contracts.RoleTopicPrefix+privRole, contracts.RoleTopicPrefix+pubRole
	const ownIssue = "notifications.dispatch.issue.PRIV.PRIV-1.comment.created"
	const otherIssue = "notifications.dispatch.issue.PUB.PUB-1.comment.created"
	const github = "notifications.github.acme.pub.pr.1"
	projects := []string{"PRIV"}
	repositories := &edgeRepositories{now: time.Now, answers: map[string]repositoryAnswer{
		"PRIV": {repositories: []string{"acme/pub"}, readAt: time.Now()},
	}}

	// object writes a JSON object of fields in order, each a key's text as it sits between its
	// quotes and a raw JSON value, so a row can repeat a key or write one with an escape.
	type field struct{ key, value string }
	object := func(fields ...field) []byte {
		parts := make([]string, len(fields))
		for i, f := range fields {
			parts[i] = `"` + f.key + `":` + f.value
		}
		return []byte("{" + strings.Join(parts, ",") + "}")
	}
	text := func(s string) string {
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	payload := func(fields ...field) string { return text(string(object(fields...))) }
	const bom = "\xef\xbb\xbf"
	deep := strings.Repeat("[", 10_001) + strings.Repeat("]", 10_001)
	if json.Valid([]byte(deep)) {
		t.Fatal("encoding/json reads a value nested 10,001 deep; the deep rows test nothing")
	}
	pubRepo := payload(field{"repo", text("acme/pub")})
	secretRepo := payload(field{"repo", text("acme/secret")})
	// privEvent is a Dispatch payload of the token's project, so a Dispatch frame below is judged
	// by the rule the row names and not by its payload (dispatchFrameWithheld).
	privEvent := payload(field{"issue_key", text("PRIV-1")}, field{"project", text("PRIV")}, field{"type", text("comment.created")},
		field{"actor", `{"kind":"user","id":"alice"}`}, field{"payload", `{"id":"c-1","body":"PRIV's comment"}`})
	passed := frameVerdict{action: framePass, answered: true}
	notHeld := frameDrops(withheldProjectNotHeld)
	notInProject := frameDrops(withheldNotInProject)
	ambiguous := frameDrops(withheldAmbiguousKey)
	prototype := frameDrops(withheldPrototypeKey)

	type row struct {
		name    string
		subject string
		data    []byte
		want    frameVerdict
		// goTopic, goRole and goRepo are what encoding/json reads of the frame's topic, role mark
		// and repository, when the row names one.
		goTopic, goRole, goRepo string
	}
	rows := []row{
		{name: "the probe: another project's topic and a later Topic naming the session's own subject", subject: own,
			data: object(field{"topic", text(otherIssue)}, field{"Topic", text(own)}), want: ambiguous, goTopic: own},
		{name: "the probe without its Topic key (control)", subject: own,
			data: object(field{"topic", text(otherIssue)}), want: notHeld},
		{name: "the probe's keys the other way round", subject: own,
			data: object(field{"Topic", text(own)}, field{"topic", text(otherIssue)}), want: ambiguous, goTopic: otherIssue},
		{name: "a lone Topic key naming the session's own subject", subject: own,
			data: object(field{"Topic", text(own)}), want: ambiguous, goTopic: own},
		{name: "a lone Topic key naming another project under the token's project", subject: ownIssue,
			data: object(field{"Topic", text(otherIssue)}), want: ambiguous, goTopic: otherIssue},
		{name: "a topic key written top\\u0069c after topic", subject: own,
			data: object(field{"topic", text(otherIssue)}, field{`top\u0069c`, text(own)}), want: ambiguous, goTopic: own},
		{name: "a key written \\u0054opic after topic", subject: own,
			data: object(field{"topic", text(otherIssue)}, field{`\u0054opic`, text(own)}), want: ambiguous, goTopic: own},
		{name: "a lone topic key written top\\u0069c naming another project under the token's project", subject: ownIssue,
			data: object(field{`top\u0069c`, text(otherIssue)}), want: notHeld, goTopic: otherIssue},

		{name: "repeated topic keys on the session's own subject", subject: own,
			data: object(field{"topic", text(otherIssue)}, field{"topic", text(own)}), want: ambiguous, goTopic: own},
		{name: "repeated topic keys under the token's project", subject: ownIssue,
			data: object(field{"topic", text(otherIssue)}, field{"topic", text(ownIssue)}), want: ambiguous, goTopic: ownIssue},
		{name: "repeated topic keys of a GitHub event", subject: github,
			data: object(field{"topic", text(otherIssue)}, field{"topic", text(github)}, field{"payload", pubRepo}), want: ambiguous, goTopic: github},
		{name: "repeated topic keys of role lanes", subject: own,
			data: object(field{"topic", text(pubLane)}, field{"topic", text(privLane)}), want: ambiguous, goTopic: privLane},

		{name: "a byte order mark before another project's topic under the token's project", subject: ownIssue,
			data: append([]byte(bom), object(field{"topic", text(otherIssue)})...), want: notHeld},
		{name: "another project's topic beside a value nested 10,001 deep under the token's project", subject: ownIssue,
			data: object(field{"topic", text(otherIssue)}, field{"deep", deep}), want: notHeld},
		{name: "a byte order mark before the session's own message", subject: own,
			data: append([]byte(bom), object(field{"topic", text(own)})...), want: framePasses()},
		{name: "a byte order mark before another project's role mark", subject: own,
			data: append([]byte(bom), object(field{"topic", text(own)}, field{"role", text(pubRole)})...), want: notHeld},
		{name: "another project's role mark beside a value nested 10,001 deep", subject: own,
			data: object(field{"topic", text(own)}, field{"role", text(pubRole)}, field{"deep", deep}), want: notHeld},

		{name: "the session's own message with text after it", subject: own,
			data: []byte(`{"source":"agent","topic":"` + own + `","payload":"hi"} x`), want: notHeld},
		{name: "the session's own message with a misspelt literal", subject: own,
			data: []byte(`{"source":"agent","topic":"` + own + `","payload":tru}`), want: notHeld},
		{name: "the token's project's event with text after it (control: judged by its subject, as the encoding/json read judged it)", subject: ownIssue,
			data: []byte(`{"source":"dispatch","topic":"` + ownIssue + `","payload":` + privEvent + `} x`), want: framePasses()},
		{name: "another project's event with text after it, under the token's project", subject: ownIssue,
			data: []byte(`{"source":"dispatch","topic":"` + otherIssue + `","payload":` + privEvent + `} x`), want: notHeld},
		{name: "a GitHub event of the token's repository with text after it", subject: github,
			data: append(object(field{"topic", text(github)}, field{"payload", pubRepo}), " x"...), want: notInProject},
		{name: "control: a frame that is no JSON, under the token's project, passes by its subject as the encoding/json read passed it", subject: ownIssue,
			data: []byte("xxxxxxxx"), want: framePasses()},

		{name: "another project's role mark and a later Role of the token's project", subject: own,
			data: object(field{"topic", text(own)}, field{"role", text(pubRole)}, field{"Role", text(privRole)}), want: ambiguous, goRole: privRole},
		{name: "repeated role marks, the last of the token's project", subject: own,
			data: object(field{"topic", text(own)}, field{"role", text(pubRole)}, field{"role", text(privRole)}), want: ambiguous, goRole: privRole},
		{name: "the token's project's role mark and a later ROLE of another project", subject: own,
			data: object(field{"topic", text(own)}, field{"role", text(privRole)}, field{"ROLE", text(pubRole)}), want: ambiguous, goRole: pubRole},
		{name: "another project's role mark beside an event id written as a number", subject: own,
			data: object(field{"topic", text(own)}, field{"role", text(pubRole)}, field{"event_id", "4626"}), want: notHeld},

		{name: "a GitHub event of another repository and a later Payload of the token's", subject: github,
			data: object(field{"topic", text(github)}, field{"payload", secretRepo}, field{"Payload", pubRepo}), want: ambiguous, goRepo: "acme/pub"},
		{name: "repeated payload keys, the last of the token's repository", subject: github,
			data: object(field{"topic", text(github)}, field{"payload", secretRepo}, field{"payload", pubRepo}), want: ambiguous, goRepo: "acme/pub"},
		{name: "a GitHub payload naming another repository and a later Repo of the token's", subject: github,
			data: object(field{"topic", text(github)}, field{"payload", payload(field{"repo", text("acme/secret")}, field{"Repo", text("acme/pub")})}), want: ambiguous, goRepo: "acme/pub"},
		{name: "a GitHub payload with repeated repo keys, the last of the token's repository", subject: github,
			data: object(field{"topic", text(github)}, field{"payload", payload(field{"repo", text("acme/secret")}, field{"repo", text("acme/pub")})}), want: ambiguous, goRepo: "acme/pub"},
		{name: "a GitHub payload whose __proto__ key holds the token's repository", subject: github,
			data: object(field{"topic", text(github)}, field{"payload", payload(field{"__proto__", string(object(field{"repo", text("acme/pub")}))})}), want: prototype},
		{name: "a GitHub payload of the token's repository followed by another repository's object", subject: github,
			data: object(field{"topic", text(github)}, field{"payload", text(`{"repo":"acme/pub"} {"repo":"acme/secret","title":"secret"}`)}), want: notInProject},
		{name: "a GitHub payload of the token's repository with a misspelt literal", subject: github,
			data: object(field{"topic", text(github)}, field{"payload", text(`{"repo":"acme/pub","x":tru}`)}), want: notInProject},
		{name: "a GitHub payload of the token's repository beside a value nested 10,001 deep", subject: github,
			data: object(field{"topic", text(github)}, field{"payload", text(`{"repo":"acme/pub","deep":` + deep + `}`)}), want: notInProject},

		{name: "control: a topic of the token's project", subject: ownIssue,
			data: object(field{"topic", text(ownIssue)}), want: framePasses()},
		{name: "control: a topic of a project the token does not list", subject: otherIssue,
			data: object(field{"topic", text(otherIssue)}), want: notHeld},
		{name: "control: another project's topic under the token's project", subject: ownIssue,
			data: object(field{"topic", text(otherIssue)}), want: notHeld},
		{name: "control: the session's own message", subject: own,
			data: object(field{"topic", text(own)}), want: framePasses()},
		{name: "control: the token's project's role-lane forward", subject: own,
			data: object(field{"topic", text(privLane)}), want: framePasses()},
		{name: "control: another project's role-lane forward", subject: own,
			data: object(field{"topic", text(pubLane)}), want: notHeld},
		{name: "control: the token's project's role mark", subject: own,
			data: object(field{"topic", text(own)}, field{"role", text(privRole)}), want: framePasses()},
		{name: "control: another project's role mark", subject: own,
			data: object(field{"topic", text(own)}, field{"role", text(pubRole)}), want: notHeld},
		{name: "control: a GitHub event of the token's repository", subject: github,
			data: object(field{"topic", text(github)}, field{"payload", pubRepo}), want: passed},
		{name: "control: a GitHub event of another repository", subject: github,
			data: object(field{"topic", text(github)}, field{"payload", secretRepo}), want: frameVerdict{action: frameDrop, reason: withheldNotInProject, answered: true}},
		{name: "control: a GitHub header whose envelope names another project's topic", subject: github,
			data: object(field{"topic", text(otherIssue)}, field{"payload", pubRepo}), want: notHeld},
		{name: "control: brackets, a comma and an escaped quote in a string before another project's topic", subject: ownIssue,
			data: object(field{"x", text(`]},"{[`)}, field{"topic", text(otherIssue)}), want: notHeld, goTopic: otherIssue},
		{name: "control: a nested topic naming the token's project beside another project's top-level topic", subject: ownIssue,
			data: object(field{"nested", string(object(field{"topic", text(ownIssue)}, field{"Topic", text(ownIssue)}))}, field{"topic", text(otherIssue)}), want: notHeld, goTopic: otherIssue},
		{name: "control: nested topic keys of any case beside the token's project's own topic", subject: ownIssue,
			data: object(field{"nested", string(object(field{"Topic", text(otherIssue)}))}, field{"list", `[{"topic":1},"]"]`}, field{"topic", text(ownIssue)}), want: framePasses()},
	}
	for _, variant := range []string{"Topic", "TOPIC", "tOpIc"} {
		rows = append(rows,
			row{name: "generic: another project's topic and a later " + variant + " of the token's project", subject: ownIssue,
				data: object(field{"topic", text(otherIssue)}, field{variant, text(ownIssue)}), want: ambiguous, goTopic: ownIssue},
			row{name: "GitHub: another project's topic and a later " + variant + " naming the GitHub event", subject: github,
				data: object(field{"topic", text(otherIssue)}, field{variant, text(github)}, field{"payload", pubRepo}), want: ambiguous, goTopic: github},
			row{name: "role: another project's role lane and a later " + variant + " naming the token's project's lane", subject: own,
				data: object(field{"topic", text(pubLane)}, field{variant, text(privLane)}), want: ambiguous, goTopic: privLane},
			row{name: "agent: another project's topic and a later " + variant + " naming the session's own subject", subject: own,
				data: object(field{"topic", text(otherIssue)}, field{variant, text(own)}), want: ambiguous, goTopic: own},
		)
	}
	// The plugin's zod read copies a "__proto__" key onto the envelope by assignment, so the key's
	// fields become the envelope's through its prototype; the second spelling decodes to the same key.
	for _, proto := range []string{"__proto__", `\u005f\u005fproto__`} {
		holding := func(fields ...field) string { return string(object(fields...)) }
		rows = append(rows,
			row{name: "generic: a " + proto + " key holding another project's topic under the token's project", subject: ownIssue,
				data: object(field{"source", text("dispatch")}, field{proto, holding(field{"topic", text(otherIssue)})}), want: prototype},
			row{name: "GitHub: a " + proto + " key holding another project's topic beside the token's repository", subject: github,
				data: object(field{"source", text("github")}, field{proto, holding(field{"topic", text(otherIssue)})}, field{"payload", pubRepo}), want: prototype},
			row{name: "role: a " + proto + " key holding another project's role mark under the session's own topic", subject: own,
				data: object(field{"topic", text(own)}, field{proto, holding(field{"role", text(pubRole)})}), want: prototype},
			row{name: "agent: a " + proto + " key holding another project's topic beside the session's own topic", subject: own,
				data: object(field{"topic", text(own)}, field{proto, holding(field{"topic", text(otherIssue)})}), want: prototype},
		)
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			var read struct {
				Topic   string `json:"topic"`
				Role    string `json:"role"`
				Payload string `json:"payload"`
			}
			_ = json.Unmarshal(tc.data, &read)
			var inner struct {
				Repo string `json:"repo"`
			}
			_ = json.Unmarshal([]byte(read.Payload), &inner)
			for _, check := range []struct{ what, want, got string }{
				{"topic", tc.goTopic, read.Topic},
				{"role mark", tc.goRole, read.Role},
				{"repository", tc.goRepo, inner.Repo},
			} {
				if check.want != "" && check.got != check.want {
					t.Fatalf("encoding/json reads the frame's %s as %q, want %q: the frame does not split the readers as the row says", check.what, check.got, check.want)
				}
			}
			verdict := edgeFrameVerdict(repositories, session, projects, tc.subject, tc.data)
			if verdict != tc.want {
				t.Fatalf("edgeFrameVerdict(subject=%q, %s) = {action %d, reason %q, answered %t}, want {action %d, reason %q, answered %t}",
					tc.subject, tc.data[:min(len(tc.data), 200)], verdict.action, verdict.reason, verdict.answered, tc.want.action, tc.want.reason, tc.want.answered)
			}
		})
	}
}

// TestEdgeFrameVerdictTellsARoleAsTheRelayDoes holds the edge's frame rule to the read the session
// relay tells a role message by (readRoleFrame), on the session's own agent subject: a message that
// read gives another project's role is withheld, and one it gives a role of the token's project
// passes, whatever encoding/json makes of the message - an event id written as a number, a leading
// byte order mark, a value nested deeper than encoding/json reads. A later key that differs from
// "role" or "topic" only in case leaves the role that read gives as it is, and the edge withholds
// such a message under ambiguous_key whatever its role, as it withholds any frame whose readers
// disagree on a field it judges by (TestEdgeFrameVerdictReadsEachFieldByItsExactKey). A message
// whose role that read cannot tell is no role message, as the relay sends it to the session whose
// subject holds it, and the edge judges it by its topic: the session's own passes and another
// project's is withheld. Each row first checks what the relay's read makes of the message, so no
// row passes on a message that read does not tell as the row names it.
func TestEdgeFrameVerdictTellsARoleAsTheRelayDoes(t *testing.T) {
	t.Parallel()
	const session = "ses_v"
	own := contracts.LegacyAgentSubject(session)
	const privRole, pubRole = "legion-priv-priv-1-tester", "legion-pub-pub-1-tester"
	const otherIssue = "notifications.dispatch.issue.PUB.PUB-1.comment.created"
	projects := []string{"PRIV"}

	marked := func(role, more string) []byte {
		return []byte(`{"topic":"` + own + `","role":"` + role + `"` + more + `}`)
	}
	forward := func(role, more string) []byte {
		return []byte(`{"topic":"` + contracts.RoleTopicPrefix + role + `"` + more + `}`)
	}
	type row struct {
		name string
		data []byte
		// role is the claim key the relay's read gives the message, empty for a message whose
		// role that read cannot tell.
		role string
		want frameVerdict
	}
	var rows []row
	for _, c := range []struct {
		role string
		want frameVerdict
	}{
		{pubRole, frameDrops(withheldProjectNotHeld)},
		{privRole, framePasses()},
	} {
		key := store.RoleClaimKeyOf(c.role)
		rows = append(rows,
			row{c.role + ": a role-marked send", marked(c.role, ""), key, c.want},
			row{c.role + ": a role-marked send whose event id is a number", marked(c.role, `,"event_id":4626`), key, c.want},
			row{c.role + `: a role-marked send whose later "ROLE" key is empty`, marked(c.role, `,"ROLE":""`), key, frameDrops(withheldAmbiguousKey)},
			row{c.role + `: a role-lane forward whose later "TOPIC" key is the session's own subject`, forward(c.role, `,"TOPIC":"`+own+`"`), key, frameDrops(withheldAmbiguousKey)},
			row{c.role + ": a role-marked send after a byte order mark", append([]byte("\xef\xbb\xbf"), marked(c.role, "")...), key, c.want},
			row{c.role + ": a role-marked send with a value nested 10,001 deep", nestedDeep(marked(c.role, "")), key, c.want},
		)
	}
	rows = append(rows,
		row{"a role mark that is a number, under the session's own subject", []byte(`{"topic":"` + own + `","role":7}`), "", framePasses()},
		row{"a role mark that is a number, under another project's topic", []byte(`{"topic":"` + otherIssue + `","role":7}`), "", frameDrops(withheldProjectNotHeld)},
	)
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := readRoleFrame(tc.data)
			switch {
			case tc.role == "" && !errors.Is(err, errRoleFrameUnreadable):
				t.Fatalf("readRoleFrame = {role %q}, %v; want errRoleFrameUnreadable", frame.role, err)
			case tc.role != "" && (err != nil || frame.role != tc.role):
				t.Fatalf("readRoleFrame = {role %q}, %v; want role %q", frame.role, err, tc.role)
			}
			verdict := edgeFrameVerdict(nil, session, projects, own, tc.data)
			if verdict != tc.want {
				t.Fatalf("edgeFrameVerdict(subject=%q) = {action %d, reason %q}, want {action %d, reason %q}",
					own, verdict.action, verdict.reason, tc.want.action, tc.want.reason)
			}
		})
	}
}

// TestEdgeFrameVerdictCountsARoleMarkOnlyOnAnAgentSubject holds the edge's frame rule to where a
// role message's topic is (roleFrameVerdict, roleMarkOnAgentSubject): a role-marked send's topic
// is an agent subject, the target session's own (contracts.AgentSendEnvelope) or, for a kept or
// taken copy, the previous holder's, and a lane forward's is its lane. A frame on the session's
// own agent subject whose role mark names the token's role but whose topic is another project's
// event, a GitHub event, an agent subject of another project or the wildcard project, or no topic
// at all, is judged by its topic, as a frame with no role mark, and withheld: the plugin shows it
// by that topic. A lane forward of the token's project's lane that carries another project's role
// mark is withheld by that mark. The controls are the role messages a producer writes - a
// role-marked send on the session's own subject in either form, a kept or taken copy on the
// previous holder's subject, a lane forward - which pass, a lane forward carrying its own lane's
// role mark, which passes too, and the frames readRoleFrame's role withholds on any topic: another
// project's role mark, and another project's lane. Each row first checks that readRoleFrame, the
// relay's read, tells the frame a role message of the role the row names, so no row passes on a
// frame that read does not take as a role message.
func TestEdgeFrameVerdictCountsARoleMarkOnlyOnAnAgentSubject(t *testing.T) {
	t.Parallel()
	const session = "ses_v"
	own := contracts.LegacyAgentSubject(session)
	const privRole, pubRole = "legion-priv-priv-1-tester", "legion-pub-pub-1-tester"
	privLane, pubLane := contracts.RoleTopicPrefix+privRole, contracts.RoleTopicPrefix+pubRole
	const ownIssue = "notifications.dispatch.issue.PRIV.PRIV-1.comment.created"
	const otherIssue = "notifications.dispatch.issue.PUB.PUB-1.comment.created"
	const github = "notifications.github.acme.pub.pr.1"
	projects := []string{"PRIV"}
	notHeld := frameDrops(withheldProjectNotHeld)

	// frame writes a JSON object of fields in order, each a key and a raw JSON value.
	frame := func(fields ...[2]string) []byte {
		parts := make([]string, len(fields))
		for i, f := range fields {
			parts[i] = `"` + f[0] + `":` + f[1]
		}
		return []byte("{" + strings.Join(parts, ",") + "}")
	}
	text := func(s string) string {
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	topic := func(s string) [2]string { return [2]string{"topic", text(s)} }
	role := func(s string) [2]string { return [2]string{"role", text(s)} }
	pubComment := [2]string{"payload", text(`{"issue_key":"PUB-1","project":"PUB","type":"comment.created","actor":{"kind":"user","id":"alice"},"payload":{"body":"PUB's comment"}}`)}
	privComment := [2]string{"payload", text(`{"issue_key":"PRIV-1","project":"PRIV","type":"comment.created","actor":{"kind":"user","id":"alice"},"payload":{"body":"PRIV's comment"}}`)}
	dispatch, agent := [2]string{"source", text("dispatch")}, [2]string{"source", text("agent")}
	privKey, pubKey := store.RoleClaimKeyOf(privRole), store.RoleClaimKeyOf(pubRole)

	for _, tc := range []struct {
		name string
		data []byte
		// role is the claim key readRoleFrame gives the frame.
		role string
		want frameVerdict
	}{
		// The review's frame with a source other than Dispatch's, so the Dispatch rule
		// (dispatchFrameWithheld) does not decide it before the role-mark rule; the row below holds
		// the Dispatch form with the token's project's payload.
		{"another project's issue topic under the token's role mark, as a frame of another source (the review's frame)",
			frame(topic(otherIssue), role(privRole), agent, pubComment), privKey, notHeld},
		{"another project's issue topic under the token's role mark, its Dispatch payload of the token's project",
			frame(topic(otherIssue), role(privRole), dispatch, privComment), privKey, notHeld},
		{"a GitHub event under the token's role mark",
			frame(topic(github), role(privRole), [2]string{"payload", text(`{"repo":"acme/secret"}`)}), privKey, notHeld},
		{"another session's agent subject under another project, with the token's role mark",
			frame(topic(contracts.AgentSubject("PUB", "ses_w")), role(privRole)), privKey, notHeld},
		{"the wildcard project's agent subject with the token's role mark",
			frame(topic(contracts.AgentTopicPrefix+"*."+session), role(privRole)), privKey, notHeld},
		{"no topic, with the token's role mark",
			frame(role(privRole), agent), privKey, notHeld},
		{"a null topic, with the token's role mark",
			frame([2]string{"topic", "null"}, role(privRole)), privKey, notHeld},
		{"a topic that is no string, with the token's role mark",
			frame([2]string{"topic", "7"}, role(privRole)), privKey, notHeld},
		{"the token's project's lane forward carrying another project's role mark",
			frame(topic(privLane), role(pubRole)), privKey, notHeld},

		{"control: the token's role-marked send on the session's own subject",
			frame(topic(own), role(privRole)), privKey, framePasses()},
		{"control: the token's role-marked send on the session's own subject in the new form",
			frame(topic(contracts.AgentSubject("PRIV", session)), role(privRole)), privKey, framePasses()},
		{"control: a kept or taken copy on the previous holder's subject",
			frame(topic(contracts.LegacyAgentSubject("ses_w")), role(privRole)), privKey, framePasses()},
		{"control: a copy on the previous holder's subject in the new form under the token's project",
			frame(topic(contracts.AgentSubject("PRIV", "ses_w")), role(privRole)), privKey, framePasses()},
		{"control: the token's project's lane forward",
			frame(topic(privLane), agent), privKey, framePasses()},
		{"control: the token's project's lane forward carrying its own role mark",
			frame(topic(privLane), role(privRole)), privKey, framePasses()},
		{"control: another project's lane forward carrying the token's role mark",
			frame(topic(pubLane), role(privRole)), pubKey, notHeld},
		{"control: another project's role mark on the session's own subject",
			frame(topic(own), role(pubRole)), pubKey, notHeld},
		{"control: another project's role mark on the token's project's event, withheld by that mark",
			frame(topic(ownIssue), role(pubRole)), pubKey, notHeld},
		{"control: the token's role mark on the token's project's event, judged by that event",
			frame(topic(ownIssue), role(privRole)), privKey, framePasses()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := readRoleFrame(tc.data); err != nil || got.role != tc.role {
				t.Fatalf("readRoleFrame = {role %q}, %v; want role %q: the frame is not the role message the row says", got.role, err, tc.role)
			}
			verdict := edgeFrameVerdict(nil, session, projects, own, tc.data)
			if verdict != tc.want {
				t.Fatalf("edgeFrameVerdict(subject=%q, %s) = {action %d, reason %q}, want {action %d, reason %q}",
					own, tc.data, verdict.action, verdict.reason, tc.want.action, tc.want.reason)
			}
		})
	}
}

// packages/envoy/internal/cipher/requests/slack_channels_test.go
// #t-d-12's release half: the airlock hands the proxy the channels a private project's
// credential may post to, and the limit is a flag, never a non-nil list.
package requests

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// slackToken is the Legion Slack bot token as a private project's agent would ask for it: shared,
// agent tier, behind the airlock, carrying project-channels=slack and no dispatch-project tag.
const slackToken = "SLACK_TOKEN"

// slackFixture is newProjectFixture, which serves slackToken beside the project secrets, with
// PRIV_KEY and slackToken both behind the airlock and Dispatch answering rows.
func slackFixture(t *testing.T, rows map[string]projectaccesstest.Row) (*Machine, string, *projectaccesstest.Server, func(names ...string) (map[string]AirlockSecret, int)) {
	t.Helper()
	rules := bound(rule(slackToken, "slack"), rule("PRIV_KEY", "example"))
	m, enr, key, _ := newProjectFixture(t, rules, publicTeam)
	dispatch := answerFrom(t, m, rows)
	// release grants names to the session and reads them as the proxy does, answering the values
	// and how many calls Dispatch took of its project route for the two (beside the member-kind
	// reads every grant and release of a person's session makes, restrictedOperator), from a client
	// of its own so that no answer an earlier release cached stands in.
	release := func(names ...string) (map[string]AirlockSecret, int) {
		t.Helper()
		fresh := answerFrom(t, m, rows)
		_, grantID := grantOf(t, m, enr, key, "post the release notes", names...)
		values, _, err := m.AirlockValues(t.Context(), grantID)
		if err != nil {
			t.Fatalf("AirlockValues(%v): %v", names, err)
		}
		return values, fresh.ProjectCalls()
	}
	return m, enr, dispatch, release
}

// slackRows is Dispatch holding PRIV, private with the fixture's operator on its list and the
// channels channels named by its owner, and TEAM as team.
func slackRows(priv projectaccesstest.Row, team projectaccesstest.Row) map[string]projectaccesstest.Row {
	priv.Visibility, priv.Owner = "private", otherPerson
	if priv.Listed == nil {
		priv.Listed = []string{fixtureOperator, insider}
	}
	return map[string]projectaccesstest.Row{privProject: priv, teamProject: team}
}

// TestAPrivateProjectsSlackGrantCarriesItsOwnersChannels is the airlock's half of #t-d-12: a
// project-channels secret released to a credential that names a private project carries Limited
// and the channels that project's owner named, bounded to airlockProjectRecheck, and asks
// Dispatch no more than a release of that project's own secret does. Every case where the
// channels cannot be known, or the projects share none, is Limited with an empty list, never
// unlimited; a credential that names no project is unlimited and asks Dispatch nothing.
func TestAPrivateProjectsSlackGrantCarriesItsOwnersChannels(t *testing.T) {
	limitedTo := func(t *testing.T, values map[string]AirlockSecret, want []string) {
		t.Helper()
		got := values[slackToken]
		if !got.Limited || got.Channels == nil || !slices.Equal(got.Channels, want) {
			t.Fatalf("%s is released with Limited=%v Channels=%#v; want Limited and %#v", slackToken, got.Limited, got.Channels, want)
		}
	}

	t.Run("limited, with the owner's channels, at the cost of the project's own secret", func(t *testing.T) {
		m, enr, _, release := slackFixture(t, slackRows(projectaccesstest.Row{SlackChannels: []string{"C0ALLOWED1", "C0ALLOWED2"}}, publicTeam))
		nameProjects(t, m, enr, privProject)
		read := time.Now()
		values, calls := release(slackToken)
		limitedTo(t, values, []string{"C0ALLOWED1", "C0ALLOWED2"})
		expires := values[slackToken].Expires
		if expires.Before(read.Add(airlockProjectRecheck)) || expires.After(time.Now().Add(airlockProjectRecheck)) {
			t.Fatalf("%s expires %v; want the read time plus %v, so a running proxy hears an owner's change", slackToken, expires, airlockProjectRecheck)
		}
		_, ownCalls := release("PRIV_KEY")
		if calls != ownCalls {
			t.Fatalf("releasing %s took %d calls to Dispatch, and PRIV's own secret %d; want the same", slackToken, calls, ownCalls)
		}
	})

	for _, c := range []struct {
		name  string
		rows  map[string]projectaccesstest.Row
		names []string
	}{
		{"the owner named no channel", slackRows(projectaccesstest.Row{}, publicTeam), []string{privProject}},
		{"two private projects share no channel", slackRows(projectaccesstest.Row{SlackChannels: []string{"C0ALLOWED1"}},
			projectaccesstest.Row{Visibility: "private", Owner: otherPerson, Listed: []string{fixtureOperator}, SlackChannels: []string{"C0TEAMONLY"}}),
			[]string{privProject, teamProject}},
		{"Dispatch answered no slack_channels key", slackRows(projectaccesstest.Row{OmitSlackChannels: true}, publicTeam), []string{privProject}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, _, release := slackFixture(t, c.rows)
			nameProjects(t, m, enr, c.names...)
			values, _ := release(slackToken)
			limitedTo(t, values, []string{})
		})
	}

	t.Run("a person who loses the project stays limited, to no channel", func(t *testing.T) {
		rows := slackRows(projectaccesstest.Row{SlackChannels: []string{"C0ALLOWED1"}}, publicTeam)
		m, enr, _, release := slackFixture(t, rows)
		nameProjects(t, m, enr, privProject)
		limitedTo(t, func() map[string]AirlockSecret { v, _ := release(slackToken); return v }(), []string{"C0ALLOWED1"})
		priv := rows[privProject]
		priv.Listed = []string{insider}
		rows[privProject] = priv
		values, _ := release(slackToken)
		limitedTo(t, values, []string{})
	})

	t.Run("a credential naming no project is unlimited and asks Dispatch nothing of a project", func(t *testing.T) {
		_, _, _, release := slackFixture(t, slackRows(projectaccesstest.Row{SlackChannels: []string{"C0ALLOWED1"}}, publicTeam))
		values, calls := release(slackToken)
		if got := values[slackToken]; got.Limited || got.Channels != nil || !got.Expires.IsZero() {
			t.Fatalf("%s for a session naming no project = Limited %v Channels %#v Expires %v; want unlimited, as today", slackToken, got.Limited, got.Channels, got.Expires)
		}
		if calls != 0 {
			t.Fatalf("a session naming no project made %d calls to Dispatch's project route for %s; want none", calls, slackToken)
		}
	})
}

// TestAJobPodsSlackTokenIsLimitedByItsList: a job entry's pod names its entry's list and no
// person, and the project-channels rule reads that list as it reads every credential's
// (policy.Requester.ListsPrivate). A list naming a private project limits the Slack token to no
// channel, whatever channels the project's owner named, since the pod sees no private project, and
// asks Dispatch nothing about a person; where no proxy profile serves the token, the pod is denied
// it project-channels-need-airlock. A list of public projects alone leaves it unlimited, as for a
// person's pod of a public project.
func TestAJobPodsSlackTokenIsLimitedByItsList(t *testing.T) {
	privateLists := []string{privProject, "public," + privProject}
	// released grants the Slack token to pod and reads it as the proxy does.
	released := func(t *testing.T, m *Machine, pod jobPod) AirlockSecret {
		t.Helper()
		_, grantID := grantOf(t, m, pod.enrollmentID, pod.key, "post the release notes", slackToken)
		values, _, err := m.AirlockValues(t.Context(), grantID)
		if err != nil {
			t.Fatalf("AirlockValues(%s) for the pod of the job list %s: %v", slackToken, pod.list, err)
		}
		return values[slackToken]
	}

	t.Run("a list naming a private project is limited to no channel, and Dispatch is asked nothing about a person", func(t *testing.T) {
		m, _, _, _ := slackFixture(t, slackRows(projectaccesstest.Row{SlackChannels: []string{"C0ALLOWED1"}}, publicTeam))
		asked := &askLog{inner: m.Projects}
		m.Projects = asked
		for _, pod := range jobPods(t, m, privateLists...) {
			if got := released(t, m, pod); !got.Limited || got.Channels == nil || len(got.Channels) != 0 {
				t.Errorf("%s released to the pod of the job list %s = Limited %v Channels %#v; want Limited to no channel", slackToken, pod.list, got.Limited, got.Channels)
			}
		}
		if about := asked.askedAbout(); len(about) != 0 {
			t.Errorf("the job pods' releases asked Dispatch's access route about %q; want no access question, since their credentials name no person", about)
		}
		questions := asked.questions()
		for _, q := range questions {
			if !strings.HasPrefix(q, "states of ") {
				t.Errorf("the job pods' releases asked Dispatch %q (they asked %v); want the listed projects' visibility and nothing else", q, questions)
			}
		}
	})

	t.Run("a list of public projects alone is unlimited", func(t *testing.T) {
		m, _, _, _ := slackFixture(t, slackRows(projectaccesstest.Row{SlackChannels: []string{"C0ALLOWED1"}}, publicTeam))
		for _, pod := range jobPods(t, m, teamProject, "public,"+teamProject, "public") {
			if got := released(t, m, pod); got.Limited || got.Channels != nil {
				t.Errorf("%s released to the pod of the job list %s = Limited %v Channels %#v; want unlimited, as for a person's pod of a public project", slackToken, pod.list, got.Limited, got.Channels)
			}
		}
	})

	t.Run("with no proxy profile a list naming a private project is denied project-channels-need-airlock", func(t *testing.T) {
		m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
		for _, pod := range jobPods(t, m, privateLists...) {
			request, err := m.Create(t.Context(), pod.enrollmentID, signRequest(t, m, pod.key, "post the release notes", slackToken), "")
			deniedFor(t, "a request for "+slackToken+", which no proxy profile serves, from the pod of the job list "+pod.list, request, err, slackToken, policy.ReasonProjectChannelsNeedAirlock)
		}
	})
}

// TestTheSlackTokenOfAPodOnABrokerWithNoProjectAccessIsServedAsBefore is R9 for the Slack limit,
// the same class as the judge's (TestTheJudgeOfAPodWithAProjectAndNoProjectAccessDecidesAsBefore):
// a broker with no project-access client loads no dispatch-project secret and cannot know any
// project is private, so a project-channels secret asked for by a pod whose exchange entry names a
// project is granted and released exactly as the merge base released it: no refusal, no limit and
// no ten-minute bound, though every pod's entry names a project.
func TestTheSlackTokenOfAPodOnABrokerWithNoProjectAccessIsServedAsBefore(t *testing.T) {
	m, _, _, _ := newProjectFixture(t, bound(rule(slackToken, "slack")), publicTeam)
	set, err := exchange.NewSet([]exchange.AccountEntry{{Account: fixtureSubject, Project: teamProject, Person: insider}}, nil, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	m.Exchange = set
	m.Projects = nil
	pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-team-"+t.Name())
	_, grantID := grantOf(t, m, pod, podKey, "post the release notes", slackToken)
	values, _, err := m.AirlockValues(t.Context(), grantID)
	if err != nil {
		t.Fatalf("AirlockValues(%s) for a pod mapped to TEAM on a broker with no project access = %v; want it released as before", slackToken, err)
	}
	if got := values[slackToken]; got.Limited || got.Channels != nil || !got.Expires.IsZero() {
		t.Fatalf("%s for that pod = Limited %v Channels %#v Expires %v; want unlimited and unbounded, as before", slackToken, got.Limited, got.Channels, got.Expires)
	}
}

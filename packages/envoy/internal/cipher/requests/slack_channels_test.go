// packages/envoy/internal/cipher/requests/slack_channels_test.go
// #t-d-12's release half: the airlock hands the proxy the channels a private project's
// credential may post to, and the limit is a flag, never a non-nil list.
package requests

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// slackToken is the Legion Slack bot token as a private project's agent would ask for it: shared,
// agent tier, behind the airlock, carrying project-channels=slack and no dispatch-project tag.
const slackToken = "SLACK_TOKEN"

// slackFixture is newProjectFixture, which serves slackToken beside the project secrets, with
// PRIV_KEY and slackToken both behind the airlock and Dispatch answering rows. The broker's stored
// private list holds every private row of rows with its channels, stored long enough ago that a
// limited grant's settle has run (storeAged), as a broker's does once it has read the list.
func slackFixture(t *testing.T, rows map[string]projectaccesstest.Row) (*Machine, string, *projectaccesstest.Server, func(names ...string) (map[string]AirlockSecret, *projectaccesstest.Server)) {
	t.Helper()
	rules := proxyrules.Rules{Secrets: []proxyrules.SecretRule{rule(slackToken, ""), rule("PRIV_KEY", "")}}
	m, enr, key, _ := newProjectFixture(t, rules, publicTeam)
	storeAged(t, m.Store, rows)
	dispatch := answerFrom(t, m, rows)
	// release grants names to the session and reads them as the proxy does, answering the values
	// and the stand-in Dispatch that answered the two, behind a client of its own so that no
	// answer an earlier release cached stands in. The client is the machine's Projects and its
	// PrivateList both, as cipherd wires one client.
	release := func(names ...string) (map[string]AirlockSecret, *projectaccesstest.Server) {
		t.Helper()
		fresh := projectaccesstest.NewServer(t, rows, nil)
		client := projectaccess.New(fresh.URL, fresh.Account, nil)
		m.Projects, m.PrivateList = client, client
		_, grantID := grantOf(t, m, enr, key, "post the release notes", names...)
		values, _, err := m.AirlockValues(t.Context(), grantID)
		if err != nil {
			t.Fatalf("AirlockValues(%v): %v", names, err)
		}
		return values, fresh
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

// TestAPrivateProjectsSlackGrantCarriesItsOwnersChannels is the airlock's half of a private
// project's Slack limit: a project-channels secret released to a credential that names a private
// project carries Limited and the channels that project's owner named, bounded to
// airlockProjectRecheck, and asks Dispatch no more than a release of that project's own secret
// does. Every case where the channels cannot be known, or the projects share none, is Limited
// with an empty list, never unlimited; a credential that names no project is unlimited, asks
// Dispatch its list of private projects alone (the public fence's read), and its value is bounded
// to airlockProjectRecheck all the same, so it learns of a private project's channels once one
// names any.
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
		values, fresh := release(slackToken)
		limitedTo(t, values, []string{"C0ALLOWED1", "C0ALLOWED2"})
		expires := values[slackToken].Expires
		if expires.Before(read.Add(airlockProjectRecheck)) || expires.After(time.Now().Add(airlockProjectRecheck)) {
			t.Fatalf("%s expires %v; want the read time plus %v, so a running proxy hears an owner's change", slackToken, expires, airlockProjectRecheck)
		}
		// The Slack release reads Dispatch's list of private projects once more, to keep the
		// broker's stored private list (channelsOf), and asks nothing else beyond what PRIV's own
		// secret asks.
		_, own := release("PRIV_KEY")
		if calls, lists, ownCalls := fresh.Calls(), fresh.PrivateProjectsCalls(), own.Calls(); lists != 1 || calls-lists != ownCalls {
			t.Fatalf("releasing %s took %d calls to Dispatch, %d of them for the list of private projects, and PRIV's own secret %d; want the list once and otherwise the same", slackToken, calls, lists, ownCalls)
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

	t.Run("a credential naming no project is unlimited and asks Dispatch the list of private projects alone", func(t *testing.T) {
		_, _, _, release := slackFixture(t, slackRows(projectaccesstest.Row{SlackChannels: []string{"C0ALLOWED1"}}, publicTeam))
		read := time.Now()
		values, fresh := release(slackToken)
		got := values[slackToken]
		if got.Limited || got.Channels != nil {
			t.Fatalf("%s for a session naming no project = Limited %v Channels %#v; want unlimited, as today", slackToken, got.Limited, got.Channels)
		}
		if got.Expires.Before(read.Add(airlockProjectRecheck)) || got.Expires.After(time.Now().Add(airlockProjectRecheck)) {
			t.Fatalf("%s for a session naming no project expires %v; want the read time plus %v, so a running proxy reads it again and meets the fence of that moment", slackToken, got.Expires, airlockProjectRecheck)
		}
		if calls, lists := fresh.Calls(), fresh.PrivateProjectsCalls(); calls != lists || lists != 1 {
			t.Fatalf("a session naming no project made %d calls to Dispatch for %s, %d of them for the list of private projects; want the list alone, once", calls, slackToken, lists)
		}
	})
}

// TestTheSlackTokenOfAPodOnABrokerWithNoProjectAccessIsServedAsBefore is R9 for the Slack limit,
// the same class as the judge's (TestTheJudgeOfAPodWithAProjectAndNoProjectAccessDecidesAsBefore):
// a broker with no project-access client loads no dispatch-project secret and cannot know any
// project is private, so a project-channels secret asked for by a pod whose exchange entry names a
// project is granted and released as the merge base released it: no refusal and no limit, though
// every pod's entry names a project. Its value carries the ten-minute bound every value of a
// project-channels name carries in every deployment, so a pod holding it reads it again once the
// broker can know a project is private.
func TestTheSlackTokenOfAPodOnABrokerWithNoProjectAccessIsServedAsBefore(t *testing.T) {
	rules := proxyrules.Rules{Secrets: []proxyrules.SecretRule{rule(slackToken, "")}}
	m, _, _, _ := newProjectFixture(t, rules, publicTeam)
	set, err := exchange.NewSet([]exchange.AccountEntry{{Account: fixtureSubject, Project: teamProject, Person: insider}}, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	m.Exchange = set
	m.Projects = nil
	pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-team-"+t.Name())
	_, grantID := grantOf(t, m, pod, podKey, "post the release notes", slackToken)
	read := time.Now()
	values, _, err := m.AirlockValues(t.Context(), grantID)
	if err != nil {
		t.Fatalf("AirlockValues(%s) for a pod mapped to TEAM on a broker with no project access = %v; want it released as before", slackToken, err)
	}
	got := values[slackToken]
	if got.Limited || got.Channels != nil || got.Excluded != nil || got.ExcludedUnread {
		t.Fatalf("%s for that pod = Limited %v Channels %#v Excluded %#v ExcludedUnread %v; want unlimited and unfenced, as before", slackToken, got.Limited, got.Channels, got.Excluded, got.ExcludedUnread)
	}
	if got.Expires.Before(read.Add(airlockProjectRecheck)) || got.Expires.After(time.Now().Add(airlockProjectRecheck)) {
		t.Fatalf("%s for that pod expires %v; want the read time plus %v, as every value of a project-channels name in every deployment", slackToken, got.Expires, airlockProjectRecheck)
	}
}

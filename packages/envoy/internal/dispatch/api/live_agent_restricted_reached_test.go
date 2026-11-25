package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestAFullMemberTheSignInRuleRestrictsKeepsWhatTheyLostOnTheirSessions: a write of a person's
// member kind can take projects from them too. gina is a full member in a group that PRIV's list
// names, so she sees PRIV through the group and PUB as a public project. A restricted member sees
// neither: a group admits only full members, and a public project is seen only by full members.
// Both of her sessions read PRIV's issue under a broker-issued token naming PRIV, which ties each
// to her and records nothing else. Then the people store writes her row restricted: in one row by
// SignInSubject, in the other by ConfirmSubject. No sign-in rule makes that write now (Google's
// sign-in and its hourly confirmation name every account they admit a full member), so the test
// makes it through the store, whose bookkeeping for it this pins. In the same transaction each of
// her sessions keeps PRIV and PUB as projects it reached (session_reached_projects), so:
//
//   - alice, an open viewer PRIV's list does not name, gets the no-responder verdict for both
//     sessions;
//   - gina, now restricted, gets it for her own session with no listener list;
//   - carol, who sees PRIV and PUB, still watches both, so the view relays at all.
//
// Without the pairs, both views pass: the open-viewer rule judges gina by what she sees now,
// which holds no private project, and the listed session's listener lists PUB alone; and the
// restricted-viewer rule passes a session that wrote in no project and is tied to gina alone.
//
// Control: the kind writes keep nothing (RecordReachedProjects handed no project). No pair is
// written, alice is relayed both sessions' replays, which hold PRIV's issue's text, gina is
// relayed hers, and both rows fail.
func TestAFullMemberTheSignInRuleRestrictsKeepsWhatTheyLostOnTheirSessions(t *testing.T) {
	t.Parallel()
	const account = "gina-account"
	for _, row := range []struct {
		name     string
		restrict func(ctx context.Context, people *store.PgPeopleStore) error
	}{
		{"sign-in", func(ctx context.Context, people *store.PgPeopleStore) error {
			return people.SignInSubject(ctx, reachedGina, account, "refresh-gina-restricted", "", model.MemberRestricted)
		}},
		{"confirmation", func(ctx context.Context, people *store.PgPeopleStore) error {
			return people.ConfirmSubject(ctx, reachedGina, account, "refresh-gina", "refresh-gina-restricted", "", model.MemberRestricted)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newReachedRig(t)
			people := store.NewPgPeopleStore(rig.seed.database.Pool, testSigningKey, nil)
			if err := people.SignInSubject(ctx, reachedGina, account, "refresh-gina", "", model.MemberFull); err != nil {
				t.Fatalf("gina's sign-in as a full member: %v", err)
			}
			reachedGroupOnPRIV(t, rig)
			for _, session := range []string{reachedListedSession, reachedUnlistedSession} {
				rig.seed.asSessionOf(t, reachedGina, session, seedPrivate, http.MethodGet, "/api/v1/issues/"+rig.seed.issue[seedPrivate], nil, http.StatusOK)
			}
			if err := row.restrict(ctx, people); err != nil {
				t.Fatalf("the %s that restricts gina: %v", row.name, err)
			}
			state := "gina restricted by the " + row.name
			sessions := []string{reachedListedSession, reachedUnlistedSession}
			for _, session := range sessions {
				var reached []string
				if err := rig.seed.database.Pool.QueryRow(ctx, `
					select array(select project_key from session_reached_projects where session_id = $1 order by project_key collate "C")`,
					session).Scan(&reached); err != nil {
					t.Fatalf("read the projects session %s reached: %v", session, err)
				}
				if want := []string{seedPrivate, seedPublic}; !slices.Equal(reached, want) {
					t.Errorf("%s, session %s reached %v, want %v", state, session, reached, want)
				}
				rig.hidden(t, state, callerAlice, session)
			}
			// Before carol's views, which subscribe to the sessions: a hidden view leaves none.
			rig.hiddenTo(t, state, reachedGina, reachedUnlistedSession)
			for _, session := range sessions {
				rig.watch(t, state, contentView{callerCarol, session, agentViewResponding, "carol sees PRIV"})
			}
		})
	}
}

// verdictOf is the person at login's first frame of their agent view of session at a browser, as
// agentVerdict is a seed caller's.
func (rig contentAgentRig) verdictOf(t *testing.T, login, session string) (*socketClient, string) {
	t.Helper()
	client := rig.seed.dialLive(t, rig.url, asUser(login))
	client.send(t, liveSubscribe("v", agentView(session), ""))
	frame := client.next(t, eventFeedWait, login+"'s agent view of "+session)
	if agent, ok := frame.(LiveAgentFrame); ok {
		return client, string(agent.Data)
	}
	got, _ := json.Marshal(frame)
	return client, string(got)
}

// hiddenTo is hidden for the person at login at a browser.
func (rig contentAgentRig) hiddenTo(t *testing.T, state, login, session string) {
	t.Helper()
	watches := rig.stream.Watches(session)
	client, got := rig.verdictOf(t, login, session)
	if got != agentViewHidden {
		relayed := "nothing"
		if got == agentViewResponding {
			replay, _ := json.Marshal(client.next(t, eventFeedWait, login+"'s replay of "+session))
			relayed = string(replay)
		}
		t.Errorf("%s, %s's agent view of %s answers %s and relays %s, want the no-responder verdict", state, login, session, got, relayed)
		return
	}
	if viewers, told := rig.stream.Viewers(session), rig.stream.Watches(session)-watches; viewers != 0 || told != 0 {
		t.Errorf("%s, %s's hidden view of %s left %d viewers subscribed and told the session %d times a viewer is attached, want none",
			state, login, session, viewers, told)
	}
}

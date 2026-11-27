// packages/envoy/internal/cipher/requests/project_fact_age_test.go
// The High of round 6 on cybermonklabs/legion#4135 (TNDeep4135 and ReviewPR4135): no decision rests
// on an answer of Dispatch's older than projectaccess.CacheTTL at the moment it is made
// (atDecision), however long the reads after that answer took.
package requests

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// slowFront is an http.RoundTripper in front of the Dispatch stand-in: the first question about
// each project slow picks is held 8 s, within the client's own 10 s bound on one call, and the
// first question it holds turns TEAM private in the stand-in, so every answer about TEAM given
// before then is the public one. Every question is then sent to the stand-in as it is.
type slowFront struct {
	dispatch *projectaccesstest.Server
	slow     func(project, person string) bool
	mu       sync.Mutex
	held     map[string]bool
	flipped  atomic.Bool
}

func (f *slowFront) RoundTrip(r *http.Request) (*http.Response, error) {
	project := strings.TrimPrefix(r.URL.Path, "/api/v1/access/projects/")
	if f.slow(project, r.URL.Query().Get("person")) && f.first(project+" "+r.URL.Query().Get("person")) {
		if f.flipped.CompareAndSwap(false, true) {
			f.dispatch.SetRow(teamProject, privateTeam(fixtureOperator))
		}
		select {
		case <-time.After(8 * time.Second):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

// first reports whether key's question is the first one, and counts it asked.
func (f *slowFront) first(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held == nil {
		f.held = map[string]bool{}
	}
	first := !f.held[key]
	f.held[key] = true
	return first
}

// TestAProjectFactCannotAgePastTheBoundBeforeTheRelease is the reviewer's probe, in both of its
// phases, through Machine.Values on a real Postgres and the real projectaccess.Client. A session
// naming four projects but not TEAM holds a grant of TEAM_TOKEN (tagged TEAM, granted while TEAM
// was public) and SLACK_TOKEN, whose project-channels tag makes the release read the four
// projects' states too. TEAM's state is asked first and answers public; TEAM turns private as
// the next slow question is sent, and four slow questions of 8 s each later the public answer
// is 32 s old, past the 30 s bound.
//
// Phase one: the four projects are public, and their state questions are the slow ones
// (Client.States asks again for an answer that aged during the call). Phase two: the four are
// private, their states answer at once, and the person's visibility questions are the slow ones,
// after States has returned (projectFacts' own bound, at its end). Either way the release is
// refused. With every question answered at once, TEAM still public, the release proceeds as
// before (R9).
func TestAProjectFactCannotAgePastTheBoundBeforeTheRelease(t *testing.T) {
	others := []string{"ZZAA", "ZZBB", "ZZCC", "ZZDD"}
	for _, c := range []struct {
		name    string
		private bool
		slow    func(project, person string) bool
	}{
		{"every question answered at once: the release proceeds", false, nil},
		{"phase one: a state read first ages while the later states are read", false,
			func(project, person string) bool { return strings.HasPrefix(project, "ZZ") && person == "" }},
		{"phase two: the states age while the person's projects are read", true,
			func(project, person string) bool { return strings.HasPrefix(project, "ZZ") && person != "" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m, enr, key, dispatch := newProjectFixture(t, bound(rule(slackToken, "slack")), publicTeam)
			nameProjects(t, m, enr, others...)
			for _, p := range others {
				row := projectaccesstest.Row{Visibility: "public", Owner: fixtureOperator}
				if c.private {
					row = projectaccesstest.Row{Visibility: "private", Owner: otherPerson, Listed: []string{fixtureOperator}}
				}
				dispatch.SetRow(p, row)
			}
			_, grantID := grantOf(t, m, enr, key, "read it", "TEAM_TOKEN", slackToken)
			front := &slowFront{dispatch: dispatch, slow: c.slow}
			if c.slow == nil {
				front.slow = func(string, string) bool { return false }
			}
			// A fresh client, so no answer the grant's own request cached stands in for one.
			m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, &http.Client{Transport: front})

			start := time.Now()
			values, err := m.Values(t.Context(), grantID, enr, ReleaseCall{})
			elapsed := time.Since(start)
			if c.slow == nil {
				if err != nil || values.Values["TEAM_TOKEN"] != "team-v1" {
					t.Fatalf("Values with every question answered at once = %+v, %v; want TEAM_TOKEN released", values, err)
				}
				return
			}
			if !front.flipped.Load() || elapsed < 32*time.Second {
				t.Fatalf("the probe missed its own case: TEAM turned private %v, the release took %s; want TEAM private and at least 32 s", front.flipped.Load(), elapsed)
			}
			if err == nil {
				t.Fatalf("after %s, a credential naming %v and not TEAM was given TEAM_TOKEN=%q, though TEAM turned private %s before", elapsed, others, values.Values["TEAM_TOKEN"], elapsed)
			}
			if !errors.Is(err, errProjectNotNamed) && !errors.Is(err, ErrProjectAccessUnavailable) {
				t.Fatalf("Values = %v; want the release refused for TEAM's sake (errProjectNotNamed) or for an answer that aged again (ErrProjectAccessUnavailable)", err)
			}
			t.Logf("after %s the release was refused: %v", elapsed, err)
		})
	}
}

// agedFront is a projectaccess.Access whose first answer about TEAM, for each person and for the
// state, is the one Dispatch gave while TEAM was public, asked a second longer ago than
// projectaccess.CacheTTL: what a reader holds of TEAM when its later reads took longer than the
// bound. Every later question, and every question of any other project, goes to inner.
type agedFront struct {
	inner projectaccess.Access
	mu    sync.Mutex
	given map[string]bool
	aged  atomic.Int32
}

// agedAnswer is TEAM's public answer for person, aged past the bound, the first time it is asked.
func (f *agedFront) agedAnswer(person string) (projectaccess.Answer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.given == nil {
		f.given = map[string]bool{}
	}
	if f.given[person] {
		return projectaccess.Answer{}, false
	}
	f.given[person] = true
	f.aged.Add(1)
	return projectaccess.Answer{
		Project: teamProject, Visibility: "public", Owner: otherPerson, Person: person,
		MemberKind: projectaccess.MemberFull, Visible: true, SlackChannelsPresent: true,
		AskedAt: time.Now().Add(-projectaccess.CacheTTL - time.Second),
	}, true
}

func (f *agedFront) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	if project == teamProject {
		if answer, ok := f.agedAnswer(record.CanonicalLogin(person)); ok {
			return answer, nil
		}
	}
	return f.inner.Access(ctx, project, person)
}

func (f *agedFront) MemberKind(ctx context.Context, person string) (string, error) {
	return f.inner.MemberKind(ctx, person)
}

func (f *agedFront) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	states, err := f.inner.States(ctx, projects)
	if err != nil || !slices.Contains(projects, teamProject) {
		return states, err
	}
	if answer, ok := f.agedAnswer(""); ok {
		states[teamProject] = answer
	}
	return states, nil
}

// TestEveryDecisionOnAnAgedProjectFactIsMadeOnAFreshOne is the class of that High: each reader
// whose decision rests on more than one answer of Dispatch's (the release, the approval page, the
// approver's list, a person's decision, the judge's project check, the airlock's channel limit)
// holds the oldest of them to the bound at the moment it decides. TEAM was public and is
// private now, its list naming the fixture's operator alone; each reader is handed TEAM's public
// answer aged past the bound, asks Dispatch afresh, and decides on what Dispatch holds now.
// The last row is a TEAM still public: its aged answer is asked again and stands, so the bound
// refuses nothing Dispatch still allows.
func TestEveryDecisionOnAnAgedProjectFactIsMadeOnAFreshOne(t *testing.T) {
	const reason = "TEAM-7: rotate the shared key"
	// pendingShared is a request for SHARED_KEY, which anyone may decide, whose reason names
	// TEAM-7, made while TEAM was public by a pod that names no person, so every signed-in
	// person's list reads it (everyonesPod); TEAM is then private, and the reader is handed the
	// aged public answer.
	pendingShared := func(t *testing.T, team projectaccesstest.Row) (*Machine, string, string, *agedFront) {
		t.Helper()
		m, _, _, dispatch := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
		pod, podKey := everyonesPod(t, m, "pod-p-shared", "")
		req, err := m.Create(t.Context(), pod, signRequest(t, m, podKey, reason, "SHARED_KEY"), "")
		if err != nil || req.State != "pending" || req.RecordID == nil {
			t.Fatalf("Create(SHARED_KEY) = %+v, %v; want it pending", req, err)
		}
		dispatch.SetRow(teamProject, team)
		front := &agedFront{inner: projectaccess.New(dispatch.URL, dispatch.Account, nil)}
		m.Projects = front
		return m, pod, *req.RecordID, front
	}
	handedAged := func(t *testing.T, front *agedFront) {
		t.Helper()
		if front.aged.Load() == 0 {
			t.Fatal("no reader was handed TEAM's aged answer: the test is blind")
		}
	}

	t.Run("the approval page", func(t *testing.T) {
		m, _, id, front := pendingShared(t, privateTeam(fixtureOperator))
		if err := m.HideUnseen(t.Context(), id, outsider); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("HideUnseen(%s) on TEAM's aged public answer = %v; want pgx.ErrNoRows: TEAM is private now", outsider, err)
		}
		handedAged(t, front)
	})
	t.Run("the approver's list", func(t *testing.T) {
		m, _, id, front := pendingShared(t, privateTeam(fixtureOperator))
		if listed := pendingRecords(t, m, outsider); slices.Contains(listed, id) {
			t.Errorf("PendingForApprover(%s) on TEAM's aged public answer = %v; want %s left out: TEAM is private now", outsider, listed, id)
		}
		handedAged(t, front)
	})
	t.Run("a person's decision", func(t *testing.T) {
		m, _, id, front := pendingShared(t, privateTeam(fixtureOperator))
		if decision, err := m.ApplyDecision(t.Context(), id, true, outsider); !errors.Is(err, pgx.ErrNoRows) || errors.Is(err, record.ErrNotApprover) {
			t.Errorf("ApplyDecision(%s) on TEAM's aged public answer = %+v, %v; want pgx.ErrNoRows alone, as for an id no record has: TEAM is private now", outsider, decision, err)
		}
		handedAged(t, front)
	})
	t.Run("the judge's project check", func(t *testing.T) {
		m, enr, _, front := pendingShared(t, privateTeam(fixtureOperator))
		if refer, err := m.JudgeProject(t.Context(), enr, "SHARED_KEY", reason); err != nil || !refer {
			t.Errorf("JudgeProject on TEAM's aged public answer = %v, %v; want the request left to a person: TEAM is private now", refer, err)
		}
		handedAged(t, front)
	})
	t.Run("the release", func(t *testing.T) {
		m, enr, key, dispatch := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
		_, grantID := grantOf(t, m, enr, key, "read it", "TEAM_TOKEN")
		dispatch.SetRow(teamProject, privateTeam(fixtureOperator))
		front := &agedFront{inner: projectaccess.New(dispatch.URL, dispatch.Account, nil)}
		m.Projects = front
		if values, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); !errors.Is(err, errProjectNotNamed) {
			t.Errorf("Values on TEAM's aged public state = %+v, %v; want errProjectNotNamed: TEAM is private now, and the session does not name it", values, err)
		}
		handedAged(t, front)
	})
	t.Run("the airlock's channel limit", func(t *testing.T) {
		m, enr, key, dispatch := newProjectFixture(t, bound(rule(slackToken, "slack")), publicTeam)
		nameProjects(t, m, enr, teamProject)
		_, grantID := grantOf(t, m, enr, key, "post the release notes", slackToken)
		checked, err := m.checkRelease(t.Context(), grantID, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if checked.facts.states[teamProject] != policy.ProjectPublic || checked.facts.asked.IsZero() {
			t.Fatalf("the release's facts = %+v; want TEAM public, asked of Dispatch at a time of its own", checked.facts)
		}
		// The values' reads took longer than the bound: the release's own answers, TEAM public
		// among them, are now that old, and TEAM has turned private since.
		checked.facts.asked = time.Now().Add(-projectaccess.CacheTTL - time.Second)
		team := privateTeam(fixtureOperator)
		team.SlackChannels = []string{"C0TEAMONLY"}
		dispatch.SetRow(teamProject, team)
		channels, limited := m.channelLimit(t.Context(), checked)
		if !limited || !slices.Equal(channels, []string{"C0TEAMONLY"}) {
			t.Errorf("channelLimit on facts that aged with TEAM public = %#v, limited %v; want limited to TEAM's own channels: TEAM is private now", channels, limited)
		}
	})
	t.Run("an aged answer Dispatch still gives stands", func(t *testing.T) {
		m, _, id, front := pendingShared(t, publicTeam)
		if err := m.HideUnseen(t.Context(), id, outsider); err != nil {
			t.Errorf("HideUnseen(%s) on TEAM's aged public answer while TEAM is still public = %v; want nil", outsider, err)
		}
		handedAged(t, front)
	})
}

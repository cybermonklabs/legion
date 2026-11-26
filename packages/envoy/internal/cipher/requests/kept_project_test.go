package requests

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// projectUnchecked is the broker_project_visibility_unchecked_total sample labelled kept.
func (p keptProcess) projectUnchecked(t *testing.T, kept string) int {
	t.Helper()
	return p.sample(t, "broker_project_visibility_unchecked_total", kept)
}

// TestWhileDispatchCannotBeAskedARowIsShownByTheKeptProjectAnswer is the person rule (maySee) on
// a Dispatch client that keeps each answer in the machine's own store, as the broker's does. Four
// pending requests for SHARED_KEY, from pods that name no person so every signed-in person's list
// reads them (everyonesPod), name in their reasons TEAM, public; PRIV, whose list names insider
// and not outsider; "SHA-256", a key of Dispatch's form that is no project; and, made only once
// Dispatch is down, OTHER, a private project whose list names insider, which nobody has asked
// about. Both people read their lists while Dispatch answers, and the answers are kept.
//
// Once Dispatch is down, a client of a broker process restarted since reads the kept answers:
// insider's list holds the TEAM, PRIV and SHA-256 rows and not OTHER's, never asked; outsider's
// holds TEAM's and SHA-256's and not PRIV's, kept hidden from him, nor OTHER's; insider approves
// PRIV's request; and outsider's approval of it is ErrProjectAccessUnavailable, leaving it
// pending, as every such refusal is while Dispatch is down. Meanwhile TEAM turns private, listing
// neither: insider's list still holds its row until Dispatch answers, and once it has, the row is
// gone, and stays gone when Dispatch stops answering again.
//
// Controls: on a client with no keeper insider's list holds none of the four rows while Dispatch
// is down, so the outage is real; and the request whose reason names no key is listed to both
// wherever their kept member kinds serve them. The fault: maySee reading without OrKept; insider's
// list holds none of the rows that name a project.
func TestWhileDispatchCannotBeAskedARowIsShownByTheKeptProjectAnswer(t *testing.T) {
	ctx := context.Background()
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	rows := dispatchRows(publicTeam)
	rows["OTHER"] = projectaccesstest.Row{Visibility: "private", Owner: otherPerson, Listed: []string{insider}}
	dispatch := projectaccesstest.NewServer(t, rows, map[string]string{
		insider: projectaccess.MemberFull, outsider: projectaccess.MemberFull, fixtureOperator: projectaccess.MemberFull,
	})
	ask := func(name, reason string) string {
		t.Helper()
		pod, key := everyonesPod(t, m, name, "")
		req, err := m.Create(ctx, pod, signRequest(t, m, key, reason, "SHARED_KEY"), "")
		if err != nil || req.RecordID == nil {
			t.Fatalf("Create(SHARED_KEY, %q) = %+v, %v; want pending", reason, req, err)
		}
		return *req.RecordID
	}
	team := ask("pod-k-team", "TEAM-1: rotate the team key")
	priv := ask("pod-k-priv", "PRIV-2: rotate the deploy key")
	hashed := ask("pod-k-hash", "check the SHA-256 of the bundle")
	plain := ask("pod-k-plain", "no issue named")
	listed := func(login string) []string {
		t.Helper()
		pending, err := m.PendingForApprover(ctx, login, PendingStates{})
		if err != nil {
			t.Fatalf("PendingForApprover(%s): %v", login, err)
		}
		var ids []string
		for _, p := range pending {
			ids = append(ids, p.RecordID)
		}
		return ids
	}
	requireListed := func(login, when string, want, not []string) {
		t.Helper()
		got := listed(login)
		for _, id := range want {
			if !slices.Contains(got, id) {
				t.Errorf("%s's list %s = %v; want %s", login, when, got, id)
			}
		}
		for _, id := range not {
			if slices.Contains(got, id) {
				t.Errorf("%s's list %s = %v; want no %s", login, when, got, id)
			}
		}
	}

	m.Projects = newKeptProcess(dispatch, m.Store).client
	requireListed(insider, "while Dispatch answers", []string{team, priv, hashed, plain}, nil)
	requireListed(outsider, "while Dispatch answers", []string{team, hashed, plain}, []string{priv})
	dispatch.SetDown(true)
	t.Cleanup(func() { dispatch.SetDown(false) })
	other := ask("pod-k-other", "OTHER-3: a project nobody asked about")

	m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
	requireListed(insider, "on a client with no keeper while Dispatch is down", nil, []string{team, priv, hashed, other})

	after := newKeptProcess(dispatch, m.Store)
	m.Projects = after.client
	requireListed(insider, "while Dispatch is down", []string{team, priv, hashed, plain}, []string{other})
	requireListed(outsider, "while Dispatch is down", []string{team, hashed, plain}, []string{priv, other})
	for kept, want := range map[string]int{"public": 2, "visible": 1, "missing": 2, "not_visible": 1, "none": 2} {
		if got := after.projectUnchecked(t, kept); got != want {
			t.Errorf(`broker_project_visibility_unchecked_total{kept=%q} after both lists = %d; want %d`, kept, got, want)
		}
	}
	if decision, err := m.ApplyDecision(ctx, priv, true, outsider); !errors.Is(err, ErrProjectAccessUnavailable) {
		t.Fatalf("an approval of PRIV's request by %s while Dispatch is down = %+v, %v; want ErrProjectAccessUnavailable", outsider, decision, err)
	}
	if decision, err := m.ApplyDecision(ctx, priv, true, insider); err != nil || decision.GrantID == "" {
		t.Fatalf("an approval of PRIV's request by %s while Dispatch is down = %+v, %v; want a grant", insider, decision, err)
	}

	dispatch.SetRow(teamProject, privateTeam())
	m.Projects = newKeptProcess(dispatch, m.Store).client
	requireListed(insider, "while Dispatch is down, TEAM turned private since", []string{team}, nil)
	dispatch.SetDown(false)
	m.Projects = newKeptProcess(dispatch, m.Store).client
	requireListed(insider, "once Dispatch answers TEAM private", []string{hashed, plain}, []string{team})
	dispatch.SetDown(true)
	m.Projects = newKeptProcess(dispatch, m.Store).client
	requireListed(insider, "while Dispatch is down again", []string{hashed, plain}, []string{team})
}

// TestAKeptNoSuchProjectShowsNoRowOnceTheProjectIsMade is the person rule (maySee) on a project
// made after Dispatch answered that it holds no such project. A pending request for SHARED_KEY,
// from a pod that names no person so every signed-in person's list reads it, names ACME-3 in its
// reason before ACME exists; both people's lists, read while Dispatch answers, keep Dispatch's
// answer that it holds no ACME, kept for everyone. ACME is then made private, its list naming
// insider, and insider's list, read while Dispatch answers, keeps her answer. Once Dispatch is
// down, a broker process restarted since does not show outsider, who has read nothing of ACME since
// it was made, the row, and his approval of it is ErrProjectAccessUnavailable, leaving it pending;
// insider is shown it and approves it.
//
// Control: while Dispatch is down before ACME is made, outsider's list holds the row on the kept no
// project, so the row's later absence rests on insider's answer and not on the outage alone.
func TestAKeptNoSuchProjectShowsNoRowOnceTheProjectIsMade(t *testing.T) {
	ctx := context.Background()
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	dispatch := projectaccesstest.NewServer(t, dispatchRows(publicTeam), map[string]string{
		insider: projectaccess.MemberFull, outsider: projectaccess.MemberFull, fixtureOperator: projectaccess.MemberFull,
	})
	pod, key := everyonesPod(t, m, "pod-k-acme", "")
	req, err := m.Create(ctx, pod, signRequest(t, m, key, "ACME-3: rotate the new project's key", "SHARED_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY, ACME-3) = %+v, %v; want pending", req, err)
	}
	acme := *req.RecordID
	listed := func(login, when string, want bool) {
		t.Helper()
		pending, err := m.PendingForApprover(ctx, login, PendingStates{})
		if err != nil {
			t.Fatalf("PendingForApprover(%s) %s: %v", login, when, err)
		}
		got := slices.ContainsFunc(pending, func(p PendingSummary) bool { return p.RecordID == acme })
		if got != want {
			t.Fatalf("%s's list %s holds ACME's row: %t; want %t", login, when, got, want)
		}
	}

	m.Projects = newKeptProcess(dispatch, m.Store).client
	listed(insider, "while Dispatch answers, before ACME is made", true)
	listed(outsider, "while Dispatch answers, before ACME is made", true)
	dispatch.SetDown(true)
	t.Cleanup(func() { dispatch.SetDown(false) })
	m.Projects = newKeptProcess(dispatch, m.Store).client
	listed(outsider, "while Dispatch is down, before ACME is made", true)

	dispatch.SetDown(false)
	dispatch.SetRow("ACME", projectaccesstest.Row{Visibility: "private", Owner: otherPerson, Listed: []string{insider}})
	m.Projects = newKeptProcess(dispatch, m.Store).client
	listed(insider, "once ACME is made", true)
	dispatch.SetDown(true)
	after := newKeptProcess(dispatch, m.Store)
	m.Projects = after.client
	listed(outsider, "while Dispatch is down, ACME made since", false)
	listed(insider, "while Dispatch is down, ACME made since", true)
	if got := after.projectUnchecked(t, "none"); got != 1 {
		t.Errorf(`broker_project_visibility_unchecked_total{kept="none"} after outsider's list = %d; want 1`, got)
	}
	if decision, err := m.ApplyDecision(ctx, acme, true, outsider); !errors.Is(err, ErrProjectAccessUnavailable) {
		t.Fatalf("an approval of ACME's request by %s while Dispatch is down = %+v, %v; want ErrProjectAccessUnavailable", outsider, decision, err)
	}
	if decision, err := m.ApplyDecision(ctx, acme, true, insider); err != nil || decision.GrantID == "" {
		t.Fatalf("an approval of ACME's request by %s while Dispatch is down = %+v, %v; want a grant", insider, decision, err)
	}
}

// TestAKeptPublicShowsNoRowOnceTheProjectIsMadePrivate is the person rule (maySee) on a project
// made private after both people's public answers were kept. A pending request for SHARED_KEY,
// from a pod that names no person so every signed-in person's list reads it, names PUBP-3 in its
// reason while PUBP is public; both people's lists, read while Dispatch answers, keep PUBP public.
// PUBP is then made private, its list naming insider, and insider's list, read while Dispatch
// answers, keeps her answer. Once Dispatch is down, a broker process restarted since does not show
// outsider, who has read nothing of PUBP since it was made private, the row, and his approval of it
// is ErrProjectAccessUnavailable, leaving it pending; insider is shown it and approves it.
//
// Control: while Dispatch is down before PUBP is made private, outsider's list holds the row on his
// kept public answer, so the row's later absence rests on insider's answer and not on the outage
// alone.
func TestAKeptPublicShowsNoRowOnceTheProjectIsMadePrivate(t *testing.T) {
	ctx := context.Background()
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	rows := dispatchRows(publicTeam)
	rows["PUBP"] = projectaccesstest.Row{Visibility: "public", Owner: otherPerson}
	dispatch := projectaccesstest.NewServer(t, rows, map[string]string{
		insider: projectaccess.MemberFull, outsider: projectaccess.MemberFull, fixtureOperator: projectaccess.MemberFull,
	})
	pod, key := everyonesPod(t, m, "pod-k-pubp", "")
	req, err := m.Create(ctx, pod, signRequest(t, m, key, "PUBP-3: rotate the project's key", "SHARED_KEY"), "")
	if err != nil || req.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY, PUBP-3) = %+v, %v; want pending", req, err)
	}
	pubp := *req.RecordID
	listed := func(login, when string, want bool) {
		t.Helper()
		pending, err := m.PendingForApprover(ctx, login, PendingStates{})
		if err != nil {
			t.Fatalf("PendingForApprover(%s) %s: %v", login, when, err)
		}
		got := slices.ContainsFunc(pending, func(p PendingSummary) bool { return p.RecordID == pubp })
		if got != want {
			t.Fatalf("%s's list %s holds PUBP's row: %t; want %t", login, when, got, want)
		}
	}

	m.Projects = newKeptProcess(dispatch, m.Store).client
	listed(insider, "while Dispatch answers, PUBP public", true)
	listed(outsider, "while Dispatch answers, PUBP public", true)
	dispatch.SetDown(true)
	t.Cleanup(func() { dispatch.SetDown(false) })
	m.Projects = newKeptProcess(dispatch, m.Store).client
	listed(outsider, "while Dispatch is down, PUBP public", true)

	dispatch.SetDown(false)
	dispatch.SetRow("PUBP", projectaccesstest.Row{Visibility: "private", Owner: otherPerson, Listed: []string{insider}})
	m.Projects = newKeptProcess(dispatch, m.Store).client
	listed(insider, "once PUBP is made private", true)
	dispatch.SetDown(true)
	after := newKeptProcess(dispatch, m.Store)
	m.Projects = after.client
	listed(outsider, "while Dispatch is down, PUBP made private since", false)
	listed(insider, "while Dispatch is down, PUBP made private since", true)
	if got := after.projectUnchecked(t, "none"); got != 1 {
		t.Errorf(`broker_project_visibility_unchecked_total{kept="none"} after outsider's list = %d; want 1`, got)
	}
	if decision, err := m.ApplyDecision(ctx, pubp, true, outsider); !errors.Is(err, ErrProjectAccessUnavailable) {
		t.Fatalf("an approval of PUBP's request by %s while Dispatch is down = %+v, %v; want ErrProjectAccessUnavailable", outsider, decision, err)
	}
	if decision, err := m.ApplyDecision(ctx, pubp, true, insider); err != nil || decision.GrantID == "" {
		t.Fatalf("an approval of PUBP's request by %s while Dispatch is down = %+v, %v; want a grant", insider, decision, err)
	}
}

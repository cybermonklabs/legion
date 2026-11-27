// packages/envoy/internal/cipher/requests/judge_alone_test.go
// Who decides a request is a setting of its keys: nobody for a key granted at once, the model
// judge for a key whose judge tag gives the judge seconds of it, and a person for any other key and every
// administrator's. A request the judge decides is held for it (requests.judge_until): it is on no
// list, no person may decide it, and the judge writes its verdict only while the hold lasts.
// Anything the judge cannot decide goes to the key's person, once.
package requests

import (
	"crypto/ecdsa"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// listedFor is the record ids GET /v1/pending lists for login. It reads both of
// PendingForApprover's statements, the one with no state and the one for the pending state, and
// fails when they list different records: the judge's hold keeps a record off both.
func listedFor(t *testing.T, m *Machine, login string) []string {
	t.Helper()
	read := func(states PendingStates) []string {
		pending, err := m.PendingForApprover(t.Context(), login, states)
		if err != nil {
			t.Fatalf("PendingForApprover(%s, %+v): %v", login, states, err)
		}
		ids := make([]string, len(pending))
		for i, p := range pending {
			ids[i] = p.RecordID
		}
		return ids
	}
	ids, byState := read(PendingStates{}), read(PendingStates{Pending: true})
	if !slices.Equal(ids, byState) {
		t.Fatalf("PendingForApprover(%s) lists %v with no state and %v for the pending state; want the same records", login, ids, byState)
	}
	return ids
}

// listedAs is the state the stateful list (Dispatch's, pending and expired) gives recordID for
// login, and "" where it does not list it.
func listedAs(t *testing.T, m *Machine, login, recordID string) string {
	t.Helper()
	pending, err := m.PendingForApprover(t.Context(), login, PendingStates{Pending: true, Expired: true})
	if err != nil {
		t.Fatalf("PendingForApprover(%s, pending and expired): %v", login, err)
	}
	for _, p := range pending {
		if p.RecordID == recordID {
			return p.State
		}
	}
	return ""
}

// TestTheJudgesHoldIsARecordStateEveryListReads: recordStateSQL, the one rule Dispatch's list,
// Dismiss and ReadRecord read, applies the judge's hold. A request the judge holds before its
// deadline is on no list, pending or expired, and cannot be dismissed, since it may still be
// decided; once the hold ends before the deadline it is listed pending; a request still held past
// its deadline is listed expired, as every request past its deadline is, and its decision stays
// refused.
func TestTheJudgesHoldIsARecordStateEveryListReads(t *testing.T) {
	t.Run("held before its deadline, then the hold ends", func(t *testing.T) {
		m, enr, key := newJudgeFixture(t)
		req := pendingJudgeable(t, m, enr, key, "post the release notes", 600, "DEEL_API_KEY")
		if got := listedAs(t, m, fixtureOperator, *req.RecordID); got != "" {
			t.Errorf("a request the judge holds is listed %q on Dispatch's list; want it on no list", got)
		}
		if slices.Contains(listedFor(t, m, fixtureOperator), *req.RecordID) {
			t.Error("a request the judge holds is on the person's pending list; want it on none")
		}
		if _, err := m.Dismiss(t.Context(), *req.RecordID, fixtureOperator); !errors.Is(err, ErrNotExpired) {
			t.Errorf("Dismiss of a request the judge holds = %v; want ErrNotExpired, since it may still be decided", err)
		}
		if detail, err := m.ReadRecord(t.Context(), *req.RecordID); err != nil || detail.State != "pending" || !detail.JudgeDeciding {
			t.Errorf("ReadRecord of a request the judge holds = %q judge_deciding %v, %v; want pending with the judge deciding", detail.State, detail.JudgeDeciding, err)
		}
		endHold(t, m, req.ID)
		if got := listedAs(t, m, fixtureOperator, *req.RecordID); got != "pending" {
			t.Errorf("a request whose hold ended before its deadline is listed %q; want pending", got)
		}
	})
	t.Run("held past its deadline", func(t *testing.T) {
		m, enr, key := newJudgeFixture(t)
		req := pendingJudgeable(t, m, enr, key, "post the release notes", 600, "DEEL_API_KEY")
		if _, err := m.Store.Pool.Exec(t.Context(), `update requests set pending_expires_at = now() - interval '1 second',
			judge_until = now() + interval '1 minute' where id=$1`, req.ID); err != nil {
			t.Fatalf("move the deadline into the past with the hold ahead: %v", err)
		}
		if got := listedAs(t, m, fixtureOperator, *req.RecordID); got != "expired" {
			t.Errorf("a request held past its deadline is listed %q; want expired", got)
		}
		if _, err := m.ApplyDecision(t.Context(), *req.RecordID, true, fixtureOperator); err == nil {
			t.Error("a person's approval of a request held past its deadline was made; want it refused")
		}
		if got := readDecided(t, m, *req.RecordID); got.events != 0 {
			t.Errorf("the refused approval left %+v; want no event", got)
		}
	})
}

// judgeHold is how long after the request was written its hold runs, and false for a request
// with no hold at all.
func judgeHold(t *testing.T, m *Machine, requestID string) (time.Duration, bool) {
	t.Helper()
	var seconds *float64
	if err := m.Store.Pool.QueryRow(t.Context(),
		`select extract(epoch from (judge_until - created_at)) from requests where id=$1`, requestID).Scan(&seconds); err != nil {
		t.Fatalf("read the judge's hold: %v", err)
	}
	if seconds == nil {
		return 0, false
	}
	return time.Duration(*seconds * float64(time.Second)), true
}

// endHold moves a request's hold into the past, as two minutes passing with no verdict does.
func endHold(t *testing.T, m *Machine, requestID string) {
	t.Helper()
	if _, err := m.Store.Pool.Exec(t.Context(), `update requests set judge_until = now() - interval '1 second' where id=$1`, requestID); err != nil {
		t.Fatalf("end the hold: %v", err)
	}
}

// handOver is a run that could not decide recordID handing it to its person, which a test of a
// person's decision of a judge key's request does first.
func handOver(t *testing.T, m *Machine, recordID string) {
	t.Helper()
	if _, err := m.LogJudged(t.Context(), recordID, judgeModel, map[string]any{"verdict": "deny", "error": "the model did not answer"}); err != nil {
		t.Fatalf("the hand-over: %v", err)
	}
}

// TestWhoDecidesIsTheKeysSetting: the broker reads who decides from the request's keys when it
// writes the request. A request whose every key that needs a decision is a judge key is held for
// the judge and handed to it, naming those keys as the ones it decides — a key the session gets
// at once beside them needs no decision and is no key the judge decides; a request with any key
// a person decides — one with no judge tag, an administrator's — is that person's from the
// start and never handed to the judge; and on a broker that runs no judge, a judge key's request
// is the person's too. No reason cites anything in any row.
func TestWhoDecidesIsTheKeysSetting(t *testing.T) {
	for _, c := range []struct {
		name    string
		names   []string
		noJudge bool
		held    bool
		decided []string // the keys the judge is handed as the ones it decides
		viewer  string
	}{
		{name: "a judge key", names: []string{"DEEL_API_KEY"}, held: true, decided: []string{"DEEL_API_KEY"}, viewer: fixtureOperator},
		{name: "two judge keys", names: []string{"DEEL_API_KEY", "DEEL_OTHER_KEY"}, held: true, decided: []string{"DEEL_API_KEY", "DEEL_OTHER_KEY"}, viewer: fixtureOperator},
		{name: "a judge key beside an agent-tier key its session gets at once", names: []string{"DEEL_API_KEY", "AGENT_KEY"}, held: true, decided: []string{"DEEL_API_KEY"}, viewer: fixtureOperator},
		{name: "a key with no judge tag", names: []string{"UNJUDGED_KEY"}, viewer: fixtureOperator},
		{name: "a judge key beside a key with no judge tag", names: []string{"DEEL_API_KEY", "UNJUDGED_KEY"}, viewer: fixtureOperator},
		{name: "an administrator's key with a judge tag", names: []string{adminSecretName}, viewer: fixtureOperator},
		{name: "a judge key on a broker that runs no judge", names: []string{"DEEL_API_KEY"}, noJudge: true, viewer: fixtureOperator},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key := newJudgeFixture(t)
			retag(t, m, policytest.Secret("UNJUDGED_KEY", fixtureOperator, policy.TierHuman, "unjudged-v1"), adminSecret(),
				policytest.Judged(policytest.Secret("DEEL_OTHER_KEY", fixtureOperator, policy.TierHuman, "other-v1"), 900, "grant for the release job"),
				policytest.Secret("AGENT_KEY", fixtureOperator, policy.TierAgent, "agent-v1"))
			var handed []PendingRequest
			m.Judge = func(p PendingRequest) { handed = append(handed, p) }
			if c.noJudge {
				m.Judge = nil
			}
			req := pendingJudgeable(t, m, enr, key, "post the release notes", 600, c.names...)

			if req.JudgeDeciding != c.held {
				t.Errorf("Create answers JudgeDeciding %v; want %v", req.JudgeDeciding, c.held)
			}
			if got, err := m.Get(t.Context(), req.ID); err != nil || got.JudgeDeciding != c.held {
				t.Errorf("Get answers JudgeDeciding %v (%v); want %v", got.JudgeDeciding, err, c.held)
			}
			hold, holds := judgeHold(t, m, req.ID)
			if holds != c.held || (c.held && (hold < JudgeHold-time.Second || hold > JudgeHold+time.Second)) {
				t.Errorf("the judge's hold is %s (set %v); want %v and, when set, %s", hold, holds, c.held, JudgeHold)
			}
			if want := map[bool]int{true: 1, false: 0}[c.held]; len(handed) != want {
				t.Errorf("the request was handed to the judge %d times; want %d", len(handed), want)
			} else if c.held && !slices.Equal(handed[0].Decided, c.decided) {
				t.Errorf("the judge was handed %v as the keys it decides; want %v", handed[0].Decided, c.decided)
			}
			if listed := slices.Contains(listedFor(t, m, c.viewer), *req.RecordID); listed == c.held {
				t.Errorf("GET /v1/pending for %s lists the record: %v; want %v", c.viewer, listed, !c.held)
			}
		})
	}
}

// TestTheJudgesGrantBesideAKeyGrantedAtOnceReleasesBoth: a request for a judge key and an
// agent-tier key its session gets at once is the judge's, and the judge's grant of it is a grant
// of both — admitted by record.MayJudge on the judge key alone, since the agent-tier key needed
// no decision, and by the chain check every release makes again. Both values are released.
func TestTheJudgesGrantBesideAKeyGrantedAtOnceReleasesBoth(t *testing.T) {
	m, enr, key := newJudgeFixture(t)
	retag(t, m, policytest.Secret("AGENT_KEY", fixtureOperator, policy.TierAgent, "agent-v1"))
	req := pendingJudgeable(t, m, enr, key, "post the release notes", 600, "DEEL_API_KEY", "AGENT_KEY")
	dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "grant"})
	if err != nil || dec.State != "granted" || dec.GrantID == "" {
		t.Fatalf("the judge's grant of a judge key beside an agent-tier key = %+v, %v; want it granted", dec, err)
	}
	release, err := m.Values(t.Context(), dec.GrantID, enr, ReleaseCall{})
	if err != nil || releasedValue(release, "DEEL_API_KEY") == "" || releasedValue(release, "AGENT_KEY") == "" {
		t.Fatalf("Values(the judge's grant) = %+v, %v; want both keys released", release, err)
	}
}

// TestAJudgeKeysLifetimeIsItsJudgeSeconds: a key with a judge tag grants at most the tag's
// seconds. A request that names no lifetime gets them, and one that asks for longer is refused
// when it is written, as a max-grant tag refuses one, naming the --ttl that would do; nothing is
// written for it and the judge is never asked.
func TestAJudgeKeysLifetimeIsItsJudgeSeconds(t *testing.T) {
	m, enr, key := newJudgeFixture(t)
	policytest.JudgeTags(t, fixtureStore(m), m.Policy, "DEEL_API_KEY", 300, "")
	handed := 0
	m.Judge = func(PendingRequest) { handed++ }

	compact, err := record.Sign(key, m.Audience, []record.AuthorizationDetail{{Type: record.KindAgentSecret, Identifier: "DEEL_API_KEY", Actions: []string{"inject"}}},
		"post the release notes", "", nil, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	req, err := m.Create(t.Context(), enr, compact, "")
	if err != nil || req.LifetimeSeconds == nil || *req.LifetimeSeconds != 300 {
		t.Fatalf("a request naming no lifetime = %+v, %v; want the key's judge seconds, 300", req, err)
	}

	_, err = m.Create(t.Context(), enr, signJudgeable(t, m, key, "post the release notes", 600, "DEEL_API_KEY"), "")
	if !errors.Is(err, ErrGrantRequiresLifetime) || handed != 1 {
		t.Fatalf("a request for 600 seconds of a key whose judge grants 300 = %v, handed %d times; want ErrGrantRequiresLifetime and no second judge run", err, handed)
	}
}

// TestTheJudgeSecondsAreTheDecidedKeysAndComeBeforeMaxGrant: the lifetime a request the judge
// holds gets is the shortest judge seconds of the keys it decides, read before the max-grant tags.
// A human-tier judge key whose max-grant is shorter than the fifteen-minute default, asked with no
// lifetime, gets its judge seconds rather than the max-grant's refusal; and a key the session gets
// at once beside it, whose own judge tag gives the judge fewer seconds, neither bounds the
// request nor is named in a refusal, since the judge decides nothing of it.
func TestTheJudgeSecondsAreTheDecidedKeysAndComeBeforeMaxGrant(t *testing.T) {
	unsigned := func(t *testing.T, m *Machine, key *ecdsa.PrivateKey, names ...string) string {
		t.Helper()
		details := make([]record.AuthorizationDetail, 0, len(names))
		for _, name := range names {
			details = append(details, record.AuthorizationDetail{Type: record.KindAgentSecret, Identifier: name, Actions: []string{"inject"}})
		}
		compact, err := record.Sign(key, m.Audience, details, "post the release notes", "", nil, time.Now())
		if err != nil {
			t.Fatalf("record.Sign: %v", err)
		}
		return compact
	}
	t.Run("a max-grant shorter than the default", func(t *testing.T) {
		m, enr, key := newJudgeFixture(t)
		capped := policytest.Judged(policytest.Secret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, "deel-v1"), 300, "")
		capped.Tags[policy.TagMaxGrant] = "10m"
		retag(t, m, capped)
		req, err := m.Create(t.Context(), enr, unsigned(t, m, key, "DEEL_API_KEY"), "")
		if err != nil || !req.JudgeDeciding || req.LifetimeSeconds == nil || *req.LifetimeSeconds != 300 {
			t.Fatalf("a request naming no lifetime for a judge key with judge 300 and max-grant 10m = %+v, %v; want it held for the judge, for 300 seconds", req, err)
		}
	})
	t.Run("a key got at once with fewer judge seconds", func(t *testing.T) {
		m, enr, key := newJudgeFixture(t)
		policytest.JudgeTags(t, fixtureStore(m), m.Policy, "DEEL_API_KEY", 600, "")
		retag(t, m, policytest.Judged(policytest.Secret("AGENT_KEY", fixtureOperator, policy.TierAgent, "agent-v1"), 60, ""))
		req, err := m.Create(t.Context(), enr, unsigned(t, m, key, "DEEL_API_KEY", "AGENT_KEY"), "")
		if err != nil || !req.JudgeDeciding || req.LifetimeSeconds == nil || *req.LifetimeSeconds != 600 {
			t.Fatalf("a request naming no lifetime for a judge key (600) beside a key got at once (60) = %+v, %v; want it held for the judge, for 600 seconds", req, err)
		}
		if err := m.Cancel(t.Context(), req.ID, enr); err != nil {
			t.Fatalf("cancel the first request: %v", err)
		}
		req, err = m.Create(t.Context(), enr, signJudgeable(t, m, key, "post the release notes", 300, "DEEL_API_KEY", "AGENT_KEY"), "")
		if err != nil || !req.JudgeDeciding || req.LifetimeSeconds == nil || *req.LifetimeSeconds != 300 {
			t.Fatalf("a request for 300 seconds of the same two keys = %+v, %v; want it held for the judge, for 300 seconds, with AGENT_KEY's 60 no bound", req, err)
		}
	})
}

// TestJudgeSecondsBoundOnlyARequestTheJudgeHolds: a key's judge seconds are the judge's bound,
// so they hold a request only when the judge decides it. A judge key beside a key with no judge
// tag is its person's request whole, and so is a judge key on a broker that runs no judge: each
// may ask for longer than the judge seconds, is written pending for its person with the lifetime
// it asked for, and is never handed to the judge.
func TestJudgeSecondsBoundOnlyARequestTheJudgeHolds(t *testing.T) {
	for _, c := range []struct {
		name    string
		names   []string
		noJudge bool
	}{
		{name: "a judge key beside a key with no judge tag", names: []string{"DEEL_API_KEY", "UNJUDGED_KEY"}},
		{name: "a judge key on a broker that runs no judge", names: []string{"DEEL_API_KEY"}, noJudge: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key := newJudgeFixture(t)
			policytest.JudgeTags(t, fixtureStore(m), m.Policy, "DEEL_API_KEY", 300, "")
			retag(t, m, policytest.Secret("UNJUDGED_KEY", fixtureOperator, policy.TierHuman, "unjudged-v1"))
			handed := 0
			m.Judge = func(PendingRequest) { handed++ }
			if c.noJudge {
				m.Judge = nil
			}
			req, err := m.Create(t.Context(), enr, signJudgeable(t, m, key, "post the release notes", 600, c.names...), "")
			if err != nil || req.State != "pending" || req.JudgeDeciding || req.LifetimeSeconds == nil || *req.LifetimeSeconds != 600 {
				t.Fatalf("a person's request for 600 seconds beside a judge key whose judge grants 300 = %+v, %v; want it written pending for its person, for 600 seconds", req, err)
			}
			if handed != 0 {
				t.Errorf("the request was handed to the judge %d times; want none", handed)
			}
			if !slices.Contains(listedFor(t, m, fixtureOperator), *req.RecordID) {
				t.Errorf("GET /v1/pending for %s does not list the record; want its person's card", fixtureOperator)
			}
		})
	}
}

// TestAPersonCannotDecideWhileTheJudgeHolds: while the judge holds a request, its person's
// decision of it — approve or deny, from any page or route — is refused and writes nothing. Once
// the hold has ended, the same decision is the person's to make.
func TestAPersonCannotDecideWhileTheJudgeHolds(t *testing.T) {
	for _, approve := range []bool{true, false} {
		m, enr, key := newJudgeFixture(t)
		req := pendingJudgeable(t, m, enr, key, "post the release notes", 600, "DEEL_API_KEY")
		if _, err := m.ApplyDecision(t.Context(), *req.RecordID, approve, fixtureOperator); !errors.Is(err, ErrJudgeHolds) {
			t.Fatalf("ApplyDecision(approve=%v) while the judge holds = %v; want ErrJudgeHolds", approve, err)
		}
		if got := readDecided(t, m, *req.RecordID); got.events != 0 || got.state != "pending" {
			t.Errorf("the refused decision left %+v; want the request pending with no event", got)
		}
		endHold(t, m, req.ID)
		if _, err := m.ApplyDecision(t.Context(), *req.RecordID, approve, fixtureOperator); err != nil {
			t.Errorf("ApplyDecision(approve=%v) after the hold = %v; want the person's decision made", approve, err)
		}
	}
}

// TestTheJudgeWritesOnlyWhileItHolds: a hold that ended — two minutes with no verdict, or a run
// that handed the request over — ends the judge's part. Its late grant and its late refusal are
// both refused, and the request is on its person's list.
func TestTheJudgeWritesOnlyWhileItHolds(t *testing.T) {
	m, enr, key := newJudgeFixture(t)
	req := pendingJudgeable(t, m, enr, key, "post the release notes", 600, "DEEL_API_KEY")
	endHold(t, m, req.ID)
	if _, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, nil); !errors.Is(err, ErrJudgeHoldEnded) {
		t.Errorf("the judge's late grant = %v; want ErrJudgeHoldEnded", err)
	}
	if _, err := m.DenyJudged(t.Context(), *req.RecordID, judgeModel, nil, "refused"); !errors.Is(err, ErrJudgeHoldEnded) {
		t.Errorf("the judge's late refusal = %v; want ErrJudgeHoldEnded", err)
	}
	if got := readDecided(t, m, *req.RecordID); got.events != 0 || got.state != "pending" {
		t.Errorf("the late verdicts left %+v; want the request pending with no event", got)
	}
	if !slices.Contains(listedFor(t, m, fixtureOperator), *req.RecordID) {
		t.Error("a request whose hold ended is not on its person's list; want it there, once")
	}
}

// TestHandingOverEndsTheHold: a verdict the judge writes without deciding — a model that did not
// answer, an answer that does not parse, a read that failed — ends the hold in the same
// transaction as its row, so the request is on its person's list at once and theirs to decide.
func TestHandingOverEndsTheHold(t *testing.T) {
	m, enr, key := newJudgeFixture(t)
	req := pendingJudgeable(t, m, enr, key, "post the release notes", 600, "DEEL_API_KEY")
	if _, err := m.LogJudged(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "deny", "error": "the model did not answer"}); err != nil {
		t.Fatalf("LogJudged: %v", err)
	}
	if _, holds := judgeHold(t, m, req.ID); holds {
		t.Error("the hold is still set after the hand-over; want it cleared")
	}
	if got, _ := m.Get(t.Context(), req.ID); got.JudgeDeciding {
		t.Error("Get still answers JudgeDeciding after the hand-over")
	}
	if !slices.Contains(listedFor(t, m, fixtureOperator), *req.RecordID) {
		t.Error("a request handed over is not on its person's list")
	}
	if _, err := m.ApplyDecision(t.Context(), *req.RecordID, true, fixtureOperator); err != nil {
		t.Errorf("the person's decision after the hand-over = %v; want it made", err)
	}
}

// TestTheJudgeRefusesARequestItHolds: the judge's refusal of a request it holds is the request's
// own denial, written as a person's denial is — one terminal event, the request denied — but under
// the judge's actor, with no login, and with the sentence its agent reads in the decision's detail.
// The verdict goes in beside it, in the same transaction, saying nothing was applied.
func TestTheJudgeRefusesARequestItHolds(t *testing.T) {
	m, enr, key := newJudgeFixture(t)
	req := pendingJudgeable(t, m, enr, key, "post the release notes", 600, "DEEL_API_KEY")
	const why = "the model judge refused this request: the guidance asks for a post its owner approved. This request will not be granted. Tell whoever gave you this work."

	dec, err := m.DenyJudged(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "deny", "reason": "no approved post"}, why)
	if err != nil || dec.State != "denied" {
		t.Fatalf("DenyJudged = %+v, %v; want the request denied", dec, err)
	}
	got := readDecided(t, m, *req.RecordID)
	judge := record.JudgeActor(judgeModel)
	if got.events != 1 || got.login != nil || got.actor != judge || got.state != "denied" || got.decidedBy != judge || len(got.approvers) != 0 {
		t.Errorf("the refusal wrote %+v; want one denied event under %s with no login, the request denied by it and no grant", got, judge)
	}
	var detail string
	if err := m.Store.Pool.QueryRow(t.Context(), `select coalesce(decision_detail,'') from requests where id=$1`, req.ID).Scan(&detail); err != nil {
		t.Fatalf("read the decision detail: %v", err)
	}
	if detail != why {
		t.Errorf("the request's decision detail is %q; want the refusal's own words, %q", detail, why)
	}
	if judged := readJudged(t, m, *req.RecordID); len(judged) != 1 || judged[0].actor != judge || judged[0].applied != "false" {
		t.Errorf("the request.judged rows are %+v; want one, under the judge, applied false", judged)
	}
}

// TestTheJudgeDeniesNothingAPersonDecides: a refusal from the judge is for a request it holds and
// for no other. A key with no judge tag, and a judge key on a broker that ran no judge, are a
// person's to decide; the judge's verdict on them is never their end.
func TestTheJudgeDeniesNothingAPersonDecides(t *testing.T) {
	for _, c := range []struct {
		name    string
		secret  string
		noJudge bool
	}{
		{"a key with no judge tag", "UNJUDGED_KEY", false},
		{"a judge key on a broker that ran no judge", "DEEL_API_KEY", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key := newJudgeFixture(t)
			retag(t, m, policytest.Secret("UNJUDGED_KEY", fixtureOperator, policy.TierHuman, "unjudged-v1"))
			if c.noJudge {
				m.Judge = nil
			}
			req := pendingJudgeable(t, m, enr, key, "post the release notes", 600, c.secret)
			if _, err := m.DenyJudged(t.Context(), *req.RecordID, judgeModel, nil, "refused"); !errors.Is(err, ErrJudgeHoldEnded) {
				t.Fatalf("DenyJudged = %v; want ErrJudgeHoldEnded", err)
			}
			if got := readDecided(t, m, *req.RecordID); got.events != 0 || got.state != "pending" {
				t.Errorf("the refused denial left %+v; want the request pending with no event", got)
			}
		})
	}
}

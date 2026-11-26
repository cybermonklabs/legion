package requests

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// slowAccess answers each question takes later, or gives up when its context ends first, as a
// Dispatch that is slow to answer does. asked, when set, counts the questions it was asked.
type slowAccess struct {
	inner projectaccess.Access
	takes time.Duration
	asked *atomic.Int32
}

func (s slowAccess) wait(ctx context.Context) error {
	if s.asked != nil {
		s.asked.Add(1)
	}
	select {
	case <-time.After(s.takes):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s slowAccess) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	if err := s.wait(ctx); err != nil {
		return projectaccess.Answer{}, err
	}
	return s.inner.Access(ctx, project, person)
}

func (s slowAccess) MemberKind(ctx context.Context, person string) (string, error) {
	if err := s.wait(ctx); err != nil {
		return "", err
	}
	return s.inner.MemberKind(ctx, person)
}

func (s slowAccess) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	if err := s.wait(ctx); err != nil {
		return nil, err
	}
	return s.inner.States(ctx, projects)
}

// TestTheJudgedRowIsWrittenWhenTheDecisionsDispatchReadIsSlow is round 4's B8 on
// cybermonklabs/legion#4135, the reviewer's probe. The judge's approval asks Dispatch again
// before its transaction, and the judge writes its request.judged row on what is left of its run
// when that approval fails. With 2 seconds of the run left and a Dispatch that takes 3 to answer,
// the approval's reads end JudgeRowReserve before the run does (JudgeDispatch), so the approval
// fails while there is still time to write the row, and the row is written on the same context.
// The approval's first read is the session's person's member kind (restrictedOperator, asked
// before the project facts so they stay the last read before the transaction), so the read cut
// short is that one: a *projectaccess.MemberKindUncheckable whose failure is the bound's
// deadline. The control is a Dispatch that answers at once: the approval is granted, writing its
// own row, and the second row is the one written after it.
func TestTheJudgedRowIsWrittenWhenTheDecisionsDispatchReadIsSlow(t *testing.T) {
	// Not parallel: it holds the judge's row to JudgeRowReserve, the one second of a run no read of Dispatch may spend, a product latency the load of the package's parallel tests could push past (LogJudged on the run's context at judge_bound_test.go:109); the scan cannot see it, since the bound is a context's deadline, not a measured time.
	for _, c := range []struct {
		name    string
		takes   time.Duration
		granted bool
		rows    int
	}{
		{"a Dispatch slower than the run", 3 * time.Second, false, 1},
		{"a Dispatch that answers at once", 0, true, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m, enr, key := newJudgeFixture(t)
			answerFrom(t, m, judgeRows(publicTeam))
			const reason = "TEAM-7: rotate the deploy key"
			req := pendingJudgeable(t, m, enr, key, reason, 600, "DEEL_API_KEY")
			if private, err := m.JudgeProject(t.Context(), enr, "DEEL_API_KEY", reason); err != nil || private {
				t.Fatalf("JudgeProject = %v, %v; want false: the run's own check passed", private, err)
			}
			m.Projects = slowAccess{inner: m.Projects, takes: c.takes}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()

			dec, err := m.ApplyJudgeDecision(ctx, *req.RecordID, judgeModel, map[string]any{"verdict": "approve"})
			if granted := err == nil && dec.State == "granted"; granted != c.granted {
				t.Fatalf("the judge's approval = %+v, %v; want granted %v", dec, err, c.granted)
			}
			var uncheckable *projectaccess.MemberKindUncheckable
			if !c.granted && (!errors.As(err, &uncheckable) || !errors.Is(err, context.DeadlineExceeded)) {
				t.Errorf("the judge's approval failed with %v; want a *projectaccess.MemberKindUncheckable cut short by the bound's deadline: its first Dispatch read was cut short", err)
			}
			// The slow case writes the costliest row LogJudged writes, one whose applied is
			// unknown, so the time the approval left covers the read that resolves it too
			// (round 6's m-h). The fast case writes a referral's row, beside the approval's own.
			row := map[string]any{"verdict": "approve"}
			if !c.granted {
				row = map[string]any{"verdict": "approve", "applied": AppliedUnknown, "not_applied": err.Error()}
			}
			if _, err := m.LogJudged(ctx, *req.RecordID, judgeModel, row); err != nil {
				t.Errorf("LogJudged on the run's context = %v; want the row written in the time the approval left it", err)
			}
			if judged := readJudged(t, m, *req.RecordID); len(judged) != c.rows {
				t.Errorf("request.judged rows = %+v; want %d", judged, c.rows)
			}
		})
	}
}

// boundLog is askLog for the judge's path: every question asked of Dispatch, under which of the
// machine's judge methods, whether it was asked under JudgeDispatch's bound, and that bound's
// deadline, so two reads under one bound show the same deadline.
type boundLog struct {
	inner projectaccess.Access
	mu    sync.Mutex
	by    string
	asked []boundQuestion
}

type boundQuestion struct {
	by, question string
	bounded      bool
	deadline     time.Time
}

func (b *boundLog) during(by string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.by = by
}

func (b *boundLog) note(ctx context.Context, question string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	deadline, _ := ctx.Deadline()
	b.asked = append(b.asked, boundQuestion{by: b.by, question: question, bounded: UnderJudgeDispatch(ctx), deadline: deadline})
}

func (b *boundLog) questions() []boundQuestion {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]boundQuestion(nil), b.asked...)
}

func (b *boundLog) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	b.note(ctx, "access "+project)
	return b.inner.Access(ctx, project, person)
}

func (b *boundLog) MemberKind(ctx context.Context, person string) (string, error) {
	b.note(ctx, "member kind of "+person)
	return b.inner.MemberKind(ctx, person)
}

func (b *boundLog) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	b.note(ctx, "states of "+strings.Join(projects, " "))
	return b.inner.States(ctx, projects)
}

// teamJudgeReason is the reason of judgeHeldTeamRequest's request, naming TEAM's issue.
const teamJudgeReason = "TEAM-7: rotate the deploy key"

// judgeHeldTeamRequest is a machine that runs a model judge, its fixture operator's session, and
// that session's pending request for TEAM_HUMAN, a human-tier secret tagged TEAM that the judge
// may decide, for teamJudgeReason: a request each of the judge's methods asks Dispatch about. A
// broker that runs a judge holds a judge key's request for it (judge_until), and only a request
// the judge holds takes its approval.
func judgeHeldTeamRequest(t *testing.T) (m *Machine, enrollmentID string, req Request) {
	t.Helper()
	rules := proxyrules.Rules{Secrets: []proxyrules.SecretRule{{Name: "TEAM_HUMAN", Hosts: []string{"team.example"}, Judge: new(uint32(900))}}}
	m, enrollmentID, key, _ := newProjectFixture(t, rules, publicTeam)
	retag(t, m, taggedSecret("TEAM_HUMAN", fixtureOperator, policy.TierHuman, teamProject, "team-human-v1"))
	m.Chain = NewChainVerifier(m.Store, testAudience, time.Minute, m.Airlock)
	m.Judge = func(PendingRequest) {}
	return m, enrollmentID, pendingJudgeable(t, m, enrollmentID, key, teamJudgeReason, 600, "TEAM_HUMAN")
}

// TestNoDispatchReadOnTheJudgesPathSkipsItsBound is round 4's class test for B7 and B8 on
// cybermonklabs/legion#4135: every question the machine asks Dispatch for the model judge — its
// policy check (JudgePolicy), its project check (JudgeProject) and its approval's checks before
// the transaction (ApplyJudgeDecision) — is asked under JudgeDispatch, whatever context the
// caller passed, so no read of a judge's run can spend the time its row needs. The request is one
// each of them asks Dispatch about: a human-tier secret tagged TEAM, whose reason names TEAM's
// issue. The test lists each question, and a method that asked Dispatch nothing at all is a
// failure too, since a list without it cannot say whether its reads are bounded.
func TestNoDispatchReadOnTheJudgesPathSkipsItsBound(t *testing.T) {
	t.Parallel()
	m, enr, req := judgeHeldTeamRequest(t)
	log := &boundLog{inner: m.Projects}
	m.Projects = log

	log.during("JudgePolicy")
	if decision, _, _, err := m.JudgePolicy(t.Context(), enr, "TEAM_HUMAN"); err != nil || decision.Outcome != policy.Approval {
		t.Fatalf("JudgePolicy = %+v, %v; want an approval", decision, err)
	}
	log.during("JudgeProject")
	if private, err := m.JudgeProject(t.Context(), enr, "TEAM_HUMAN", teamJudgeReason); err != nil || private {
		t.Fatalf("JudgeProject = %v, %v; want false: TEAM is public", private, err)
	}
	log.during("JudgeRequestState")
	if state, err := m.JudgeRequestState(t.Context(), *req.RecordID); err != nil || state != "pending" {
		t.Fatalf("JudgeRequestState = %q, %v; want pending", state, err)
	}
	log.during("ApplyJudgeDecision")
	if dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve"}); err != nil || dec.State != "granted" {
		t.Fatalf("ApplyJudgeDecision = %+v, %v; want granted", dec, err)
	}

	asked := log.questions()
	t.Logf("the judge's path asked Dispatch: %+v", asked)
	for _, by := range []string{"JudgePolicy", "JudgeProject", "ApplyJudgeDecision"} {
		if !slices.ContainsFunc(asked, func(q boundQuestion) bool { return q.by == by }) {
			t.Errorf("%s asked Dispatch nothing; want at least one question, or this test cannot say its reads are bounded", by)
		}
	}
	for _, q := range asked {
		if !q.bounded {
			t.Errorf("%s asked %q outside JudgeDispatch's bound", q.by, q.question)
		}
	}
}

// TestAnApprovalAsksTheMemberKindFirstUnderTheDecisionsOneBound: an approval's member-kind read
// (restrictedOperator) is asked before the project facts the decision rests on, so the facts'
// atDecision read is the last read of Dispatch before the transaction and the facts are under
// projectaccess.CacheTTL old when the decision is made; and for the model judge every read of its
// approval shares one JudgeDispatch bound, the same deadline, never one bound after another.
func TestAnApprovalAsksTheMemberKindFirstUnderTheDecisionsOneBound(t *testing.T) {
	t.Parallel()
	m, _, req := judgeHeldTeamRequest(t)
	log := &boundLog{inner: m.Projects}
	m.Projects = log
	log.during("ApplyJudgeDecision")
	if dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve"}); err != nil || dec.State != "granted" {
		t.Fatalf("ApplyJudgeDecision = %+v, %v; want granted", dec, err)
	}
	asked := log.questions()
	if len(asked) < 2 {
		t.Fatalf("the judge's approval asked Dispatch %+v; want the member kind and the project facts", asked)
	}
	if !strings.HasPrefix(asked[0].question, "member kind of ") {
		t.Errorf("the judge's approval asked Dispatch %+v; want the member kind first, so the project facts are the last read before the transaction", asked)
	}
	for _, q := range asked[1:] {
		if strings.HasPrefix(q.question, "member kind of ") {
			t.Errorf("the judge's approval asked the member kind again after the project facts (%+v)", asked)
		}
	}
	for _, q := range asked {
		if q.deadline.IsZero() || !q.deadline.Equal(asked[0].deadline) {
			t.Errorf("the judge's approval asked %q under the deadline %v, the first under %v; want one JudgeDispatch bound for all its reads", q.question, q.deadline, asked[0].deadline)
		}
	}
}

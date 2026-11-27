// kept_member_kind_test.go holds the request machine's half of the rule that while Dispatch cannot
// be asked a person last answered a full member is served as one: the machine reads its person's
// member kind through a client that keeps each answer in the machine's own store
// (projectaccess.Client.Keep), as the broker's does, and each read below is made by a client of a
// broker process restarted since Dispatch went down, so the kept row, and nothing in memory,
// decides it.
package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// keptProcess is one broker process's Dispatch client, keeping in a store: the lines it logs and
// the registry it counts on. Its clock stands still, so every read it makes falls in one minute
// whatever the box's load, and it counts each person once.
type keptProcess struct {
	client   *projectaccess.Client
	logs     *logtest.Recorder
	registry *metrics.Registry
}

// newKeptProcess is a client of dispatch keeping its answers in st, as cmd/cipherd builds one.
func newKeptProcess(dispatch *projectaccesstest.Server, st *store.Store) keptProcess {
	p := keptProcess{client: projectaccess.New(dispatch.URL, dispatch.Account, nil), logs: &logtest.Recorder{}, registry: metrics.New()}
	now := time.Now()
	p.client.UseClock(func() time.Time { return now })
	p.client.Keep(st, slog.New(p.logs), p.registry)
	return p
}

// unchecked is the broker_member_kind_unchecked_total sample labelled kept, 0 when there is none.
func (p keptProcess) unchecked(t *testing.T, kept string) int {
	t.Helper()
	return p.sample(t, "broker_member_kind_unchecked_total", kept)
}

// sample is metric's sample labelled kept on p's registry, 0 when there is none.
func (p keptProcess) sample(t *testing.T, metric, kept string) int {
	t.Helper()
	recorder := httptest.NewRecorder()
	p.registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	series := metric + `{kept="` + kept + `"} `
	for sample := range strings.Lines(recorder.Body.String()) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(sample), series); ok {
			n, err := strconv.Atoi(value)
			if err != nil {
				t.Fatalf("the sample %q holds no count", sample)
			}
			return n
		}
	}
	return 0
}

// servedLines is how many KeptFullMessage lines p logged for person.
func (p keptProcess) servedLines(person string) int {
	n := 0
	for _, r := range p.logs.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if r.Message == projectaccess.KeptFullMessage && a.Key == "person" && a.Value.String() == person {
				n++
			}
			return true
		})
	}
	return n
}

// requireUncheckable fails t unless err is the *projectaccess.MemberKindUncheckable naming person
// that a read Dispatch did not answer is.
func requireUncheckable(t *testing.T, what string, err error, person string) {
	t.Helper()
	var refused *projectaccess.MemberKindUncheckable
	if !errors.As(err, &refused) || refused.Person != person || !projectaccess.Unanswered(err) {
		t.Fatalf("%s while Dispatch is down = %v; want MemberKindUncheckable naming %s, a read Dispatch did not answer", what, err, person)
	}
}

// TestWhileDispatchCannotBeAskedAPersonKeptFullIsServed: ines, whom Dispatch last answered a full
// member, is released the grant she holds, given and released a new shared agent-tier grant, and
// her grant's airlock connection opens, while Dispatch is down, with one WARN line and one count
// labelled full. outsider, last answered restricted, and uma, never read, are each refused their
// release and their ask (*projectaccess.MemberKindUncheckable), with nothing released and no
// request written, counted restricted and none: the uncheckable refusal, which the API answers
// 503 MEMBER_KIND_UNCHECKABLE.
//
// Control: a client with no keeper refuses ines's release while Dispatch is down, so the outage is
// real. The fault: MemberKind not reading the kept kind; ines's release is refused.
func TestWhileDispatchCannotBeAskedAPersonKeptFullIsServed(t *testing.T) {
	const (
		outsider = "outsider@outside.example"
		unasked  = "uma@example.com"
	)
	ctx := context.Background()
	m, _, _, _ := newFixture(t)
	retag(t, m,
		policytest.Secret("SHARED_TOKEN", policy.OwnerShared, policy.TierAgent, "shared-token-v1"),
		policytest.Secret("SHARED_OTHER", policy.OwnerShared, policy.TierAgent, "shared-other-v1"),
		policytest.Secret("SHARED_FRESH", policy.OwnerShared, policy.TierAgent, "shared-fresh-v1"))
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{
		fixtureOperator: projectaccess.MemberFull,
		outsider:        projectaccess.MemberRestricted,
		unasked:         projectaccess.MemberFull,
	})
	type session struct {
		id  string
		key *ecdsa.PrivateKey
	}
	open := func(person string) session {
		id, key := newEnrollment(t, m.Store, "session", person+"-box-"+t.Name(), new(person), nil)
		return session{id, key}
	}
	granted := func(t *testing.T, s session, name string) string {
		t.Helper()
		req, err := m.Create(ctx, s.id, signRequest(t, m, s.key, "needs "+name, name), "")
		if err != nil || req.State != "granted" || req.GrantID == nil {
			t.Fatalf("Create(%s) = %+v, %v; want it granted at once", name, req, err)
		}
		return *req.GrantID
	}
	released := func(t *testing.T, s session, grant, name, value, when string) {
		t.Helper()
		if release, err := m.Values(ctx, grant, s.id, ReleaseCall{}); err != nil || release.Values[name] != value {
			t.Fatalf("Values(%s) %s = %v, %v; want %q", name, when, release.Values, err, value)
		}
	}
	requests := func(s session) int {
		t.Helper()
		var n int
		if err := m.Store.Pool.QueryRow(ctx, `select count(*) from requests where enrollment_id=$1`, s.id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ines, outside, uma := open(fixtureOperator), open(outsider), open(unasked)
	// Each holds a grant made before the machine asked Dispatch anything.
	inesGrant, outsideGrant, umaGrant := granted(t, ines, "SHARED_TOKEN"), granted(t, outside, "SHARED_TOKEN"), granted(t, uma, "SHARED_TOKEN")

	// The process before the outage reads ines and outsider, and keeps what Dispatch answers.
	m.Projects = newKeptProcess(dispatch, m.Store).client
	released(t, ines, inesGrant, "SHARED_TOKEN", "shared-token-v1", "while Dispatch answers")
	if req, err := m.Create(ctx, outside.id, signRequest(t, m, outside.key, "needs it", "SHARED_FRESH"), ""); err != nil || req.State != "denied" {
		t.Fatalf("Create(SHARED_FRESH) under %s while Dispatch answers = %+v, %v; want it denied, a restricted member's", outsider, req, err)
	}

	dispatch.SetDown(true)
	t.Cleanup(func() { dispatch.SetDown(false) })
	m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
	if _, err := m.Values(ctx, inesGrant, ines.id, ReleaseCall{}); err == nil {
		t.Fatal("a client with no keeper released ines's grant while Dispatch is down: the outage is not real")
	}
	after := newKeptProcess(dispatch, m.Store)
	m.Projects = after.client

	t.Run("a person kept full is released and given what they ask for", func(t *testing.T) {
		released(t, ines, inesGrant, "SHARED_TOKEN", "shared-token-v1", "while Dispatch is down, kept full")
		grant := granted(t, ines, "SHARED_OTHER")
		released(t, ines, grant, "SHARED_OTHER", "shared-other-v1", "of a grant made while Dispatch is down")
		if counted, lines := after.unchecked(t, "full"), after.servedLines(fixtureOperator); counted != 1 || lines != 1 {
			t.Fatalf("serving %s while Dispatch is down counted %d and logged %d WARN lines; want 1 of each", fixtureOperator, counted, lines)
		}
	})

	for _, refused := range []struct {
		person string
		s      session
		grant  string
		kept   string
	}{{outsider, outside, outsideGrant, "restricted"}, {unasked, uma, umaGrant, "none"}} {
		t.Run("a person kept "+refused.kept+" is refused their release and their ask, with nothing written", func(t *testing.T) {
			before := requests(refused.s)
			release, err := m.Values(ctx, refused.grant, refused.s.id, ReleaseCall{})
			requireUncheckable(t, "Values(SHARED_TOKEN) under "+refused.person, err, refused.person)
			if len(release.Values) != 0 {
				t.Fatalf("the refused release under %s released %v; want nothing", refused.person, release.Values)
			}
			_, err = m.Create(ctx, refused.s.id, signRequest(t, m, refused.s.key, "needs it while Dispatch is down", "SHARED_OTHER"), "")
			requireUncheckable(t, "Create(SHARED_OTHER) under "+refused.person, err, refused.person)
			if n := requests(refused.s); n != before {
				t.Fatalf("the refused ask under %s left %d requests, want %d", refused.person, n, before)
			}
			if got := after.unchecked(t, refused.kept); got != 1 {
				t.Fatalf(`broker_member_kind_unchecked_total{kept=%q} after refusing %s twice = %d; want 1`, refused.kept, refused.person, got)
			}
		})
	}

	t.Run("the airlock connection of a person kept full opens", func(t *testing.T) {
		airlocked, _, grantID, release := twoNameGrant(t, "carried by the airlock")
		stand := projectaccesstest.NewServer(t, nil, map[string]string{fixtureOperator: projectaccess.MemberFull})
		airlocked.Projects = newKeptProcess(stand, airlocked.Store).client
		placeholder := release.Airlock.ProxyPassword
		if conn, err := airlocked.AirlockConnection(ctx, placeholder); err != nil || conn.GrantID != grantID {
			t.Fatalf("AirlockConnection while Dispatch answers = %+v, %v; want grant %s", conn, err, grantID)
		}
		stand.SetDown(true)
		airlocked.Projects = newKeptProcess(stand, airlocked.Store).client
		if conn, err := airlocked.AirlockConnection(ctx, placeholder); err != nil || conn.GrantID != grantID {
			t.Fatalf("AirlockConnection while Dispatch is down, kept full = %+v, %v; want grant %s", conn, err, grantID)
		}
	})
}

// TestAnApproverReadsTheKeptKindWhileDispatchCannotBeAsked holds the approver's reads, the pending
// list (PendingForApprover), the grant list (GrantsForApprover), the dismissal (Dismiss), the revoke
// (RevokeByApprover) and the approval page's own read (DecidesSecrets), to the kept kind while
// Dispatch is down: secondMember, a member of the group that owns TEAM_KEY whom Dispatch last
// answered a full member, is listed the group's pending and expired requests and its grant, decides
// on the page, dismisses the expired request and ends the grant, as while Dispatch answers;
// guestMember, a member of the same group last answered restricted, is listed none of them, and
// their dismissal and their page are refused *projectaccess.MemberKindUncheckable, with nothing
// written, as for a person of whom no kind is kept (TestAnApproverWhoseMemberKindCannotBeReadIsNoApprover).
//
// Control: the same reads of secondMember on a client with no keeper leave the group's rows off
// their list, so the outage is real. The fault: MemberKind not reading the kept kind; secondMember's
// list leaves the group's rows off.
func TestAnApproverReadsTheKeptKindWhileDispatchCannotBeAsked(t *testing.T) {
	rows := groupApproverRows(t)
	m := rows.m
	ctx := context.Background()
	// The process before the outage reads both members, and keeps what Dispatch answers.
	m.Projects = newKeptProcess(rows.dispatch, m.Store).client
	for _, person := range []string{secondMember, guestMember} {
		listedStates(t, m, person, bothStates)
	}
	rows.dispatch.SetDown(true)
	t.Cleanup(func() { rows.dispatch.SetDown(false) })
	groupRows := []string{listing(rows.pending, "pending"), listing(rows.expired, "expired")}
	m.Projects = projectaccess.New(rows.dispatch.URL, rows.dispatch.Account, nil)
	if got := listedStates(t, m, secondMember, bothStates); slices.Contains(got, groupRows[0]) {
		t.Fatalf("PendingForApprover(%s) on a client with no keeper while Dispatch is down = %v; want no group row: the outage is not real", secondMember, got)
	}
	after := newKeptProcess(rows.dispatch, m.Store)
	m.Projects = after.client

	t.Run("a member kept restricted is listed nothing and refused", func(t *testing.T) {
		got := listedStates(t, m, guestMember, bothStates)
		for _, row := range groupRows {
			if slices.Contains(got, row) {
				t.Errorf("PendingForApprover(%s) while Dispatch is down, kept restricted = %v; want no %s", guestMember, got, row)
			}
		}
		if listsGrant(t, m, guestMember, rows.grant) {
			t.Errorf("GrantsForApprover(%s) while Dispatch is down, kept restricted holds the group's grant %s", guestMember, rows.grant)
		}
		_, err := m.Dismiss(ctx, rows.expired, guestMember)
		requireUncheckable(t, "Dismiss by "+guestMember, err, guestMember)
		if n := dismissals(t, m, rows.expired); n != 0 {
			t.Fatalf("the refused dismissal wrote %d dismissals", n)
		}
		_, err = m.DecidesSecrets(ctx, guestMember)
		requireUncheckable(t, "DecidesSecrets of "+guestMember, err, guestMember)
		if got := after.unchecked(t, "restricted"); got != 1 {
			t.Fatalf(`broker_member_kind_unchecked_total{kept="restricted"} after %s's reads = %d; want 1`, guestMember, got)
		}
	})

	t.Run("a member kept full is listed, decides, dismisses and revokes as while Dispatch answers", func(t *testing.T) {
		got := listedStates(t, m, secondMember, bothStates)
		for _, row := range groupRows {
			if !slices.Contains(got, row) {
				t.Errorf("PendingForApprover(%s) while Dispatch is down, kept full = %v; want %s", secondMember, got, row)
			}
		}
		if !listsGrant(t, m, secondMember, rows.grant) {
			t.Errorf("GrantsForApprover(%s) while Dispatch is down, kept full does not hold the group's grant %s", secondMember, rows.grant)
		}
		if decides, err := m.DecidesSecrets(ctx, secondMember); err != nil || !decides {
			t.Fatalf("DecidesSecrets(%s) while Dispatch is down, kept full = %t, %v; want true", secondMember, decides, err)
		}
		if d, err := m.Dismiss(ctx, rows.expired, secondMember); err != nil || !d.Wrote {
			t.Fatalf("Dismiss by %s while Dispatch is down, kept full = %+v, %v; want it written", secondMember, d, err)
		}
		if err := m.RevokeByApprover(ctx, rows.grant, secondMember, record.HumanActor(secondMember)); err != nil {
			t.Fatalf("RevokeByApprover of the group's grant by %s while Dispatch is down, kept full = %v; want it ended", secondMember, err)
		}
		requireRevokedBy(t, m, rows.grant, secondMember)
		if counted, lines := after.unchecked(t, "full"), after.servedLines(secondMember); counted != 1 || lines != 1 {
			t.Fatalf("serving %s while Dispatch is down counted %d and logged %d WARN lines; want 1 of each", secondMember, counted, lines)
		}
	})
}

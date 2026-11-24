package requests_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// restrictedProfileFixture is newProfileFixture with Dispatch answering memberKind for the
// session's person: APP_KEY a human-tier profile GitHub serves, so a request a person decides
// gets the check mint, and OTHER_APP_KEY a profile the file refuses, its github block naming an
// account its source's github-owners tag does not.
func restrictedProfileFixture(t *testing.T, memberKind string) *profileFixture {
	t.Helper()
	f := newProfileFixture(t, map[string]int64{standInOwner: 7}, map[string]string{
		"APP_KEY":       `{"accounts": ["ada"], "repositories": ["ada/proofs"], "permissions": {"contents": "read", "metadata": "read"}}`,
		"OTHER_APP_KEY": `{"accounts": ["nobody-listed"], "permissions": {"metadata": "read"}}`,
	})
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{checkOperator: memberKind})
	f.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
	return f
}

// TestARestrictedMembersProfileRequestIsDeniedBeforeAnyCheckMintOrProfileAnswer: a session whose
// person Dispatch calls a restricted member asks for a human-tier GitHub profile, which a full
// member's request would put to its owner after the check mint, and for a profile the profiles
// file refuses. Each is denied restricted-member, with no approval record and no token request to
// GitHub: the check mint runs only for a request the policy sends to a person, and the policy
// sends a restricted member's to nobody. The refused profile is told restricted-member too, never
// the file's refusal, which would name its source and why it fails. The control is a full member
// of the same fixture, whose request for the profile is pending after one check mint and whose
// request for the refused profile is PROFILE_REFUSED.
func TestARestrictedMembersProfileRequestIsDeniedBeforeAnyCheckMintOrProfileAnswer(t *testing.T) {
	t.Run("the control: a full member", func(t *testing.T) {
		f := restrictedProfileFixture(t, projectaccess.MemberFull)
		if req, err := f.create(t, "APP_KEY"); err != nil || req.State != "pending" || req.RecordID == nil {
			t.Fatalf("Create(APP_KEY) for a full member = %+v, %v; want it pending on its approver", req, err)
		}
		if asked := f.stand.tokenRequests(); len(asked) != 1 {
			t.Fatalf("a full member's request made %d token requests; want the one check mint", len(asked))
		}
		if req, err := f.create(t, "OTHER_APP_KEY"); !errors.Is(err, policy.ErrProfileRefused) {
			t.Fatalf("Create(OTHER_APP_KEY) for a full member = %+v, %v; want ErrProfileRefused", req, err)
		}
	})
	t.Run("a restricted member", func(t *testing.T) {
		f := restrictedProfileFixture(t, projectaccess.MemberRestricted)
		for _, name := range []string{"APP_KEY", "OTHER_APP_KEY"} {
			req, err := f.create(t, name)
			if err != nil || req.State != "denied" || req.RecordID != nil || req.GrantID != nil || req.Detail == nil ||
				!strings.Contains(*req.Detail, name+": "+policy.ReasonRestrictedMember) || strings.Contains(*req.Detail, "profile") {
				t.Fatalf("Create(%s) for a restricted member = %+v, %v; want it denied naming %s and no profile", name, req, err, policy.ReasonRestrictedMember)
			}
		}
		if asked := f.stand.tokenRequests(); len(asked) != 0 {
			t.Fatalf("a restricted member's requests made %d token requests to GitHub; want none", len(asked))
		}
	})
}

// guestApprover is a person an access list admitted from outside the sign-in domain, whom
// Dispatch calls a restricted member in the rows below.
const guestApprover = "guest@outside.example"

// approverProfileFixture is newProfileFixture whose one stored secret, APP_KEY, a human-tier
// github-app secret, approver owns, served through two profiles: APP_KEY, of its source's own
// name, and APP_READ, a profile of APP_KEY under a name of its own. The session is
// checkOperator's, whom Dispatch calls a full member, so a request for either profile is pending
// on approver after the check mint. Dispatch answers approverKind for approver until a row
// changes it (projectaccesstest.Server.SetMemberKind), each change read by a client of its own.
func approverProfileFixture(t *testing.T, approver, approverKind string) (*profileFixture, *projectaccesstest.Server) {
	t.Helper()
	f := newProfileFixture(t, map[string]int64{standInOwner: 7}, nil)
	local := memory.New(policytest.TypeSecret("APP_KEY", approver, policy.TierHuman, policy.TypeGitHubApp, standInAppValue(t)))
	const block = `{"accounts": ["ada"], "permissions": {"contents": "read", "metadata": "read"}}`
	f.Policy = policytest.ProfiledCurrent(t, local, policytest.ProfilesFile(
		`{"name": "APP_KEY", "github": `+block+`}`,
		`{"name": "APP_READ", "source": "APP_KEY", "github": `+block+`}`,
	))
	f.Secrets = local
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{checkOperator: projectaccess.MemberFull, approver: approverKind})
	f.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
	return f, dispatch
}

// TestARestrictedMemberIsNoApproverOfAProfilesRequest: a profile is served from its source, and
// a request for it waits on its source's owner. A person Dispatch calls a restricted member, the
// owner of APP_KEY, is no approver of a request for either of its profiles, the one of its own
// name and the one of another name: their pending list holds neither request and their approval
// of it is refused requests.ErrRestrictedDecider, the request still pending; and a grant of the
// profile they approved while Dispatch called them a full member leaves their grant list, and
// their revoke of it is refused, once Dispatch calls them restricted. The control is a full
// member in the same place, whose list holds the request, whose approval grants it, and whose
// grant list and revoke reach the grant.
func TestARestrictedMemberIsNoApproverOfAProfilesRequest(t *testing.T) {
	for _, profile := range []string{"APP_KEY", "APP_READ"} {
		for _, who := range []struct {
			name, person string
			restricted   bool
		}{
			{"a restricted member", guestApprover, true},
			{"a full member", checkOutsider, false},
		} {
			t.Run(profile+", "+who.name, func(t *testing.T) {
				kind := projectaccess.MemberFull
				if who.restricted {
					kind = projectaccess.MemberRestricted
				}
				f, _ := approverProfileFixture(t, who.person, kind)
				req, err := f.create(t, profile)
				if err != nil || req.State != "pending" || req.RecordID == nil {
					t.Fatalf("Create(%s) = %+v, %v; want it pending on a record", profile, req, err)
				}
				recordID := *req.RecordID
				var approver string
				if err := f.Store.Pool.QueryRow(t.Context(), `select approver from credential_requests where id=$1`, recordID).Scan(&approver); err != nil || approver != who.person {
					t.Fatalf("the %s request's record waits on %q, %v; want %s, its source's owner", profile, approver, err, who.person)
				}

				listed, err := f.PendingForApprover(t.Context(), who.person, requests.PendingStates{Pending: true})
				if err != nil {
					t.Fatalf("PendingForApprover(%s): %v", who.person, err)
				}
				if holds := slices.ContainsFunc(listed, func(p requests.PendingSummary) bool { return p.RecordID == recordID }); holds == who.restricted {
					t.Errorf("PendingForApprover(%s) holds the %s request: %v, want %v", who.person, profile, holds, !who.restricted)
				}

				decision, err := f.ApplyDecision(t.Context(), recordID, true, who.person)
				if who.restricted {
					if !errors.Is(err, requests.ErrRestrictedDecider) {
						t.Fatalf("ApplyDecision(%s, approve) by %s = %+v, %v; want ErrRestrictedDecider", profile, who.person, decision, err)
					}
					if requestRows, grants, _ := heldRows(t, f.Machine); requestRows != 1 || grants != 0 {
						t.Fatalf("after the refused approval the broker holds %d request rows and %d grants; want the one request, ungranted", requestRows, grants)
					}
					return
				}
				if err != nil || decision.State != "granted" || decision.GrantID == "" {
					t.Fatalf("ApplyDecision(%s, approve) by %s = %+v, %v; want it granted", profile, who.person, decision, err)
				}
				grants, err := f.GrantsForApprover(t.Context(), who.person)
				if err != nil || !slices.ContainsFunc(grants, func(g requests.ApproverGrant) bool { return g.GrantID == decision.GrantID }) {
					t.Fatalf("GrantsForApprover(%s) = %+v, %v; want the %s grant it approved", who.person, grants, err, profile)
				}
				if err := f.RevokeByApprover(t.Context(), decision.GrantID, who.person, record.HumanActor(who.person)); err != nil {
					t.Fatalf("RevokeByApprover(%s) by %s = %v; want it ended", decision.GrantID, who.person, err)
				}
			})
		}
		t.Run(profile+", a grant its approver made before Dispatch called them restricted", func(t *testing.T) {
			f, dispatch := approverProfileFixture(t, guestApprover, projectaccess.MemberFull)
			req, err := f.create(t, profile)
			if err != nil || req.RecordID == nil {
				t.Fatalf("Create(%s) = %+v, %v; want it pending on a record", profile, req, err)
			}
			decision, err := f.ApplyDecision(t.Context(), *req.RecordID, true, guestApprover)
			if err != nil || decision.GrantID == "" {
				t.Fatalf("ApplyDecision(%s, approve) by %s while a full member = %+v, %v; want a grant", profile, guestApprover, decision, err)
			}
			dispatch.SetMemberKind(guestApprover, projectaccess.MemberRestricted)
			f.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)

			grants, err := f.GrantsForApprover(t.Context(), guestApprover)
			if err != nil || slices.ContainsFunc(grants, func(g requests.ApproverGrant) bool { return g.GrantID == decision.GrantID }) {
				t.Errorf("GrantsForApprover(%s) once restricted = %+v, %v; want the %s grant left off", guestApprover, grants, err, profile)
			}
			if err := f.RevokeByApprover(t.Context(), decision.GrantID, guestApprover, record.HumanActor(guestApprover)); !errors.Is(err, requests.ErrNotApprover) || !strings.Contains(err.Error(), "restricted member") {
				t.Fatalf("RevokeByApprover(%s) by %s once restricted = %v; want ErrNotApprover saying they are a restricted member", decision.GrantID, guestApprover, err)
			}
			var revoked bool
			if err := f.Store.Pool.QueryRow(t.Context(), `select revoked_at is not null from grants where id=$1`, decision.GrantID).Scan(&revoked); err != nil || revoked {
				t.Fatalf("grant %s after the refused revoke: revoked %v, %v; want it live", decision.GrantID, revoked, err)
			}
		})
	}
}
